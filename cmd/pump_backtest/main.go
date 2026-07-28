// pump_backtest — replay a collected pump.fun event dataset chronologically and
// simulate candidate strategies against REAL data, reporting honest per-strategy
// EV, win-rate, and PnL distribution.
//
// It reads `runs/pump/events.jsonl` (produced by `pump_collect`) and NEVER signs
// or submits a transaction. It shares no state with the liquidation engine.
//
// ════════════════════════════════════════════════════════════════════════════
//
//	#1 CORRECTNESS INVARIANT: NO LOOK-AHEAD BIAS — enforced STRUCTURALLY
//
// ════════════════════════════════════════════════════════════════════════════
// A decision at time T may only use information observable at or before T. We do
// not rely on discipline; the code makes the leak impossible by construction:
//
//   - Each mint's events are sorted once into strict chronological order.
//   - The ENTRY decision is computed in ONE dedicated pass over the prefix slice
//     `events[:k]` where `k` is chosen so every event in it has `ms <= T_entry`
//     (`T_entry = create_ms + detection_latency`). The entry's curve price, the
//     dev-allocation / dev-prebuy flags, the first-second buyer count and the
//     liquidity are ALL derived from that prefix and nothing else. The suffix
//     (the future) is simply not in scope for the entry — it is a different slice.
//   - The EXIT is a forward-only walk over the suffix `events[k:]`. It consumes
//     events one at a time, updating the "as-of now" curve state to each event's
//     POST-trade reserves and then testing the exit rule. It never indexes ahead.
//     A take-profit fires only when the replayed price ACTUALLY crossed the target
//     at some t' > entry; a stop/dev-dump that crashes the price first is seen
//     first (in event order), so the strategy eats the post-crash price. A
//     time-based exit fires at `T_exit` using the state as-of `T_exit` (the state
//     BEFORE the first event past it), never the crashing event's own price.
//   - The tests at the bottom (pump_backtest_test.go) pin this: an entry cannot
//     see a future price spike, and a spike after a later entry-time IS seen —
//     proving the boundary is respected, not merely intended.
//
// ════════════════════════════════════════════════════════════════════════════
//
//	COST MODEL (a backtest that ignores costs lies too) — all VERIFIED or configurable
//
// ════════════════════════════════════════════════════════════════════════════
//   - Detection latency: you cannot buy at the creation instant — faster bots
//     already did. Entry is at `create_ms + DETECTION_LATENCY_MS` (default 1200).
//   - Bonding-curve slippage: buys/sells move the price along the curve. We use
//     the constant-product reserves math from `internal/pump` (VERIFIED to the
//     lamport against a captured trade) at the curve state as-of the action.
//   - pump.fun trading fee: 125 bps (95 protocol + 30 creator), READ from a real
//     captured trade's `fee_basis_points`+`creator_fee_basis_points`. Charged on
//     top of the SOL that enters the curve on a buy, and off the SOL received on a
//     sell. Configurable via PUMP_FEE_BPS.
//   - Jito tip + network fee: sniping needs a competitive tip. Charged per trade
//     (entry and exit), configurable (ENTRY_TIP_SOL default 0.001, EXIT_TIP_SOL
//     default 0.0005, BASE_FEE_SOL default 0.000005).
//   - Rug / dev-dump: reconstructed from the real buy/sell path; if the price
//     collapses before our exit rule fires, we realize the loss at the real
//     post-dump curve price. On migration the bonding curve is gone, so we exit at
//     the last curve price (we have no PumpSwap price feed — see caveats).
//   - Honeypot (sell-revert): NOT in the data (collector records successes only).
//     We do not pretend it is zero — it is flagged as unmodeled downside.
//
// MODELING ASSUMPTION (stated honestly): our own buy/sell is priced against the
// curve as-of the action (we pay real slippage), but we do NOT perturb the
// recorded path for other traders (a small-trader assumption). For the buy sizes
// swept here this is a mild optimism on the exit side; larger sizes would need a
// full re-simulation of every participant.
//
// Usage: `EVENTS=runs/pump/events.jsonl go run ./cmd/pump_backtest`
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/pump"
)

const lamportsPerSOL = 1e9

// initVirtualSOL is the canonical pump.fun launch virtual-SOL reserve (=
// the constant virtual offset, so `realSOL = virtualSOL - initVirtualSOL`).
// VERIFIED in internal/pump tests.
const initVirtualSOL uint64 = 30_000_000_000

