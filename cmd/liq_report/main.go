// Command liq_report digests a liquidation executor run — reads the JSONL
// ledgers and summarizes so you can answer "is it working / did it earn?"
// at a glance without tailing the stream. Reads {RUN_DIR}/decisions.jsonl +
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
	"time"

	"arbengine/internal/config"
)

func readJSONL(path string) []map[string]any {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var v map[string]any
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			continue
		}
		out = append(out, v)
	}
	return out
}

func f(v map[string]any, k string) float64 {
	x, ok := v[k].(float64)
	if !ok {
		return 0.0
	}
	return x
}

func s(v map[string]any, k string) (string, bool) {
	x, ok := v[k].(string)
	return x, ok
}

func present(v map[string]any, k string) bool {
	x, ok := v[k]
	return ok && x != nil
}

func boolOr(v map[string]any, k string, def bool) bool {
	x, ok := v[k].(bool)
	if !ok {
		return def
	}
	return x
}

func report(runDir string) {
	decisions := readJSONL(runDir + "/decisions.jsonl")
	trades := readJSONL(runDir + "/trades.jsonl")

	// Liquidation decision rows across ALL executor schemas: marginfi keys the
	// borrower as "liquidatee", Save/Kamino as "obligation" — but all three have
	// "reason" + "fired" (and the arb-engine rows don't), so match on those.
	var liqDecisions []map[string]any
	for _, d := range decisions {
		if present(d, "reason") && present(d, "fired") {
			liqDecisions = append(liqDecisions, d)
		}
	}
	fired := 0
	for _, d := range liqDecisions {
		if boolOr(d, "fired", false) {
			fired++
		}
	}
	reasons := make(map[string]int)
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
		if present(t, "est_profit_usdc") {
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
		if present(t, "realized_usdc") {
			landed = append(landed, t)
		}
	}
	var realized float64
	for _, t := range landed {
		realized += f(t, "realized_usdc")
	}
	var errors []map[string]any
	for _, t := range submitted {
		if present(t, "error") {
			errors = append(errors, t)
		}
	}

	fmt.Printf("═══ liquidation report (%s) ═══\n", runDir)
	fmt.Printf("decisions logged: %d (liquidation rows)\n", len(liqDecisions))
	fmt.Printf("  fired:   %d\n", fired)
	fmt.Printf("  skipped: %d\n", len(liqDecisions)-fired)
	if len(reasons) > 0 {
		fmt.Println("  reasons:")
		type rn struct {
			reason string
			n      int
		}
		sorted := make([]rn, 0, len(reasons))
		for r, n := range reasons {
			sorted = append(sorted, rn{r, n})
		}
		sort.Slice(sorted, func(i, j int) bool {
			if sorted[i].n != sorted[j].n {
				return sorted[i].n > sorted[j].n
			}
			return sorted[i].reason < sorted[j].reason
		})
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
	fmt.Printf("  errored:    %d\n", len(errors))
	fmt.Printf("  landed:     %d\n", len(landed))
	fmt.Printf("  realized P&L: $%.2f\n", realized)
	if len(landed) == 0 && len(submitted) == 0 {
		fmt.Println("\n→ no fires yet. In a calm market that's expected — the bot only fires a")
		fmt.Println("  marginfi-confirmed, profitable liquidation. Leave it running.")
	} else if len(landed) != 0 {
		fmt.Println("\n→ ★ the strategy has landed real liquidations. That's the money question answered.")
	}
}

func main() {
	config.LoadDotenv()
	runDir := config.EnvOr("RUN_DIR", "runs/liq")
	watch := config.EnvOr("WATCH", "") == "1"
	refresh := config.EnvInt("REFRESH_SECS", 30)
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
