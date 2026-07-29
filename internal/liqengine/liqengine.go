// Package liqengine is an event-driven liquidation engine: an in-memory
// watch-set that recomputes account health on every Pyth Lazer tick with
// ZERO RPC in the hot path, replacing the 5-second poll as the trigger.
//
// marginfi maintenance health is LINEAR in each bank's price:
//
//	weighted_assets      = Σ_b (asset_shares·asset_share_value/scale·w_maint_a)·price_b
//	weighted_liabilities = Σ_b (liab_shares ·liab_share_value /scale·w_maint_l)·price_b
//
// so per account we precompute, once per rescan, the price COEFFICIENTS and
// split them by whether the bank's price comes from a Lazer feed (fast) or
// the on-chain baseline (slow-moving). A tick then costs O(#mapped feeds)
// per account — a few multiply-adds — instead of an RPC round-trip. The
// result is bit-for-bit the same number as liquidation.MaintenanceHealth
// (unit-tested).
package liqengine

import (
	"sort"

	"arbengine/internal/liquidation"
	"arbengine/internal/solana"
)

// WatchAccount is one account, reduced to price coefficients. The const_*
// fields fold in the banks priced off the on-chain baseline (captured at
// build time); the feed_* maps are the per-Lazer-feed coefficients applied
// to live tick prices.
type WatchAccount struct {
	Pubkey solana.Pubkey
	// constA is the weighted-assets contribution from baseline-priced banks (USD).
	constA float64
	// constL is the weighted-liabilities contribution from baseline-priced banks (USD).
	constL float64
	// feedA maps Lazer feed id -> summed asset coefficient (multiply by live price -> USD).
	feedA map[uint32]float64
	feedL map[uint32]float64
	// WeightedAssetsSnapshot is the total weighted assets at build-time
	// prices — for the min-collateral gate.
	WeightedAssetsSnapshot float64
	// Complete is false when a balance's bank or baseline price was
	// unresolved -> health INCOMPLETE, never trusted for a fire decision
	// (mirrors liquidation.HealthResult.Missing).
	Complete bool
}

// BuildWatchAccount builds the coefficient form. baseline = on-chain prices
// (authoritative for anything not on a Lazer feed); mintFeed maps a bank's
// mint to its Lazer feed id; direct is the subset of mints priced 1:1 by
// their feed (everything else feed-mapped is anchor-scaled to baseline —
// see below). lazerNow are the Lazer prices at build time, used to anchor
// non-1:1 banks and to seed WeightedAssetsSnapshot consistently.
func BuildWatchAccount(
	acct *liquidation.MarginfiAccount,
	banks liquidation.BankMap,
	baseline liquidation.PriceMap,
	mintFeed map[solana.Pubkey]uint32,
	direct map[solana.Pubkey]struct{},
	lazerNow map[uint32]float64,
) WatchAccount {
	constA := 0.0
	constL := 0.0
	feedA := make(map[uint32]float64)
	feedL := make(map[uint32]float64)
	complete := true

	// Liability banks for the emode intersection rule (matches liquidation.MaintenanceHealth).
	var liabBanks []*liquidation.Bank
	for _, b := range acct.Balances {
		if b.LiabilityShares <= 0.0 {
			continue
		}
		if bank, ok := banks[b.BankPk]; ok {
			liabBanks = append(liabBanks, bank)
		}
	}

	for _, b := range acct.Balances {
		bank, ok := banks[b.BankPk]
		if !ok {
			if b.AssetShares > 0.0 || b.LiabilityShares > 0.0 {
				complete = false
			}
			continue
		}
		scale := pow10(bank.MintDecimals)
		coefA := 0.0
		if b.AssetShares > 0.0 {
			w := liquidation.EffectiveAssetWeightMaint(bank, liabBanks)
			coefA = b.AssetShares * bank.AssetShareValue / scale * w
		}
		coefL := 0.0
		if b.LiabilityShares > 0.0 {
			coefL = b.LiabilityShares * bank.LiabilityShareValue / scale * bank.LiabilityWeightMaint
		}
		if coefA == 0.0 && coefL == 0.0 {
			continue
		}

		feed, hasFeed := mintFeed[bank.Mint]
		switch {
		case hasFeed:
			_, isDirect := direct[bank.Mint]
			if isDirect {
				// 1:1 bank (its on-chain price IS the feed's asset): raw feed
				// coefficient. The feed's absolute level is the true price, so
				// the engine deliberately LEADS a stale on-chain oracle — the
				// crank edge depends on this.
				feedA[feed] += coefA
				feedL[feed] += coefL
				continue
			}
			// Feed-mapped but NOT 1:1 (LSTs -> SOL feed): anchor-scale to
			// the on-chain baseline. k = baseline/feed makes the bank's
			// effective price equal its oracle price at build time and
			// track the feed's relative movement afterwards. Unscaled, an
			// LST reads 15-35% under its real value (the raw SOL price)
			// and healthy accounts look deep underwater — the phantom-
			// candidate bug. Without a live feed price to anchor against,
			// fold the baseline into the const part (correct level, just
			// not tick-driven until the next rescan).
			bp, bpOk := baseline[b.BankPk]
			lp, lpOk := lazerNow[feed]
			switch {
			case bpOk && lpOk && lp > 0.0:
				k := bp / lp
				feedA[feed] += coefA * k
				feedL[feed] += coefL * k
			case bpOk:
				constA += coefA * bp
				constL += coefL * bp
			default:
				complete = false
			}
		default:
			// Bank priced off the on-chain baseline: fold its price in now.
			if price, ok := baseline[b.BankPk]; ok {
				constA += coefA * price
				constL += coefL * price
			} else {
				complete = false
			}
		}
	}

	// Snapshot weighted assets at build-time prices (baseline already folded;
	// add the Lazer-priced part at current Lazer prices).
	wa := constA
	for feed, ca := range feedA {
		wa += ca * lazerNow[feed]
	}

	return WatchAccount{
		Pubkey:                 acct.Authority, // display only; keyed externally by account pk
		constA:                 constA,
		constL:                 constL,
		feedA:                  feedA,
		feedL:                  feedL,
		WeightedAssetsSnapshot: wa,
		Complete:               complete,
	}
}

