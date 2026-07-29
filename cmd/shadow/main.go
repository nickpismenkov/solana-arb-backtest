// Command shadow reproduces the original `shadow` harness: a live price feed
// into the fee-adjusted Detector, with a reaction-budget report and a live
// heartbeat.
package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/signal"
	"syscall"
	"time"

	"arbengine/internal/config"
	"arbengine/internal/detector"
	"arbengine/internal/grpcfeed"
	"arbengine/internal/pools"
)

func main() {
	config.LoadDotenv()

	rpcEndpoint := config.EnvOr("RPC_ENDPOINT", config.EnvOr("GRPC_ENDPOINT", "https://api.mainnet-beta.solana.com"))
	runMs := config.EnvInt("RUN_MS", 120_000)

	cfg := pools.Pair()
	det := detector.New("Orca", "Raydium", cfg.OrcaFeeBps, cfg.RayFeeBps)
	fmt.Printf("\nshadow (Go) — price feed, pair %s. Threshold %g bps. Running %ds...\n\n",
		cfg.Label, det.ThresholdBps, runMs/1000)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	deadline, cancelDeadline := context.WithTimeout(ctx, time.Duration(runMs)*time.Millisecond)
	defer cancelDeadline()

	ticks := make(chan detector.Tick, 256)
	go func() {
		if err := grpcfeed.Run(deadline, rpcEndpoint, ticks); err != nil && err != context.DeadlineExceeded && err != context.Canceled {
			fmt.Fprintf(os.Stderr, "feed error: %v\n", err)
		}
	}()

	var events []detector.ArbEvent
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()

	lastOrca, lastRay := math.NaN(), math.NaN()
	var tickCount uint64

loop:
	for {
		select {
		case <-deadline.Done():
			break loop
		case <-heartbeat.C:
			spread := "n/a"
			if !math.IsNaN(lastOrca) && !math.IsNaN(lastRay) {
				spread = fmt.Sprintf("%.1f bps", (lastRay-lastOrca)/math.Min(lastOrca, lastRay)*10_000.0)
			}
			fmt.Printf("[feed] ticks=%d Orca=$%.4f Raydium=$%.4f spread=%s (arb>%gbps)\n",
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
			result := det.OnTick(t)
			switch result.Kind {
			case detector.TickOpen:
				fmt.Printf("⚡ arb OPEN slot %d net %.1fbps\n", t.Slot, result.NetBps)
			case detector.TickClose:
				ev := result.Event
				fmt.Printf("   closed %d slots / %dms · peak %.1fbps\n", ev.LifetimeSlots, ev.LifetimeMs, ev.PeakNetBps)
				events = append(events, ev)
			}
		}
	}

	fmt.Printf("\n──────── shadow report (%ds) ────────\n", runMs/1000)
	fmt.Printf("feed ticks: %d\n", tickCount)
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
	fmt.Printf("  lifetime (reaction budget): median %d slots / %d ms\n", detector.MedianU64(slots), detector.MedianU64(ms))
}