// maxHoldMs is a safety cap so a position never rides forever in the sim.
const maxHoldMs int64 = 3_600_000

// ── env helpers ──────────────────────────────────────────────────────────────

func envF64(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envI64(k string, def int64) int64 {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

// Costs are shared by every simulated trade. All in SOL except the fee (bps).
type Costs struct {
	PumpFeeBps float64
	EntryTip   float64
	ExitTip    float64
	BaseFee    float64
}

func costsFromEnv() Costs {
	return Costs{
		PumpFeeBps: envF64("PUMP_FEE_BPS", 125.0),
		EntryTip:   envF64("ENTRY_TIP_SOL", 0.001),
		ExitTip:    envF64("EXIT_TIP_SOL", 0.0005),
		BaseFee:    envF64("BASE_FEE_SOL", 0.000005),
	}
}

// ── event model ──────────────────────────────────────────────────────────────

type Kind int

const (
	KindBuy Kind = iota
	KindSell
	KindMigrate
)

// Ev is one decoded JSONL record, reduced to what the sim needs.
type Ev struct {
	Ms    int64
	Kind  Kind
	Actor string
	// Vsol, Vtok are the post-trade virtual reserves (for Create these are
	// the initial reserves).
	Vsol uint64
	Vtok uint64
	Tok  uint64
}

// Mint is everything known about one launch, built from its events.
type Mint struct {
	CreateMs    int64
	Dev         string
	InitVsol    uint64
	InitVtok    uint64
	TotalSupply uint64
	Migrated    bool
	// Events are trades + migrate, strictly sorted by (ms, original order).
	Events []Ev
}

func satSub(a, b uint64) uint64 {
	if b >= a {
		return 0
	}
	return a - b
}

func satAdd(a, b uint64) uint64 {
	s := a + b
	if s < a { // overflow
		return ^uint64(0)
	}
	return s
}

func load(path string) (mints []*Mint, tsMin, tsMax int64) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot open %s: %v\n", path, err)
		os.Exit(1)
	}
	defer f.Close()

	byMint := map[string]*Mint{}
	tsMin = int64(1)<<62 - 1 // effectively MaxInt64
	tsMax = 0

	getOrCreate := func(mint string) *Mint {
		m, ok := byMint[mint]
		if !ok {
			m = &Mint{}
			byMint[mint] = m
		}
		return m
	}

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
		ms := asInt64(v["unix_ms"])
		if ms == 0 {
			continue
		}
		if ms < tsMin {
			tsMin = ms
		}
		if ms > tsMax {
			tsMax = ms
		}
		mint := asStr(v["mint"])
		if mint == "" {
			continue
		}
		et := asStr(v["event_type"])
		actor := asStr(v["actor"])
		switch et {
		case "create":
			m := getOrCreate(mint)
			m.CreateMs = ms
			dev := asStr(v["dev"])
			if dev == "" {
				dev = actor
			}
			m.Dev = dev
			m.InitVsol = asU64Default(v["init_virtual_sol_reserves"], initVirtualSOL)
			m.InitVtok = asU64(v["init_virtual_token_reserves"])
			m.TotalSupply = asU64(v["token_total_supply"])
		case "buy", "sell":
			m := getOrCreate(mint)
			k := KindSell
			if et == "buy" {
				k = KindBuy
			}
			m.Events = append(m.Events, Ev{
				Ms:    ms,
				Kind:  k,
				Actor: actor,
				Vsol:  asU64(v["virtual_sol_reserves"]),
				Vtok:  asU64(v["virtual_token_reserves"]),
				Tok:   asU64(v["token_amount"]),
			})
		case "migrate":
			m := getOrCreate(mint)
			m.Migrated = true
			m.Events = append(m.Events, Ev{Ms: ms, Kind: KindMigrate, Actor: actor})
		}
	}

	mints = make([]*Mint, 0, len(byMint))
	for _, m := range byMint {
		sort.SliceStable(m.Events, func(i, j int) bool { return m.Events[i].Ms < m.Events[j].Ms })
		mints = append(mints, m)
	}
	if tsMin == int64(1)<<62-1 {
		tsMin = 0
	}
	return mints, tsMin, tsMax
}

func asStr(v any) string {
	s, _ := v.(string)
	return s
}

