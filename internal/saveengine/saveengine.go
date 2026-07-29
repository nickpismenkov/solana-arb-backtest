// Package saveengine implements off-chain, Lazer-driven health tracking for
// Solend obligations — the event-driven trigger that replaces
// liq_save_executor's 30s stored-health poll. That poll lost every race
// (census: 45 USDC-debt Solend liquidations in 48h, 0 caught) because
// competitors react to the oracle in ms while we looked every 30s.
//
// Design (robust, avoids Solend's fiddly absolute price/amount scaling):
// ANCHOR on the obligation's own on-chain health — the STORED BorrowedValue
// and UnhealthyBorrowValue, correct as of Solend's last refresh — and TRACK
// it by the Lazer price RATIO. At rescan we snapshot each side's Lazer feed
// price; on every tick we scale the stored values by lazer_now /
// lazer_at_rescan (exactly 1.0 at rescan, so it reproduces the on-chain
// values, and tracks ms-latency moves between rescans with ZERO RPC).
// Anchoring on the Lazer price (not the reserve MarketPrice) is what makes
// LST collateral correct: mSOL/jitoSOL map to the SOL feed but their reserve
// price carries the staking premium — the ratio only cares about the feed's
// relative move.
//
// v1 scope: single deposit + single borrow (matches the fire path). The full
// on-chain simulateBundle remains the authoritative fire gate; this engine
// only decides WHO to spend that sim budget on, fast.
package saveengine

import (
	"sort"

	"arbengine/internal/save"
	"arbengine/internal/solana"
)

// SolendWatch is one watched Solend obligation reduced to price-ratio
// tracking.
type SolendWatch struct {
	Obligation      solana.Pubkey
	CollReserve     solana.Pubkey
	DebtReserve     solana.Pubkey
	borrowedStored  float64
	unhealthyStored float64

	// freshBorrowed/freshUnhealthy are the FRESH-price health at rescan —
	// borrowed/unhealthy recomputed from the freshly-fetched reserves via
	// the cToken exchange rate (Obligation.FreshHealth), i.e. the value
	// Solend's own liquidate recomputes at settle time. The FIRE tier gates
	// on THIS, not on the lazily-stale stored verdict that over-reports
	// phantoms. Zeroed if a referenced reserve was missing.
	freshBorrowed  float64
	freshUnhealthy float64

	// collFeed/debtFeed is the Lazer feed for each side (ok=false = priced
	// off a non-Lazer/baseline oracle, so it can't move between rescans ->
	// ratio stays 1.0).
	collFeed    uint32
	hasCollFeed bool
	debtFeed    uint32
	hasDebtFeed bool

	// collAnchor/debtAnchor is the Lazer feed price captured at rescan (the
	// ratio anchor). ok=false if the feed had no live price at rescan.
	collAnchor    float64
	hasCollAnchor bool
	debtAnchor    float64
	hasDebtAnchor bool

	// Complete is true iff UnhealthyBorrowValue > 0 and both reserves
	// priced — else never trusted.
	Complete bool
}

func ratio(feed uint32, hasFeed bool, anchor float64, hasAnchor bool, lazer map[uint32]float64) float64 {
	if hasFeed && hasAnchor && anchor > 0.0 {
		if p, ok := lazer[feed]; ok {
			return p / anchor
		}
		return 1.0
	}
	return 1.0
}

