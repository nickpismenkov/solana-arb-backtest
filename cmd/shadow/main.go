// Command shadow reproduces the TS `shadow` harness: gRPC prices -> the
// fee-adjusted Detector -> reaction-budget report, with a live price
// heartbeat. Testable locally against Tatum; the ShredStream feed + shred-time
// pricing land in later PRs.
package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"

	"solana-arb-backtest/detector"
	ygrpc "solana-arb-backtest/grpc"
	"solana-arb-backtest/pools"
)

func main() {
	_ = godotenv.Load()

	// Go's net/tls does not need rustls' explicit process-level crypto
	// provider install, so there is no equivalent of the Rust
	// rustls::crypto::ring::default_provider().install_default() call here.

	endpoint := os.Getenv("GRPC_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://solana-mainnet-grpc.gateway.tatum.io"
	}
	xToken, ok := os.LookupEnv("GRPC_X_TOKEN")
	if !ok {
		fmt.Fprintln(os.Stderr, "set GRPC_X_TOKEN in .env")
		os.Exit(1)
	}
	runMs := uint64(120_000)
	if v := os.Getenv("RUN_MS"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			runMs = n
		}
	}

	cfg := pools.Pair()
	det := detector.New("Orca", "Raydium", cfg.OrcaFeeBps, cfg.RayFeeBps)
	fmt.Printf("\nshadow (Go) — gRPC prices, pair %s. Threshold %g bps. Running %ds…\n\n",
		cfg.Label, det.ThresholdBps, runMs/1000)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(runMs)*time.Millisecond)
	defer cancel()

	tx := make(chan detector.Tick, 1024)
	go func() {
		if err := ygrpc.RunGRPCFeed(ctx, endpoint, xToken, tx); err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "gRPC feed error: %v\n", err)
		}
	}()

	var events []detector.ArbEvent
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()

	lastOrca, lastRay := math.NaN(), math.NaN()
	var ticks uint64

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-heartbeat.C:
			spread := "n/a"
			if !math.IsNaN(lastOrca) && !math.IsNaN(lastRay) {
				spread = fmt.Sprintf("%.1f bps", (lastRay-lastOrca)/math.Min(lastOrca, lastRay)*10_000.0)
			}
			fmt.Printf("[gRPC] ticks=%d Orca=$%.4f Raydium=$%.4f spread=%s (arb>%gbps)\n",
				ticks, lastOrca, lastRay, spread, det.ThresholdBps)
		case t, chOk := <-tx:
			if !chOk {
				break loop
			}
			ticks++
			if t.Venue == "Orca" {
				lastOrca = t.Price
			} else {
				lastRay = t.Price
			}
			res := det.OnTick(&t)
			switch res.Kind {
			case detector.TickResultOpen:
				fmt.Printf("⚡ arb OPEN slot %d net %.1fbps\n", t.Slot, res.NetBps)
			case detector.TickResultClose:
				ev := res.Event
				fmt.Printf("   closed %d slots / %dms · peak %.1fbps\n", ev.LifetimeSlots, ev.LifetimeMs, ev.PeakNetBps)
				events = append(events, ev)
			}
		}
	}

	fmt.Printf("\n──────── shadow report (%ds) ────────\n", runMs/1000)
	fmt.Printf("gRPC ticks: %d\n", ticks)
	fmt.Printf("Real fee-adjusted arbs: %d\n", len(events))
	if len(events) == 0 {
		fmt.Println("  none: pair arbed to within fees at this feed resolution.")
		return
	}
	slots := make([]uint64, len(events))
	ms := make([]uint64, len(events))
	nets := make([]float64, len(events))
	maxNet := math.Inf(-1)
	for i, e := range events {
		slots[i] = e.LifetimeSlots
		ms[i] = e.LifetimeMs
		nets[i] = e.PeakNetBps
		if e.PeakNetBps > maxNet {
			maxNet = e.PeakNetBps
		}
	}
	fmt.Printf("  peak net edge: median %.1f bps, max %.1f bps\n", detector.MedianF64(nets), maxNet)
	fmt.Printf("  lifetime (reaction budget): median %d slots / %d ms\n", detector.MedianU128(slots), detector.MedianU128(ms))
}