func asInt64(v any) int64 {
	if f, ok := v.(float64); ok {
		return int64(f)
	}
	return 0
}

func asU64(v any) uint64 {
	if f, ok := v.(float64); ok && f >= 0 {
		return uint64(f)
	}
	return 0
}

func asU64Default(v any, def uint64) uint64 {
	if f, ok := v.(float64); ok && f >= 0 {
		return uint64(f)
	}
	return def
}

// ── strategy config ──────────────────────────────────────────────────────────

// Filter is an entry-time-observable filter (strategy 2). Zero-value fields
// (nil pointers / false) mean "don't care".
type Filter struct {
	// MaxDevAlloc rejects if dev's initial token allocation (fraction of
	// supply) exceeds this.
	MaxDevAlloc *float64
	// RejectDevPrebuy rejects if the dev pre-bought at/near create.
	RejectDevPrebuy bool
	// MinBuyers requires at least this many DISTINCT buyers before T_entry.
	MinBuyers *uint32
	// MinLiqSOL requires at least this much real SOL liquidity at T_entry.
	MinLiqSOL *float64
}

type ExitKind int

const (
	// ExitTakeProfit sells when price >= entry * (1 + pct/100).
	ExitTakeProfit ExitKind = iota
	// ExitHold sells at entry + secs (at the state as-of that time).
	ExitHold
	// ExitTrailing sells when price drops pct% from the running peak.
	ExitTrailing
	// ExitFirstDevSell sells on the first sell by the dev wallet after entry.
	ExitFirstDevSell
)

type Exit struct {
	Kind    ExitKind
	Pct     float64 // for TakeProfit / Trailing
	HoldSec int64   // for Hold
}

func exitTakeProfit(pct float64) Exit { return Exit{Kind: ExitTakeProfit, Pct: pct} }
func exitHold(secs int64) Exit        { return Exit{Kind: ExitHold, HoldSec: secs} }
func exitTrailing(pct float64) Exit   { return Exit{Kind: ExitTrailing, Pct: pct} }
func exitFirstDevSell() Exit          { return Exit{Kind: ExitFirstDevSell} }

type Strat struct {
	LatencyMs int64
	BuySOL    float64
	Filter    Filter
	Exit      Exit
}

// Trade is the outcome of a single simulated round-trip.
type Trade struct {
	EntryMs int64
	PnlSOL  float64
	PnlPct  float64
}

// ── the per-mint simulation (look-ahead-free by construction) ────────────────

