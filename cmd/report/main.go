// Rollup of the executor ledgers (decisions.jsonl + trades.jsonl): decodable
// victims evaluated, profitable predictions, fires, landings, realized P&L,
// tips paid. Reads the JSONL the executor writes; safe to run while it's live.
//
// Usage: RUN_DIR=runs go run ./cmd/report
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
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
		if err := json.Unmarshal(sc.Bytes(), &v); err == nil {
			out = append(out, v)
		}
	}
	return out
}

func main() {
	dir := os.Getenv("RUN_DIR")
	if dir == "" {
		dir = "runs"
	}
	decisions := readJSONL(fmt.Sprintf("%s/decisions.jsonl", dir))
	trades := readJSONL(fmt.Sprintf("%s/trades.jsonl", dir))

	// Decisions: one per decodable victim we evaluated (routed/CPI skipped).
	evaluated := len(decisions)
	var profitable, below, fired int
	for _, d := range decisions {
		if s, _ := d["reason"].(string); s == "profitable" {
			profitable++
		}
		if s, _ := d["reason"].(string); s == "below_threshold" {
			below++
		}
		if b, _ := d["fired"].(bool); b {
			fired++
		}
	}

	// Trades: submit errors, and confirmed on-chain landings (realized_usdc set).
	var submitErrors int
	var landed []map[string]any
	for _, t := range trades {
		if _, ok := t["error"].(string); ok {
			submitErrors++
		}
		if _, ok := t["realized_usdc"].(float64); ok {
			landed = append(landed, t)
		}
	}
	var realized float64
	for _, t := range landed {
		if v, ok := t["realized_usdc"].(float64); ok {
			realized += v
		}
	}
	var tipsSol float64
	for _, t := range landed {
		if v, ok := t["tip_lamports"].(float64); ok {
			tipsSol += v
		}
	}
	tipsSol /= 1e9

	fmt.Printf("\n──────── executor rollup (%s) ────────\n", dir)
	fmt.Printf("decodable victims evaluated:  %d\n", evaluated)
	fmt.Printf("  predicted PROFITABLE:       %d   (below threshold: %d)\n", profitable, below)
	fmt.Printf("  fired live:                 %d   (0 in dry run)\n", fired)
	fmt.Printf("submit errors:                %d\n", submitErrors)
	fmt.Printf("LANDED on-chain:              %d\n", len(landed))
	fmt.Printf("realized P&L:                 %+.4f USDC\n", realized)
	fmt.Printf("tips paid (landed only):      %.6f SOL\n", tipsSol)
	if len(landed) == 0 && profitable > 0 {
		fmt.Printf("\n→ found %d profitable predictions but nothing landed = losing the race (or dry run).\n", profitable)
	} else if profitable == 0 {
		fmt.Println("\n→ no profitable predictions = no capturable edge in this window (or market quiet).")
	}
	fmt.Println()
}
