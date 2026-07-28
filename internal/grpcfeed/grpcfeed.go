// Package grpcfeed implements the Yellowstone gRPC account-subscription feed
// → price Ticks. The swappable measurement feed (a ShredStream feed emits the
// same Tick later). Sends ticks over a channel so the harness owns the
// detector loop.
package grpcfeed

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"github.com/gagliardetto/solana-go/base58"
	ys "github.com/rpcpool/yellowstone-grpc/examples/golang/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"solana-arb-backtest-go/internal/detector"
	"solana-arb-backtest-go/internal/pools"
)

func nowMs() uint64 { return uint64(time.Now().UnixMilli()) }

type xTokenCreds struct{ token string }

func (x xTokenCreds) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{"x-token": x.token}, nil
}
func (x xTokenCreds) RequireTransportSecurity() bool { return true }

// RunGRPCFeed connects to a Yellowstone Geyser gRPC endpoint, subscribes to
// our two pool accounts, and pushes decoded price Ticks to tx until ctx is
// canceled or the stream ends.
func RunGRPCFeed(ctx context.Context, endpoint, xToken string, tx chan<- detector.Tick) error {
	creds := credentials.NewTLS(&tls.Config{})
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
	if xToken != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(xTokenCreds{token: xToken}))
	}
	conn, err := grpc.NewClient(endpoint, opts...)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	client := ys.NewGeyserClient(conn)
	stream, err := client.Subscribe(ctx)
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	commitment := ys.CommitmentLevel_PROCESSED
	req := &ys.SubscribeRequest{
		Accounts: map[string]*ys.SubscribeRequestFilterAccounts{
			"pools": {Account: []string{pools.Pair().OrcaPool, pools.Pair().RayPool}},
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
		if acc == nil || acc.GetAccount() == nil {
			continue
		}
		info := acc.GetAccount()
		slot := acc.GetSlot()
		pk := base58.Encode(info.GetPubkey())
		var venue string
		var price float64
		var ok bool
		switch pk {
		case pools.Pair().OrcaPool:
			venue = "Orca"
			price, ok = pools.OrcaPrice(info.GetData())
		case pools.Pair().RayPool:
			venue = "Raydium"
			price, ok = pools.RayClmmPrice(info.GetData())
		default:
			continue
		}
		if !ok {
			continue
		}
		select {
		case tx <- detector.Tick{Venue: venue, Price: price, Slot: slot, TsMs: nowMs()}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