// simulate simulates one strategy on one launch. Returns (Trade, true) or
// (_, false) if the launch is not tradeable (no create seen) or the filter
// rejects it.
func simulate(m *Mint, s *Strat, c *Costs) (Trade, bool) {
	if m.CreateMs == 0 || m.InitVtok == 0 {
		return Trade{}, false // we only trade launches whose birth (and initial curve) we saw
	}
	tEntry := m.CreateMs + s.LatencyMs

	// ── ENTRY PASS: only the prefix with ms <= T_entry is in scope. ──────────
	// Curve state as-of T_entry starts at the create's initial reserves and is
	// advanced by every trade at or before T_entry.
	vsol, vtok := m.InitVsol, m.InitVtok
	buyers := map[string]struct{}{}
	var devAllocTokens uint64
	devPrebought := false
	firstSec := m.CreateMs + 1000
	k := 0
	for i := range m.Events {
		e := &m.Events[i]
		if e.Ms > tEntry {
			break
		}
		k++
		switch e.Kind {
		case KindBuy:
			if e.Ms <= firstSec {
				buyers[e.Actor] = struct{}{}
			}
			if e.Actor == m.Dev {
				devPrebought = true
				devAllocTokens = satAdd(devAllocTokens, e.Tok)
			}
			vsol = e.Vsol
			vtok = e.Vtok
		case KindSell:
			vsol = e.Vsol
			vtok = e.Vtok
		case KindMigrate:
			return Trade{}, false // already graduated before we could enter
		}
	}

	// ── FILTER (uses only the prefix-derived features above) ─────────────────
	f := &s.Filter
	if f.MaxDevAlloc != nil {
		alloc := 0.0
		if m.TotalSupply > 0 {
			alloc = float64(devAllocTokens) / float64(m.TotalSupply)
		}
		if alloc > *f.MaxDevAlloc {
			return Trade{}, false
		}
	}
	if f.RejectDevPrebuy && devPrebought {
		return Trade{}, false
	}
	if f.MinBuyers != nil {
		if uint32(len(buyers)) < *f.MinBuyers {
			return Trade{}, false
		}
	}
	if f.MinLiqSOL != nil {
		realSol := float64(satSub(vsol, initVirtualSOL)) / lamportsPerSOL
		if realSol < *f.MinLiqSOL {
			return Trade{}, false
		}
	}
	if vtok == 0 {
		return Trade{}, false
	}

	// ── OPEN POSITION at the as-of-T_entry curve state. ──────────────────────
	// Buy budget BuySOL is total SOL out of pocket for the buy incl. pump fee;
	// the SOL that actually enters the curve is the budget net of the fee.
	feeMul := 1.0 + c.PumpFeeBps/1e4
	solIntoCurve := uint64(s.BuySOL / feeMul * lamportsPerSOL)
	positionTokens := pump.CurveBuyTokensOut(vsol, vtok, solIntoCurve)
	if positionTokens == 0 {
		return Trade{}, false
	}
	entryCost := s.BuySOL + c.EntryTip + c.BaseFee
	entryPrice := float64(vsol) / float64(vtok) // raw SOL-lamports per raw token

	// ── EXIT PASS: forward-only walk of the suffix events[k:]. ───────────────
	peak := entryPrice
	tHardExit := tEntry + maxHoldMs
	tTimedExit := tHardExit
	if s.Exit.Kind == ExitHold {
		tTimedExit = tEntry + s.Exit.HoldSec*1000
		if tTimedExit > tHardExit {
			tTimedExit = tHardExit
		}
	}

	// exit_state is the curve reserves at which we finally sell.
	exitVsol, exitVtok := vsol, vtok

exitWalk:
	for i := k; i < len(m.Events); i++ {
		e := &m.Events[i]
		// (a) Time-based exit fires BEFORE consuming this event, using the state
		//     as-of the exit instant (i.e. the state BEFORE this later event) —
		//     never this event's own (possibly crashing) price. Look-ahead-free.
		if e.Ms >= tTimedExit {
			break
		}
		switch e.Kind {
		case KindMigrate:
			// Curve gone; realize at the last curve state (no PumpSwap feed).
			break exitWalk
		case KindBuy, KindSell:
			// (b) React to the event: advance curve to its POST-trade state.
			exitVsol = e.Vsol
			exitVtok = e.Vtok
			price := float64(exitVsol) / float64(exitVtok)
			if price > peak {
				peak = price
			}
			hit := false
			switch s.Exit.Kind {
			case ExitTakeProfit:
				hit = price >= entryPrice*(1.0+s.Exit.Pct/100.0)
			case ExitTrailing:
				hit = price <= peak*(1.0-s.Exit.Pct/100.0)
			case ExitFirstDevSell:
				hit = e.Kind == KindSell && e.Actor == m.Dev
			case ExitHold:
				// handled by the timed branch above
			}
			if hit {
				break exitWalk
			}
		}
	}
	// If we ran out of events still open, exit at the last known curve state
	// (equal to the entry state if nothing traded) — a real, forced close.

	// ── REALIZE the sell into the exit-state curve. ──────────────────────────
	// We price OTHERS' trades against the recorded (unperturbed) path, but we DO
	// carry our own buy's footprint: our SOL is still sitting in the pool and our
	// tokens are still out of it. So we sell back into the recorded reserves with
	// our own delta added (+sol_into_curve to vsol, -position_tokens from vtok).
	// Without this we would double-charge curve convexity — pay entry slippage AND
	// exit slippage on a static curve — inventing a loss that isn't there. With it,
	// a flat market round-trips to ~0 (minus fees/tips) and a real loss comes only
	// from OTHERS moving the price (dev dumps etc.), which is the honest cost.
	adjVsol := satAdd(exitVsol, solIntoCurve)
	adjVtok := satSub(exitVtok, positionTokens)
	if adjVtok < 1 {
		adjVtok = 1
	}
	solOutCurve := pump.CurveSellSOLOut(adjVsol, adjVtok, positionTokens)
	netSol := float64(solOutCurve)/lamportsPerSOL*(1.0-c.PumpFeeBps/1e4) - c.ExitTip - c.BaseFee
	pnlSol := netSol - entryCost
	return Trade{
		EntryMs: tEntry,
		PnlSOL:  pnlSol,
		PnlPct:  100.0 * pnlSol / entryCost,
	}, true
}

