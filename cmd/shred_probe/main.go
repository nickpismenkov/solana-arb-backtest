// Standalone ShredStream feed probe — run on the co-located box to confirm
// the fast feed is live and hitting our pools before wiring it into the
// shadow harness. `RUN_MS=60000 go run ./cmd/shred_probe`.
package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/shredstream"
)

func main() {
	envfile.LoadDotEnv()

	port := uint64(20000)
	if v := os.Getenv("SHREDSTREAM_PORT"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 16); err == nil {
			port = n
		}
	}
	runMs := uint64(60_000)
	if v := os.Getenv("RUN_MS"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			runMs = n
		}
	}
	// ALT resolution needs an RPC; set RPC_ENDPOINT to catch routed swaps.
	rpc := os.Getenv("RPC_ENDPOINT")
	altStatus := "off"
	if rpc != "" {
		altStatus = "on"
	}
	fmt.Printf("shred-probe — listening udp/%d for %ds (ALT resolution: %s)…\n\n",
		port, runMs/1000, altStatus)

	triggers := make(chan shredstream.Trigger, 256)
	go func() {
		if err := shredstream.RunFeed(uint16(port), rpc, nil, triggers); err != nil {
			fmt.Fprintf(os.Stderr, "shredstream feed error: %v\n", err)
		}
	}()

	var count uint64
	deadline := time.After(time.Duration(runMs) * time.Millisecond)
loop:
	for {
		select {
		case <-deadline:
			break loop
		case t, ok := <-triggers:
			if !ok {
				break loop
			}
			count++
			if count <= 20 || count%100 == 0 {
				sig := t.Sig
				if len(sig) > 8 {
					sig = sig[:8]
				}
				fmt.Printf("trigger #%d %s slot %d sig %s…\n", count, t.Venue, t.Slot, sig)
			}
		}
	}
	fmt.Printf("\nshred-probe: %d pool triggers in %ds\n", count, runMs/1000)
}
