// Race monitor — the decisive "are we losing on SPEED or on TIP?" diagnostic.
//
// Scans actual on-chain liquidations that COMPETITORS won on Solend, then
// cross-references our own detection log ({RUN_DIR}/decisions.jsonl, which
// records the obligation + timestamp each time we processed it). For every
// real liquidation it classifies us as:
//
//	AHEAD   — we had flagged that obligation BEFORE it was liquidated → we
//	          lost the fill on ACTION/TIP (auction), not detection.
//	BEHIND  — we flagged it only AFTER it was already liquidated → detection
//	          was too slow.
//	MISSED  — we never saw it at all → detection missed it entirely (poll too
//	          slow / not in the watch-set).
//
// The AHEAD vs BEHIND/MISSED split tells us exactly which bottleneck to fix.
//
// Usage: HELIUS_RPC=<url> [RUN_DIR=runs/save] [PAGES=6] go run ./cmd/liq_race
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/gagliardetto/solana-go/base58"

	"solana-arb-backtest-go/internal/envfile"
)

const solendProgram = "So1endDq2YkqhipRh3WViPa8hdiSpxWy6z3Z6tMCpAo"

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
		time.Sleep(time.Duration(400<<attempt) * time.Millisecond)
	}
	return nil, false
}

// ourFirstSeen returns the earliest unix-second we logged each obligation,
// from our decisions ledger.
func ourFirstSeen(runDir string) map[string]uint64 {
	m := map[string]uint64{}
	f, err := os.Open(runDir + "/decisions.jsonl")
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var v map[string]any
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			continue
		}
		// Save/Kamino key the account as "obligation"; marginfi as "liquidatee".
		var acct string
		if a, ok := v["obligation"].(string); ok {
			acct = a
		} else if a, ok := v["liquidatee"].(string); ok {
			acct = a
		} else {
			continue
		}
		tf, ok := v["t"].(float64)
		if !ok {
			continue
		}
		t := uint64(tf)
		if cur, exists := m[acct]; !exists || t < cur {
			m[acct] = t
		}
	}
	return m
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
	runDir := os.Getenv("RUN_DIR")
	if runDir == "" {
		runDir = "runs/save"
	}
	pages := 6
	if v := os.Getenv("PAGES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			pages = n
		}
	}

	seen := ourFirstSeen(runDir)
	fmt.Fprintf(os.Stderr, "[race] loaded %d distinct obligations we logged (from %s/decisions.jsonl)\n", len(seen), runDir)

	// Page recent Solend signatures.
	type sigEntry struct {
		sig string
		bt  *uint64
	}
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
		result, _ := v["result"].([]any)
		page := make([]map[string]any, 0, len(result))
		for _, e := range result {
			if m, ok := e.(map[string]any); ok {
				page = append(page, m)
			}
		}
		if len(page) == 0 {
			break
		}
		if last := page[len(page)-1]; last != nil {
			if sg, ok := last["signature"].(string); ok {
				before = sg
			}
		}
		for _, e := range page {
			if e["err"] != nil {
				continue
			}
			sig, _ := e["signature"].(string)
			var bt *uint64
			if btf, ok := e["blockTime"].(float64); ok {
				b := uint64(btf)
				bt = &b
			}
			sigs = append(sigs, sigEntry{sig, bt})
		}
	}
	fmt.Fprintf(os.Stderr, "[race] scanning %d Solend txs for competitor liquidations …\n", len(sigs))

	var ahead, behind, missed, total uint32
	var aheadSecs []int64
	for _, e := range sigs {
		v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getTransaction",
			"params": []any{e.sig, map[string]any{"encoding": "jsonParsed", "maxSupportedTransactionVersion": 0, "commitment": "confirmed"}}})
		if !ok {
			continue
		}
		result, _ := v["result"].(map[string]any)
		if result == nil {
			continue
		}
		var ixs []any
		if tx, ok := result["transaction"].(map[string]any); ok {
			if msg, ok := tx["message"].(map[string]any); ok {
				if instrs, ok := msg["instructions"].([]any); ok {
					ixs = append(ixs, instrs...)
				}
			}
		}
		if meta, ok := result["meta"].(map[string]any); ok {
			if inner, ok := meta["innerInstructions"].([]any); ok {
				for _, g := range inner {
					if gm, ok := g.(map[string]any); ok {
						if instrs, ok := gm["instructions"].([]any); ok {
							ixs = append(ixs, instrs...)
						}
					}
				}
			}
		}
		for _, ixAny := range ixs {
			ix, ok := ixAny.(map[string]any)
			if !ok {
				continue
			}
			if pid, _ := ix["programId"].(string); pid != solendProgram {
				continue
			}
			dataStr, _ := ix["data"].(string)
			data, err := base58.Decode(dataStr)
			if err != nil || len(data) == 0 {
				continue
			}
			tag := data[0]
			// obligation account index: tag 17 (LiquidateAndRedeem) → [10]; tag 12 → [6].
			var idx int
			switch tag {
			case 17:
				idx = 10
			case 12:
				idx = 6
			default:
				continue
			}
			accts, _ := ix["accounts"].([]any)
			if idx >= len(accts) {
				continue
			}
			obl, ok := accts[idx].(string)
			if !ok {
				continue
			}
			total++
			var landed uint64
			if e.bt != nil {
				landed = *e.bt
			}
			if ourT, exists := seen[obl]; exists {
				if ourT <= landed {
					ahead++
					aheadSecs = append(aheadSecs, int64(landed)-int64(ourT))
				} else {
					behind++
				}
			} else {
				missed++
			}
		}
		time.Sleep(15 * time.Millisecond)
	}

	fmt.Printf("\n═══ race analysis (Solend, vs our %s detections) ═══\n", runDir)
	fmt.Printf("competitor liquidations seen: %d\n", total)
	fmt.Printf("  AHEAD  (we flagged it BEFORE it was liquidated → lost on TIP/ACTION): %d\n", ahead)
	fmt.Printf("  BEHIND (flagged only after it was already gone → detection slow):     %d\n", behind)
	fmt.Printf("  MISSED (never saw it at all → detection missed entirely):             %d\n", missed)
	if len(aheadSecs) > 0 {
		sort.Slice(aheadSecs, func(i, j int) bool { return aheadSecs[i] < aheadSecs[j] })
		med := aheadSecs[len(aheadSecs)/2]
		fmt.Printf("  of the AHEAD ones, median lead time: %ds (we had this long to win the auction)\n", med)
	}
	fmt.Println("\nVERDICT:")
	if total == 0 {
		fmt.Println("  no competitor liquidations in window — raise PAGES or run during activity.")
	} else if ahead >= behind+missed {
		fmt.Println("  Mostly AHEAD → detection is NOT the bottleneck; we're losing the AUCTION.")
		fmt.Println("  Optimize: tip sizing (TIP_FRACTION_BPS), arm coverage, submit colocation — not detection.")
	} else {
		fmt.Println("  Mostly BEHIND/MISSED → DETECTION SPEED is the bottleneck.")
		fmt.Println("  Optimize: event-driven Lazer trigger (kill polls), watch-set coverage, crank front-run.")
	}
}