// ── aggregate stats ──────────────────────────────────────────────────────────

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return nan()
	}
	idx := int(roundHalfAwayFromZero((float64(len(sorted)) - 1.0) * p))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	if idx < 0 {
		idx = 0
	}
	return sorted[idx]
}

func nan() float64 {
	var z float64
	return z / z
}

func roundHalfAwayFromZero(x float64) float64 {
	if x >= 0 {
		return float64(int64(x + 0.5))
	}
	return float64(int64(x - 0.5))
}

type Stats struct {
	N         int
	WinRate   float64
	MeanSOL   float64
	MedianSOL float64
	MeanPct   float64
	TotalSOL  float64
	MaxDD     float64
	P10       float64
	P90       float64
}

func summarize(trades []Trade) Stats {
	n := len(trades)
	if n == 0 {
		return Stats{}
	}
	wins := 0
	var totalSol float64
	var sumPct float64
	solSorted := make([]float64, n)
	pctSorted := make([]float64, n)
	for i, t := range trades {
		if t.PnlSOL > 0 {
			wins++
		}
		totalSol += t.PnlSOL
		sumPct += t.PnlPct
		solSorted[i] = t.PnlSOL
		pctSorted[i] = t.PnlPct
	}
	meanSol := totalSol / float64(n)
	meanPct := sumPct / float64(n)
	sort.Float64s(solSorted)
	sort.Float64s(pctSorted)
	medianSol := pct(solSorted, 0.50)

	// Max drawdown of the equity curve, trades ordered by entry time.
	ordered := make([]Trade, n)
	copy(ordered, trades)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].EntryMs < ordered[j].EntryMs })
	var equity, peak, maxDD float64
	for _, t := range ordered {
		equity += t.PnlSOL
		if equity > peak {
			peak = equity
		}
		if dd := peak - equity; dd > maxDD {
			maxDD = dd
		}
	}

	return Stats{
		N:         n,
		WinRate:   100.0 * float64(wins) / float64(n),
		MeanSOL:   meanSol,
		MedianSOL: medianSol,
		MeanPct:   meanPct,
		TotalSOL:  totalSol,
		MaxDD:     maxDD,
		P10:       pct(pctSorted, 0.10),
		P90:       pct(pctSorted, 0.90),
	}
}

func exitLabel(e Exit) string {
	switch e.Kind {
	case ExitTakeProfit:
		return fmt.Sprintf("TP+%.0f%%", e.Pct)
	case ExitHold:
		return fmt.Sprintf("hold%ds", e.HoldSec)
	case ExitTrailing:
		return fmt.Sprintf("trail%.0f%%", e.Pct)
	case ExitFirstDevSell:
		return "devSell"
	}
	return "?"
}

func filterLabel(f Filter) string {
	var parts []string
	if f.MaxDevAlloc != nil {
		parts = append(parts, fmt.Sprintf("devAlloc<%.0f%%", *f.MaxDevAlloc*100.0))
	}
	if f.RejectDevPrebuy {
		parts = append(parts, "noDevPrebuy")
	}
	if f.MinBuyers != nil {
		parts = append(parts, fmt.Sprintf(">=%dbuyers", *f.MinBuyers))
	}
	if f.MinLiqSOL != nil {
		parts = append(parts, fmt.Sprintf(">=%gSOL", *f.MinLiqSOL))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "+")
}

// Row is one row of the results table.
type Row struct {
	Label string
	St    Stats
}

func runSweep(mints []*Mint, strats []Strat, c *Costs, labelOf func(*Strat) string) []Row {
	rows := make([]Row, 0, len(strats))
	for i := range strats {
		s := &strats[i]
		var trades []Trade
		for _, m := range mints {
			if t, ok := simulate(m, s, c); ok {
				trades = append(trades, t)
			}
		}
		rows = append(rows, Row{Label: labelOf(s), St: summarize(trades)})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].St.MeanSOL > rows[j].St.MeanSOL })
	return rows
}