// BuildSolendWatch builds a SolendWatch for a v1 obligation (1 deposit, 1
// borrow). lazerNow is the Lazer snapshot at rescan, used to anchor the
// ratios.
func BuildSolendWatch(
	o *save.Obligation,
	obligation solana.Pubkey,
	reserves map[solana.Pubkey]save.Reserve,
	mintFeed map[solana.Pubkey]uint32,
	lazerNow map[uint32]float64,
) (SolendWatch, bool) {
	if len(o.Deposits) != 1 || len(o.Borrows) != 1 {
		return SolendWatch{}, false
	}
	coll, ok := reserves[o.Deposits[0].Reserve]
	if !ok {
		return SolendWatch{}, false
	}
	debt, ok := reserves[o.Borrows[0].Reserve]
	if !ok {
		return SolendWatch{}, false
	}
	collFeed, hasCollFeed := mintFeed[coll.LiquidityMint]
	debtFeed, hasDebtFeed := mintFeed[debt.LiquidityMint]
	var collAnchor float64
	hasCollAnchor := false
	if hasCollFeed {
		collAnchor, hasCollAnchor = lazerNow[collFeed]
	}
	var debtAnchor float64
	hasDebtAnchor := false
	if hasDebtFeed {
		debtAnchor, hasDebtAnchor = lazerNow[debtFeed]
	}

	// Fresh-price health from the current reserves (cToken exchange rate) —
	// the phantom-free fire-tier verdict. Falls back to (0,0) only if a
	// reserve is unexpectedly absent, which leaves the obligation NOT
	// fire-eligible (safe: it stays merely watched).
	freshBorrowed, freshUnhealthy, freshOk := o.FreshHealth(reserves)
	if !freshOk {
		freshBorrowed, freshUnhealthy = 0.0, 0.0
	}

	return SolendWatch{
		Obligation:      obligation,
		CollReserve:     coll.Reserve,
		DebtReserve:     debt.Reserve,
		borrowedStored:  o.BorrowedValue,
		unhealthyStored: o.UnhealthyBorrowValue,
		freshBorrowed:   freshBorrowed,
		freshUnhealthy:  freshUnhealthy,
		collFeed:        collFeed,
		hasCollFeed:     hasCollFeed,
		debtFeed:        debtFeed,
		hasDebtFeed:     hasDebtFeed,
		collAnchor:      collAnchor,
		hasCollAnchor:   hasCollAnchor,
		debtAnchor:      debtAnchor,
		hasDebtAnchor:   hasDebtAnchor,
		Complete:        o.UnhealthyBorrowValue > 0.0 && coll.MarketPrice > 0.0 && debt.MarketPrice > 0.0,
	}, true
}

// Borrowed is the ratio-scaled borrowed value at the given Lazer prices.
func (w *SolendWatch) Borrowed(lazer map[uint32]float64) float64 {
	return w.borrowedStored * ratio(w.debtFeed, w.hasDebtFeed, w.debtAnchor, w.hasDebtAnchor, lazer)
}

// Unhealthy is the ratio-scaled unhealthy threshold at the given Lazer
// prices.
func (w *SolendWatch) Unhealthy(lazer map[uint32]float64) float64 {
	return w.unhealthyStored * ratio(w.collFeed, w.hasCollFeed, w.collAnchor, w.hasCollAnchor, lazer)
}

// Liquidatable reports whether the ratio-projected position is
// liquidatable at the given Lazer prices.
func (w *SolendWatch) Liquidatable(lazer map[uint32]float64) bool {
	return w.Complete && w.Borrowed(lazer) > w.Unhealthy(lazer)
}

// RatioNow is borrowed/unhealthy at the given prices (>1.0 = underwater).
func (w *SolendWatch) RatioNow(lazer map[uint32]float64) float64 {
	u := w.Unhealthy(lazer)
	if u <= 0.0 {
		return 0.0
	}
	return w.Borrowed(lazer) / u
}

// FeedsReady is true unless a Lazer-mapped side is missing a live price
// (then the ratio would silently fall back to 1.0 and hide a move — don't
// trust it).
func (w *SolendWatch) FeedsReady(lazer map[uint32]float64) bool {
	ok := func(feed uint32, hasFeed bool, hasAnchor bool) bool {
		if hasFeed && hasAnchor {
			_, present := lazer[feed]
			return present
		}
		return true
	}
	return ok(w.collFeed, w.hasCollFeed, w.hasCollAnchor) && ok(w.debtFeed, w.hasDebtFeed, w.hasDebtAnchor)
}

