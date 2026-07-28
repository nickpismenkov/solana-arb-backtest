// Definitive Triton liveness test: subscribe to SLOTS + a few specific accounts
// on one connection and count each separately. Slots tick ~2.5×/s unconditionally
// — so this discriminates:
//   - slots > 0, accounts = 0  → connection & stream fine; account sub is the issue
//   - slots = 0, accounts = 0  → whole stream throttled/banned (transport is up but
//     no data flows) → it's the rate-limit penalty box
//
// Usage: GRPC_ENDPOINT=<url> GRPC_X_TOKEN=<tok> [SECS=15] go run ./cmd/grpc_ping
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	ys "github.com/rpcpool/yellowstone-grpc/examples/golang/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"solana-arb-backtest-go/internal/envfile"
)

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
	secs := uint64(15)
	if v := os.Getenv("SECS"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			secs = n
		}
	}

	fmt.Fprintln(os.Stderr, "[ping] connecting …")
	t0 := time.Now()
	creds := credentials.NewTLS(&tls.Config{})
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithPerRPCCredentials(xTokenCreds{token: xToken}),
	}
	conn, err := grpc.NewClient(endpoint, opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ping] connect failed: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()
	client := ys.NewGeyserClient(conn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.Subscribe(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ping] subscribe failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[ping] connected in %s\n", time.Since(t0))

	filterByCommitment := false
	slots := map[string]*ys.SubscribeRequestFilterSlots{
		"s": {FilterByCommitment: &filterByCommitment},
	}

	// Default includes the Clock sysvar — it updates EVERY slot, so if even Clock
	// yields 0 account updates, account subscriptions are broadly not delivering.
	var acctList []string
	if v := os.Getenv("ACCOUNTS"); v != "" {
		for _, s := range strings.Split(v, ",") {
			acctList = append(acctList, strings.TrimSpace(s))
		}
	} else {
		acctList = []string{
			"SysvarC1ock11111111111111111111111111111111",  // Clock — ticks every slot
			"2s37akK2eyBbp8DZgCm7RtsaEz8eJP3Nxd4urLHQv7yB", // marginfi USDC bank
			"DeyH7QxWvnbbaVB4zFrf4hoq7Q8z1ZT14co42BGwGtfM", // marginfi BONK bank
			"CCKtUs6Cgwo4aaQUmBPmyoApH2gUDErxNZCAntD6LYGh", // marginfi wSOL bank
		}
	}
	var commitment ys.CommitmentLevel
	switch os.Getenv("COMMITMENT") {
	case "confirmed":
		commitment = ys.CommitmentLevel_CONFIRMED
	case "finalized":
		commitment = ys.CommitmentLevel_FINALIZED
	default:
		commitment = ys.CommitmentLevel_PROCESSED
	}
	fmt.Fprintf(os.Stderr, "[ping] %d accounts, commitment=%s\n", len(acctList), commitment)

	accounts := map[string]*ys.SubscribeRequestFilterAccounts{
		"a": {Account: acctList},
	}
	req := &ys.SubscribeRequest{Slots: slots, Accounts: accounts, Commitment: &commitment}
	if err := stream.Send(req); err != nil {
		fmt.Fprintf(os.Stderr, "[ping] subscribe send failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[ping] subscribed (slots + %d accounts, processed). listening %ds …\n\n", len(acctList), secs)

	var nSlot, nAcct, nOther uint64
	deadline := time.Now().Add(time.Duration(secs) * time.Second)

	type recvResult struct {
		msg *ys.SubscribeUpdate
		err error
	}
	recvCh := make(chan recvResult, 1)
	recvLoop := func() {
		for {
			msg, err := stream.Recv()
			recvCh <- recvResult{msg, err}
			if err != nil {
				return
			}
		}
	}
	go recvLoop()

loop:
	for {
		if time.Now().After(deadline) {
			break
		}
		select {
		case r := <-recvCh:
			if r.err != nil {
				if errors.Is(r.err, io.EOF) {
					fmt.Fprintln(os.Stderr, "[ping] stream closed by server")
				} else {
					fmt.Fprintf(os.Stderr, "[ping] stream error: %v\n", r.err)
				}
				break loop
			}
			switch {
			case r.msg.GetSlot() != nil:
				nSlot++
			case r.msg.GetAccount() != nil:
				nAcct++
			case r.msg.GetPing() != nil:
				// ignore
			default:
				nOther++
			}
		case <-time.After(500 * time.Millisecond):
			// tick and re-check deadline
		}
	}

	fmt.Printf("\n═══ Triton liveness (%ds) ═══\n", secs)
	fmt.Printf("  SLOT updates:    %d  (%.1f/s)\n", nSlot, float64(nSlot)/float64(secs))
	fmt.Printf("  ACCOUNT updates: %d\n", nAcct)
	fmt.Printf("  other:           %d\n", nOther)
	if nSlot > 0 && nAcct > 0 {
		fmt.Println("  VERDICT: ✅ FULLY LIVE — stream + account subscription both delivering.")
	} else if nSlot > 0 {
		fmt.Println("  VERDICT: ⚠ stream is LIVE (slots flow) but ACCOUNT updates = 0 → subscription/filter issue, NOT a ban.")
	} else {
		fmt.Println("  VERDICT: ❌ stream SILENT (0 slots too) — connection up but no data → rate-limit penalty box still active.")
	}
}
