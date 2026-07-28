// Off-chain, Lazer-driven health for Kamino (KLend) obligations — the
// event-driven trigger that replaces a 30s poll.
//
// Design: same anchor-on-stored-health approach as the Save engine. Kamino
// stores its own on-chain-correct health on the obligation as Fraction
// fixed-point: BfAdjustedDebt (the borrow-factor-adjusted debt value) and
// UnhealthyBorrowValue (the liquidation threshold), with
//
//	liquidatable  ⟺  bf_adjusted_debt ≥ unhealthy_borrow_value
//
// Those values are correct as of the obligation's last on-chain refresh. We
// ANCHOR on them and TRACK by the Lazer price RATIO: at rescan we snapshot
// each side's Lazer feed price; on every tick we scale the stored values by
// lazer_now / lazer_at_rescan.
package kamino

import (
	"sort"

	"github.com/gagliardetto/solana-go"
)

// Watch is one watched Kamino obligation reduced to price-ratio tracking.
type Watch struct {
	Obligation  solana.PublicKey
	CollReserve solana.PublicKey
	DebtReserve solana.PublicKey
	// Stored borrow-factor-adjusted debt (USD) — the value compared to threshold.
	bfDebtStored float64
	// Stored liquidation threshold (USD).
	unhealthyStored float64
	// Lazer feed for each side (nil = priced off a non-Lazer oracle, so it
	// can't move between rescans → ratio stays 1.0).
	collFeed *uint32
	debtFeed *uint32
	// Lazer feed price captured at rescan (the ratio anchor).
	collAnchor *float64
	debtAnchor *float64
	// bf_adjusted_debt > 0 and unhealthy_borrow_value > 0 — else never trusted.
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

// BuildWatch builds a Watch for a v1 obligation (1 deposit, 1 borrow,
// non-elevation). reserveFeed maps each reserve pubkey → its Lazer feed id
// (via the reserve's liquidity mint); lazerNow is the Lazer snapshot at
// rescan, used to anchor the ratios.
func BuildWatch(o *Obligation, obligation solana.PublicKey, reserveFeed map[solana.PublicKey]uint32, lazerNow map[uint32]float64) (*Watch, bool) {
	if len(o.Deposits) != 1 || len(o.Borrows) != 1 || o.ElevationGroup != 0 {
		return nil, false
	}
	collReserve := o.Deposits[0].Reserve
	debtReserve := o.Borrows[0].Reserve
	var collFeed, debtFeed *uint32
	if f, ok := reserveFeed[collReserve]; ok {
		collFeed = &f
	}
	if f, ok := reserveFeed[debtReserve]; ok {
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
	return &Watch{
		Obligation: obligation, CollReserve: collReserve, DebtReserve: debtReserve,
		bfDebtStored: o.BfAdjustedDebt, unhealthyStored: o.UnhealthyBorrowValue,
		collFeed: collFeed, debtFeed: debtFeed,
		collAnchor: collAnchor, debtAnchor: debtAnchor,
		Complete: o.UnhealthyBorrowValue > 0.0 && o.BfAdjustedDebt > 0.0,
	}, true
}

// BfDebt is the borrow-factor-adjusted debt scaled to the current debt-feed price.
func (w *Watch) BfDebt(lazer map[uint32]float64) float64 {
	return w.bfDebtStored * ratio(w.debtFeed, w.debtAnchor, lazer)
}

// Unhealthy is the liquidation threshold scaled to the current collateral-feed price.
func (w *Watch) Unhealthy(lazer map[uint32]float64) float64 {
	return w.unhealthyStored * ratio(w.collFeed, w.collAnchor, lazer)
}
func (w *Watch) Liquidatable(lazer map[uint32]float64) bool {
	return w.Complete && w.BfDebt(lazer) >= w.Unhealthy(lazer)
}

// RatioNow is bf_debt / unhealthy — ≥ 1.0 = underwater; how close otherwise.
func (w *Watch) RatioNow(lazer map[uint32]float64) float64 {
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
func (w *Watch) OnChainRatio() float64 {
	if w.unhealthyStored <= 0.0 {
		return 0.0
	}
	return w.bfDebtStored / w.unhealthyStored
}

// OnChainLiquidatable is true if liquidatable at the on-chain Scope price
// captured at the last rescan.
func (w *Watch) OnChainLiquidatable() bool {
	return w.Complete && w.bfDebtStored >= w.unhealthyStored && w.unhealthyStored > 0.0
}

// OnChainDeficit is the USD deficit at the on-chain price (bf_debt −
// threshold); ranking key for the fire tier so the biggest real opportunity
// wins the capped budget.
func (w *Watch) OnChainDeficit() float64 { return w.bfDebtStored - w.unhealthyStored }

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

// Engine is an in-memory watch-set, rebuilt on rescan, queried on every Lazer tick.
type Engine struct {
	Accounts []*Watch
	MinDebt  float64
	// Reject obligations whose ratio exceeds this — an absurd ratio (debt ≫
	// threshold) means the collateral is mis-priced near zero (dust / dead
	// feed), never a real opportunity.
	RatioCap float64
}

func NewEngine(minDebt, ratioCap float64) *Engine {
	return &Engine{MinDebt: minDebt, RatioCap: ratioCap}
}

// ObligationEntry pairs a decoded obligation with its account pubkey.
type ObligationEntry struct {
	Pubkey     solana.PublicKey
	Obligation *Obligation
}

// Rebuild rebuilds from decoded obligations. Keeps v1-shaped, ≥ min_debt
// (borrowed market value), complete obligations near threshold (watchRatio
// ≤ ratio ≤ ratioCap at build prices).
func (e *Engine) Rebuild(obls []ObligationEntry, reserveFeed map[solana.PublicKey]uint32, watchRatio float64, lazerNow map[uint32]float64) int {
	e.Accounts = nil
	for _, ent := range obls {
		if ent.Obligation.BorrowedValue < e.MinDebt {
			continue
		}
		w, ok := BuildWatch(ent.Obligation, ent.Pubkey, reserveFeed, lazerNow)
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

// Crossed returns liquidatable obligations (fireRatio ≤ ratio ≤ ratioCap) at these prices.
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

type rankedEntry struct {
	Obligation solana.PublicKey
	Deficit    float64
}

// CrossedRanked is the same as Crossed, ranked by USD deficit (bf_debt −
// unhealthy) desc — biggest real opportunity first.
func (e *Engine) CrossedRanked(lazer map[uint32]float64, fireRatio float64) []rankedEntry {
	var v []rankedEntry
	for _, w := range e.Accounts {
		if !w.Complete || !w.FeedsReady(lazer) {
			continue
		}
		r := w.RatioNow(lazer)
		if r >= fireRatio && r <= e.RatioCap {
			v = append(v, rankedEntry{w.Obligation, w.BfDebt(lazer) - w.Unhealthy(lazer)})
		}
	}
	sort.Slice(v, func(i, j int) bool { return v[i].Deficit > v[j].Deficit })
	return v
}

// OnChainLiquidatableRanked is the FIRE tier: obligations liquidatable at
// the ON-CHAIN Scope price captured at the last rescan (stored health — NOT
// the Lazer projection), ranked by USD deficit desc.
func (e *Engine) OnChainLiquidatableRanked() []rankedEntry {
	var v []rankedEntry
	for _, w := range e.Accounts {
		if w.OnChainLiquidatable() && w.OnChainRatio() <= e.RatioCap {
			v = append(v, rankedEntry{w.Obligation, w.OnChainDeficit()})
		}
	}
	sort.Slice(v, func(i, j int) bool { return v[i].Deficit > v[j].Deficit })
	return v
}

// OnChainLiquidatableCount counts on-chain-liquidatable obligations (for the
// heartbeat's real-fire tell).
func (e *Engine) OnChainLiquidatableCount() int {
	n := 0
	for _, w := range e.Accounts {
		if w.OnChainLiquidatable() && w.OnChainRatio() <= e.RatioCap {
			n++
		}
	}
	return n
}

// ReservesOf looks up a watched obligation's reserves (for building the fire/refresh).
func (e *Engine) ReservesOf(obligation solana.PublicKey) (coll, debt solana.PublicKey, ok bool) {
	for _, w := range e.Accounts {
		if w.Obligation.Equals(obligation) {
			return w.CollReserve, w.DebtReserve, true
		}
	}
	return solana.PublicKey{}, solana.PublicKey{}, false
}
