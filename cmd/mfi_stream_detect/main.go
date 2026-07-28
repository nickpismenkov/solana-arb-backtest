// Step 1 of the streaming migration: prove Dragon's Mouth OWNER-subscription
// streams the marginfi program's accounts and populates a LIVE in-memory loan
// book — the thing that removes the hot-path RPC poll. Read-only: decodes each
// account update into MarginfiAccount / Bank maps, counts the update rate, and
// reports freshness (slot lag vs an independent RPC tip). No firing.
//
// Usage: GRPC_ENDPOINT=<triton-url-with-token> GRPC_X_TOKEN=<tok> HELIUS_RPC=<url>
//
//	[SECS=30] go run ./cmd/mfi_stream_detect
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gagliardetto/solana-go"
	ys "github.com/rpcpool/yellowstone-grpc/examples/golang/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/liquidation"
)

const marginfiProgram = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA"

var httpClient = &http.Client{Timeout: 15 * time.Second}

func rpc(endpoint string, body map[string]any) (map[string]any, bool) {
	for attempt := 0; attempt < 4; attempt++ {
		b, _ := json.Marshal(body)
		resp, err := httpClient.Post(endpoint, "application/json", bytes.NewReader(b))
		if err == nil {
			var v map[string]any
			decErr := json.NewDecoder(resp.Body).Decode(&v)
			resp.Body.Close()
			if decErr == nil {
				return v, true
			}
		}
		time.Sleep(time.Duration(400<<attempt) * time.Millisecond)
	}
	return nil, false
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}
func asArray(v any) []any {
	a, _ := v.([]any)
	return a
}
func asStr(v any) string {
	s, _ := v.(string)
	return s
}

type xTokenCreds struct{ token string }

func (x xTokenCreds) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{"x-token": x.token}, nil
}
func (x xTokenCreds) RequireTransportSecurity() bool { return true }

