// Verify the in-memory CLMM math against reality. For a range of USDC sizes,
// compare our apply_swap (USDC→base, per venue) against Jupiter's quote for
// that exact single-venue swap. Close match = our within-tick math is right
// and we can trust the profit optimiser. Divergence at larger sizes = where
// tick-crossing starts to matter (Stage 1b).
//
// Then print the current optimal cross-venue arb (size + exact profit) so we
// can see, live, whether SOL/USDC ever shows a positive number.
//
// Usage: RPC_ENDPOINT=<url> go run ./cmd/clmm_probe
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"solana-arb-backtest-go/internal/clmm"
	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/pools"
)

func rpc(endpoint string, body map[string]any) (map[string]any, bool) {
	for attempt := 0; attempt < 4; attempt++ {
		b, _ := json.Marshal(body)
		resp, err := http.Post(endpoint, "application/json", bytes.NewReader(b))
		if err == nil {
			var v map[string]any
			decErr := json.NewDecoder(resp.Body).Decode(&v)
			resp.Body.Close()
			if decErr == nil {
				return v, true
			}
		}
		time.Sleep(time.Duration(300<<attempt) * time.Millisecond)
	}
	return nil, false
}

func accountData(endpoint, addr string) []byte {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{addr, map[string]string{"encoding": "base64"}}})
	if !ok {
		panic("rpc")
	}
	result := v["result"].(map[string]any)
	value := result["value"].(map[string]any)
	dataArr := value["data"].([]any)
	data, err := base64.StdEncoding.DecodeString(dataArr[0].(string))
	if err != nil {
		panic("data")
	}
	return data
}

const usdcMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

// jupQuote is a Jupiter quote: exact-in USDC→SOL restricted to one DEX.
// Returns SOL out (raw lamports) as float64.
func jupQuote(baseMint string, amountUsdcRaw uint64, dex string) (float64, bool) {
	url := fmt.Sprintf("https://lite-api.jup.ag/swap/v1/quote?inputMint=%s&outputMint=%s&amount=%d&onlyDirectRoutes=true&swapMode=ExactIn&dexes=%s",
		usdcMint, baseMint, amountUsdcRaw, dex)
	resp, err := http.Get(url)
	if err != nil {
		fmt.Printf("  (jup err: %v)\n", err)
		return 0, false
	}
	defer resp.Body.Close()
	var v map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return 0, false
	}
	outStr, ok := v["outAmount"].(string)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(outStr, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

func main() {
	envfile.LoadDotEnv()
	endpoint := os.Getenv("RPC_ENDPOINT")
	if endpoint == "" {
		fmt.Fprintln(os.Stderr, "set RPC_ENDPOINT")
		os.Exit(1)
	}
	cfg := pools.Pair()
	base := clmm.WSOL()

	od := accountData(endpoint, cfg.OrcaPool)
	rd := accountData(endpoint, cfg.RayPool)
	// Orca decimals by mint order: mintA@101.
	orcaProbe, ok := clmm.FromOrca(od, 0, 0, cfg.OrcaFeeBps)
	if !ok {
		panic("orca decode")
	}
	mintA := orcaProbe.Mint0
	baseIsA := mintA.Equals(base)
	oaDec0, oaDec1 := cfg.QuoteDec, cfg.BaseDec
	if baseIsA {
		oaDec0, oaDec1 = cfg.BaseDec, cfg.QuoteDec
	}
	orca, ok := clmm.FromOrca(od, oaDec0, oaDec1, cfg.OrcaFeeBps)
	if !ok {
		panic("orca decode")
	}
	ray, ok := clmm.FromRay(rd, cfg.RayFeeBps)
	if !ok {
		panic("ray decode")
	}

	fmt.Printf("Orca  ui_price=%.4f  L=%.3e  fee=%vbp\n", orca.UIPrice(), orca.Liquidity, cfg.OrcaFeeBps)
	fmt.Printf("Ray   ui_price=%.4f  L=%.3e  fee=%vbp\n", ray.UIPrice(), ray.Liquidity, cfg.RayFeeBps)

	fmt.Println("\n═══ apply_swap vs Jupiter (USDC→SOL, single venue) ═══")
	for _, usdc := range []float64{100.0, 500.0, 2000.0, 10000.0} {
		raw := uint64(usdc * 1e6)
		usdcIs0Orca := !orca.Mint0.Equals(base)
		oursOrca := orca.ApplySwap(usdcIs0Orca, usdc*1e6) / 1e9
		jupOrca, jupOrcaOk := jupQuote(cfg.BaseMint, raw, "Whirlpool")
		usdcIs0Ray := !ray.Mint0.Equals(base)
		oursRay := ray.ApplySwap(usdcIs0Ray, usdc*1e6) / 1e9
		jupRay, jupRayOk := jupQuote(cfg.BaseMint, raw, "Raydium%20CLMM")

		errStr := func(ours float64, jup float64, jupOk bool) string {
			if !jupOk {
				return "jup=n/a"
			}
			j := jup / 1e9
			return fmt.Sprintf("jup=%.6f Δ=%+.3f%%", j, 100.0*(ours-j)/j)
		}
		fmt.Printf("  %7.0f USDC | Orca ours=%.6f %s\n", usdc, oursOrca, errStr(oursOrca, jupOrca, jupOrcaOk))
		fmt.Printf("          | Ray  ours=%.6f %s\n", oursRay, errStr(oursRay, jupRay, jupRayOk))
		time.Sleep(400 * time.Millisecond)
	}

	fmt.Println("\n═══ optimal cross-venue arb RIGHT NOW ═══")
	size, profit, buyOrca := clmm.OptimalArb(orca, ray, base, 50_000.0*1e6)
	dir := "buy-Ray/sell-Orca"
	if buyOrca {
		dir = "buy-Orca/sell-Ray"
	}
	fmt.Printf("  optimal borrow=%.1f USDC → net profit=%.4f USDC, dir=%s (after %vbp fees, before tip)\n",
		size/1e6, profit/1e6, dir, cfg.RoundTripFeeBps())
	if profit <= 0.0 {
		fmt.Println("  → no profitable arb at this instant (expected on SOL/USDC)")
	}
}
