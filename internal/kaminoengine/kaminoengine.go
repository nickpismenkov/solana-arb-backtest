// Package kaminoengine implements off-chain, Lazer-driven health for Kamino
// (KLend) obligations — the event-driven trigger that replaces
// liq_kamino_executor's 30s poll. The Save census (45 USDC-debt
// liquidations in 48h, 0 caught) proved the poll fatal: competitors react to
// the oracle in milliseconds while we looked every 30s.
//
// Design — same anchor-on-stored-health approach as the save engine. Kamino
// stores its own on-chain-correct health on the obligation as `Fraction`
// fixed-point: BfAdjustedDebt (the borrow-factor-adjusted debt value) and
// UnhealthyBorrowValue (the liquidation threshold), with
//
//	liquidatable  <=>  bf_adjusted_debt >= unhealthy_borrow_value
//
// Those values are correct as of the obligation's last on-chain refresh. We
// ANCHOR on them and TRACK by the Lazer price RATIO: at rescan we snapshot
// each side's Lazer feed price; on every tick we scale the stored values by
// lazer_now / lazer_at_rescan. The debt side scales by the DEBT feed, the
// threshold side by the COLLATERAL feed. Exactly 1.0 at rescan (reproduces
// the on-chain values) and it tracks ms moves between rescans with ZERO RPC.
// The borrow-factor / liquidation-threshold multipliers are already baked
// into the stored values, so a proportional price move preserves them.
//
// Anchoring on the Lazer feed price (not the reserve's Scope market_price)
// is what makes LST collateral correct: mSOL/jitoSOL map to the SOL feed but
// their reserve price carries the staking premium — the ratio only cares
// about the feed's relative move.
//
// v1 scope: single deposit + single borrow, non-elevation (matches the fire
// path). The full on-chain fire-tx simulation remains the authoritative
// gate; this engine only decides WHO to spend that sim/arm budget on, fast.
package kaminoengine

import (
	"sort"

	"arbengine/internal/kamino"
	"arbengine/internal/solana"
)

