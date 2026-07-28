// Ground truth: how many liquidations ACTUALLY happened on Save/Solend in
// the recent window, and how many were in OUR scope (v1: single-collateral,
// single-USDC-debt)? If real in-scope liquidations happened and our bot
// fired zero, that's a bug/miss — not "no opportunity." Scans the program's
// recent liquidate txs (tag 12/17), extracts repay reserve (debt) + withdraw
// reserve (collateral) from the ix accounts, and tallies USDC-debt vs other.
//
// Usage: HELIUS_RPC=<url> [PAGES=6] go run ./cmd/save_liq_census
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/gagliardetto/solana-go/base58"

	"solana-arb-backtest-go/internal/envfile"
)

const (
	solendProgram = "So1endDq2YkqhipRh3WViPa8hdiSpxWy6z3Z6tMCpAo"
	usdcReserve   = "BgxfHJDzm44T7XG68MYKx7YisTjZu73tVovyZSjJMpmw"
)

var httpClient = &http.Client{Timeout: 20 * time.Second}

func rpc(endpoint string, body map[string]any) (map[string]any, bool) {
	for attempt := 0; attempt < 4; attempt++ {
		b, _ := json.Marshal(body)
		resp, err := httpClient.Post(endpoint, "application/json", bytes.NewReader(b))
		if err == nil {
			raw, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr == nil {
				var v map[string]any
				if json.Unmarshal(raw, &v) == nil {
					return v, true
				}
			}
		}
		time.Sleep(time.Duration(400<<attempt) * time.Millisecond)
	}
	return nil, false
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func asArray(v any) []any        { a, _ := v.([]any); return a }
func asStr(v any) string         { s, _ := v.(string); return s }

type sigEntry struct {
	sig       string
	blockTime int64
	hasTime   bool
}

func truncate(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

func main() {
	envfile.LoadDotEnv()

	endpoint := os.Getenv("HELIUS_RPC")
	if endpoint == "" {
		endpoint = os.Getenv("RPC_HTTP")
	}
	if endpoint == "" {
		fmt.Fprintln(os.Stderr, "HELIUS_RPC")
		os.Exit(1)
	}
	pages := 6
	if v := os.Getenv("PAGES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			pages = n
		}
	}

	// Page recent program signatures.
	var sigs []sigEntry
	var before string
	for i := 0; i < pages; i++ {
		params := map[string]any{"limit": 1000}
		if before != "" {
			params["before"] = before
		}
		v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getSignaturesForAddress",
			"params": []any{solendProgram, params}})
		if !ok {
			break
		}
		page := asArray(v["result"])
		if len(page) == 0 {
			break
		}
		before = asStr(asMap(page[len(page)-1])["signature"])
		for _, ev := range page {
			e := asMap(ev)
			if e["err"] == nil {
				bt, hasTime := e["blockTime"].(float64)
				sigs = append(sigs, sigEntry{sig: asStr(e["signature"]), blockTime: int64(bt), hasTime: hasTime})
			}
		}
		fmt.Fprintf(os.Stderr, "[census] paged %d sigs\n", len(sigs))
	}
	spanH := 0.0
	if len(sigs) > 0 {
		first, last := sigs[0], sigs[len(sigs)-1]
		if first.hasTime && last.hasTime {
			spanH = float64(first.blockTime-last.blockTime) / 3600.0
		}
	}

	liqs, usdcDebt := 0, 0
	collateralReserves := map[string]int{}
	var examples []string
	for _, se := range sigs {
		tx, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getTransaction",
			"params": []any{se.sig, map[string]any{"encoding": "jsonParsed", "maxSupportedTransactionVersion": 0, "commitment": "confirmed"}}})
		if !ok {
			continue
		}
		result := asMap(tx["result"])
		if result == nil {
			continue
		}
		message := asMap(asMap(result["transaction"])["message"])
		var ixs []map[string]any
		for _, ixv := range asArray(message["instructions"]) {
			ixs = append(ixs, asMap(ixv))
		}
		meta := asMap(result["meta"])
		for _, innerV := range asArray(meta["innerInstructions"]) {
			inner := asMap(innerV)
			for _, ixv := range asArray(inner["instructions"]) {
				ixs = append(ixs, asMap(ixv))
			}
		}
		for _, ix := range ixs {
			if asStr(ix["programId"]) != solendProgram {
				continue
			}
			data, err := base58.Decode(asStr(ix["data"]))
			if err != nil || len(data) == 0 {
				continue
			}
			tag := data[0]
			if tag != 12 && tag != 17 {
				continue
			}
			liqs++
			// tag 17 accounts: [3]=repay_reserve, [5]=withdraw_reserve.
			accts := asArray(ix["accounts"])
			getAcct := func(i int) string {
				if i < len(accts) {
					return asStr(accts[i])
				}
				return ""
			}
			repay := getAcct(3)
			withdraw := getAcct(5)
			if repay == usdcReserve {
				usdcDebt++
			}
			collateralReserves[withdraw]++
			if len(examples) < 8 {
				repayLabel := truncate(repay, 8)
				if repay == usdcReserve {
					repayLabel = "USDC"
				}
				withdrawLabel := ""
				if len(withdraw) >= 8 {
					withdrawLabel = withdraw[:8]
				} else {
					withdrawLabel = withdraw
				}
				examples = append(examples, fmt.Sprintf("%s  repay=%s withdraw=%s", se.sig, repayLabel, withdrawLabel))
			}
		}
		time.Sleep(15 * time.Millisecond)
	}

	fmt.Println("\n═══ Save/Solend liquidation census ═══")
	fmt.Printf("window: %d txs over ~%.1f h\n", len(sigs), spanH)
	fmt.Printf("LIQUIDATIONS that actually happened: %d\n", liqs)
	fmt.Printf("  of which USDC-debt (our v1 scope for debt): %d\n", usdcDebt)
	liqRate, usdcRate := 0.0, 0.0
	if spanH > 0.0 {
		liqRate = float64(liqs) / spanH
		usdcRate = float64(usdcDebt) / spanH
	}
	fmt.Printf("  → est rate: %.1f liquidations/hour, %.1f USDC-debt/hour\n", liqRate, usdcRate)
	fmt.Println("\ncollateral reserves seized (top):")
	type crEntry struct {
		reserve string
		n       int
	}
	var cr []crEntry
	for r, n := range collateralReserves {
		cr = append(cr, crEntry{r, n})
	}
	sort.Slice(cr, func(i, j int) bool { return cr[i].n > cr[j].n })
	for i, e := range cr {
		if i >= 8 {
			break
		}
		fmt.Printf("  %3d  %s\n", e.n, truncate(e.reserve, 16))
	}
	fmt.Println("\nexamples:")
	for _, e := range examples {
		fmt.Printf("  %s\n", e)
	}
	if usdcDebt == 0 && liqs > 0 {
		fmt.Println("\n→ liquidations happened but NONE were USDC-debt — our v1 debt scope is the gap.")
	} else if usdcDebt > 0 {
		fmt.Printf("\n→ %d USDC-debt liquidations happened that we should be able to fire — if we fired 0, investigate why (shape filter, sizing, or timing).\n", usdcDebt)
	}
}
