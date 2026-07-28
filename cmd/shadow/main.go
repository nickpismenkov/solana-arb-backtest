// shadow (Go) — reproduces the TS/Rust `shadow` harness: gRPC prices → the
// fee-adjusted Detector → reaction-budget report, with a live price heartbeat.
package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strconv"
	"time"

	"solana-arb-backtest-go/internal/detector"
	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/grpcfeed"
	"solana-arb-backtest-go/internal/pools"
)

func main() {
	envfile.LoadDotEnv()

	endpoint := os.Getenv("GRPC_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://solana-mainnet-grpc.gateway.tatum.io"
	}
	xToken := os.Getenv("GRPC_X_TOKEN")
	if xToken == "" {
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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	ticks := make(chan detector.Tick, 256)
	go func() {
		if err := grpcfeed.RunGRPCFeed(ctx, endpoint, xToken, ticks); err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "gRPC feed error: %v\n", err)
		}
	}()

	var events []detector.ArbEvent
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	deadline := time.NewTimer(time.Duration(runMs) * time.Millisecond)
	defer deadline.Stop()

	var lastOrca, lastRay float64 = math.NaN(), math.NaN()
	var tickCount uint64

loop:
	for {
		select {
		case <-deadline.C:
			break loop
		case <-heartbeat.C:
			spread := "n/a"
			if isFinite(lastOrca) && isFinite(lastRay) {
				m := lastOrca
				if lastRay < m {
					m = lastRay
				}
				spread = fmt.Sprintf("%.1f bps", (lastRay-lastOrca)/m*10000.0)
			}
			fmt.Printf("[gRPC] ticks=%d Orca=$%.4f Raydium=$%.4f spread=%s (arb>%gbps)\n",
				tickCount, lastOrca, lastRay, spread, det.ThresholdBps)
		case t, ok := <-ticks:
			if !ok {
				break loop
			}
			tickCount++
			if t.Venue == "Orca" {
				lastOrca = t.Price
			} else {
				lastRay = t.Price
			}
			res := det.OnTick(t)
			switch res.Kind {
			case detector.ResultOpen:
				fmt.Printf("⚡ arb OPEN slot %d net %.1fbps\n", t.Slot, res.NetBps)
			case detector.ResultClose:
				ev := res.Event
				fmt.Printf("   closed %d slots / %dms · peak %.1fbps\n", ev.LifetimeSlots, ev.LifetimeMs, ev.PeakNetBps)
				events = append(events, ev)
			}
		}
	}

	fmt.Printf("\n──────── shadow report (%ds) ────────\n", runMs/1000)
	fmt.Printf("gRPC ticks: %d\n", tickCount)
	fmt.Printf("Real fee-adjusted arbs: %d\n", len(events))
	if len(events) == 0 {
		fmt.Println("  none: pair arbed to within fees at this feed resolution.")
	} else {
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
		fmt.Printf("  lifetime (reaction budget): median %d slots / %d ms\n", detector.MedianU64(slots), detector.MedianU64(ms))
	}
}

func isFinite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }
