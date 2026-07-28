// pump_census — read `runs/pump/events.jsonl` (produced by `pump_collect`) and
// report the ground-truth opportunity size for pump.fun launches:
//   - launches per hour (and the observation window)
//   - % of launches that graduate vs die (within the window)
//   - median / distribution of time-to-graduation
//   - distribution of peak-price-multiple after launch
//   - a dev-dump rug proxy (launches where the dev wallet sells, how fast)
//
// This answers "is there even money here, and where" BEFORE any capital risk.
// It is a pure read of the collected file — no RPC, no chain writes.
//
// Usage: `go run ./cmd/pump_census [-- path/to/events.jsonl]`
//
//	env: PUMP_OUT (default runs/pump/events.jsonl)
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"

	"solana-arb-backtest-go/internal/envfile"
)

// token is the per-mint rollup built from the event stream.
type token struct {
	createMs     int64
	hasCreate    bool
	dev          string
	initPrice    float64 // price_in_sol implied by the create's initial reserves.
	trades       uint64
	buys         uint64
	sells        uint64
	peakPrice    float64
	migrateMs    int64
	hasMigrate   bool
	devFirstSell int64
	hasDevFirst  bool
	devSold      bool
}

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	idx := int(math.Round((float64(len(sorted)) - 1.0) * p))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	if idx < 0 {
		idx = 0
	}
	return sorted[idx]
}

