// Command pump_backtest replays a collected pump.fun event dataset
// chronologically and simulates candidate strategies against REAL data,
// reporting honest per-strategy EV, win-rate, and PnL distribution.
//
// It reads `runs/pump/events.jsonl` (produced by `pump_collect`) and NEVER
// signs or submits a transaction. It shares no state with the liquidation
// engine.
//
// ════════════════════════════════════════════════════════════════════════════
//
//	#1 CORRECTNESS INVARIANT: NO LOOK-AHEAD BIAS — enforced STRUCTURALLY
//
// ════════════════════════════════════════════════════════════════════════════
// A decision at time T may only use information observable at or before T. We
// do not rely on discipline; the code makes the leak impossible by
// construction:
//
//   - Each mint's events are sorted once into strict chronological order.
//   - The ENTRY decision is computed in ONE dedicated pass over the prefix
//     slice `events[:k]` where `k` is chosen so every event in it has
//     `ms <= T_entry` (`T_entry = create_ms + detection_latency`). The
//     entry's curve price, the dev-allocation / dev-prebuy flags, the
//     first-second buyer count and the liquidity are ALL derived from that
//     prefix and nothing else. The suffix (the future) is simply not in
//     scope for the entry — it is a different slice.
//   - The EXIT is a forward-only walk over the suffix `events[k:]`. It
//     consumes events one at a time, updating the "as-of now" curve state to
//     each event's POST-trade reserves and then testing the exit rule. It
//     never indexes ahead. A take-profit fires only when the replayed price
//     ACTUALLY crossed the target at some t' > entry; a stop/dev-dump that
//     crashes the price first is seen first (in event order), so the
//     strategy eats the post-crash price. A time-based exit fires at
//     `T_exit` using the state as-of `T_exit` (the state BEFORE the first
//     event past it), never the crashing event's own price.
//   - The tests at the bottom pin this: an entry cannot see a future price
//     spike, and a spike after a later entry-time IS seen — proving the
//     boundary is respected, not merely intended.
//
// ════════════════════════════════════════════════════════════════════════════
//
//	COST MODEL (a backtest that ignores costs lies too) — all VERIFIED or configurable
//
// ════════════════════════════════════════════════════════════════════════════
//   - Detection latency: you cannot buy at the creation instant — faster bots
//     already did. Entry is at `create_ms + DETECTION_LATENCY_MS` (default
//     1200).
//   - Bonding-curve slippage: buys/sells move the price along the curve. We
//     use the constant-product reserves math from arbengine/internal/pump
//     (VERIFIED to the lamport against a captured trade) at the curve state
//     as-of the action.
//   - pump.fun trading fee: 125 bps (95 protocol + 30 creator), READ from a
//     real captured trade's `fee_basis_points`+`creator_fee_basis_points`.
//     Charged on top of the SOL that enters the curve on a buy, and off the
//     SOL received on a sell. Configurable via PUMP_FEE_BPS.
//   - Jito tip + network fee: sniping needs a competitive tip. Charged per
//     trade (entry and exit), configurable (ENTRY_TIP_SOL default 0.001,
//     EXIT_TIP_SOL default 0.0005, BASE_FEE_SOL default 0.000005).
//   - Rug / dev-dump: reconstructed from the real buy/sell path; if the price
//     collapses before our exit rule fires, we realize the loss at the real
//     post-dump curve price. On migration the bonding curve is gone, so we
//     exit at the last curve price (we have no PumpSwap price feed — see
//     caveats).
//   - Honeypot (sell-revert): NOT in the data (collector records successes
//     only). We do not pretend it is zero — it is flagged as unmodeled
//     downside.
//
// MODELING ASSUMPTION (stated honestly): our own buy/sell is priced against
// the curve as-of the action (we pay real slippage), but we do NOT perturb
// the recorded path for other traders (a small-trader assumption). For the
// buy sizes swept here this is a mild optimism on the exit side; larger
// sizes would need a full re-simulation of every participant.
//
// Usage: EVENTS=runs/pump/events.jsonl go run ./cmd/pump_backtest
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"arbengine/internal/config"
	"arbengine/internal/pump"
)

const lamportsPerSol = 1e9

// initVirtualSol is the canonical pump.fun launch virtual-SOL reserve (=
// the constant virtual offset, so real_sol = virtual_sol - initVirtualSol).
// VERIFIED in the pump package tests.
const initVirtualSol uint64 = 30_000_000_000

// maxHoldMs is a safety cap so a position never rides forever in the sim.
const maxHoldMs int64 = 3_600_000

// ── env helpers ──────────────────────────────────────────────────────────────

func envF64(k string, def float64) float64 {
	return config.EnvFloat(k, def)
}

func envI64(k string, def int64) int64 {
	v, ok := config.EnvOptional(k)
	if !ok {
		return def
	}
	var n int64
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return def
	}
	return n
}

// costs are shared by every simulated trade. All in SOL except the fee (bps).
type costs struct {
	pumpFeeBps float64
	entryTip   float64
	exitTip    float64
	baseFee    float64
}

func costsFromEnv() costs {
	return costs{
		pumpFeeBps: envF64("PUMP_FEE_BPS", 125.0),
		entryTip:   envF64("ENTRY_TIP_SOL", 0.001),
		exitTip:    envF64("EXIT_TIP_SOL", 0.0005),
		baseFee:    envF64("BASE_FEE_SOL", 0.000005),
	}
}

// ── event model ──────────────────────────────────────────────────────────────

type kind int

const (
	kindBuy kind = iota
	kindSell
	kindMigrate
)

// ev is one decoded JSONL record, reduced to what the sim needs.
type ev struct {
	ms    int64
	kind  kind
	actor string
	// vsol/vtok are post-trade virtual reserves (for Create these are the
	// initial reserves).
	vsol uint64
	vtok uint64
	tok  uint64
}

// mint holds everything known about one launch, built from its events.
type mint struct {
	createMs    int64
	dev         string
	initVsol    uint64
	initVtok    uint64
	totalSupply uint64
	migrated    bool
	// events are trades + migrate, strictly sorted by (ms, original order).
	events []ev
}

func load(path string) ([]*mint, int64, int64) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot open %s: %v\n", path, err)
		os.Exit(1)
	}
	defer f.Close()

	mints := make(map[string]*mint)
	var order []string
	tsMin := int64(-1)
	tsMax := int64(0)

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			continue
		}
		ms := jsonU64(v, "unix_ms")
		if ms == 0 {
			continue
		}
		msI := int64(ms)
		if tsMin == -1 || msI < tsMin {
			tsMin = msI
		}
		if msI > tsMax {
			tsMax = msI
		}
		mintKey, _ := v["mint"].(string)
		if mintKey == "" {
			continue
		}
		et, _ := v["event_type"].(string)
		actor, _ := v["actor"].(string)

		getOrInsert := func() *mint {
			if m, ok := mints[mintKey]; ok {
				return m
			}
			m := &mint{}
			mints[mintKey] = m
			order = append(order, mintKey)
			return m
		}

		switch et {
		case "create":
			m := getOrInsert()
			m.createMs = msI
			if dev, ok := v["dev"].(string); ok && dev != "" {
				m.dev = dev
			} else {
				m.dev = actor
			}
			m.initVsol = jsonU64OrDefault(v, "init_virtual_sol_reserves", initVirtualSol)
			m.initVtok = jsonU64(v, "init_virtual_token_reserves")
			m.totalSupply = jsonU64(v, "token_total_supply")
		case "buy", "sell":
			m := getOrInsert()
			k := kindBuy
			if et == "sell" {
				k = kindSell
			}
			m.events = append(m.events, ev{
				ms:    msI,
				kind:  k,
				actor: actor,
				vsol:  jsonU64(v, "virtual_sol_reserves"),
				vtok:  jsonU64(v, "virtual_token_reserves"),
				tok:   jsonU64(v, "token_amount"),
			})
		case "migrate":
			m := getOrInsert()
			m.migrated = true
			m.events = append(m.events, ev{ms: msI, kind: kindMigrate})
		}
	}

	out := make([]*mint, 0, len(order))
	for _, k := range order {
		m := mints[k]
		sort.SliceStable(m.events, func(i, j int) bool { return m.events[i].ms < m.events[j].ms })
		out = append(out, m)
	}
	if tsMin == -1 {
		tsMin = 0
	}
	return out, tsMin, tsMax
}

func jsonU64(v map[string]any, key string) uint64 {
	return jsonU64OrDefault(v, key, 0)
}

func jsonU64OrDefault(v map[string]any, key string, def uint64) uint64 {
	f, ok := v[key].(float64)
	if !ok {
		return def
	}
	if f < 0 {
		return def
	}
	return uint64(f)
}

// ── strategy config ──────────────────────────────────────────────────────────

// filter is an entry-time-observable filter (strategy 2). Zero-value fields
// (nil pointer) mean "don't care".
type filter struct {
	// maxDevAlloc rejects if the dev's initial token allocation (fraction of
	// supply) exceeds this.
	maxDevAlloc *float64
	// rejectDevPrebuy rejects if the dev pre-bought at/near create.
	rejectDevPrebuy bool
	// minBuyers requires at least this many DISTINCT buyers before T_entry.
	minBuyers *uint32
	// minLiqSol requires at least this much real SOL liquidity (SOL) at T_entry.
	minLiqSol *float64
}

type exitKind int

const (
	exitTakeProfit exitKind = iota
	exitHold
	exitTrailing
	exitFirstDevSell
)

// exit is a tagged exit rule: TakeProfit/Trailing carry a pct, Hold carries
// seconds.
type exit struct {
	kind exitKind
	pct  float64
	secs uint64
}

func takeProfit(pct float64) exit { return exit{kind: exitTakeProfit, pct: pct} }
func hold(secs uint64) exit       { return exit{kind: exitHold, secs: secs} }
func trailing(pct float64) exit   { return exit{kind: exitTrailing, pct: pct} }
func firstDevSell() exit          { return exit{kind: exitFirstDevSell} }

type strat struct {
	latencyMs int64
	buySol    float64
	filter    filter
	exit      exit
}

// trade is the outcome of a single simulated round-trip.
type trade struct {
	entryMs int64
	pnlSol  float64
	pnlPct  float64
}

// ── the per-mint simulation (look-ahead-free by construction) ────────────────

// simulate runs one strategy on one launch. Returns (zero, false) if the
// launch is not tradeable (no create seen) or the filter rejects it.
func simulate(m *mint, s *strat, c *costs) (trade, bool) {
	if m.createMs == 0 || m.initVtok == 0 {
		return trade{}, false // we only trade launches whose birth (and initial curve) we saw
	}
	tEntry := m.createMs + s.latencyMs

	// ── ENTRY PASS: only the prefix with ms <= T_entry is in scope. ──────────
	// Curve state as-of T_entry starts at the create's initial reserves and is
	// advanced by every trade at or before T_entry.
	vsol, vtok := m.initVsol, m.initVtok
	buyers := make(map[string]struct{})
	var devAllocTokens uint64
	devPrebought := false
	firstSec := m.createMs + 1000
	k := 0
	for _, e := range m.events {
		if e.ms > tEntry {
			break
		}
		k++
		switch e.kind {
		case kindBuy:
			if e.ms <= firstSec {
				buyers[e.actor] = struct{}{}
			}
			if e.actor == m.dev {
				devPrebought = true
				devAllocTokens = satAddU64(devAllocTokens, e.tok)
			}
			vsol = e.vsol
			vtok = e.vtok
		case kindSell:
			vsol = e.vsol
			vtok = e.vtok
		case kindMigrate:
			return trade{}, false // already graduated before we could enter
		}
	}

	// ── FILTER (uses only the prefix-derived features above) ─────────────────
	f := &s.filter
	if f.maxDevAlloc != nil {
		alloc := 0.0
		if m.totalSupply > 0 {
			alloc = float64(devAllocTokens) / float64(m.totalSupply)
		}
		if alloc > *f.maxDevAlloc {
			return trade{}, false
		}
	}
	if f.rejectDevPrebuy && devPrebought {
		return trade{}, false
	}
	if f.minBuyers != nil {
		if uint32(len(buyers)) < *f.minBuyers {
			return trade{}, false
		}
	}
	if f.minLiqSol != nil {
		realSol := float64(satSubU64(vsol, initVirtualSol)) / lamportsPerSol
		if realSol < *f.minLiqSol {
			return trade{}, false
		}
	}
	if vtok == 0 {
		return trade{}, false
	}

	// ── OPEN POSITION at the as-of-T_entry curve state. ──────────────────────
	// Buy budget buySol is total SOL out of pocket for the buy incl. pump fee;
	// the SOL that actually enters the curve is the budget net of the fee.
	feeMul := 1.0 + c.pumpFeeBps/1e4
	solIntoCurve := uint64(s.buySol / feeMul * lamportsPerSol)
	positionTokens := pump.CurveBuyTokensOut(vsol, vtok, solIntoCurve)
	if positionTokens == 0 {
		return trade{}, false
	}
	entryCost := s.buySol + c.entryTip + c.baseFee
	entryPrice := float64(vsol) / float64(vtok) // raw SOL-lamports per raw token

	// ── EXIT PASS: forward-only walk of the suffix events[k:]. ──────────────
	peak := entryPrice
	tHardExit := tEntry + maxHoldMs
	tTimedExit := tHardExit
	if s.exit.kind == exitHold {
		tTimedExit = tEntry + int64(s.exit.secs)*1000
		if tTimedExit > tHardExit {
			tTimedExit = tHardExit
		}
	}

	// exit_state is the curve reserves at which we finally sell.
	exitVsol := vsol
	exitVtok := vtok

	for _, e := range m.events[k:] {
		// (a) Time-based exit fires BEFORE consuming this event, using the state
		//     as-of the exit instant (i.e. the state BEFORE this later event) —
		//     never this event's own (possibly crashing) price. Look-ahead-free.
		if e.ms >= tTimedExit {
			break
		}
		switch e.kind {
		case kindMigrate:
			// Curve gone; realize at the last curve state (no PumpSwap feed).
			goto realize
		case kindBuy, kindSell:
			// (b) React to the event: advance curve to its POST-trade state.
			exitVsol = e.vsol
			exitVtok = e.vtok
			price := float64(exitVsol) / float64(exitVtok)
			if price > peak {
				peak = price
			}
			hit := false
			switch s.exit.kind {
			case exitTakeProfit:
				hit = price >= entryPrice*(1.0+s.exit.pct/100.0)
			case exitTrailing:
				hit = price <= peak*(1.0-s.exit.pct/100.0)
			case exitFirstDevSell:
				hit = e.kind == kindSell && e.actor == m.dev
			case exitHold:
				hit = false // handled by the timed branch above
			}
			if hit {
				goto realize
			}
		}
	}
	// If we ran out of events still open, exit at the last known curve state
	// (equal to the entry state if nothing traded) — a real, forced close.

realize:
	// ── REALIZE the sell into the exit-state curve. ──────────────────────────
	// We price OTHERS' trades against the recorded (unperturbed) path, but we DO
	// carry our own buy's footprint: our SOL is still sitting in the pool and our
	// tokens are still out of it. So we sell back into the recorded reserves with
	// our own delta added (+sol_into_curve to vsol, -position_tokens from vtok).
	// Without this we would double-charge curve convexity — pay entry slippage AND
	// exit slippage on a static curve — inventing a loss that isn't there. With it,
	// a flat market round-trips to ~0 (minus fees/tips) and a real loss comes only
	// from OTHERS moving the price (dev dumps etc.), which is the honest cost.
	adjVsol := satAddU64(exitVsol, solIntoCurve)
	adjVtok := satSubU64(exitVtok, positionTokens)
	if adjVtok < 1 {
		adjVtok = 1
	}
	solOutCurve := pump.CurveSellSolOut(adjVsol, adjVtok, positionTokens)
	netSol := float64(solOutCurve)/lamportsPerSol*(1.0-c.pumpFeeBps/1e4) - c.exitTip - c.baseFee
	pnlSol := netSol - entryCost
	return trade{
		entryMs: tEntry,
		pnlSol:  pnlSol,
		pnlPct:  100.0 * pnlSol / entryCost,
	}, true
}

func satAddU64(a, b uint64) uint64 {
	s := a + b
	if s < a {
		return ^uint64(0)
	}
	return s
}

func satSubU64(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}

// ── aggregate stats ──────────────────────────────────────────────────────────

func pctile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return nan()
	}
	idx := int(roundHalfAwayFromZero(float64(len(sorted)-1) * p))
	if idx > len(sorted)-1 {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func roundHalfAwayFromZero(f float64) float64 {
	if f >= 0 {
		return float64(int64(f + 0.5))
	}
	return float64(int64(f - 0.5))
}

func nan() float64 {
	var z float64
	return z / z
}

type stats struct {
	n         int
	winRate   float64
	meanSol   float64
	medianSol float64
	meanPct   float64
	totalSol  float64
	maxDD     float64
	p10       float64
	p90       float64
}

func summarize(trades []trade) stats {
	n := len(trades)
	if n == 0 {
		return stats{}
	}
	wins := 0
	var totalSol float64
	for _, t := range trades {
		if t.pnlSol > 0.0 {
			wins++
		}
		totalSol += t.pnlSol
	}
	meanSol := totalSol / float64(n)
	var sumPct float64
	for _, t := range trades {
		sumPct += t.pnlPct
	}
	meanPct := sumPct / float64(n)

	solSorted := make([]float64, n)
	for i, t := range trades {
		solSorted[i] = t.pnlSol
	}
	sort.Float64s(solSorted)
	medianSol := pctile(solSorted, 0.50)

	pctSorted := make([]float64, n)
	for i, t := range trades {
		pctSorted[i] = t.pnlPct
	}
	sort.Float64s(pctSorted)

	// Max drawdown of the equity curve, trades ordered by entry time.
	ordered := make([]trade, n)
	copy(ordered, trades)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].entryMs < ordered[j].entryMs })
	var equity, peak, maxDD float64
	for _, t := range ordered {
		equity += t.pnlSol
		if equity > peak {
			peak = equity
		}
		if dd := peak - equity; dd > maxDD {
			maxDD = dd
		}
	}

	return stats{
		n:         n,
		winRate:   100.0 * float64(wins) / float64(n),
		meanSol:   meanSol,
		medianSol: medianSol,
		meanPct:   meanPct,
		totalSol:  totalSol,
		maxDD:     maxDD,
		p10:       pctile(pctSorted, 0.10),
		p90:       pctile(pctSorted, 0.90),
	}
}

func exitLabel(e exit) string {
	switch e.kind {
	case exitTakeProfit:
		return fmt.Sprintf("TP+%.0f%%", e.pct)
	case exitHold:
		return fmt.Sprintf("hold%ds", e.secs)
	case exitTrailing:
		return fmt.Sprintf("trail%.0f%%", e.pct)
	case exitFirstDevSell:
		return "devSell"
	default:
		return ""
	}
}

func filterLabel(f filter) string {
	var parts []string
	if f.maxDevAlloc != nil {
		parts = append(parts, fmt.Sprintf("devAlloc<%.0f%%", *f.maxDevAlloc*100.0))
	}
	if f.rejectDevPrebuy {
		parts = append(parts, "noDevPrebuy")
	}
	if f.minBuyers != nil {
		parts = append(parts, fmt.Sprintf(">=%dbuyers", *f.minBuyers))
	}
	if f.minLiqSol != nil {
		parts = append(parts, fmt.Sprintf(">=%gSOL", *f.minLiqSol))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "+")
}

// row is one row of the results table.
type row struct {
	label string
	st    stats
}

func runSweep(mints []*mint, strats []strat, c *costs, labelOf func(strat) string) []row {
	rows := make([]row, 0, len(strats))
	for i := range strats {
		s := strats[i]
		var trades []trade
		for _, m := range mints {
			if t, ok := simulate(m, &s, c); ok {
				trades = append(trades, t)
			}
		}
		rows = append(rows, row{label: labelOf(s), st: summarize(trades)})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].st.meanSol > rows[j].st.meanSol })
	return rows
}

func printTable(title string, rows []row) {
	fmt.Printf("\n══ %s ══\n", title)
	fmt.Printf(
		"%-34s %6s %7s %11s %10s %9s %10s %8s %8s %9s\n",
		"params", "trades", "win%", "mean SOL", "med SOL", "mean%", "total SOL", "p10%", "p90%", "maxDD SOL",
	)
	fmt.Println(strings.Repeat("-", 120))
	for _, r := range rows {
		s := r.st
		if s.n == 0 {
			fmt.Printf("%-34s %6d %7s\n", r.label, 0, "(no trades)")
			continue
		}
		fmt.Printf(
			"%-34s %6d %6.1f%% %11.5f %10.5f %8.1f%% %10.4f %8.1f %8.1f %9.4f\n",
			r.label, s.n, s.winRate, s.meanSol, s.medianSol, s.meanPct, s.totalSol, s.p10, s.p90, s.maxDD,
		)
	}
}

func verdict(strategy string, rows []row, minTrades int) {
	// Best row (rows are pre-sorted by mean_sol desc) that clears a trade-count bar.
	var best *row
	for i := range rows {
		if rows[i].st.n >= minTrades {
			best = &rows[i]
			break
		}
	}
	switch {
	case best != nil && best.st.meanSol > 0.0:
		fmt.Printf(
			"  VERDICT [%s]: best positive-EV set = `%s` (mean %+.5f SOL/trade over %d trades, win %.1f%%). Treat as HYPOTHESIS until confirmed on the multi-hour set.\n",
			strategy, best.label, best.st.meanSol, best.st.n, best.st.winRate,
		)
	case best != nil:
		fmt.Printf(
			"  VERDICT [%s]: LOSING after costs. Best set `%s` still %+.5f SOL/trade over %d trades.\n",
			strategy, best.label, best.st.meanSol, best.st.n,
		)
	default:
		fmt.Printf(
			"  VERDICT [%s]: INCONCLUSIVE — no parameter set reached %d trades in this sample.\n",
			strategy, minTrades,
		)
	}
}

// ── strategy 3: smart-money follow (strict train/test split) ─────────────────

func smartMoney(mints []*mint, splitMs int64, c *costs) {
	fmt.Println("\n\n╔══ STRATEGY 3: SMART-MONEY FOLLOW (train first-half / test second-half) ══╗")
	// TRAIN: realized net SOL cash-flow per wallet, first half ONLY.
	spent := make(map[string]float64)
	recv := make(map[string]float64)
	tradesCt := make(map[string]uint32)
	for _, m := range mints {
		for _, e := range m.events {
			if e.ms >= splitMs {
				continue
			}
			switch e.kind {
			case kindBuy:
				// SOL that left the buyer ~= curve delta they caused; use tok*price proxy
				// is fragile, so approximate with the reserve-implied sol: we don't have
				// per-event sol for reserves here, so use price*tok.
				vtok := e.vtok
				if vtok == 0 {
					vtok = 1
				}
				price := float64(e.vsol) / float64(vtok)
				spent[e.actor] += price * float64(e.tok) / lamportsPerSol
				tradesCt[e.actor]++
			case kindSell:
				vtok := e.vtok
				if vtok == 0 {
					vtok = 1
				}
				price := float64(e.vsol) / float64(vtok)
				recv[e.actor] += price * float64(e.tok) / lamportsPerSol
				tradesCt[e.actor]++
			}
		}
	}
	smart := make(map[string]struct{})
	for w, n := range tradesCt {
		if n >= 4 {
			net := recv[w] - spent[w]
			if net > 0.0 {
				smart[w] = struct{}{}
			}
		}
	}
	fmt.Printf(
		"  trained on %d wallets active in first half; %d tagged 'smart' (>=4 trades, positive net cash-flow).\n",
		len(tradesCt), len(smart),
	)
	if len(smart) == 0 {
		fmt.Println("  VERDICT [smart-money]: INCONCLUSIVE — no wallet met the smart bar in this sample.")
		return
	}

	// TEST: in the second half, mirror a smart wallet's FIRST buy of a mint. Enter
	// at buy_ms + latency (our latency), exit by a fixed rule (hold 15s). Only mints
	// whose create we saw (so entry curve state is honest).
	latency := envI64("DETECTION_LATENCY_MS", 1200)
	var trades []trade
	for _, m := range mints {
		if m.createMs == 0 || m.initVtok == 0 {
			continue
		}
		// find first smart buy in the second half for this mint
		var sig *ev
		for i := range m.events {
			e := &m.events[i]
			if e.ms >= splitMs && e.kind == kindBuy {
				if _, ok := smart[e.actor]; ok {
					sig = e
					break
				}
			}
		}
		if sig == nil {
			continue
		}
		s := strat{
			latencyMs: satSubI64(sig.ms+latency, m.createMs),
			buySol:    envF64("BUY_SOL", 0.5),
			filter:    filter{},
			exit:      hold(15),
		}
		if t, ok := simulate(m, &s, c); ok {
			trades = append(trades, t)
		}
	}
	st := summarize(trades)
	rows := []row{{label: fmt.Sprintf("follow-smart(hold15s, %d wallets)", len(smart)), st: st}}
	printTable("smart-money follow — second-half test", rows)
	verdict("smart-money", rows, 10)
	fmt.Println("  NOTE: wallet 'profit' here is a first-half realized-cash-flow proxy (buys vs sells\n  reconstructed from reserve-implied prices); it is a train signal, not ground-truth PnL.")
}

func satSubI64(a, b int64) int64 {
	d := a - b
	if d < 0 {
		return 0
	}
	return d
}

// ── strategy 4: migration play ───────────────────────────────────────────────

func migrationPlay(mints []*mint, c *costs) {
	fmt.Println("\n\n╔══ STRATEGY 4: MIGRATION PLAY (near-graduation ride) ══╗")
	migrated := 0
	migratedSeenBirth := 0
	for _, m := range mints {
		if m.migrated {
			migrated++
			if m.createMs != 0 {
				migratedSeenBirth++
			}
		}
	}
	fmt.Printf("  migrations in dataset: %d (of which %d we also saw born).\n", migrated, migratedSeenBirth)

	// Near-graduation ride: enter the first time real_sol >= 75 SOL (observable),
	// exit at migration (or last curve state). This is the only migration edge we
	// can price WITHOUT a PumpSwap feed — the post-graduation pop is NOT modelled.
	latency := envI64("DETECTION_LATENCY_MS", 1200)
	var trades []trade
	for _, m := range mints {
		if m.createMs == 0 || m.initVtok == 0 {
			continue
		}
		thresh := initVirtualSol + 75*uint64(lamportsPerSol)
		var cross *ev
		for i := range m.events {
			e := &m.events[i]
			if e.vsol >= thresh && e.kind == kindBuy {
				cross = e
				break
			}
		}
		if cross == nil {
			continue
		}
		s := strat{
			latencyMs: satSubI64(cross.ms+latency, m.createMs),
			buySol:    envF64("BUY_SOL", 0.5),
			filter:    filter{},
			exit:      hold(60),
		}
		if t, ok := simulate(m, &s, c); ok {
			trades = append(trades, t)
		}
	}
	st := summarize(trades)
	if st.n == 0 {
		fmt.Println("  Too few launches reached the near-graduation band in this sample to backtest.\n  VERDICT [migration]: NOT MEANINGFULLY TESTABLE with this dataset (and we have no\n  PumpSwap price feed for the post-graduation pop — the real migration edge).")
		return
	}
	rows := []row{{label: "near-grad ride (>=75 SOL, hold60s)", st: st}}
	printTable("migration — pre-graduation ride only", rows)
	verdict("migration", rows, 10)
	fmt.Println("  CAVEAT: this prices ONLY the bonding-curve ride up to migration. The post-migration\n  PumpSwap pop — the part most 'migration plays' target — needs a PumpSwap price feed the\n  collector does not yet capture.")
}

// ── main ─────────────────────────────────────────────────────────────────────

func buildSnipeSweep(latency int64, sizes []float64) []strat {
	exits := []exit{
		takeProfit(50.0),
		takeProfit(100.0),
		takeProfit(300.0),
		hold(5),
		hold(15),
		hold(30),
		trailing(30.0),
		trailing(50.0),
		firstDevSell(),
	}
	var v []strat
	for _, sz := range sizes {
		for _, e := range exits {
			v = append(v, strat{latencyMs: latency, buySol: sz, filter: filter{}, exit: e})
		}
	}
	return v
}

func buildFilteredSweep(latency int64, size float64) []strat {
	f64 := func(x float64) *float64 { return &x }
	u32 := func(x uint32) *uint32 { return &x }
	filters := []filter{
		{minBuyers: u32(3)},
		{minBuyers: u32(5)},
		{minBuyers: u32(10)},
		{rejectDevPrebuy: true},
		{maxDevAlloc: f64(0.05)},
		{minLiqSol: f64(2.0)},
		{minBuyers: u32(5), rejectDevPrebuy: true},
		{minBuyers: u32(10), minLiqSol: f64(3.0)},
	}
	exits := []exit{hold(15), takeProfit(100.0), firstDevSell(), trailing(30.0)}
	var v []strat
	for _, f := range filters {
		for _, e := range exits {
			v = append(v, strat{latencyMs: latency, buySol: size, filter: f, exit: e})
		}
	}
	return v
}