func printTable(title string, rows []Row) {
	fmt.Printf("\n══ %s ══\n", title)
	fmt.Printf("%-34s %6s %7s %11s %10s %9s %10s %8s %8s %9s\n",
		"params", "trades", "win%", "mean SOL", "med SOL", "mean%", "total SOL", "p10%", "p90%", "maxDD SOL")
	fmt.Println(strings.Repeat("-", 120))
	for _, r := range rows {
		s := r.St
		if s.N == 0 {
			fmt.Printf("%-34s %6d %7s\n", r.Label, 0, "(no trades)")
			continue
		}
		fmt.Printf("%-34s %6d %6.1f%% %11.5f %10.5f %8.1f%% %10.4f %8.1f %8.1f %9.4f\n",
			r.Label, s.N, s.WinRate, s.MeanSOL, s.MedianSOL, s.MeanPct, s.TotalSOL, s.P10, s.P90, s.MaxDD)
	}
}

func verdict(strategy string, rows []Row, minTrades int) {
	// Best row (rows are pre-sorted by mean_sol desc) that clears a trade-count bar.
	var best *Row
	for i := range rows {
		if rows[i].St.N >= minTrades {
			best = &rows[i]
			break
		}
	}
	switch {
	case best != nil && best.St.MeanSOL > 0.0:
		fmt.Printf("  VERDICT [%s]: best positive-EV set = `%s` (mean %+.5f SOL/trade over %d trades, win %.1f%%). "+
			"Treat as HYPOTHESIS until confirmed on the multi-hour set.\n",
			strategy, best.Label, best.St.MeanSOL, best.St.N, best.St.WinRate)
	case best != nil:
		fmt.Printf("  VERDICT [%s]: LOSING after costs. Best set `%s` still %+.5f SOL/trade over %d trades.\n",
			strategy, best.Label, best.St.MeanSOL, best.St.N)
	default:
		fmt.Printf("  VERDICT [%s]: INCONCLUSIVE — no parameter set reached %d trades in this sample.\n",
			strategy, minTrades)
	}
}

// ── strategy 3: smart-money follow (strict train/test split) ─────────────────

func smartMoney(mints []*Mint, splitMs int64, c *Costs) {
	fmt.Println("\n\n╔══ STRATEGY 3: SMART-MONEY FOLLOW (train first-half / test second-half) ══╗")
	// TRAIN: realized net SOL cash-flow per wallet, first half ONLY.
	spent := map[string]float64{}
	recv := map[string]float64{}
	tradesCt := map[string]uint32{}
	for _, m := range mints {
		for _, e := range m.Events {
			if e.Ms >= splitMs {
				continue
			}
			switch e.Kind {
			case KindBuy:
				// SOL that left the buyer ~ curve delta they caused; use tok*price
				// proxy is fragile, so approximate with the reserve-implied sol: we
				// don't have per-event sol for reserves here, so use price*tok.
				vtok := e.Vtok
				if vtok == 0 {
					vtok = 1
				}
				price := float64(e.Vsol) / float64(vtok)
				spent[e.Actor] += price * float64(e.Tok) / lamportsPerSOL
				tradesCt[e.Actor]++
			case KindSell:
				vtok := e.Vtok
				if vtok == 0 {
					vtok = 1
				}
				price := float64(e.Vsol) / float64(vtok)
				recv[e.Actor] += price * float64(e.Tok) / lamportsPerSOL
				tradesCt[e.Actor]++
			}
		}
	}
	smart := map[string]struct{}{}
	for w, n := range tradesCt {
		if n >= 4 {
			net := recv[w] - spent[w]
			if net > 0.0 {
				smart[w] = struct{}{}
			}
		}
	}
	fmt.Printf("  trained on %d wallets active in first half; %d tagged 'smart' (>=4 trades, positive net cash-flow).\n",
		len(tradesCt), len(smart))
	if len(smart) == 0 {
		fmt.Println("  VERDICT [smart-money]: INCONCLUSIVE — no wallet met the smart bar in this sample.")
		return
	}

	// TEST: in the second half, mirror a smart wallet's FIRST buy of a mint. Enter
	// at buy_ms + latency (our latency), exit by a fixed rule (hold 15s). Only mints
	// whose create we saw (so entry curve state is honest).
	latency := envI64("DETECTION_LATENCY_MS", 1200)
	var trades []Trade
	for _, m := range mints {
		if m.CreateMs == 0 || m.InitVtok == 0 {
			continue
		}
		// find first smart buy in the second half for this mint
		var sig *Ev
		for i := range m.Events {
			e := &m.Events[i]
			if e.Ms >= splitMs && e.Kind == KindBuy {
				if _, ok := smart[e.Actor]; ok {
					sig = e
					break
				}
			}
		}
		if sig == nil {
			continue
		}
		latMs := sig.Ms + latency - m.CreateMs
		if latMs < 0 {
			latMs = 0
		}
		s := Strat{
			LatencyMs: latMs,
			BuySOL:    envF64("BUY_SOL", 0.5),
			Filter:    Filter{},
			Exit:      exitHold(15),
		}
		if t, ok := simulate(m, &s, c); ok {
			trades = append(trades, t)
		}
	}
	st := summarize(trades)
	rows := []Row{{Label: fmt.Sprintf("follow-smart(hold15s, %d wallets)", len(smart)), St: st}}
	printTable("smart-money follow — second-half test", rows)
	verdict("smart-money", rows, 10)
	fmt.Println("  NOTE: wallet 'profit' here is a first-half realized-cash-flow proxy (buys vs sells\n  reconstructed from reserve-implied prices); it is a train signal, not ground-truth PnL.")
}

