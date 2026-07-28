package grpc

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/mr-tron/base58"
	"github.com/nickpismenkov/solana-arb-backtest/detector"
	"github.com/nickpismenkov/solana-arb-backtest/pools"
	pb "github.com/rpcpool/yellowstone-grpc/examples/golang/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

func RunGrpcFeed(
	ctx context.Context,
	endpoint string,
	xToken string,
	ticks chan<- detector.Tick,
) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("invalid endpoint URL: %w", err)
	}

	host := u.Host
	if host == "" {
		host = endpoint
	}
	if !strings.Contains(host, ":") {
		if u.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}

	var opts []grpc.DialOption
	if u.Scheme == "https" || strings.HasSuffix(host, ":443") {
		config := &tls.Config{InsecureSkipVerify: false}
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(config)))
	} else {
		opts = append(opts, grpc.WithInsecure())
	}

	conn, err := grpc.DialContext(ctx, host, opts...)
	if err != nil {
		return fmt.Errorf("failed to dial grpc: %w", err)
	}
	defer conn.Close()

	client := pb.NewGeyserClient(conn)

	// Add x-token to metadata if present
	if xToken != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-token", xToken)
	}

	stream, err := client.Subscribe(ctx)
	if err != nil {
		return fmt.Errorf("failed to subscribe: %w", err)
	}

	p := pools.Pair()
	req := &pb.SubscribeRequest{
		Accounts: map[string]*pb.SubscribeRequestFilterAccounts{
			"pools": {
				Account: []string{p.OrcaPool, p.RayPool},
			},
		},
	}

	if err := stream.Send(req); err != nil {
		return fmt.Errorf("failed to send subscribe request: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			resp, err := stream.Recv()
			if err != nil {
				return fmt.Errorf("stream recv error: %w", err)
			}

			update, ok := resp.UpdateOneof.(*pb.SubscribeUpdate_Account)
			if !ok || update == nil || update.Account == nil || update.Account.Account == nil {
				continue
			}

			acc := update.Account
			pk := base58.Encode(acc.Account.Pubkey)
			var venue string
			var price float64
			var found bool

			if pk == p.OrcaPool {
				venue = "Orca"
				price, found = pools.OrcaPrice(acc.Account.Data)
			} else if pk == p.RayPool {
				venue = "Raydium"
				price, found = pools.RayClmmPrice(acc.Account.Data)
			}

			if found {
				ticks <- detector.Tick{
					Venue: venue,
					Price: price,
					Slot:  acc.Slot,
					TsMs:  uint64(time.Now().UnixNano() / int64(time.Millisecond)),
				}
			}
		}
	}
}