func asFloat(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func asUint64(v any) uint64 {
	if f, ok := v.(float64); ok && f >= 0 {
		return uint64(f)
	}
	return 0
}

func asStr(v any) string {
	s, _ := v.(string)
	return s
}

// recordIsSane rejects records whose shape or internal consistency betrays a
// torn write.
func recordIsSane(v map[string]any) bool {
	et := asStr(v["event_type"])
	switch et {
	case "create", "buy", "sell", "migrate":
	default:
		return false
	}
	// base58 pubkey = 32-44 chars, signature = 86-88 chars
	mint := asStr(v["mint"])
	mintOK := len(mint) >= 32 && len(mint) <= 44
	sig := asStr(v["signature"])
	sigOK := len(sig) >= 80 && len(sig) <= 90
	if !mintOK || !sigOK || asUint64(v["unix_ms"]) == 0 {
		return false
	}
	if et == "buy" || et == "sell" {
		// Zero values are legitimate (e.g. dust sells into an emptied curve,
		// vsr = 0 → price 0); torn lines betray themselves by MISSING fields or
		// by a price that disagrees with the reserves it was derived from.
		vs, vsOK := asFloat(v["virtual_sol_reserves"])
		vt, vtOK := asFloat(v["virtual_token_reserves"])
		p, pOK := asFloat(v["price_in_sol"])
		if !vsOK || !vtOK || !pOK {
			return false
		}
		if math.IsNaN(p) || math.IsInf(p, 0) || p < 0.0 || vs < 0.0 || vt < 0.0 {
			return false
		}
		if vt > 0.0 {
			recomputed := (vs / 1e9) / (vt / 1e6)
			if math.Abs(p-recomputed) > recomputed*0.01 {
				return false
			}
		}
	}
	return true
}

func main() {
	envfile.LoadDotEnv()

	path := ""
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	if path == "" {
		path = os.Getenv("PUMP_OUT")
	}
	if path == "" {
		path = "runs/pump/events.jsonl"
	}

	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot open %s: %v\n", path, err)
		os.Exit(1)
	}
	defer f.Close()

	tokens := map[string]*token{}
	var nEvents, nCreate, nBuy, nSell, nMigrate, nSkipped uint64
	var tsMin int64 = math.MaxInt64
	var tsMax int64

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			nSkipped++
			continue
		}
		// Torn-line guard: interleaved writers have produced lines that still
		// parse as JSON but carry woven-together garbage fields. Require a sane
		// base shape, and on trades a price consistent with the raw reserves.
		if !recordIsSane(v) {
			nSkipped++
			continue
		}
		nEvents++
		ts := int64(asUint64(v["unix_ms"]))
		if ts > 0 {
			if ts < tsMin {
				tsMin = ts
			}
			if ts > tsMax {
				tsMax = ts
			}
		}
		mint := asStr(v["mint"])
		if mint == "" {
			continue
		}
		et := asStr(v["event_type"])
		t, ok := tokens[mint]
		if !ok {
			t = &token{}
			tokens[mint] = t
		}
		switch et {
		case "create":
			nCreate++
			t.createMs = ts
			t.hasCreate = true
			t.dev = asStr(v["dev"])
			vs, _ := asFloat(v["init_virtual_sol_reserves"])
			vt, _ := asFloat(v["init_virtual_token_reserves"])
			if vt > 0.0 {
				// price_in_sol with 6 decimals: (vs/1e9)/(vt/1e6)
				t.initPrice = (vs / 1e9) / (vt / 1e6)
			}
		case "buy", "sell":
			if et == "buy" {
				nBuy++
				t.buys++
			} else {
				nSell++
				t.sells++
			}
			t.trades++
			p, _ := asFloat(v["price_in_sol"])
			if p > t.peakPrice {
				t.peakPrice = p
			}
			// dev-dump proxy: this sell's actor is the create's dev wallet.
			if et == "sell" {
				actor := asStr(v["actor"])
				if t.dev != "" && actor != "" && t.dev == actor {
					t.devSold = true
					if !t.hasDevFirst {
						t.devFirstSell = ts
						t.hasDevFirst = true
					}
				}
			}
		case "migrate":
			nMigrate++
			if !t.hasMigrate {
				t.migrateMs = ts
				t.hasMigrate = true
			}
		}
	}

	if nEvents == 0 {
		fmt.Fprintf(os.Stderr, "no events in %s\n", path)
		os.Exit(1)
	}

	spanMs := tsMax - tsMin
	if spanMs < 0 {
		spanMs = 0
	}
	spanHours := float64(spanMs) / 3_600_000.0

	fmt.Printf("═══ pump.fun census — %s ═══\n", path)
	fmt.Printf("events %d  (create %d, buy %d, sell %d, migrate %d)  [skipped %d torn/malformed lines]\n",
		nEvents, nCreate, nBuy, nSell, nMigrate, nSkipped)
	fmt.Printf("observation window: %.2f min (%.3f h)  |  distinct mints seen: %d\n",
		float64(spanMs)/60_000.0, spanHours, len(tokens))
	if spanHours > 0.0 {
		fmt.Printf("launch rate: %.1f launches/hour  (%d creates in window)\n",
			float64(nCreate)/spanHours, nCreate)
		fmt.Printf("migration rate: %.2f migrations/hour  (%d migrates in window)\n",
			float64(nMigrate)/spanHours, nMigrate)
	}

	// ── Launch-cohort analysis: only mints whose CREATE we captured in-window ──
	var cohort []*token
	for _, t := range tokens {
		if t.hasCreate {
			cohort = append(cohort, t)
		}
	}
	fmt.Printf("\n── launch cohort (create seen in-window): %d ──\n", len(cohort))
	if len(cohort) == 0 {
		fmt.Println("(no creates captured in this window — run the collector longer)")
		return
	}

	var graduated, noTrades, devDumped int
	for _, t := range cohort {
		if t.hasMigrate {
			graduated++
		}
		if t.trades == 0 {
			noTrades++
		}
		if t.devSold {
			devDumped++
		}
	}

	fmt.Printf("graduated (migrated) in-window: %d/%d = %.2f%%\n",
		graduated, len(cohort), 100.0*float64(graduated)/float64(len(cohort)))
	fmt.Printf("died-so-far proxy (0 trades after launch): %d/%d = %.2f%%\n",
		noTrades, len(cohort), 100.0*float64(noTrades)/float64(len(cohort)))
	fmt.Println("NOTE: window is short, so 'graduated %' is a floor and 'died %' is a")
	fmt.Println("      ceiling — most launches' fate falls outside the capture window.")

	// time-to-graduation for the ones that did migrate in-window
	var ttg []float64
	for _, t := range cohort {
		if t.hasCreate && t.hasMigrate && t.migrateMs >= t.createMs {
			ttg = append(ttg, float64(t.migrateMs-t.createMs)/1000.0)
		}
	}
	sort.Float64s(ttg)
	if len(ttg) > 0 {
		fmt.Printf("\ntime-to-graduation (s): p50 %.0f  p25 %.0f  p75 %.0f  min %.0f  max %.0f  (n=%d)\n",
			pct(ttg, 0.50), pct(ttg, 0.25), pct(ttg, 0.75), ttg[0], ttg[len(ttg)-1], len(ttg))
	} else {
		fmt.Println("\ntime-to-graduation: no create→migrate pair fully inside the window")
	}

	// peak price multiple vs the launch price, for launches that traded
	var mult []float64
	for _, t := range cohort {
		if t.trades > 0 && t.initPrice > 0.0 && t.peakPrice > 0.0 {
			mult = append(mult, t.peakPrice/t.initPrice)
		}
	}
	sort.Float64s(mult)
	if len(mult) > 0 {
		fmt.Printf("\npeak price multiple (peak / launch price), launches that traded (n=%d):\n", len(mult))
		fmt.Printf("  p10 %.2fx  p50 %.2fx  p75 %.2fx  p90 %.2fx  p99 %.2fx  max %.2fx\n",
			pct(mult, 0.10), pct(mult, 0.50), pct(mult, 0.75), pct(mult, 0.90), pct(mult, 0.99), mult[len(mult)-1])
		for _, thr := range []float64{2.0, 5.0, 10.0, 50.0} {
			c := 0
			for _, m := range mult {
				if m >= thr {
					c++
				}
			}
			fmt.Printf("  ≥%4.0fx : %5d / %d = %.1f%%\n", thr, c, len(mult), 100.0*float64(c)/float64(len(mult)))
		}
	}

	// dev-dump rug proxy
	fmt.Printf("\ndev-dump proxy: %d/%d = %.2f%% of launches had the dev wallet SELL in-window\n",
		devDumped, len(cohort), 100.0*float64(devDumped)/float64(len(cohort)))
	var devDt []float64
	for _, t := range cohort {
		if t.hasCreate && t.hasDevFirst && t.devFirstSell >= t.createMs {
			devDt = append(devDt, float64(t.devFirstSell-t.createMs)/1000.0)
		}
	}
	sort.Float64s(devDt)
	if len(devDt) > 0 {
		fmt.Printf("  dev time-to-first-sell (s): p50 %.1f  p25 %.1f  min %.1f  (n=%d)\n",
			pct(devDt, 0.50), pct(devDt, 0.25), devDt[0], len(devDt))
	}
	fmt.Println("  (sell-revert honeypots are NOT visible here: the collector records only\n   successful txs. That rug flavour needs a failed-tx scan — future work.)")
}