// ── FRESH-price on-chain health — FIRE-tier gate ────────────────────────
// The Lazer projection above decides WHO to re-check; these decide whether
// an obligation is ACTUALLY liquidatable at the on-chain oracle price Solend
// settles against — recomputed at rescan from the FRESH reserve prices +
// cToken exchange rate (Obligation.FreshHealth), the exact value Solend's
// own liquidate recomputes at settle time.
//
// We do NOT gate on the obligation's STORED borrowed/unhealthy: Solend
// refreshes obligation health LAZILY, so hundreds read stale-HIGH on the
// collateral side (priced when the collateral was worth more) and look
// liquidatable while a fresh refresh proves them healthy — the "phantom"
// flood (live: 396 stored-liquidatable vs 0 at fresh price in a calm
// market). Because FreshHealth reproduces Solend's refresh to 0.0000% on
// same-slot obligations, gating on it never UNDER-states liquidatability
// relative to what Solend itself will compute — a genuinely underwater
// position is still flagged — while the phantoms collapse away. The
// executor still confirms every fire with a full sim as the ground-truth
// backstop.

// OnchainLiquidatable reports liquidatability at fresh on-chain prices.
func (w *SolendWatch) OnchainLiquidatable() bool {
	return w.Complete && w.freshUnhealthy > 0.0 && w.freshBorrowed > w.freshUnhealthy
}

// OnchainDeficit is the USD deficit (borrowed − unhealthy) at fresh prices;
// > 0 iff liquidatable.
func (w *SolendWatch) OnchainDeficit() float64 {
	return w.freshBorrowed - w.freshUnhealthy
}

// OnchainRatio is the fresh-price ratio (borrowed / unhealthy); > 1 =
// underwater on-chain.
func (w *SolendWatch) OnchainRatio() float64 {
	if w.freshUnhealthy <= 0.0 {
		return 0.0
	}
	return w.freshBorrowed / w.freshUnhealthy
}

// Engine is an in-memory watch-set, rebuilt on rescan, queried on every
// Lazer tick.
type Engine struct {
	Accounts []SolendWatch
	MinDebt  float64
	// RatioCap rejects obligations whose ratio exceeds this — an absurd
	// ratio (borrowed >> unhealthy) means the collateral is mis-priced near
	// zero (dust / dead feed), never a real opportunity. Without it,
	// deficit-ranking would put these un-fireable accounts FIRST (huge
	// borrowed − ~0 unhealthy) and starve the genuine near-threshold ones.
	// Census-proven fix from the old poller.
	RatioCap float64
}

// NewEngine constructs an empty Engine.
func NewEngine(minDebt, ratioCap float64) *Engine {
	return &Engine{Accounts: nil, MinDebt: minDebt, RatioCap: ratioCap}
}

// Rebuild rebuilds from decoded obligations. Keeps v1-shaped, >= MinDebt,
// complete obligations near threshold (watchRatio <= ratio <= RatioCap at
// build prices).
func (e *Engine) Rebuild(
	obls []ObligationRef,
	reserves map[solana.Pubkey]save.Reserve,
	mintFeed map[solana.Pubkey]uint32,
	watchRatio float64,
	lazerNow map[uint32]float64,
) int {
	e.Accounts = e.Accounts[:0]
	for _, ob := range obls {
		if ob.Obligation.BorrowedValue < e.MinDebt {
			continue
		}
		w, ok := BuildSolendWatch(&ob.Obligation, ob.Pubkey, reserves, mintFeed, lazerNow)
		if !ok {
			continue
		}
		r := w.RatioNow(lazerNow)
		if w.Complete && r >= watchRatio && r <= e.RatioCap {
			e.Accounts = append(e.Accounts, w)
		}
	}
	return len(e.Accounts)
}

// ObligationRef pairs an obligation's pubkey with its decoded contents (the
// Go equivalent of Rust's &[(Pubkey, Obligation)]).
type ObligationRef struct {
	Pubkey     solana.Pubkey
	Obligation save.Obligation
}

