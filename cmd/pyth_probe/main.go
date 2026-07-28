// Pyth Lazer feed probe — connects with PYTH_LAZER_TOKEN, subscribes to a few
// feeds, and prints live prices from the shared table for ~10s. Confirms the
// pyth feed package works end-to-end (auth, subscribe, parse, scale).
//
// Usage: PYTH_LAZER_TOKEN=<key> [FEED_IDS=6,7] go run ./cmd/pyth_probe
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/pyth"
)

func main() {
	envfile.LoadDotEnv()

	token := os.Getenv("PYTH_LAZER_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "PYTH_LAZER_TOKEN (.env)")
		os.Exit(1)
	}
	feedIDsStr := os.Getenv("FEED_IDS")
	if feedIDsStr == "" {
		feedIDsStr = "6,7"
	}
	var feedIDs []uint32
	for _, s := range strings.Split(feedIDsStr, ",") {
		s = strings.TrimSpace(s)
		if n, err := strconv.ParseUint(s, 10, 32); err == nil {
			feedIDs = append(feedIDs, uint32(n))
		}
	}

	names := map[uint32]string{1: "BTC", 2: "ETH", 6: "SOL", 7: "USDC"}

	fmt.Fprintf(os.Stderr, "[pyth_probe] subscribing to feeds %v …\n", feedIDs)
	table := pyth.NewTable()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pyth.SpawnLazer(ctx, token, feedIDs, table)

	for tick := 0; tick < 10; tick++ {
		time.Sleep(1000 * time.Millisecond)
		line := fmt.Sprintf("t+%ds  ", tick)
		for _, id := range feedIDs {
			name, ok := names[id]
			if !ok {
				name = "?"
			}
			if p, ok := pyth.Get(table, id); ok {
				line += fmt.Sprintf("%s(%d)=$%.4f [%dµs]  ", name, id, p.Price, p.TsUs)
			} else {
				line += fmt.Sprintf("%s(%d)=…  ", name, id)
			}
		}
		fmt.Println(line)
	}
	fmt.Fprintln(os.Stderr, "[pyth_probe] done")
}
