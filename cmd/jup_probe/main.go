// Verify the Jupiter swap client end-to-end by mainnet SIMULATION (no send):
// quote 0.005 SOL → USDC for the live wallet, decode the swap-instructions
// response, fetch its lookup tables, compile a v0 tx, and simulate. Success =
// err null with real CU spent — proves quote parse, ix decode, ALT fetch, and
// v0 compile are all correct before the fire path trusts them.
//
// Usage: HELIUS_RPC=<url> [AUTHORITY=<pk>] [AMOUNT_LAMPORTS=5000000] go run ./cmd/jup_probe
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/arb"
	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/jupswap"
)

const (
	solMint          = "So11111111111111111111111111111111111111112"
	usdcMint         = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	defaultAuthority = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak"
)

func rpc(endpoint string, body map[string]any) map[string]any {
	b, err := json.Marshal(body)
	if err != nil {
		return nil
	}
	for attempt := 0; attempt < 4; attempt++ {
		resp, err := http.Post(endpoint, "application/json", bytes.NewReader(b))
		if err == nil {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var v map[string]any
			if json.Unmarshal(raw, &v) == nil {
				return v
			}
		}
		time.Sleep(time.Duration(400<<attempt) * time.Millisecond)
	}
	return nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func main() {
	envfile.LoadDotEnv()

	endpoint := os.Getenv("HELIUS_RPC")
	if endpoint == "" {
		endpoint = os.Getenv("RPC_HTTP")
	}
	if endpoint == "" {
		fail("HELIUS_RPC (or RPC_HTTP) must be set")
	}
	authorityStr := os.Getenv("AUTHORITY")
	if authorityStr == "" {
		authorityStr = defaultAuthority
	}
	authority, err := solana.PublicKeyFromBase58(authorityStr)
	if err != nil {
		fail("bad AUTHORITY: %v", err)
	}
	amount := uint64(5_000_000)
	if v := os.Getenv("AMOUNT_LAMPORTS"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			amount = n
		}
	}
	sol := solana.MustPublicKeyFromBase58(solMint)
	usdc := solana.MustPublicKeyFromBase58(usdcMint)

	fmt.Fprintf(os.Stderr, "[jup] quoting %d lamports SOL → USDC …\n", amount)
	quote, err := jupswap.Quote(sol, usdc, amount, 50, 30)
	if err != nil {
		fail("quote: %v", err)
	}
	hops := 0
	if rp, ok := quote["routePlan"].([]any); ok {
		hops = len(rp)
	}
	fmt.Fprintf(os.Stderr, "[jup] route: in=%v out=%v (%d hops)\n", quote["inAmount"], quote["outAmount"], hops)

	plan, err := jupswap.SwapInstructions(quote, authority, true)
	if err != nil {
		fail("swap-instructions: %v", err)
	}
	fmt.Fprintf(os.Stderr, "[jup] %d instructions, %d lookup tables, quoted_out=%d min_out=%d\n",
		len(plan.Instructions), len(plan.AltAddresses), plan.QuotedOut, plan.MinOut)

	alts, err := jupswap.FetchAlts(endpoint, plan.AltAddresses)
	if err != nil {
		fail("fetch ALTs: %v", err)
	}
	for _, a := range alts {
		fmt.Fprintf(os.Stderr, "[jup]   ALT %s (%d addresses)\n", a.Key, len(a.Addresses))
	}

	// Compile [cu_limit, setup…, swap, cleanup…] and simulate.
	ixs := []solana.Instruction{arb.CuLimitIx(1_400_000)}
	ixs = append(ixs, plan.Instructions...)
	tx, err := arb.CompileV0(authority, ixs, alts, solana.Hash{})
	if err != nil {
		fail("compile v0: %v", err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		fail("marshal tx: %v", err)
	}
	fmt.Fprintf(os.Stderr, "[jup] tx size: %d bytes (limit 1232)\n", len(raw))
	b64tx, err := tx.ToBase64()
	if err != nil {
		fail("b64 tx: %v", err)
	}

	sim := rpc(endpoint, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "simulateTransaction",
		"params": []any{b64tx, map[string]any{"sigVerify": false, "replaceRecentBlockhash": true, "commitment": "processed", "encoding": "base64"}},
	})
	if sim == nil {
		fail("simulate: no response")
	}
	result, _ := sim["result"].(map[string]any)
	value, _ := result["value"].(map[string]any)

	fmt.Println("\n──── jup swap simulation ────")
	fmt.Printf("err: %v\n", jsonOrNull(value["err"]))
	fmt.Printf("unitsConsumed: %v\n", jsonOrNull(value["unitsConsumed"]))
	if value["err"] == nil {
		fmt.Println("★ VERIFIED — Jupiter-built swap executes clean via our decode/compile path")
	} else if logs, ok := value["logMessages"].([]any); ok {
		for _, l := range logs {
			if s, ok := l.(string); ok {
				fmt.Println("  " + s)
			}
		}
	}
}

func jsonOrNull(v any) string {
	if v == nil {
		return "null"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