// ── strategy 4: migration play ───────────────────────────────────────────────

func migrationPlay(mints []*Mint, c *Costs) {
	fmt.Println("\n\n╔══ STRATEGY 4: MIGRATION PLAY (near-graduation ride) ══╗")
	migrated := 0
	migratedSeenBirth := 0
	for _, m := range mints {
		if m.Migrated {
			migrated++
			if m.CreateMs != 0 {
				migratedSeenBirth++
			}
		}
	}
	fmt.Printf("  migrations in dataset: %d (of which %d we also saw born).\n", migrated, migratedSeenBirth)

	// Near-graduation ride: enter the first time real_sol >= 75 SOL (observable),
	// exit at migration (or last curve state). This is the only migration edge we
	// can price WITHOUT a PumpSwap feed — the post-graduation pop is NOT modelled.
	latency := envI64("DETECTION_LATENCY_MS", 1200)
	thresh := initVirtualSOL + 75*uint64(lamportsPerSOL)
	var trades []Trade
	for _, m := range mints {
		if m.CreateMs == 0 || m.InitVtok == 0 {
			continue
		}
		var cross *Ev
		for i := range m.Events {
			e := &m.Events[i]
			if e.Kind == KindBuy && e.Vsol >= thresh {
				cross = e
				break
			}
		}
		if cross == nil {
			continue
		}
		latMs := cross.Ms + latency - m.CreateMs
		if latMs < 0 {
			latMs = 0
		}
		s := Strat{
			LatencyMs: latMs,
			BuySOL:    envF64("BUY_SOL", 0.5),
			Filter:    Filter{},
			Exit:      exitHold(60),
		}
		if t, ok := simulate(m, &s, c); ok {
			trades = append(trades, t)
		}
	}
	st := summarize(trades)
	if st.N == 0 {
		fmt.Println("  Too few launches reached the near-graduation band in this sample to backtest.\n  VERDICT [migration]: NOT MEANINGFULLY TESTABLE with this dataset (and we have no\n  PumpSwap price feed for the post-graduation pop — the real migration edge).")
		return
	}
	rows := []Row{{Label: "near-grad ride (>=75 SOL, hold60s)", St: st}}
	printTable("migration — pre-graduation ride only", rows)
	verdict("migration", rows, 10)
	fmt.Println("  CAVEAT: this prices ONLY the bonding-curve ride up to migration. The post-migration\n  PumpSwap pop — the part most 'migration plays' target — needs a PumpSwap price feed the\n  collector does not yet capture.")
}

// ── main ─────────────────────────────────────────────────────────────────────

func buildSnipeSweep(latency int64, sizes []float64) []Strat {
	exits := []Exit{
		exitTakeProfit(50.0),
		exitTakeProfit(100.0),
		exitTakeProfit(300.0),
		exitHold(5),
		exitHold(15),
		exitHold(30),
		exitTrailing(30.0),
		exitTrailing(50.0),
		exitFirstDevSell(),
	}
	var v []Strat
	for _, sz := range sizes {
		for _, e := range exits {
			v = append(v, Strat{LatencyMs: latency, BuySOL: sz, Filter: Filter{}, Exit: e})
		}
	}
	return v
}

func f64p(f float64) *float64 { return &f }
func u32p(u uint32) *uint32   { return &u }

