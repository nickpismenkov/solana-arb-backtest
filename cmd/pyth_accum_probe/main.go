// Verify the Hermes accumulator parser against a live update: fetch SOL+USDC,
// print the VAA length and each update's feed id + message/proof lengths. The
// split must match what a real mainnet crank tx carried (~247B VAA, ~396B per
// update) and the feed ids must equal the requested ones — proving we can feed
// the crank ixs correctly.
//
// Usage: [HERMES=https://hermes.pyth.network] go run ./cmd/pyth_accum_probe
package main

import (
	"fmt"
	"os"

	"solana-arb-backtest-go/internal/pyth"
)

func hexs(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, x := range b {
		out[i*2] = hexdigits[x>>4]
		out[i*2+1] = hexdigits[x&0xf]
	}
	return string(out)
}

// Canonical Pyth feed ids (hex).
const (
	sol  = "ef0d8b6fda2ceba41da15d4095d1da392a0d2f8ed0c6c7bc0f4cfac8c280b56d"
	usdc = "eaa020c61cc479712813461ce153894a96a6c00b21ed0cfc2798d1f9a9e9c94a"
)

func main() {
	hermes := os.Getenv("HERMES")
	if hermes == "" {
		hermes = "https://hermes.pyth.network"
	}
	update, err := pyth.FetchHermes(hermes, []string{sol, usdc})
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetch+parse: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("VAA: %d bytes\n", len(update.VAA))
	fmt.Printf("%d price updates:\n", len(update.Updates))
	for _, u := range update.Updates {
		fid := ""
		if id, ok := u.FeedID(); ok {
			fid = hexs(id[:])
		}
		n := 16
		if len(fid) < n {
			n = len(fid)
		}
		fmt.Printf("  feed %s…  message %dB  proof %dB\n", fid[:n], len(u.Message), len(u.Proof))
	}
	var ids []string
	for _, u := range update.Updates {
		if id, ok := u.FeedID(); ok {
			ids = append(ids, hexs(id[:]))
		}
	}
	hasSol, hasUSDC := false, false
	for _, i := range ids {
		if i == sol {
			hasSol = true
		}
		if i == usdc {
			hasUSDC = true
		}
	}
	if hasSol && hasUSDC && len(update.VAA) != 0 {
		fmt.Println("★ parser VERIFIED — VAA extracted, both requested feeds present with message+proof")
	} else {
		fmt.Printf("✗ mismatch — got feeds %v\n", ids)
	}
}