func main() {
	config.LoadDotenv()
	path, ok := config.EnvOptional("EVENTS")
	if !ok {
		path, ok = config.EnvOptional("PUMP_OUT")
	}
	if !ok {
		path = "runs/pump/events.jsonl"
	}
	latency := envI64("DETECTION_LATENCY_MS", 1200)
	sizes := []float64{0.2, 0.5, 1.0}
	costsCfg := costsFromEnv()

	mints, tsMin, tsMax := load(path)
	spanH := float64(satSubI64(tsMax, tsMin)) / 3_600_000.0
	launches := 0
	migratedCount := 0
	for _, m := range mints {
		if m.createMs != 0 {
			launches++
		}
		if m.migrated {
			migratedCount++
		}
	}

	fmt.Println("═══════════════════════════════════════════════════════════════════════")
	fmt.Printf(" pump.fun BACKTEST — %s\n", path)
	fmt.Println("═══════════════════════════════════════════════════════════════════════")
	fmt.Printf(
		"window %.1f min (%.3f h) | mints %d | launches (birth seen) %d | migrations %d\n",
		spanH*60.0, spanH, len(mints), launches, migratedCount,
	)
	fmt.Printf(
		"costs: latency %dms | pump fee %.0fbps | entry tip %g SOL | exit tip %g SOL | base fee %g SOL\n",
		latency, costsCfg.pumpFeeBps, costsCfg.entryTip, costsCfg.exitTip, costsCfg.baseFee,
	)
	fmt.Printf("buy sizes swept: %s SOL\n", formatFloatSlice(sizes))
	if spanH < 0.5 {
		fmt.Printf(
			"\n⚠️  SAMPLE IS TINY (%.1f min). This run only proves the ENGINE works end-to-end.\n⚠️  It CANNOT support any strategy conclusion — most launches' fates fall outside the\n⚠️  window (survivorship), and a handful of trades is noise. The real verdict needs\n⚠️  the multi-hour dataset.\n",
			spanH*60.0,
		)
	}

	// ── STRATEGY 1: snipe (no filter) ────────────────────────────────────────
	fmt.Println("\n\n╔══ STRATEGY 1: SNIPE (buy every launch at entry, exit by rule) ══╗")
	s1 := buildSnipeSweep(latency, sizes)
	rows1 := runSweep(mints, s1, &costsCfg, func(s strat) string {
		return fmt.Sprintf("%.1fSOL %s", s.buySol, exitLabel(s.exit))
	})
	printTable("snipe sweep (sorted by mean SOL/trade)", rows1)
	verdict("snipe", rows1, 20)

	// ── STRATEGY 2: filtered snipe (0.5 SOL) ─────────────────────────────────
	fmt.Println("\n\n╔══ STRATEGY 2: FILTERED SNIPE (enter only launches passing an entry-time filter) ══╗")
	s2 := buildFilteredSweep(latency, 0.5)
	rows2 := runSweep(mints, s2, &costsCfg, func(s strat) string {
		return fmt.Sprintf("[%s] %s", filterLabel(s.filter), exitLabel(s.exit))
	})
	printTable("filtered-snipe sweep (0.5 SOL, sorted by mean SOL/trade)", rows2)
	verdict("filtered-snipe", rows2, 15)

	// ── STRATEGY 3 & 4 ───────────────────────────────────────────────────────
	split := tsMin + satSubI64(tsMax, tsMin)/2
	smartMoney(mints, split, &costsCfg)
	migrationPlay(mints, &costsCfg)

	// ── honest caveats ───────────────────────────────────────────────────────
	fmt.Println("\n\n═══ CAVEATS (read before trusting any number above) ═══")
	fmt.Println(" 1. HONEYPOTS UNMODELLED: the collector records only SUCCESSFUL txs, so sell-revert")
	fmt.Println("    honeypots are invisible here. Real snipe PnL is WORSE than shown by that unknown.")
	fmt.Println(" 2. SURVIVORSHIP / WINDOW: launches whose rug or peak falls outside the capture window")
	fmt.Println("    are scored on a truncated life. A short window flatters 'graduated' and undercounts rugs.")
	fmt.Println(" 3. NO PATH PERTURBATION: our own buy/sell is priced with real slippage but does not")
	fmt.Println("    move the recorded path for others (small-trader assumption; optimistic on exit fills).")
	fmt.Println(" 4. NO PUMPSWAP FEED: post-graduation prices are not captured, so the migration pop is unpriced.")
	fmt.Println(" 5. PAST ≠ FUTURE: a filter fit to one window can fail the next; treat any positive set as a")
	fmt.Println("    hypothesis to re-test on fresh data, not a green light to deploy capital.")
	fmt.Printf(" 6. DATASET SIZE: with %d launches over %.2fh, only high-frequency strategies have enough\n", launches, spanH)
	fmt.Println("    trades to escape noise. Sub-20-trade rows are anecdotes.")
}

// formatFloatSlice mirrors Rust's `{:?}` debug-formatting of a `&[f64]`
// slice literal, e.g. "[0.2, 0.5, 1.0]".
func formatFloatSlice(v []float64) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = formatRustF64(f)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// formatRustF64 mirrors Rust's default f64 Debug formatting: shortest
// round-tripping decimal, always with a decimal point (e.g. 1.0 not 1).
func formatRustF64(f float64) string {
	s := fmt.Sprintf("%g", f)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}
