// Kamino LIVE-health finder — recomputes each obligation's health from CURRENT
// reserve prices (replicating refresh_obligation), instead of trusting the
// obligation's stored (possibly stale) values.
//
// Two outputs:
//  1. VALIDATION — for fresh obligations, recomputed vs stored aggregates
//     should match (proves the recompute math against on-chain truth).
//  2. ALPHA — obligations that are liquidatable at current prices, especially
//     ones whose STORED values still say healthy (stale → a refresh_obligation
//     would flag them; catching these ahead of the crank is the only edge).
//
// Reserve prices come from each reserve's cached market_price (refresh_reserve),
// which stays fresh because reserves are cranked constantly — so we sidestep
// Scope. Freshness of those prices is reported.
//
// Usage: HELIUS_RPC=<url> [MARKET=all] [MIN_COLLATERAL_USD=50] [NEAR=25]
//
//	go run ./cmd/liq_kamino_live
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/kamino"
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

func rpc(endpoint string, body map[string]any) (map[string]any, bool) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, false
	}
	for attempt := 0; attempt < 4; attempt++ {
		resp, err := httpClient.Post(endpoint, "application/json", bytes.NewReader(b))
		if err == nil {
			var v map[string]any
			decErr := json.NewDecoder(resp.Body).Decode(&v)
			resp.Body.Close()
			if decErr == nil {
				return v, true
			}
		}
		time.Sleep(time.Duration(400<<attempt) * time.Millisecond)
	}
	return nil, false
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}
func asArray(v any) []any {
	a, _ := v.([]any)
	return a
}
func b64(data any) ([]byte, bool) {
	arr, ok := data.([]any)
	if !ok || len(arr) == 0 {
		return nil, false
	}
	s, ok := arr[0].(string)
	if !ok {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	return raw, true
}

func getMultiple(endpoint string, keys []solana.PublicKey, sliceLen uint64) map[solana.PublicKey][]byte {
	out := map[solana.PublicKey][]byte{}
	for start := 0; start < len(keys); start += 100 {
		end := start + 100
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[start:end]
		strs := make([]string, len(chunk))
		for i, k := range chunk {
			strs[i] = k.String()
		}
		v, ok := rpc(endpoint, map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "getMultipleAccounts",
			"params": []any{strs, map[string]any{"encoding": "base64", "dataSlice": map[string]any{"offset": 0, "length": sliceLen}}},
		})
		if !ok {
			continue
		}
		values := asArray(asMap(v["result"])["value"])
		for i, accV := range values {
			acc := asMap(accV)
			if acc == nil {
				continue
			}
			if raw, ok := b64(acc["data"]); ok {
				out[chunk[i]] = raw
			}
		}
		time.Sleep(40 * time.Millisecond)
	}
	return out
}

func envF64(name string, def float64) float64 {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n
		}
	}
	return def
}
func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func satSub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

