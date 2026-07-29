// Package grpc streams the Yellowstone gRPC account-subscription feed into
// price detector.Ticks. The swappable measurement feed (a ShredStream feed
// will emit the same Tick shape later). Sends ticks over a channel so the
// harness owns the detector loop.
package grpc

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"github.com/mr-tron/base58"
	ggrpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	"solana-arb-backtest/detector"
	"solana-arb-backtest/pb"
	"solana-arb-backtest/pools"
)

func nowMs() uint64 {
	return uint64(time.Now().UnixMilli())
}

// RunGRPCFeed dials endpoint, subscribes to account updates for both
// configured pools, and streams decoded price Ticks to tx until ctx is
// cancelled or the stream errors.
func RunGRPCFeed(ctx context.Context, endpoint, xToken string, tx chan<- detector.Tick) error {
	creds := credentials.NewTLS(&tls.Config{})
	conn, err := ggrpc.NewClient(endpoint, ggrpc.WithTransportCredentials(creds))
	if err != nil {
		return fmt.Errorf("dial %s: %w", endpoint, err)
	}
	defer conn.Close()

	client := pb.NewGeyserClient(conn)
	streamCtx := ctx
	if xToken != "" {
		streamCtx = metadata.AppendToOutgoingContext(ctx, "x-token", xToken)
	}
	stream, err := client.Subscribe(streamCtx)
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	cfg := pools.Pair()
	commitment := pb.CommitmentLevel_PROCESSED
	req := &pb.SubscribeRequest{
		Accounts: map[string]*pb.SubscribeRequestFilterAccounts{
			"pools": {Account: []string{cfg.OrcaPool, cfg.RayPool}},
		},
		Commitment: &commitment,
	}
	if err := stream.Send(req); err != nil {
		return fmt.Errorf("send subscribe request: %w", err)
	}

	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		acc := msg.GetAccount()
		if acc == nil {
			continue
		}
		info := acc.GetAccount()
		if info == nil {
			continue
		}
		pk := base58.Encode(info.Pubkey)
		var venue string
		var price float64
		var ok bool
		switch pk {
		case cfg.OrcaPool:
			venue = "Orca"
			price, ok = pools.OrcaPrice(info.Data)
		case cfg.RayPool:
			venue = "Raydium"
			price, ok = pools.RayClmmPrice(info.Data)
		default:
			continue
		}
		if !ok {
			continue
		}
		select {
		case tx <- detector.Tick{Venue: venue, Price: price, Slot: acc.Slot, TsMs: nowMs()}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
