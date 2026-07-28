package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
	"github.com/nickpismenkov/solana-arb-backtest/detector"
	"github.com/nickpismenkov/solana-arb-backtest/grpc"
	"github.com/nickpismenkov/solana-arb-backtest/pools"
)

func main() {
	_ = godotenv.Load()

	endpoint := os.Getenv("GRPC_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://solana-mainnet-grpc.gateway.tatum.io"
	}
	xToken := os.Getenv("GRPC_X_TOKEN")

	runMsStr := os.Getenv("RUN_MS")
	runMs := uint64(120000) // default 120s
	if runMsStr != "" {
		if val, err := strconv.ParseUint(runMsStr, 10, 64); err == nil {
			runMs = val
		}
	}

	cfg := pools.Pair()
	det := detector.NewDetector("Orca", "Raydium", cfg.OrcaFeeBps, cfg.RayFeeBps)

	fmt.Printf("\nshadow (Go) — gRPC prices, pair %s. Threshold %.1f bps. Running %ds…\n\n",
		cfg.Label,
		det.ThresholdBps,
		runMs/1000,
	)

	ticksChan := make(chan detector.Tick, 1000)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(runMs)*time.Millisecond)
	defer cancel()

	go func() {
		if err := grpc.RunGrpcFeed(ctx, endpoint, xToken, ticksChan); err != nil {
			log.Printf("gRPC feed error: %v", err)
		}
	}()

	var events []detector.ArbEvent
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()

	lastOrca := math.NaN()
	lastRay := math.NaN()
	ticksCount := uint64(0)

	for {
		select {
		case <-ctx.Done():
			printReport(runMs, ticksCount, events)
			return
		case <-heartbeat.C:
			spreadStr := "n/a"
			if !math.IsNaN(lastOrca) && !math.IsNaN(lastRay) {
				minPrice := math.Min(lastOrca, lastRay)
				spreadStr = fmt.Sprintf("%.1f bps", ((lastRay-lastOrca)/minPrice)*10000.0)
			}
			fmt.Printf("[gRPC] ticks=%d Orca=$%.4f Raydium=$%.4f spread=%s (arb>%.1fbps)\n",
				ticksCount, lastOrca, lastRay, spreadStr, det.ThresholdBps)
		case t, ok := <-ticksChan:
			if !ok {
				printReport(runMs, ticksCount, events)
				return
			}
			ticksCount++
			if t.Venue == "Orca" {
				lastOrca = t.Price
			} else {
				lastRay = t.Price
			}

			res := det.OnTick(&t)
			switch res.Type {
			case detector.ResultOpen:
				fmt.Printf("⚡ arb OPEN slot %d net %.1fbps\n", t.Slot, res.NetBps)
			case detector.ResultClose:
				ev := res.Event
				fmt.Printf("   closed %d slots / %dms · peak %.1fbps\n",
					ev.LifetimeSlots, ev.LifetimeMs, ev.PeakNetBps)
				events = append(events, *ev)
			}
		}
	}
}

func printReport(runMs uint64, ticks uint64, events []detector.ArbEvent) {
	fmt.Printf("\n──────── shadow report (%ds) ────────\n", runMs/1000)
	fmt.Printf("gRPC ticks: %d\n", ticks)
	fmt.Printf("Real fee-adjusted arbs: %d\n", len(events))

	if len(events) == 0 {
		fmt.Println("  none: pair arbed to within fees at this feed resolution.")
	} else {
		var slots []uint64
		var ms []uint64
		var nets []float64
		maxNet := -math.MaxFloat64

		for _, e := range events {
			slots = append(slots, e.LifetimeSlots)
			ms = append(ms, e.LifetimeMs)
			nets = append(nets, e.PeakNetBps)
			if e.PeakNetBps > maxNet {
				maxNet = e.PeakNetBps
			}
		}

		fmt.Printf("  peak net edge: median %.1f bps, max %.1f bps\n",
			detector.MedianF64(nets),
			maxNet,
		)
		fmt.Printf("  lifetime (reaction budget): median %d slots / %d ms\n",
			detector.MedianU64(slots),
			detector.MedianU64(ms),
		)
	}
}