func shortStr(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

type hit struct {
	o         *kamino.Obligation
	r         *kamino.Recomputed
	storedLiq bool
}

func main() {
	envfile.LoadDotEnv()

	endpoint := os.Getenv("HELIUS_RPC")
	if endpoint == "" {
		endpoint = os.Getenv("RPC_HTTP")
	}
	if endpoint == "" {
		fmt.Fprintln(os.Stderr, "HELIUS_RPC in .env")
		os.Exit(1)
	}
	market := os.Getenv("MARKET")
	if market == "" {
		market = "all"
	}
	minCollateral := envF64("MIN_COLLATERAL_USD", 50.0)
	nearN := envInt("NEAR", 25)

	curSlot := uint64(0)
	if v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getSlot",
		"params": []any{map[string]any{"commitment": "confirmed"}}}); ok {
		if f, ok := v["result"].(float64); ok {
			curSlot = uint64(f)
		}
	}

	// 1) Obligations (dataSlice through has_debt @2287).
	filters := []any{map[string]any{"dataSize": kamino.ObligationSize}}
	if market != "all" {
		filters = append(filters, map[string]any{"memcmp": map[string]any{"offset": 32, "bytes": market}})
	}
	fmt.Fprintln(os.Stderr, "[live] getProgramAccounts obligations …")
	resp, _ := rpc(endpoint, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{kamino.KlendProgram, map[string]any{"encoding": "base64", "dataSlice": map[string]any{"offset": 0, "length": 2288}, "filters": filters}},
	})
	entries := asArray(resp["result"])
	var obs []*kamino.Obligation
	for _, ev := range entries {
		e := asMap(ev)
		raw, ok := b64(asMap(e["account"])["data"])
		if !ok {
			continue
		}
		o, ok := kamino.DecodeObligation(raw)
		if !ok || len(o.Borrows) == 0 {
			continue
		}
		obs = append(obs, o)
	}
	fmt.Fprintf(os.Stderr, "[live] %d obligations with debt, current slot %d\n", len(obs), curSlot)
	if len(obs) == 0 {
		return
	}

	// 2) Fetch + decode every referenced reserve (need through borrow_factor @5008).
	seen := map[solana.PublicKey]bool{}
	var reservePks []solana.PublicKey
	for _, o := range obs {
		for _, d := range o.Deposits {
			if !seen[d.Reserve] {
				seen[d.Reserve] = true
				reservePks = append(reservePks, d.Reserve)
			}
		}
		for _, b := range o.Borrows {
			if !seen[b.Reserve] {
				seen[b.Reserve] = true
				reservePks = append(reservePks, b.Reserve)
			}
		}
	}
	fmt.Fprintf(os.Stderr, "[live] fetching %d reserves …\n", len(reservePks))
	reserveRaw := getMultiple(endpoint, reservePks, 5016)
	reserves := map[solana.PublicKey]*kamino.Reserve{}
	for pk, raw := range reserveRaw {
		if r, ok := kamino.DecodeReserve(raw); ok {
			reserves[pk] = r
		}
	}
	// Reserve price freshness (these cached prices drive the recompute).
	var ages []uint64
	for _, r := range reserves {
		ages = append(ages, satSub(curSlot, r.PriceSlot))
	}
	sort.Slice(ages, func(i, j int) bool { return ages[i] < ages[j] })
	medAge := uint64(0)
	if len(ages) > 0 {
		medAge = ages[len(ages)/2]
	}
	maxAge := uint64(0)
	if len(ages) > 0 {
		maxAge = ages[len(ages)-1]
	}
	fmt.Fprintf(os.Stderr, "[live] decoded %d reserves; cached-price age median %dsl (~%ds), max %dsl\n",
		len(reserves), medAge, medAge*2/5, maxAge)

	// Population diagnostics.
	nElev, nStale, nTrust := 0, 0, 0
	for _, o := range obs {
		if o.ElevationGroup != 0 {
			nElev++
		}
		if o.Stale {
			nStale++
		}
		if kamino.Recompute(o, reserves).Trustworthy() {
			nTrust++
		}
	}
	fmt.Fprintf(os.Stderr, "[live] population: %d debt obs | %d elevation-group | %d stale | %d trustworthy(non-elev, fully priced)\n",
		len(obs), nElev, nStale, nTrust)

	// 3) Validation: recomputed vs stored. Compare on trustworthy obligations
	// whose recompute used fresh-enough reserve prices AND that were refreshed
	// recently (so stored ≈ current). Match ⇒ recompute math is correct.
	var valErr []float64
	shown := 0
	fmt.Println("\n──── VALIDATION: recomputed vs stored ────")
	for _, o := range obs {
		r := kamino.Recompute(o, reserves)
		if !r.Trustworthy() || o.UnhealthyBorrowValue < 100.0 {
			continue
		}
		// both the obligation and the reserve prices it uses must be recent.
		if satSub(curSlot, o.LastUpdateSlot) > 300 {
			continue
		}
		if satSub(curSlot, r.OldestPriceSlot) > 300 {
			continue
		}
		err := abs(r.UnhealthyBorrowValue-o.UnhealthyBorrowValue) / o.UnhealthyBorrowValue
		valErr = append(valErr, err)
		if shown < 10 {
			fmt.Printf("  stored unhealthy=$%.2f debt=$%.2f depos=$%.2f  |  recomp unhealthy=$%.2f debt=$%.2f depos=$%.2f  (err %.2f%%)\n",
				o.UnhealthyBorrowValue, o.BfAdjustedDebt, o.DepositedValue,
				r.UnhealthyBorrowValue, r.BfAdjustedDebt, r.DepositedValue, err*100.0)
			shown++
		}
	}
	sort.Float64s(valErr)
	if len(valErr) == 0 {
		fmt.Println("  (no obligation with both itself + its reserve prices fresh enough to validate)")
	} else {
		p90idx := len(valErr) * 9 / 10
		if p90idx >= len(valErr) {
			p90idx = len(valErr) - 1
		}
		fmt.Printf("  → %d validated, median error %.3f%%, p90 %.3f%%\n",
			len(valErr), valErr[len(valErr)/2]*100.0, valErr[p90idx]*100.0)
	}

	// ALPHA: liquidatable at current prices, ranked by seizable collateral.
	var hits []hit
	type nearEntry struct {
		ratio float64
		o     *kamino.Obligation
		r     *kamino.Recomputed
	}
	var near []nearEntry
	for _, o := range obs {
		r := kamino.Recompute(o, reserves)
		if !r.Trustworthy() || r.DepositedValue < minCollateral {
			continue
		}
		if r.Liquidatable() {
			hits = append(hits, hit{o, r, o.Liquidatable()})
		} else if r.Ratio() > 0.90 {
			near = append(near, nearEntry{r.Ratio(), o, r})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].r.DepositedValue > hits[j].r.DepositedValue })
	hiddenAlpha := 0
	for _, h := range hits {
		if !h.storedLiq {
			hiddenAlpha++
		}
	}

	fmt.Println("\n════ Kamino LIVE liquidatable (recomputed at current prices) ════")
	fmt.Printf("obligations w/ debt: %d   collateral ≥ $%.0f\n", len(obs), minCollateral)
	fmt.Printf("LIQUIDATABLE NOW: %d   [%d already flagged by stored values, %d HIDDEN (stored says healthy = stale alpha)]\n",
		len(hits), len(hits)-hiddenAlpha, hiddenAlpha)
	for i, h := range hits {
		if i >= 40 {
			break
		}
		age := satSub(curSlot, h.o.LastUpdateSlot)
		tag := "known"
		if !h.storedLiq {
			tag = "ALPHA"
		}
		staleTag := ""
		if h.o.Stale {
			staleTag = ", stale"
		}
		fmt.Printf("  %s %s…  collateral=$%.2f  debt=$%.2f  thresh=$%.2f  ratio=%.4f  (obl age %dsl%s)\n",
			tag, shortStr(h.o.Owner.String(), 8),
			h.r.DepositedValue, h.r.BfAdjustedDebt, h.r.UnhealthyBorrowValue, h.r.Ratio(),
			age, staleTag)
	}

	sort.Slice(near, func(i, j int) bool { return near[i].ratio > near[j].ratio })
	fmt.Println("\nclosest to liquidation at current prices (ratio→1.0):")
	for i, n := range near {
		if i >= nearN {
			break
		}
		fmt.Printf("  %s…  ratio=%.4f  debt=$%.2f  thresh=$%.2f  collateral=$%.2f\n",
			shortStr(n.o.Owner.String(), 8), n.ratio, n.r.BfAdjustedDebt, n.r.UnhealthyBorrowValue, n.r.DepositedValue)
	}
	fmt.Println()
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
