// Kamino liquidatable-obligation finder (read-only, Stage 1 live test).
//
// Scans every klend Obligation, reads its STORED health values (no oracle
// needed — Kamino pre-computes them), and lists who is liquidatable
// (borrow_factor_adjusted_debt ≥ unhealthy_borrow_value), ranked by seizable
// collateral. Reports staleness: a "fresh" liquidatable obligation is a
// high-confidence opportunity; a "stale" one needs an on-chain refresh to
// confirm (its stored values predate the latest price).
//
// Usage: HELIUS_RPC=<url> [MARKET=<pubkey|all>] [MIN_COLLATERAL_USD=50]
//
//	[NEAR=25] [STALE_SLOTS=150] go run ./cmd/liq_kamino
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

func envF64(name string, def float64) float64 {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n
		}
	}
	return def
}
func envU64(name string, def uint64) uint64 {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
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
	staleSlots := envU64("STALE_SLOTS", 150)

	// Current slot → staleness age of each obligation.
	curSlot := uint64(0)
	if v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getSlot",
		"params": []any{map[string]any{"commitment": "confirmed"}}}); ok {
		if f, ok := v["result"].(float64); ok {
			curSlot = uint64(f)
		}
	}

	// Obligations: dataSize filter, dataSlice trims to the fields we read.
	filters := []any{map[string]any{"dataSize": kamino.ObligationSize}}
	if market != "all" {
		filters = append(filters, map[string]any{"memcmp": map[string]any{"offset": 32, "bytes": market}})
	}
	label := market
	if market == "all" {
		label = "all"
	} else if len(market) > 8 {
		label = market[:8]
	}
	fmt.Fprintf(os.Stderr, "[kamino] getProgramAccounts (market=%s) …\n", label)
	resp, _ := rpc(endpoint, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{kamino.KlendProgram, map[string]any{
			"encoding":  "base64",
			"dataSlice": map[string]any{"offset": 0, "length": 2272},
			"filters":   filters,
		}},
	})
	entries := asArray(resp["result"])
	fmt.Fprintf(os.Stderr, "[kamino] %d obligations, current slot %d\n", len(entries), curSlot)
	if len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "[kamino] nothing returned — RPC must support getProgramAccounts")
		return
	}

	var obs []*kamino.Obligation
	for _, ev := range entries {
		e := asMap(ev)
		raw, ok := b64(asMap(e["account"])["data"])
		if !ok {
			continue
		}
		o, ok := kamino.DecodeObligation(raw)
		if !ok || o.BorrowedValue <= 0.0 {
			continue
		}
		obs = append(obs, o)
	}

	// Liquidatable, split by freshness, ranked by seizable collateral.
	var liq []*kamino.Obligation
	for _, o := range obs {
		if o.Liquidatable() && o.DepositedValue >= minCollateral {
			liq = append(liq, o)
		}
	}
	sort.Slice(liq, func(i, j int) bool { return liq[i].DepositedValue > liq[j].DepositedValue })
	isFresh := func(o *kamino.Obligation) bool {
		return !o.Stale && satSub(curSlot, o.LastUpdateSlot) <= staleSlots
	}
	dust := 0
	for _, o := range obs {
		if o.Liquidatable() && o.DepositedValue < minCollateral {
			dust++
		}
	}

	fmt.Println("\n════ Kamino liquidatable finder ════")
	fmt.Printf("borrowers scanned:       %d\n", len(obs))
	freshLiq := 0
	for _, o := range liq {
		if isFresh(o) {
			freshLiq++
		}
	}
	fmt.Printf("LIQUIDATABLE (collateral ≥ $%.0f): %d   [%d fresh, %d stale, +%d dust]\n",
		minCollateral, len(liq), freshLiq, len(liq)-freshLiq, dust)
	for i, o := range liq {
		if i >= 50 {
			break
		}
		age := satSub(curSlot, o.LastUpdateSlot)
		tag := "stale"
		if isFresh(o) {
			tag = "FRESH"
		}
		fmt.Printf("  %s %s…  collateral=$%.2f  debt=$%.2f  thresh=$%.2f  ratio=%.4f  (age %dsl)\n",
			tag, shortStr(o.Owner.String(), 8), o.DepositedValue, o.BfAdjustedDebt,
			o.UnhealthyBorrowValue, o.Ratio(), age)
	}

	// Closest healthy obligations with real collateral — monitor candidates.
	var near []*kamino.Obligation
	for _, o := range obs {
		if !o.Liquidatable() && o.DepositedValue >= minCollateral && o.UnhealthyBorrowValue > 0.0 {
			near = append(near, o)
		}
	}
	sort.Slice(near, func(i, j int) bool { return near[i].Ratio() > near[j].Ratio() })
	fmt.Println("\nclosest to liquidation (debt/threshold → 1.0):")
	for i, o := range near {
		if i >= nearN {
			break
		}
		fmt.Printf("  %s…  ratio=%.4f  debt=$%.2f  thresh=$%.2f  collateral=$%.2f\n",
			shortStr(o.Owner.String(), 8), o.Ratio(), o.BfAdjustedDebt, o.UnhealthyBorrowValue, o.DepositedValue)
	}
	fmt.Println()
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
