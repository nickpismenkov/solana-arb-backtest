package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/nickpismenkov/solana-arb-backtest/pkg/detector"
	"github.com/nickpismenkov/solana-arb-backtest/pkg/grpc"
	"github.com/nickpismenkov/solana-arb-backtest/pkg/pools"
)

func main() {
	_ = godotenv.Load()

	endpoint := os.Getenv("GRPC_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://solana-mainnet-grpc.gateway.tatum.io"
	}
	xToken := os.Getenv("GRPC_X_TOKEN")

	runMsStr := os.Getenv("RUN_MS")
	runMs := uint64(120000)
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		if err := grpc.RunGrpcFeed(ctx, endpoint, xToken, ticksChan); err != nil {
			fmt.Printf("gRPC feed error: %v\n", err)
		}
	}()

	var events []detector.ArbEvent
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()

	deadline := time.After(time.Duration(runMs) * time.Millisecond)

	var lastOrca, lastRay float64 = math.NaN(), math.NaN()
	var ticks uint64 = 0

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

loop:
	for {
		select {
		case <-sigChan:
			break loop
		case <-deadline:
			break loop
		case <-heartbeat.C:
			spreadStr := "n/a"
			if !math.IsNaN(lastOrca) && !math.IsNaN(lastRay) {
				minPrice := lastOrca
				if lastRay < minPrice {
					minPrice = lastRay
				}
				spreadStr = fmt.Sprintf("%.1f bps", (lastRay-lastOrca)/minPrice*10000.0)
			}
			fmt.Printf("[gRPC] ticks=%d Orca=$%.4f Raydium=$%.4f spread=%s (arb>%.1fbps)\n",
				ticks, lastOrca, lastRay, spreadStr, det.ThresholdBps,
			)
		case t := <-ticksChan:
			ticks++
			if t.Venue == "Orca" {
				lastOrca = t.Price
			} else {
				lastRay = t.Price
			}

			res := det.OnTick(t)
			switch res.Type {
			case detector.TickResultOpen:
				fmt.Printf("⚡ arb OPEN slot %d net %.1fbps\n", t.Slot, res.NetBps)
			case detector.TickResultClose:
				fmt.Printf("   closed %d slots / %dms · peak %.1fbps\n",
					res.Event.LifetimeSlots, res.Event.LifetimeMs, res.Event.PeakNetBps,
				)
				events = append(events, res.Event)
			}
		}
	}

	cancel()

	fmt.Printf("\n──────── shadow report (%ds) ────────\n", runMs/1000)
	fmt.Printf("gRPC ticks: %d\n", ticks)
	fmt.Printf("Real fee-adjusted arbs: %d\n", len(events))
	if len(events) == 0 {
		fmt.Println("  none: pair arbed to within fees at this feed resolution.")
	} else {
		var slots []uint64
		var ms []uint64
		var nets []float64
		var maxNet float64 = -math.MaxFloat64

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
			detector.MedianUint64(slots),
			detector.MedianUint64(ms),
		)
	}
}
