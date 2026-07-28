// Off-chain, Lazer-driven health for Solend obligations — the event-driven
// trigger that replaces liq_save_executor's 30s stored-health poll. That
// poll lost every race (census: 45 USDC-debt Solend liquidations in 48h, 0
// caught) because competitors react to the oracle in ms while we looked
// every 30s.
//
// Design (robust, avoids Solend's fiddly absolute price/amount scaling):
// ANCHOR on the obligation's own on-chain health — the STORED
// BorrowedValue and UnhealthyBorrowValue, correct as of Solend's last
// refresh — and TRACK it by the Lazer price RATIO. At rescan we snapshot
// each side's Lazer feed price; on every tick we scale the stored values by
// lazer_now / lazer_at_rescan (exactly 1.0 at rescan, so it reproduces the
// on-chain values, and tracks ms-latency moves between rescans with ZERO
// RPC). Anchoring on the *Lazer* price (not the reserve MarketPrice) is
// what makes LST collateral correct: mSOL/jitoSOL map to the SOL feed but
// their reserve price carries the staking premium — the ratio only cares
// about the feed's relative move.
//
// v1 scope: single deposit + single borrow (matches the fire path). The
// full on-chain simulateBundle remains the authoritative fire gate; this
// engine only decides WHO to spend that sim budget on, fast.
package save

import (
	"sort"

	"github.com/gagliardetto/solana-go"
)

// Watch is one watched Solend obligation reduced to price-ratio tracking.
type Watch struct {
	Obligation      solana.PublicKey
	CollReserve     solana.PublicKey
	DebtReserve     solana.PublicKey
	borrowedStored  float64
	unhealthyStored float64
	// FreshBorrowed/FreshUnhealthy is the FRESH-price health at rescan —
	// borrowed/unhealthy recomputed from the freshly-fetched reserves via
	// the cToken exchange rate (Obligation.FreshHealth), i.e. the value
	// Solend's own `liquidate` recomputes at settle time. The FIRE tier
	// gates on THIS, not on the lazily-stale stored verdict that
	// over-reports phantoms. Zeroed if a referenced reserve was missing.
	freshBorrowed  float64
	freshUnhealthy float64
	// Lazer feed for each side (nil = priced off a non-Lazer/baseline
	// oracle, so it can't move between rescans → ratio stays 1.0).
	collFeed *uint32
	debtFeed *uint32
	// Lazer feed price captured at rescan (the ratio anchor). nil if the
	// feed had no live price at rescan.
	collAnchor *float64
	debtAnchor *float64
	// Complete is true iff unhealthy_borrow_value > 0 and both reserves
	// priced — else never trusted.
	Complete bool
}

func ratio(feed *uint32, anchor *float64, lazer map[uint32]float64) float64 {
	if feed == nil || anchor == nil || *anchor <= 0.0 {
		return 1.0
	}
	if p, ok := lazer[*feed]; ok {
		return p / *anchor
	}
	return 1.0
}

// BuildWatch builds a Watch for a v1 obligation (1 deposit, 1 borrow).
// lazerNow is the Lazer snapshot at rescan, used to anchor the ratios.
func BuildWatch(
	o *Obligation,
	obligation solana.PublicKey,
	reserves map[solana.PublicKey]*Reserve,
	mintFeed map[solana.PublicKey]uint32,
	lazerNow map[uint32]float64,
) (*Watch, bool) {
	if len(o.Deposits) != 1 || len(o.Borrows) != 1 {
		return nil, false
	}
	coll, ok := reserves[o.Deposits[0].Reserve]
	if !ok {
		return nil, false
	}
	debt, ok := reserves[o.Borrows[0].Reserve]
	if !ok {
		return nil, false
	}
	var collFeed, debtFeed *uint32
	if f, ok := mintFeed[coll.LiquidityMint]; ok {
		collFeed = &f
	}
	if f, ok := mintFeed[debt.LiquidityMint]; ok {
		debtFeed = &f
	}
	var collAnchor, debtAnchor *float64
	if collFeed != nil {
		if p, ok := lazerNow[*collFeed]; ok {
			collAnchor = &p
		}
	}
	if debtFeed != nil {
		if p, ok := lazerNow[*debtFeed]; ok {
			debtAnchor = &p
		}
	}
	// Fresh-price health from the current reserves (cToken exchange rate) —
	// the phantom-free fire-tier verdict. Falls back to (0,0) only if a
	// reserve is unexpectedly absent, which leaves the obligation NOT
	// fire-eligible (safe: it stays merely watched).
	freshBorrowed, freshUnhealthy, freshOk := o.FreshHealth(reserves)
	if !freshOk {
		freshBorrowed, freshUnhealthy = 0.0, 0.0
	}
	return &Watch{
		Obligation:      obligation,
		CollReserve:     coll.Reserve,
		DebtReserve:     debt.Reserve,
		borrowedStored:  o.BorrowedValue,
		unhealthyStored: o.UnhealthyBorrowValue,
		freshBorrowed:   freshBorrowed,
		freshUnhealthy:  freshUnhealthy,
		collFeed:        collFeed,
		debtFeed:        debtFeed,
		collAnchor:      collAnchor,
		debtAnchor:      debtAnchor,
		Complete:        o.UnhealthyBorrowValue > 0.0 && coll.MarketPrice > 0.0 && debt.MarketPrice > 0.0,
	}, true
}

