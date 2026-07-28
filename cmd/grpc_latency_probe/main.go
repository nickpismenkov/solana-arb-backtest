// Measure the REAL freshness of the Yellowstone gRPC account stream — the thing
// that decides whether streaming can replace hot-path RPC polling for the
// liquidation fire loop. Subscribes to marginfi program account updates + slot
// updates, and for each account update at slot S computes the lag against the
// latest tip slot we've seen (lag 0-1 = we get updates as blocks are produced;
// lag 3+ ≈ >1s behind = too slow to fire competitively).
//
// Usage: GRPC_ENDPOINT=<url> GRPC_X_TOKEN=<tok> [SECS=30] go run ./cmd/grpc_latency_probe
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	ys "github.com/rpcpool/yellowstone-grpc/examples/golang/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"solana-arb-backtest-go/internal/envfile"
)

const marginfiProgram = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA"

type xTokenCreds struct{ token string }

func (x xTokenCreds) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{"x-token": x.token}, nil
}
func (x xTokenCreds) RequireTransportSecurity() bool { return true }

func mustEnv(key string) string {
	v, ok := os.LookupEnv(key)
	if !ok {
		fmt.Fprintf(os.Stderr, "%s must be set\n", key)
		os.Exit(1)
	}
	return v
}

func main() {
	envfile.LoadDotEnv()
	endpoint := mustEnv("GRPC_ENDPOINT")
	xToken := mustEnv("GRPC_X_TOKEN")
	rpc := mustEnv("HELIUS_RPC")
	secs := uint64(30)
	if v := os.Getenv("SECS"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			secs = n
		}
	}

	// Independent tip-slot reference: poll getSlot(processed) via RPC on a bg
	// goroutine so lag = rpc_tip − gRPC_account_slot is an ABSOLUTE latency (not
	// a self-referential max-seen proxy). A stream FRESHER than RPC yields lag ≤ 0.
	var tip int64
	httpClient := &http.Client{Timeout: 5 * time.Second}
	go func() {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getSlot",
			"params": []any{map[string]any{"commitment": "processed"}}})
		for {
			resp, err := httpClient.Post(rpc, "application/json", bytes.NewReader(body))
			if err == nil {
				var v map[string]any
				if json.NewDecoder(resp.Body).Decode(&v) == nil {
					if result, ok := v["result"].(float64); ok {
						atomic.StoreInt64(&tip, int64(result))
					}
				}
				resp.Body.Close()
			}
			time.Sleep(400 * time.Millisecond)
		}
	}()

	hostPart := endpoint
	if parts := strings.SplitN(endpoint, "/", 4); len(parts) >= 3 {
		hostPart = parts[2]
	}
	fmt.Fprintf(os.Stderr, "[grpc] connecting to %s …\n", hostPart)
	tConnect := time.Now()
	creds := credentials.NewTLS(&tls.Config{})
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithPerRPCCredentials(xTokenCreds{token: xToken}),
	}
	conn, err := grpc.NewClient(endpoint, opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[grpc] connect failed: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()
	client := ys.NewGeyserClient(conn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.Subscribe(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[grpc] subscribe failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[grpc] connected in %s\n", time.Since(tConnect))

	// Tatum's gateway tier appears to reject owner (program-wide) subscriptions,
	// so subscribe to specific high-activity accounts — marginfi USDC + BONK
	// banks (update on every deposit/borrow/interest tick). ACCOUNTS env
	// overrides with a comma-separated list.
	var watch []string
	if v := os.Getenv("ACCOUNTS"); v != "" {
		for _, s := range strings.Split(v, ",") {
			watch = append(watch, strings.TrimSpace(s))
		}
	} else {
		watch = []string{
			"2s37akK2eyBbp8DZgCm7RtsaEz8eJP3Nxd4urLHQv7yB", // marginfi USDC bank
			"DeyH7QxWvnbbaVB4zFrf4hoq7Q8z1ZT14co42BGwGtfM", // marginfi BONK bank
			"CCKtUs6Cgwo4aaQUmBPmyoApH2gUDErxNZCAntD6LYGh", // marginfi wSOL bank
		}
	}
	_ = marginfiProgram
	accounts := map[string]*ys.SubscribeRequestFilterAccounts{
		"watch": {Account: watch},
	}
	fmt.Fprintf(os.Stderr, "[grpc] watching %d specific accounts\n", len(watch))

	commitment := ys.CommitmentLevel_PROCESSED
	req := &ys.SubscribeRequest{Accounts: accounts, Commitment: &commitment}
	if err := stream.Send(req); err != nil {
		fmt.Fprintf(os.Stderr, "[grpc] subscribe send failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[grpc] subscribed (marginfi accounts, processed). measuring %ds …\n\n", secs)

	var acctUpdates uint64
	var lags []int64
	deadline := time.Now().Add(time.Duration(secs) * time.Second)

	type recvResult struct {
		msg *ys.SubscribeUpdate
		err error
	}
	recvCh := make(chan recvResult, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			recvCh <- recvResult{msg, err}
			if err != nil {
				return
			}
		}
	}()

	// Timeout each recv so the loop exits at the deadline even if the stream
	// goes silent (otherwise it would block forever with 0 updates).
loop:
	for {
		if time.Now().After(deadline) {
			break
		}
		select {
		case r := <-recvCh:
			if r.err != nil {
				if !errors.Is(r.err, io.EOF) {
					fmt.Fprintf(os.Stderr, "[grpc] stream error: %v\n", r.err)
				}
				break loop
			}
			if acc := r.msg.GetAccount(); acc != nil && acc.GetAccount() != nil {
				acctUpdates++
				t := atomic.LoadInt64(&tip)
				if t > 0 {
					lags = append(lags, t-int64(acc.GetSlot()))
				}
			}
		case <-time.After(500 * time.Millisecond):
			// 500ms tick with no message — loop and re-check deadline
		}
	}

	sort.Slice(lags, func(i, j int) bool { return lags[i] < lags[j] })
	n := len(lags)
	var med, p90, best int64
	if n > 0 {
		med = lags[n/2]
		idx := n * 9 / 10
		if idx >= n {
			idx = n - 1
		}
		p90 = lags[idx]
		best = lags[0]
	}
	fmt.Printf("═══ gRPC stream freshness (Tatum, %ds) ═══\n", secs)
	fmt.Printf("  account updates: %d  (%.0f/s)\n", acctUpdates, float64(acctUpdates)/float64(secs))
	fmt.Printf("  slot lag (RPC_tip − gRPC_account_slot): median %d, p90 %d, best %d  [≤1=fresh, 3+=slow]\n", med, p90, best)
	fmt.Println("  (note: RPC tip itself lags ~1 slot, so lag ~0-1 means gRPC keeps pace with the chain)")
	fmt.Printf("  → ~%.0fms median staleness (at ~400ms/slot)\n", float64(med)*400.0)
	switch {
	case med <= 1:
		fmt.Println("  VERDICT: FRESH — stream keeps pace with block production. Good enough to fire on.")
	case med <= 2:
		fmt.Println("  VERDICT: OK — ~1 slot behind, usable.")
	default:
		fmt.Printf("  VERDICT: SLOW — %d slots behind, too stale to fire competitively; need a better provider.\n", med)
	}
}
