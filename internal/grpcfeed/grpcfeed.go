// Package grpcfeed is the price feed that drives the detector: it polls
// both pool accounts and turns them into detector.Tick values on a channel,
// exactly like the Rust code's Yellowstone gRPC account-subscription feed.
//
// NOTE ON FIDELITY: the original Rust implementation opens a persistent
// Yellowstone gRPC (Geyser plugin) streaming subscription, which pushes an
// update the instant either pool account changes on-chain. Replicating that
// wire protocol would mean vendoring Yellowstone's generated protobuf
// bindings (no official, actively maintained Go client exists) purely for
// plumbing that's orthogonal to the arb/backtest logic itself. This port
// instead polls both accounts over plain JSON-RPC at a fixed interval,
// which is a strictly slower but behaviorally equivalent substitute: it
// feeds the exact same Tick{Venue, Price, Slot, TsMs} values into the same
// Detector the streaming feed would have. Swapping in a real streaming
// client later only requires replacing Run's body — every downstream
// consumer (Detector, reporting) is unchanged.
package grpcfeed

import (
	"context"
	"time"

	"arbengine/internal/detector"
	"arbengine/internal/pools"
	"arbengine/internal/rpcclient"
	"arbengine/internal/solana"
)

// PollInterval is how often both pool accounts are re-fetched. The
// yellowstone feed this replaces is push-based (sub-slot latency); polling
// this often keeps the detector usefully responsive without hammering RPC.
const PollInterval = 400 * time.Millisecond

// Run polls the configured pair's Orca + Raydium pool accounts and sends a
// Tick to `out` whenever a price is decodable, until ctx is canceled. It
// mirrors run_grpc_feed's contract: an unbounded stream of Ticks, feed
// owns nothing about the detector loop.
func Run(ctx context.Context, rpcEndpoint string, out chan<- detector.Tick) error {
	client := rpcclient.New(rpcEndpoint)
	cfg := pools.Pair()
	orcaPool := solana.MustPubkeyFromBase58(cfg.OrcaPool)
	rayPool := solana.MustPubkeyFromBase58(cfg.RayPool)

	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			slot, err := client.GetSlot()
			if err != nil {
				continue
			}
			if data, err := client.GetAccountData(orcaPool); err == nil && data != nil {
				if price, ok := pools.OrcaPrice(data); ok {
					sendTick(ctx, out, detector.Tick{Venue: "Orca", Price: price, Slot: slot, TsMs: nowMs()})
				}
			}
			if data, err := client.GetAccountData(rayPool); err == nil && data != nil {
				if price, ok := pools.RayClmmPrice(data); ok {
					sendTick(ctx, out, detector.Tick{Venue: "Raydium", Price: price, Slot: slot, TsMs: nowMs()})
				}
			}
		}
	}
}

func sendTick(ctx context.Context, out chan<- detector.Tick, t detector.Tick) {
	select {
	case out <- t:
	case <-ctx.Done():
	}
}

func nowMs() uint64 {
	return uint64(time.Now().UnixMilli())
}