// Borrowed is the stored borrowed value scaled to the current debt-feed price.
func (w *Watch) Borrowed(lazer map[uint32]float64) float64 {
	return w.borrowedStored * ratio(w.debtFeed, w.debtAnchor, lazer)
}

// Unhealthy is the stored unhealthy-borrow threshold scaled to the current
// collateral-feed price.
func (w *Watch) Unhealthy(lazer map[uint32]float64) float64 {
	return w.unhealthyStored * ratio(w.collFeed, w.collAnchor, lazer)
}

// Liquidatable is true if the Lazer-projected borrowed exceeds the
// Lazer-projected unhealthy threshold.
func (w *Watch) Liquidatable(lazer map[uint32]float64) bool {
	return w.Complete && w.Borrowed(lazer) > w.Unhealthy(lazer)
}

// RatioNow is borrowed/unhealthy at the given prices (>1.0 = underwater).
func (w *Watch) RatioNow(lazer map[uint32]float64) float64 {
	u := w.Unhealthy(lazer)
	if u <= 0.0 {
		return 0.0
	}
	return w.Borrowed(lazer) / u
}

// FeedsReady is true unless a Lazer-mapped side is missing a live price
// (then the ratio would silently fall back to 1.0 and hide a move — don't
// trust it).
func (w *Watch) FeedsReady(lazer map[uint32]float64) bool {
	ok := func(feed *uint32, anchor *float64) bool {
		if feed != nil && anchor != nil {
			_, present := lazer[*feed]
			return present
		}
		return true
	}
	return ok(w.collFeed, w.collAnchor) && ok(w.debtFeed, w.debtAnchor)
}

// ── FRESH-price on-chain health — FIRE-tier gate ────────────────────────
// The Lazer projection above decides WHO to re-check; these decide whether
// an obligation is ACTUALLY liquidatable at the on-chain oracle price
// Solend settles against — recomputed at rescan from the FRESH reserve
// prices + cToken exchange rate (Obligation.FreshHealth), the exact value
// Solend's own `liquidate` recomputes at settle time.
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

// OnChainLiquidatable is true if liquidatable at the FRESH on-chain
// (reserve) price captured at the last rescan.
func (w *Watch) OnChainLiquidatable() bool {
	return w.Complete && w.freshUnhealthy > 0.0 && w.freshBorrowed > w.freshUnhealthy
}

// OnChainDeficit is the USD deficit (borrowed − unhealthy) at fresh prices;
// > 0 iff liquidatable.
func (w *Watch) OnChainDeficit() float64 {
	return w.freshBorrowed - w.freshUnhealthy
}

// OnChainRatio is the fresh-price ratio (borrowed / unhealthy); > 1 =
// underwater on-chain.
func (w *Watch) OnChainRatio() float64 {
	if w.freshUnhealthy <= 0.0 {
		return 0.0
	}
	return w.freshBorrowed / w.freshUnhealthy
}

// Engine is an in-memory watch-set, rebuilt on rescan, queried on every
// Lazer tick.
type Engine struct {
	Accounts []*Watch
	MinDebt  float64
	// RatioCap rejects obligations whose ratio exceeds this — an absurd
	// ratio (borrowed ≫ unhealthy) means the collateral is mis-priced near
	// zero (dust / dead feed), never a real opportunity. Without it,
	// deficit-ranking would put these un-fireable accounts FIRST (huge
	// borrowed − ~0 unhealthy) and starve the genuine near-threshold ones.
	// Census-proven fix from the old poller.
	RatioCap float64
}