// KaminoWatch is one watched Kamino obligation reduced to price-ratio
// tracking.
type KaminoWatch struct {
	Obligation  solana.Pubkey
	CollReserve solana.Pubkey
	DebtReserve solana.Pubkey
	// bfDebtStored is the stored borrow-factor-adjusted debt (USD) — the
	// value compared to threshold.
	bfDebtStored float64
	// unhealthyStored is the stored liquidation threshold (USD).
	unhealthyStored float64
	// Lazer feed for each side (ok=false = priced off a non-Lazer oracle, so
	// it can't move between rescans -> ratio stays 1.0).
	collFeed    uint32
	hasCollFeed bool
	debtFeed    uint32
	hasDebtFeed bool
	// Lazer feed price captured at rescan (the ratio anchor). hasAnchor=false
	// if the feed had no live price at rescan.
	collAnchor    float64
	hasCollAnchor bool
	debtAnchor    float64
	hasDebtAnchor bool
	// Complete is true when bf_adjusted_debt > 0 and unhealthy_borrow_value >
	// 0 — else never trusted.
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

// BuildKaminoWatch builds a KaminoWatch for a v1 obligation (1 deposit, 1
// borrow, non-elevation). reserveFeed maps each reserve pubkey -> its Lazer
// feed id (via the reserve's liquidity mint); lazerNow is the Lazer snapshot
// at rescan, used to anchor the ratios.
func BuildKaminoWatch(o *kamino.Obligation, obligation solana.Pubkey, reserveFeed map[solana.Pubkey]uint32, lazerNow map[uint32]float64) (KaminoWatch, bool) {
	if len(o.Deposits) != 1 || len(o.Borrows) != 1 || o.ElevationGroup != 0 {
		return KaminoWatch{}, false
	}
	collReserve := o.Deposits[0].Reserve
	debtReserve := o.Borrows[0].Reserve
	collFeed, hasCollFeed := reserveFeed[collReserve]
	debtFeed, hasDebtFeed := reserveFeed[debtReserve]
	var collAnchor float64
	var hasCollAnchor bool
	if hasCollFeed {
		collAnchor, hasCollAnchor = lazerNow[collFeed]
	}
	var debtAnchor float64
	var hasDebtAnchor bool
	if hasDebtFeed {
		debtAnchor, hasDebtAnchor = lazerNow[debtFeed]
	}
	return KaminoWatch{
		Obligation:      obligation,
		CollReserve:     collReserve,
		DebtReserve:     debtReserve,
		bfDebtStored:    o.BfAdjustedDebt,
		unhealthyStored: o.UnhealthyBorrowValue,
		collFeed:        collFeed,
		hasCollFeed:     hasCollFeed,
		debtFeed:        debtFeed,
		hasDebtFeed:     hasDebtFeed,
		collAnchor:      collAnchor,
		hasCollAnchor:   hasCollAnchor,
		debtAnchor:      debtAnchor,
		hasDebtAnchor:   hasDebtAnchor,
		Complete:        o.UnhealthyBorrowValue > 0.0 && o.BfAdjustedDebt > 0.0,
	}, true
}

// BfDebt is the borrow-factor-adjusted debt scaled to the current debt-feed
// price.
func (w *KaminoWatch) BfDebt(lazer map[uint32]float64) float64 {
	return w.bfDebtStored * ratio(w.debtFeed, w.hasDebtFeed, w.debtAnchor, w.hasDebtAnchor, lazer)
}

// Unhealthy is the liquidation threshold scaled to the current
// collateral-feed price.
func (w *KaminoWatch) Unhealthy(lazer map[uint32]float64) float64 {
	return w.unhealthyStored * ratio(w.collFeed, w.hasCollFeed, w.collAnchor, w.hasCollAnchor, lazer)
}

// Liquidatable reports whether the watch is currently liquidatable at the
// Lazer-projected prices.
func (w *KaminoWatch) Liquidatable(lazer map[uint32]float64) bool {
	return w.Complete && w.BfDebt(lazer) >= w.Unhealthy(lazer)
}

// RatioNow is bf_debt / unhealthy — >= 1.0 = underwater; how close otherwise.
func (w *KaminoWatch) RatioNow(lazer map[uint32]float64) float64 {
	u := w.Unhealthy(lazer)
	if u <= 0.0 {
		return 0.0
	}
	return w.BfDebt(lazer) / u
}

// OnChainRatio is the on-chain (Scope) ratio as of the LAST RESCAN — the
// stored bf_debt / unhealthy with NO Lazer scaling. KLend liquidations
// settle at the on-chain Scope price, not the Lazer-projected one, so this
// (not RatioNow) is the authoritative gate for spending the expensive
// quote/sim/submit budget.
func (w *KaminoWatch) OnChainRatio() float64 {
	if w.unhealthyStored <= 0.0 {
		return 0.0
	}
	return w.bfDebtStored / w.unhealthyStored
}

// OnChainLiquidatable reports liquidatability at the on-chain Scope price
// captured at the last rescan.
func (w *KaminoWatch) OnChainLiquidatable() bool {
	return w.Complete && w.bfDebtStored >= w.unhealthyStored && w.unhealthyStored > 0.0
}

// OnChainDeficit is the USD deficit at the on-chain price (bf_debt -
// threshold); ranking key for the fire tier so the biggest real opportunity
// wins the capped budget.
func (w *KaminoWatch) OnChainDeficit() float64 {
	return w.bfDebtStored - w.unhealthyStored
}

// FeedsReady is true unless a Lazer-mapped side is missing a live price
// (then the ratio would silently fall back to 1.0 and hide a move — don't
// trust it).
func (w *KaminoWatch) FeedsReady(lazer map[uint32]float64) bool {
	ok := func(feed uint32, hasFeed bool, hasAnchor bool) bool {
		if hasFeed && hasAnchor {
			_, present := lazer[feed]
			return present
		}
		return true
	}
	return ok(w.collFeed, w.hasCollFeed, w.hasCollAnchor) && ok(w.debtFeed, w.hasDebtFeed, w.hasDebtAnchor)
}

// Engine is an in-memory watch-set, rebuilt on rescan, queried on every
// Lazer tick.
type Engine struct {
	Accounts []KaminoWatch
	MinDebt  float64
	// RatioCap rejects obligations whose ratio exceeds this — an absurd
	// ratio (debt >> threshold) means the collateral is mis-priced near zero
	// (dust / dead feed), never a real opportunity. Without it,
	// deficit-ranking would put these un-fireable accounts FIRST (huge debt
	// - ~0 threshold) and starve the genuine near-threshold ones.
	// Census-proven fix from the old poller.
	RatioCap float64
}

// NewEngine builds an Engine with the given min-debt and ratio-cap
// guardrails.
func NewEngine(minDebt, ratioCap float64) *Engine {
	return &Engine{MinDebt: minDebt, RatioCap: ratioCap}
}

// ObligationEntry pairs an obligation's pubkey with its decoded contents,
// the input shape for Rebuild.
type ObligationEntry struct {
	Pubkey     solana.Pubkey
	Obligation kamino.Obligation
}

// RankedObligation pairs an obligation's pubkey with a USD deficit ranking
// key, the output shape for CrossedRanked and OnChainLiquidatableRanked.
type RankedObligation struct {
	Obligation solana.Pubkey
	Deficit    float64
}

// Rebuild rebuilds the watch-set from decoded obligations. Keeps
// v1-shaped, >= MinDebt (borrowed market value), complete obligations near
// threshold (watchRatio <= ratio <= RatioCap at build prices).
func (e *Engine) Rebuild(obls []ObligationEntry, reserveFeed map[solana.Pubkey]uint32, watchRatio float64, lazerNow map[uint32]float64) int {
	e.Accounts = e.Accounts[:0]
	for _, item := range obls {
		if item.Obligation.BorrowedValue < e.MinDebt {
			continue
		}
		w, ok := BuildKaminoWatch(&item.Obligation, item.Pubkey, reserveFeed, lazerNow)
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

// Crossed returns liquidatable obligations (fireRatio <= ratio <= RatioCap)
// at these prices.
func (e *Engine) Crossed(lazer map[uint32]float64, fireRatio float64) []solana.Pubkey {
	var out []solana.Pubkey
	for i := range e.Accounts {
		w := &e.Accounts[i]
		if !(w.Complete && w.FeedsReady(lazer)) {
			continue
		}
		r := w.RatioNow(lazer)
		if r >= fireRatio && r <= e.RatioCap {
			out = append(out, w.Obligation)
		}
	}
	return out
}

// CrossedRanked is the same as Crossed, ranked by USD deficit (bf_debt -
// unhealthy) desc — biggest real opportunity first, with the mis-priced-dust
// tail excluded by RatioCap.
func (e *Engine) CrossedRanked(lazer map[uint32]float64, fireRatio float64) []RankedObligation {
	var v []RankedObligation
	for i := range e.Accounts {
		w := &e.Accounts[i]
		if !(w.Complete && w.FeedsReady(lazer)) {
			continue
		}
		r := w.RatioNow(lazer)
		if r >= fireRatio && r <= e.RatioCap {
			v = append(v, RankedObligation{w.Obligation, w.BfDebt(lazer) - w.Unhealthy(lazer)})
		}
	}
	sort.SliceStable(v, func(i, j int) bool { return v[i].Deficit > v[j].Deficit })
	return v
}

// OnChainLiquidatableRanked is the FIRE tier: obligations liquidatable at
// the ON-CHAIN Scope price captured at the last rescan (stored health — NOT
// the Lazer projection), ranked by USD deficit desc, with the mis-priced
// dust tail excluded by RatioCap. Only these are worth an expensive Jupiter
// quote + sim + submit. In a calm market this is near-empty even while the
// Lazer-projected Crossed set is large — that gap is exactly the
// phantom-flag bug this split fixes.
func (e *Engine) OnChainLiquidatableRanked() []RankedObligation {
	var v []RankedObligation
	for i := range e.Accounts {
		w := &e.Accounts[i]
		if w.OnChainLiquidatable() && w.OnChainRatio() <= e.RatioCap {
			v = append(v, RankedObligation{w.Obligation, w.OnChainDeficit()})
		}
	}
	sort.SliceStable(v, func(i, j int) bool { return v[i].Deficit > v[j].Deficit })
	return v
}

// OnChainLiquidatableCount is the count of on-chain-liquidatable
// obligations (for the heartbeat's real-fire tell).
func (e *Engine) OnChainLiquidatableCount() int {
	n := 0
	for i := range e.Accounts {
		w := &e.Accounts[i]
		if w.OnChainLiquidatable() && w.OnChainRatio() <= e.RatioCap {
			n++
		}
	}
	return n
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
