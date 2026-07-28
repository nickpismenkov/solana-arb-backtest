// Flow probe — two go/no-go measurements before we build the co-bundler:
//  1. DIRECT vs ROUTED: of the swaps hitting our pools, how many call the
//     DEX program top-level (decodable from a shred → co-bundlable) vs only
//     via CPI (opaque). This is the addressable direct-swap volume.
//  2. WINNING TIPS: for cross-venue arbs that landed on our pools, how much
//     did the winner tip Jito (balance delta of a Jito tip account) and what
//     did they net (fee-payer USDC delta)? → what it costs to compete.
//
// Read-only, RPC-only (use Helius). No money.
//
// Usage: RPC_ENDPOINT=<helius> [LIMIT=800] go run ./cmd/flow_probe
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"solana-arb-backtest-go/internal/decode"
	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/jito"
	"solana-arb-backtest-go/internal/pools"
)

const usdcMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

var httpClient = &http.Client{Timeout: 15 * time.Second}

func rpc(endpoint string, body map[string]any) (map[string]any, bool) {
	for attempt := 0; attempt < 4; attempt++ {
		b, _ := json.Marshal(body)
		resp, err := httpClient.Post(endpoint, "application/json", bytes.NewReader(b))
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

func recentSigs(endpoint, pool string, limit int) []string {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getSignaturesForAddress",
		"params": []any{pool, map[string]any{"limit": limit}}})
	if !ok {
		return nil
	}
	arr, _ := v["result"].([]any)
	var out []string
	for _, ev := range arr {
		em, _ := ev.(map[string]any)
		if em == nil || em["err"] != nil {
			continue
		}
		if s, ok := em["signature"].(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1)*p + 0.5)
	return sorted[i]
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asArray(v any) []any {
	a, _ := v.([]any)
	return a
}

func main() {
	envfile.LoadDotEnv()
	endpoint := os.Getenv("RPC_ENDPOINT")
	if endpoint == "" {
		fmt.Fprintln(os.Stderr, "RPC_ENDPOINT (use Helius)")
		os.Exit(1)
	}
	limit := 800
	if v := os.Getenv("LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	sleepMs := 25
	if v := os.Getenv("SLEEP_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			sleepMs = n
		}
	}
	cfg := pools.Pair()

	tipAccounts := map[string]bool{}
	if accts, err := jito.GetTipAccounts(jito.DefaultBlockEngine()); err == nil {
		for _, a := range accts {
			tipAccounts[a.String()] = true
		}
	}
	fmt.Fprintf(os.Stderr, "flow-probe %s — %d Jito tip accounts; scanning ~%d sigs/pool\n", cfg.Label, len(tipAccounts), limit)

	// Gather unique recent signatures across both pools.
	sigSet := map[string]bool{}
	for _, pool := range []string{cfg.OrcaPool, cfg.RayPool} {
		for _, s := range recentSigs(endpoint, pool, limit) {
			sigSet[s] = true
		}
	}
	fmt.Fprintf(os.Stderr, "scanning %d unique txns…\n", len(sigSet))

	var direct, routed, arbs uint64
	var arbTips, arbProfits, allTips []float64
	var scanned uint64
	var fetchFail uint64

	for sig := range sigSet {
		time.Sleep(time.Duration(sleepMs) * time.Millisecond)
		v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getTransaction",
			"params": []any{sig, map[string]any{"encoding": "jsonParsed", "maxSupportedTransactionVersion": 0, "commitment": "confirmed"}}})
		if !ok {
			fetchFail++
			continue
		}
		r := asMap(v["result"])
		if r == nil {
			continue
		}
		meta := asMap(r["meta"])
		if meta == nil || meta["err"] != nil {
			continue
		}
		scanned++

		// Full ordered account list: static keys, then loaded writable, readonly.
		message := asMap(asMap(r["transaction"])["message"])
		var keys []string
		for _, k := range asArray(message["accountKeys"]) {
			km := asMap(k)
			if s, ok := km["pubkey"].(string); ok {
				keys = append(keys, s)
			}
		}
		loadedAddresses := asMap(meta["loadedAddresses"])
		for _, grp := range []string{"writable", "readonly"} {
			for _, k := range asArray(loadedAddresses[grp]) {
				if s, ok := k.(string); ok {
					keys = append(keys, s)
				}
			}
		}
		keySet := map[string]bool{}
		for _, k := range keys {
			keySet[k] = true
		}
		touchOrca := keySet[cfg.OrcaPool]
		touchRay := keySet[cfg.RayPool]
		if !touchOrca && !touchRay {
			continue
		}

		// Top-level program IDs.
		top := map[string]bool{}
		for _, ix := range asArray(message["instructions"]) {
			ixm := asMap(ix)
			if s, ok := ixm["programId"].(string); ok {
				top[s] = true
			}
		}
		dexTopLevel := top[decode.OrcaProgram] || top[decode.RayClmmProgram]
		if dexTopLevel {
			direct++
		} else {
			routed++
		}

		// Tip: balance delta of any Jito tip account in this tx.
		pre := asArray(meta["preBalances"])
		post := asArray(meta["postBalances"])
		var tip float64
		if pre != nil && post != nil {
			for i, k := range keys {
				if !tipAccounts[k] {
					continue
				}
				var preV, postV float64
				if i < len(pre) {
					preV, _ = pre[i].(float64)
				}
				if i < len(post) {
					postV, _ = post[i].(float64)
				}
				d := postV - preV
				if d > 0 {
					tip += d
				}
			}
		}
		if tip > 0 {
			allTips = append(allTips, tip)
		}

		// Cross-venue arb = touches BOTH pools.
		if touchOrca && touchRay {
			arbs++
			if tip > 0 {
				arbTips = append(arbTips, tip)
			}
			// fee-payer USDC delta = arb profit
			accountKeys := asArray(message["accountKeys"])
			var payer string
			if len(accountKeys) > 0 {
				payer, _ = asMap(accountKeys[0])["pubkey"].(string)
			}
			sum := func(key string) float64 {
				var total float64
				for _, b := range asArray(meta[key]) {
					bm := asMap(b)
					if bm["mint"] != usdcMint || bm["owner"] != payer {
						continue
					}
					uiTokenAmount := asMap(bm["uiTokenAmount"])
					if f, ok := uiTokenAmount["uiAmount"].(float64); ok {
						total += f
					}
				}
				return total
			}
			arbProfits = append(arbProfits, sum("postTokenBalances")-sum("preTokenBalances"))
		}
	}

	tot := direct + routed
	if tot == 0 {
		tot = 1
	}
	fmt.Printf("\n═══ FLOW (%d pool txns scanned, %d fetch fails) ═══\n", scanned, fetchFail)
	fmt.Printf("DIRECT swaps (DEX top-level → decodable/co-bundlable): %d (%.1f%%)\n", direct, 100.0*float64(direct)/float64(tot))
	fmt.Printf("ROUTED swaps (DEX via CPI → opaque in shred):          %d (%.1f%%)\n", routed, 100.0*float64(routed)/float64(tot))

	fmt.Println("\n═══ WINNING TIPS (SOL) ═══")
	report := func(label string, v []float64) {
		sort.Float64s(v)
		if len(v) == 0 {
			fmt.Printf("%s: (none)\n", label)
			return
		}
		fmt.Printf("%s: n=%d | p25=%.5f med=%.5f p75=%.5f max=%.5f\n",
			label, len(v), pct(v, 0.25)/1e9, pct(v, 0.5)/1e9, pct(v, 0.75)/1e9, pct(v, 1.0)/1e9)
	}
	report("all pool-tx tips", allTips)
	report("cross-venue ARB tips", arbTips)

	fmt.Printf("\n═══ ARBS (%d cross-venue) ═══\n", arbs)
	sort.Float64s(arbProfits)
	if len(arbProfits) > 0 {
		fmt.Printf("payer USDC profit: med=%.4f max=%.4f (note: excl. tip/fees; many are $0 routed user swaps)\n",
			pct(arbProfits, 0.5), pct(arbProfits, 1.0))
	}
}