// Crossed returns liquidatable obligations (fireRatio <= ratio <= RatioCap)
// at these prices.
func (e *Engine) Crossed(lazer map[uint32]float64, fireRatio float64) []solana.Pubkey {
	var out []solana.Pubkey
	for i := range e.Accounts {
		w := &e.Accounts[i]
		if !w.Complete || !w.FeedsReady(lazer) {
			continue
		}
		r := w.RatioNow(lazer)
		if r >= fireRatio && r <= e.RatioCap {
			out = append(out, w.Obligation)
		}
	}
	return out
}

// ObligationDeficit pairs an obligation pubkey with a USD deficit, used for
// ranked output.
type ObligationDeficit struct {
	Obligation solana.Pubkey
	Deficit    float64
}

// CrossedRanked is the same as Crossed, ranked by USD deficit (borrowed −
// unhealthy) desc — biggest real opportunity first, with the mis-priced-dust
// tail excluded by RatioCap.
func (e *Engine) CrossedRanked(lazer map[uint32]float64, fireRatio float64) []ObligationDeficit {
	var v []ObligationDeficit
	for i := range e.Accounts {
		w := &e.Accounts[i]
		if !w.Complete || !w.FeedsReady(lazer) {
			continue
		}
		r := w.RatioNow(lazer)
		if r >= fireRatio && r <= e.RatioCap {
			v = append(v, ObligationDeficit{Obligation: w.Obligation, Deficit: w.Borrowed(lazer) - w.Unhealthy(lazer)})
		}
	}
	sort.SliceStable(v, func(i, j int) bool { return v[i].Deficit > v[j].Deficit })
	return v
}

// OnchainLiquidatableRanked is the FIRE tier — obligations liquidatable at
// the ON-CHAIN oracle price (stored health from the last rescan), ranked by
// USD deficit desc so the biggest real opportunity wins the capped sim
// budget. Lazer only NARROWS who to watch; the on-chain price GATES the
// expensive sim. The mis-priced-dust tail (borrowed >> ~0 unhealthy) is
// excluded by RatioCap, same as CrossedRanked.
func (e *Engine) OnchainLiquidatableRanked() []ObligationDeficit {
	var v []ObligationDeficit
	for i := range e.Accounts {
		w := &e.Accounts[i]
		if w.OnchainLiquidatable() && w.OnchainRatio() <= e.RatioCap {
			v = append(v, ObligationDeficit{Obligation: w.Obligation, Deficit: w.OnchainDeficit()})
		}
	}
	sort.SliceStable(v, func(i, j int) bool { return v[i].Deficit > v[j].Deficit })
	return v
}

// OnchainLiquidatableCount is the count of on-chain-liquidatable
// obligations — the REAL fire-candidate count for the heartbeat, distinct
// from the (much larger) Lazer-flagged set.
func (e *Engine) OnchainLiquidatableCount() int {
	n := 0
	for i := range e.Accounts {
		w := &e.Accounts[i]
		if w.OnchainLiquidatable() && w.OnchainRatio() <= e.RatioCap {
			n++
		}
	}
	return n
}

// OnchainRatioOf is the on-chain (stored) ratio for a specific watched
// obligation.
func (e *Engine) OnchainRatioOf(obligation solana.Pubkey) (float64, bool) {
	for i := range e.Accounts {
		w := &e.Accounts[i]
		if w.Obligation == obligation {
			return w.OnchainRatio(), true
		}
	}
	return 0, false
}

// ReservesOf looks up a watched obligation's reserves (for building the
// fire/refresh).
func (e *Engine) ReservesOf(obligation solana.Pubkey) (collReserve, debtReserve solana.Pubkey, ok bool) {
	for i := range e.Accounts {
		w := &e.Accounts[i]
		if w.Obligation == obligation {
			return w.CollReserve, w.DebtReserve, true
		}
	}
	return solana.Pubkey{}, solana.Pubkey{}, false
}