func main() {
	envfile.LoadDotEnv()

	endpoint := os.Getenv("GRPC_ENDPOINT")
	if endpoint == "" {
		fmt.Fprintln(os.Stderr, "GRPC_ENDPOINT")
		os.Exit(1)
	}
	xToken := os.Getenv("GRPC_X_TOKEN")
	if xToken == "" {
		fmt.Fprintln(os.Stderr, "GRPC_X_TOKEN")
		os.Exit(1)
	}
	rpcEndpoint := os.Getenv("HELIUS_RPC")
	if rpcEndpoint == "" {
		fmt.Fprintln(os.Stderr, "HELIUS_RPC")
		os.Exit(1)
	}
	secs := uint64(30)
	if v := os.Getenv("SECS"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			secs = n
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// Independent tip-slot reference (bg goroutine) -> absolute freshness of the stream.
	var tip atomic.Uint64
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if v, ok := rpc(rpcEndpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getSlot",
				"params": []any{map[string]any{"commitment": "processed"}}}); ok {
				if s, ok := v["result"].(float64); ok {
					tip.Store(uint64(s))
				}
			}
			time.Sleep(400 * time.Millisecond)
		}
	}()

	// The LIVE loan book — populated purely from the stream. This is what the
	// fire loop will read instead of polling+re-fetching on the hot path.
	var mu sync.RWMutex
	accounts := map[solana.PublicKey]*liquidation.MarginfiAccount{}
	banks := map[solana.PublicKey]*liquidation.Bank{}

	// Triton tier3 rejects owner (program-wide) subscriptions (0 updates), but
	// supports thousands of SPECIFIC accounts on one connection. For liquidations
	// we know our set: scan the marginfi accounts (pubkeys only) and subscribe to
	// a capped batch — the real migration pattern (subscribe the watch-set).
	maxSub := 2000
	if v := os.Getenv("MAX_SUB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			maxSub = n
		}
	}
	fmt.Fprintln(os.Stderr, "[stream] scanning marginfi account pubkeys to subscribe …")
	scan, ok := rpc(rpcEndpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{marginfiProgram, map[string]any{"encoding": "base64", "dataSlice": map[string]any{"offset": 0, "length": 0},
			"filters": []any{map[string]any{"dataSize": liquidation.MASize}}}}})
	if !ok {
		fmt.Fprintln(os.Stderr, "[stream] scan failed")
		os.Exit(1)
	}
	var subAccounts []string
	for _, ev := range asArray(scan["result"]) {
		if len(subAccounts) >= maxSub {
			break
		}
		if pk := asStr(asMap(ev)["pubkey"]); pk != "" {
			subAccounts = append(subAccounts, pk)
		}
	}
	// Prepend the 3 known high-activity banks (update every slot-ish) so we can
	// tell a subscription-size limit (no bank updates either) from quiet
	// borrowers (bank updates flow, borrowers just idle).
	knownBanks := []string{
		"2s37akK2eyBbp8DZgCm7RtsaEz8eJP3Nxd4urLHQv7yB",
		"DeyH7QxWvnbbaVB4zFrf4hoq7Q8z1ZT14co42BGwGtfM",
		"CCKtUs6Cgwo4aaQUmBPmyoApH2gUDErxNZCAntD6LYGh",
	}
	subAccounts = append(append([]string{}, knownBanks...), subAccounts...)
	fmt.Fprintf(os.Stderr, "[stream] subscribing to %d accounts (3 active banks + %d borrowers)\n",
		len(subAccounts), len(subAccounts)-3)

	fmt.Fprintln(os.Stderr, "[stream] connecting to Dragon's Mouth …")
	creds := credentials.NewTLS(&tls.Config{})
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds), grpc.WithPerRPCCredentials(xTokenCreds{token: xToken})}
	conn, err := grpc.NewClient(endpoint, opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[stream] dial: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	client := ys.NewGeyserClient(conn)
	stream, err := client.Subscribe(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[stream] subscribe: %v\n", err)
		os.Exit(1)
	}

	commitment := ys.CommitmentLevel_PROCESSED
	req := &ys.SubscribeRequest{
		Accounts: map[string]*ys.SubscribeRequestFilterAccounts{
			"mfi": {Account: subAccounts, Owner: []string{}, Filters: []*ys.SubscribeRequestFilterAccountsFilter{}},
		},
		Commitment: &commitment,
	}
	if err := stream.Send(req); err != nil {
		fmt.Fprintf(os.Stderr, "[stream] send subscribe: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[stream] owner-subscribed marginfi (processed). building live loan book for %ds …\n\n", secs)

	var updates, acctDecodes, bankDecodes uint64
	var lags []int64
	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	lastLog := time.Now()

	// msgCh decouples stream.Recv() (blocking) from the deadline/log-tick loop.
	type recvResult struct {
		msg *ys.SubscribeUpdate
		err error
	}
	msgCh := make(chan recvResult, 64)
	go func() {
		for {
			msg, err := stream.Recv()
			select {
			case msgCh <- recvResult{msg, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

loop:
	for {
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			break loop
		case rr := <-msgCh:
			if rr.err != nil {
				fmt.Fprintf(os.Stderr, "[stream] error: %v\n", rr.err)
				break loop
			}
			if acc := rr.msg.GetAccount(); acc != nil {
				updates++
				t := tip.Load()
				if t > 0 {
					lags = append(lags, int64(t)-int64(acc.GetSlot()))
				}
				if info := acc.GetAccount(); info != nil {
					pk := solana.PublicKeyFromBytes(info.GetPubkey())
					{
						data := info.GetData()
						if len(data) == liquidation.MASize {
							if a, ok := liquidation.DecodeMarginfiAccount(data); ok {
								mu.Lock()
								accounts[pk] = a
								mu.Unlock()
								acctDecodes++
							}
						} else if b, ok := liquidation.DecodeBank(data); ok {
							mu.Lock()
							banks[pk] = b
							mu.Unlock()
							bankDecodes++
						}
					}
				}
			}
		case <-time.After(500 * time.Millisecond):
		}
		if time.Since(lastLog) >= 5*time.Second {
			mu.RLock()
			na, nb := len(accounts), len(banks)
			mu.RUnlock()
			fmt.Fprintf(os.Stderr, "[stream] %d updates | live loan book: %d accounts, %d banks\n", updates, na, nb)
			lastLog = time.Now()
		}
	}

	sort.Slice(lags, func(i, j int) bool { return lags[i] < lags[j] })
	n := len(lags)
	med := int64(0)
	if n > 0 {
		med = lags[n/2]
	}
	mu.RLock()
	na, nb := len(accounts), len(banks)
	mu.RUnlock()

	fmt.Printf("\n═══ marginfi streaming detector (Dragon's Mouth, %ds) ═══\n", secs)
	fmt.Printf("  account updates received: %d  (%.0f/s)\n", updates, float64(updates)/float64(secs))
	fmt.Printf("  decoded into live map:    %d account-writes, %d bank-writes\n", acctDecodes, bankDecodes)
	fmt.Printf("  live loan book now holds: %d accounts, %d banks\n", na, nb)
	fmt.Printf("  stream freshness (slot lag vs RPC tip): median %d  [<=1 or negative = leads block production]\n", med)
	switch {
	case updates > 0 && med <= 1:
		fmt.Println("  VERDICT: ✅ live loan book populates from the stream, fresh. Hot-path RPC poll is replaceable.")
	case updates == 0:
		fmt.Println("  VERDICT: ❌ no updates — owner subscription not delivering (check tier/endpoint).")
	default:
		fmt.Printf("  VERDICT: ⚠ updates flowing but stale (median %d slots) — investigate.\n", med)
	}
}