// NewEngine constructs an Engine.
func NewEngine(minDebt, ratioCap float64) *Engine {
	return &Engine{MinDebt: minDebt, RatioCap: ratioCap}
}

// ObligationEntry pairs a decoded obligation with its account pubkey.
type ObligationEntry struct {
	Pubkey     solana.PublicKey
	Obligation *Obligation
}

// Rebuild rebuilds from decoded obligations. Keeps v1-shaped, ≥ MinDebt,
// complete obligations near threshold (watchRatio ≤ ratio ≤ RatioCap at
// build prices).
func (e *Engine) Rebuild(
	obls []ObligationEntry,
	reserves map[solana.PublicKey]*Reserve,
	mintFeed map[solana.PublicKey]uint32,
	watchRatio float64,
	lazerNow map[uint32]float64,
) int {
	e.Accounts = nil
	for _, ent := range obls {
		if ent.Obligation.BorrowedValue < e.MinDebt {
			continue
		}
		w, ok := BuildWatch(ent.Obligation, ent.Pubkey, reserves, mintFeed, lazerNow)
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

// Crossed returns liquidatable obligations (fireRatio ≤ ratio ≤ RatioCap)
// at these prices.
func (e *Engine) Crossed(lazer map[uint32]float64, fireRatio float64) []solana.PublicKey {
	var out []solana.PublicKey
	for _, w := range e.Accounts {
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

// RankedEntry pairs an obligation with its ranking deficit (USD).
type RankedEntry struct {
	Obligation solana.PublicKey
	Deficit    float64
}

// CrossedRanked is the same as Crossed, ranked by USD deficit (borrowed −
// unhealthy) desc — biggest real opportunity first, with the
// mis-priced-dust tail excluded by RatioCap.
func (e *Engine) CrossedRanked(lazer map[uint32]float64, fireRatio float64) []RankedEntry {
	var v []RankedEntry
	for _, w := range e.Accounts {
		if !w.Complete || !w.FeedsReady(lazer) {
			continue
		}
		r := w.RatioNow(lazer)
		if r >= fireRatio && r <= e.RatioCap {
			v = append(v, RankedEntry{w.Obligation, w.Borrowed(lazer) - w.Unhealthy(lazer)})
		}
	}
	sort.Slice(v, func(i, j int) bool { return v[i].Deficit > v[j].Deficit })
	return v
}

// OnChainLiquidatableRanked is the FIRE tier — obligations liquidatable at
// the ON-CHAIN oracle price (fresh health from the last rescan), ranked by
// USD deficit desc so the biggest real opportunity wins the capped sim
// budget. Lazer only NARROWS who to watch; the on-chain price GATES the
// expensive sim. The mis-priced-dust tail (borrowed ≫ ~0 unhealthy) is
// excluded by RatioCap, same as CrossedRanked.
func (e *Engine) OnChainLiquidatableRanked() []RankedEntry {
	var v []RankedEntry
	for _, w := range e.Accounts {
		if w.OnChainLiquidatable() && w.OnChainRatio() <= e.RatioCap {
			v = append(v, RankedEntry{w.Obligation, w.OnChainDeficit()})
		}
	}
	sort.Slice(v, func(i, j int) bool { return v[i].Deficit > v[j].Deficit })
	return v
}

// OnChainLiquidatableCount is the count of on-chain-liquidatable
// obligations — the REAL fire-candidate count for the heartbeat, distinct
// from the (much larger) Lazer-flagged set.
func (e *Engine) OnChainLiquidatableCount() int {
	n := 0
	for _, w := range e.Accounts {
		if w.OnChainLiquidatable() && w.OnChainRatio() <= e.RatioCap {
			n++
		}
	}
	return n
}

// OnChainRatioOf is the on-chain (stored) ratio for a specific watched
// obligation.
func (e *Engine) OnChainRatioOf(obligation solana.PublicKey) (float64, bool) {
	for _, w := range e.Accounts {
		if w.Obligation.Equals(obligation) {
			return w.OnChainRatio(), true
		}
	}
	return 0, false
}

// ReservesOf looks up a watched obligation's reserves (for building the
// fire/refresh).
func (e *Engine) ReservesOf(obligation solana.PublicKey) (coll, debt solana.PublicKey, ok bool) {
	for _, w := range e.Accounts {
		if w.Obligation.Equals(obligation) {
			return w.CollReserve, w.DebtReserve, true
		}
	}
	return solana.PublicKey{}, solana.PublicKey{}, false
}
