// Digest of a liquidation executor run — read the JSONL ledgers and
// summarize so you can answer "is it working / did it earn?" at a glance
// without tailing the stream. Reads {RUN_DIR}/decisions.jsonl +
// trades.jsonl (both schemas tolerated). WATCH=1 reprints every
// REFRESH_SECS.
//
// Usage: [RUN_DIR=runs/liq] [WATCH=1] [REFRESH_SECS=30] go run ./cmd/liq_report
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"solana-arb-backtest-go/internal/envfile"
)

func readJSONL(path string) []map[string]any {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var v map[string]any
		if err := json.Unmarshal(sc.Bytes(), &v); err == nil {
			out = append(out, v)
		}
	}
	return out
}

func f(v map[string]any, k string) float64 {
	if x, ok := v[k].(float64); ok {
		return x
	}
	return 0.0
}

func s(v map[string]any, k string) (string, bool) {
	x, ok := v[k].(string)
	return x, ok
}

func report(runDir string) {
	decisions := readJSONL(runDir + "/decisions.jsonl")
	trades := readJSONL(runDir + "/trades.jsonl")

	// Liquidation decision rows across ALL executor schemas: marginfi keys the
	// borrower as "liquidatee", Save/Kamino as "obligation" — but all three
	// have "reason" + "fired" (and the arb-engine rows don't), so match on those.
	var liqDecisions []map[string]any
	for _, d := range decisions {
		if _, hasReason := d["reason"]; hasReason {
			if _, hasFired := d["fired"]; hasFired {
				liqDecisions = append(liqDecisions, d)
			}
		}
	}
	fired := 0
	for _, d := range liqDecisions {
		if b, ok := d["fired"].(bool); ok && b {
			fired++
		}
	}
	reasons := map[string]int{}
	for _, d := range liqDecisions {
		r, ok := s(d, "reason")
		if !ok {
			r = "(none)"
		}
		reasons[r]++
	}

	// Trades: liquidation trade rows (have "est_profit_usdc", unlike arb rows).
	// Submissions have a signature; landings have realized_usdc.
	var liqTrades []map[string]any
	for _, t := range trades {
		if _, ok := t["est_profit_usdc"]; ok {
			liqTrades = append(liqTrades, t)
		}
	}
	var submitted []map[string]any
	for _, t := range liqTrades {
		if _, ok := s(t, "signature"); ok {
			submitted = append(submitted, t)
		}
	}
	var landed []map[string]any
	for _, t := range trades {
		if v, ok := t["realized_usdc"]; ok && v != nil {
			landed = append(landed, t)
		}
	}
	realized := 0.0
	for _, t := range landed {
		realized += f(t, "realized_usdc")
	}
	errors := 0
	for _, t := range submitted {
		if v, ok := t["error"]; ok && v != nil {
			errors++
		}
	}

	fmt.Printf("═══ liquidation report (%s) ═══\n", runDir)
	fmt.Printf("decisions logged: %d (liquidation rows)\n", len(liqDecisions))
	fmt.Printf("  fired:   %d\n", fired)
	skipped := len(liqDecisions) - fired
	if skipped < 0 {
		skipped = 0
	}
	fmt.Printf("  skipped: %d\n", skipped)
	if len(reasons) > 0 {
		fmt.Println("  reasons:")
		type rc struct {
			reason string
			n      int
		}
		sorted := make([]rc, 0, len(reasons))
		for r, n := range reasons {
			sorted = append(sorted, rc{r, n})
		}
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].n > sorted[j].n })
		if len(sorted) > 12 {
			sorted = sorted[:12]
		}
		for _, e := range sorted {
			r := e.reason
			if len(r) > 90 {
				r = r[:90]
			}
			fmt.Printf("    %5d  %s\n", e.n, r)
		}
	}
	fmt.Println("trades:")
	fmt.Printf("  submitted:  %d\n", len(submitted))
	fmt.Printf("  errored:    %d\n", errors)
	fmt.Printf("  landed:     %d\n", len(landed))
	fmt.Printf("  realized P&L: $%.2f\n", realized)
	if len(landed) == 0 && len(submitted) == 0 {
		fmt.Println("\n→ no fires yet. In a calm market that's expected — the bot only fires a")
		fmt.Println("  marginfi-confirmed, profitable liquidation. Leave it running.")
	} else if len(landed) > 0 {
		fmt.Println("\n→ ★ the strategy has landed real liquidations. That's the money question answered.")
	}
}

func main() {
	envfile.LoadDotEnv()

	runDir := os.Getenv("RUN_DIR")
	if runDir == "" {
		runDir = "runs/liq"
	}
	watch := os.Getenv("WATCH") == "1"
	refresh := 30
	if v := os.Getenv("REFRESH_SECS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			refresh = n
		}
	}
	if !watch {
		report(runDir)
		return
	}
	for {
		fmt.Print("\x1b[2J\x1b[H") // clear screen
		report(runDir)
		time.Sleep(time.Duration(refresh) * time.Second)
	}
}