func buildFilteredSweep(latency int64, size float64) []Strat {
	filters := []Filter{
		{MinBuyers: u32p(3)},
		{MinBuyers: u32p(5)},
		{MinBuyers: u32p(10)},
		{RejectDevPrebuy: true},
		{MaxDevAlloc: f64p(0.05)},
		{MinLiqSOL: f64p(2.0)},
		{MinBuyers: u32p(5), RejectDevPrebuy: true},
		{MinBuyers: u32p(10), MinLiqSOL: f64p(3.0)},
	}
	exits := []Exit{exitHold(15), exitTakeProfit(100.0), exitFirstDevSell(), exitTrailing(30.0)}
	var v []Strat
	for _, f := range filters {
		for _, e := range exits {
			v = append(v, Strat{LatencyMs: latency, BuySOL: size, Filter: f, Exit: e})
		}
	}
	return v
}

func main() {
	envfile.LoadDotEnv()

	path := os.Getenv("EVENTS")
	if path == "" {
		path = os.Getenv("PUMP_OUT")
	}
	if path == "" {
		path = "runs/pump/events.jsonl"
	}
	latency := envI64("DETECTION_LATENCY_MS", 1200)
	sizes := []float64{0.2, 0.5, 1.0}
	costs := costsFromEnv()

	mints, tsMin, tsMax := load(path)
	spanMs := tsMax - tsMin
	if spanMs < 0 {
		spanMs = 0
	}
	spanH := float64(spanMs) / 3_600_000.0
	launches := 0
	migrations := 0
	for _, m := range mints {
		if m.CreateMs != 0 {
			launches++
		}
		if m.Migrated {
			migrations++
		}
	}

	fmt.Println("═══════════════════════════════════════════════════════════════════════")
	fmt.Printf(" pump.fun BACKTEST — %s\n", path)
	fmt.Println("═══════════════════════════════════════════════════════════════════════")
	fmt.Printf("window %.1f min (%.3f h) | mints %d | launches (birth seen) %d | migrations %d\n",
		spanH*60.0, spanH, len(mints), launches, migrations)
	fmt.Printf("costs: latency %dms | pump fee %.0fbps | entry tip %g SOL | exit tip %g SOL | base fee %g SOL\n",
		latency, costs.PumpFeeBps, costs.EntryTip, costs.ExitTip, costs.BaseFee)
	fmt.Printf("buy sizes swept: %v SOL\n", sizes)
	if spanH < 0.5 {
		fmt.Printf("\n⚠️  SAMPLE IS TINY (%.1f min). This run only proves the ENGINE works end-to-end.\n"+
			"⚠️  It CANNOT support any strategy conclusion — most launches' fates fall outside the\n"+
			"⚠️  window (survivorship), and a handful of trades is noise. The real verdict needs\n"+
			"⚠️  the multi-hour dataset.\n", spanH*60.0)
	}

	// ── STRATEGY 1: snipe (no filter) ────────────────────────────────────────
	fmt.Println("\n\n╔══ STRATEGY 1: SNIPE (buy every launch at entry, exit by rule) ══╗")
	s1 := buildSnipeSweep(latency, sizes)
	rows1 := runSweep(mints, s1, &costs, func(s *Strat) string {
		return fmt.Sprintf("%.1fSOL %s", s.BuySOL, exitLabel(s.Exit))
	})
	printTable("snipe sweep (sorted by mean SOL/trade)", rows1)
	verdict("snipe", rows1, 20)

	// ── STRATEGY 2: filtered snipe (0.5 SOL) ─────────────────────────────────
	fmt.Println("\n\n╔══ STRATEGY 2: FILTERED SNIPE (enter only launches passing an entry-time filter) ══╗")
	s2 := buildFilteredSweep(latency, 0.5)
	rows2 := runSweep(mints, s2, &costs, func(s *Strat) string {
		return fmt.Sprintf("[%s] %s", filterLabel(s.Filter), exitLabel(s.Exit))
	})
	printTable("filtered-snipe sweep (0.5 SOL, sorted by mean SOL/trade)", rows2)
	verdict("filtered-snipe", rows2, 15)

	// ── STRATEGY 3 & 4 ───────────────────────────────────────────────────────
	split := tsMin + spanMs/2
	smartMoney(mints, split, &costs)
	migrationPlay(mints, &costs)

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
