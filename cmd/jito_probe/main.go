// Connectivity check for the Jito block engine (safe — read-only, no bundle
// sent, no money). Confirms we can reach the region-matched block engine and
// fetch tip accounts before wiring live submission.
//
// Usage: JITO_BLOCK_ENGINE=https://amsterdam.mainnet.block-engine.jito.wtf \
//
//	go run ./cmd/jito_probe
package main

import (
	"fmt"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/jito"
)

func main() {
	envfile.LoadDotEnv()
	be := jito.DefaultBlockEngine()
	fmt.Printf("block engine: %s\n", be)
	accts, err := jito.GetTipAccounts(be)
	if err != nil {
		fmt.Printf("FAILED: %+v\n", err)
		return
	}
	fmt.Printf("reachable ✓  %d tip accounts:\n", len(accts))
	for _, a := range accts {
		fmt.Printf("  %s\n", a)
	}
}