// Health computes health at the given Lazer prices (baseline banks are
// already folded). A feed with no live price falls back to 0 -> treated as
// unresolved, so callers should only trust Complete accounts with all feeds
// present.
func (wa *WatchAccount) Health(lazer map[uint32]float64) liquidation.Health {
	waAssets := wa.constA
	waLiabs := wa.constL
	for feed, ca := range wa.feedA {
		waAssets += ca * lazer[feed]
	}
	for feed, cl := range wa.feedL {
		waLiabs += cl * lazer[feed]
	}
	return liquidation.Health{WeightedAssets: waAssets, WeightedLiabilities: waLiabs}
}

// FeedsReady is true once we have a live Lazer price for every feed this
// account depends on (else the linear health is missing a term and must not
// be trusted).
func (wa *WatchAccount) FeedsReady(lazer map[uint32]float64) bool {
	for f := range wa.feedA {
		if _, ok := lazer[f]; !ok {
			return false
		}
	}
	for f := range wa.feedL {
		if _, ok := lazer[f]; !ok {
			return false
		}
	}
	return true
}

// watchEntry pairs a keying pubkey with its coefficient form.
type watchEntry struct {
	pk solana.Pubkey
	wa WatchAccount
}

// Engine is the in-memory watch-set. Rebuilt on rescan; queried on every tick.
type Engine struct {
	accounts      []watchEntry
	MinCollateral float64
}

// NewEngine constructs an empty Engine with the given min-collateral gate.
func NewEngine(minCollateral float64) *Engine {
	return &Engine{MinCollateral: minCollateral}
}

// Accounts exposes the current watch-set as (pubkey, WatchAccount) pairs.
func (e *Engine) Accounts() []struct {
	Pubkey solana.Pubkey
	WA     WatchAccount
} {
	out := make([]struct {
		Pubkey solana.Pubkey
		WA     WatchAccount
	}, len(e.accounts))
	for i, a := range e.accounts {
		out[i] = struct {
			Pubkey solana.Pubkey
			WA     WatchAccount
		}{a.pk, a.wa}
	}
	return out
}

// Len returns the number of accounts currently in the watch-set.
func (e *Engine) Len() int { return len(e.accounts) }

// Rebuild rebuilds the watch-set from decoded accounts. watchRatio
// pre-filters to accounts already near threshold (at build-time prices) so
// ticks evaluate a small hot set. Returns how many were kept.
func (e *Engine) Rebuild(
	accts []struct {
		Pubkey  solana.Pubkey
		Account liquidation.MarginfiAccount
	},
	banks liquidation.BankMap,
	baseline liquidation.PriceMap,
	mintFeed map[solana.Pubkey]uint32,
	direct map[solana.Pubkey]struct{},
	lazerNow map[uint32]float64,
	watchRatio float64,
) int {
	e.accounts = e.accounts[:0]
	for _, a := range accts {
		wa := BuildWatchAccount(&a.Account, banks, baseline, mintFeed, direct, lazerNow)
		if !wa.Complete || wa.WeightedAssetsSnapshot < e.MinCollateral {
			continue
		}
		h := wa.Health(lazerNow)
		if h.Ratio() >= watchRatio {
			e.accounts = append(e.accounts, watchEntry{pk: a.Pubkey, wa: wa})
		}
	}
	return len(e.accounts)
}

// Crossed returns accounts that are liquidatable (ratio >= fireRatio,
// health complete, all feeds priced) at the given Lazer prices. Pure
// arithmetic, no RPC.
func (e *Engine) Crossed(lazer map[uint32]float64, fireRatio float64) []solana.Pubkey {
	var out []solana.Pubkey
	for _, a := range e.accounts {
		wa := a.wa
		if wa.Complete && wa.FeedsReady(lazer) && wa.Health(lazer).Ratio() >= fireRatio {
			out = append(out, a.pk)
		}
	}
	return out
}

// CrossedRanked returns the same set as Crossed, but each paired with a
// priority score (the USD deficit = weighted_liabilities - weighted_assets)
// and sorted most-urgent first. A larger score means deeper underwater /
// bigger position, so the caller can cap per-cycle sim work to the top-K
// without starving the biggest real opportunities. For the arm-set (score <
// 0, not yet crossed) this ranks the accounts closest to crossing first.
func (e *Engine) CrossedRanked(lazer map[uint32]float64, fireRatio float64) []struct {
	Pubkey solana.Pubkey
	Score  float64
} {
	var v []struct {
		Pubkey solana.Pubkey
		Score  float64
	}
	for _, a := range e.accounts {
		wa := a.wa
		if !(wa.Complete && wa.FeedsReady(lazer)) {
			continue
		}
		h := wa.Health(lazer)
		if h.Ratio() >= fireRatio {
			v = append(v, struct {
				Pubkey solana.Pubkey
				Score  float64
			}{a.pk, h.WeightedLiabilities - h.WeightedAssets})
		}
	}
	sort.Slice(v, func(i, j int) bool { return v[i].Score > v[j].Score })
	return v
}

func pow10(exp uint8) float64 {
	r := 1.0
	for i := uint8(0); i < exp; i++ {
		r *= 10.0
	}
	return r
}
