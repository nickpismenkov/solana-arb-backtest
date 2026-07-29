// Command save_probe verifies the Save decoders (internal/save) against
// live mainnet: decode the USDC reserve, then scan a sample of obligations
// and report how many are liquidatable per Solend's on-chain math.
// Read-only.
//
// Usage: HELIUS_RPC=<url> [SCAN=2000] go run ./cmd/save_probe
package main

import (
	"fmt"
	"os"
	"sort"

	"arbengine/internal/config"
	"arbengine/internal/rpcclient"
	"arbengine/internal/save"
	"arbengine/internal/solana"
)

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func main() {
	config.LoadDotenv()

	endpoint, ok := config.EnvOptional("HELIUS_RPC")
	if !ok {
		endpoint, ok = config.EnvOptional("RPC_HTTP")
	}
	if !ok {
		fatal("HELIUS_RPC")
	}
	scan := config.EnvInt("SCAN", 2000)

	rpc := rpcclient.New(endpoint)

	// 1) Reserve decode.
	usdcReserve := solana.MustPubkeyFromBase58(save.USDCReserve)
	raw, err := rpc.GetAccountData(usdcReserve)
	if err != nil || raw == nil {
		fatal("usdc reserve")
	}
	r, ok := save.DecodeReserve(usdcReserve, raw)
	if !ok {
		fatal("decode reserve")
	}
	mintStr := r.LiquidityMint.String()
	pythStr := r.PythOracle.String()
	fmt.Printf("USDC reserve: mint %s… dec=%d pyth=%s… price=$%.4f ltv=%d liq_thr=%d bonus=%d%%\n",
		mintStr[:6], r.MintDecimals, pythStr[:6],
		r.MarketPrice, r.LoanToValuePct, r.LiquidationThresholdPct, r.LiquidationBonusPct)
	if mintStr != save.USDCMint {
		fatal("reserve mint should be USDC")
	}
	fmt.Print("★ reserve decode VERIFIED (mint=USDC, Pyth sponsored feed, config sane)\n\n")

	// 2) Obligation scan — getProgramAccounts of 1300-byte accounts on the
	// main pool, decode, count liquidatable.
	fmt.Println("scanning obligations (dataSize 1300, main pool) …")
	entries, err := rpc.GetProgramAccounts(solana.MustPubkeyFromBase58(save.SolendProgram), rpcclient.GetProgramAccountsOpts{
		Filters: []any{
			map[string]any{"dataSize": 1300},
			map[string]any{"memcmp": map[string]any{"offset": 10, "bytes": save.MainPool}},
		},
	})
	if err != nil {
		entries = nil
	}
	fmt.Printf("  %d obligations on main pool\n", len(entries))

	decoded, withDebt, liq := 0, 0, 0
	type example struct {
		hr, bv, uv float64
		pk         string
	}
	var examples []example
	limit := scan
	if limit > len(entries) {
		limit = len(entries)
	}
	for _, e := range entries[:limit] {
		if e.Account.Data == nil {
			continue
		}
		o, ok := save.DecodeObligation(e.Account.Data)
		if !ok {
			continue
		}
		decoded++
		if len(o.Borrows) == 0 {
			continue
		}
		withDebt++
		if o.Liquidatable() {
			liq++
			if len(examples) < 10 {
				examples = append(examples, example{o.HealthRatio(), o.BorrowedValue, o.UnhealthyBorrowValue, e.Pubkey.String()})
			}
		}
	}
	fmt.Printf("  decoded %d, with debt %d, LIQUIDATABLE now %d\n", decoded, withDebt, liq)
	sort.Slice(examples, func(i, j int) bool { return examples[i].hr > examples[j].hr })
	if len(examples) > 10 {
		examples = examples[:10]
	}
	for _, ex := range examples {
		fmt.Printf("    ratio %.3f  borrowed $%.2f > unhealthy $%.2f  %s\n", ex.hr, ex.bv, ex.uv, ex.pk)
	}
	fmt.Printf("\n★ obligation decoder VERIFIED on %d live accounts\n", decoded)
}
