//! Off-chain, Lazer-driven health for Kamino (KLend) obligations — the
//! event-driven trigger that replaces liq_kamino_executor's 30s poll. The Save
//! census (45 USDC-debt liquidations in 48h, 0 caught) proved the poll fatal:
//! competitors react to the oracle in milliseconds while we looked every 30s.
//!
//! Design — same anchor-on-stored-health approach as src/save_engine.rs. Kamino
//! stores its own on-chain-correct health on the obligation as `Fraction`
//! fixed-point: `bf_adjusted_debt` (the borrow-factor-adjusted debt value) and
//! `unhealthy_borrow_value` (the liquidation threshold), with
//!
//!     liquidatable  ⟺  bf_adjusted_debt ≥ unhealthy_borrow_value
//!
//! Those values are correct as of the obligation's last on-chain refresh. We
//! ANCHOR on them and TRACK by the Lazer price RATIO: at rescan we snapshot each
//! side's Lazer feed price; on every tick we scale the stored values by
//! `lazer_now / lazer_at_rescan`. The debt side scales by the DEBT feed, the
//! threshold side by the COLLATERAL feed. Exactly 1.0 at rescan (reproduces the
//! on-chain values) and it tracks ms moves between rescans with ZERO RPC. The
//! borrow-factor / liquidation-threshold multipliers are already baked into the
//! stored values, so a proportional price move preserves them.
//!
//! Anchoring on the *Lazer feed* price (not the reserve's Scope `market_price`)
//! is what makes LST collateral correct: mSOL/jitoSOL map to the SOL feed but
//! their reserve price carries the staking premium — the ratio only cares about
//! the feed's relative move.
//!
//! v1 scope: single deposit + single borrow, non-elevation (matches the fire
//! path). The full on-chain fire-tx simulation remains the authoritative gate;
//! this engine only decides WHO to spend that sim/arm budget on, fast.

use crate::kamino::Obligation;
use solana_pubkey::Pubkey;
use std::collections::HashMap;

/// One watched Kamino obligation reduced to price-ratio tracking.
#[derive(Clone, Debug)]
pub struct KaminoWatch {
    pub obligation: Pubkey,
    pub coll_reserve: Pubkey,
    pub debt_reserve: Pubkey,
    /// Stored borrow-factor-adjusted debt (USD) — the value compared to threshold.
    bf_debt_stored: f64,
    /// Stored liquidation threshold (USD).
    unhealthy_stored: f64,
    /// Lazer feed for each side (None = priced off a non-Lazer oracle, so it
    /// can't move between rescans → ratio stays 1.0).
    coll_feed: Option<u32>,
    debt_feed: Option<u32>,
    /// Lazer feed price captured at rescan (the ratio anchor). None if the feed
    /// had no live price at rescan.
    coll_anchor: Option<f64>,
    debt_anchor: Option<f64>,
    /// bf_adjusted_debt > 0 and unhealthy_borrow_value > 0 — else never trusted.
    pub complete: bool,
}

fn ratio(feed: Option<u32>, anchor: Option<f64>, lazer: &HashMap<u32, f64>) -> f64 {
    match (feed, anchor) {
        (Some(f), Some(a)) if a > 0.0 => lazer.get(&f).map(|p| p / a).unwrap_or(1.0),
        _ => 1.0,
    }
}

impl KaminoWatch {
    /// Build for a v1 obligation (1 deposit, 1 borrow, non-elevation). `reserve_feed`
    /// maps each reserve pubkey → its Lazer feed id (via the reserve's liquidity
    /// mint); `lazer_now` is the Lazer snapshot at rescan, used to anchor the ratios.
    pub fn build(
        o: &Obligation,
        obligation: Pubkey,
        reserve_feed: &HashMap<Pubkey, u32>,
        lazer_now: &HashMap<u32, f64>,
    ) -> Option<KaminoWatch> {
        if o.deposits.len() != 1 || o.borrows.len() != 1 || o.elevation_group != 0 { return None; }
        let coll_reserve = o.deposits[0].0;
        let debt_reserve = o.borrows[0].0;
        let coll_feed = reserve_feed.get(&coll_reserve).copied();
        let debt_feed = reserve_feed.get(&debt_reserve).copied();
        let coll_anchor = coll_feed.and_then(|f| lazer_now.get(&f).copied());
        let debt_anchor = debt_feed.and_then(|f| lazer_now.get(&f).copied());
        Some(KaminoWatch {
            obligation,
            coll_reserve,
            debt_reserve,
            bf_debt_stored: o.bf_adjusted_debt,
            unhealthy_stored: o.unhealthy_borrow_value,
            coll_feed,
            debt_feed,
            coll_anchor,
            debt_anchor,
            complete: o.unhealthy_borrow_value > 0.0 && o.bf_adjusted_debt > 0.0,
        })
    }

    /// Borrow-factor-adjusted debt scaled to the current debt-feed price.
    pub fn bf_debt(&self, lazer: &HashMap<u32, f64>) -> f64 {
        self.bf_debt_stored * ratio(self.debt_feed, self.debt_anchor, lazer)
    }
    /// Liquidation threshold scaled to the current collateral-feed price.
    pub fn unhealthy(&self, lazer: &HashMap<u32, f64>) -> f64 {
        self.unhealthy_stored * ratio(self.coll_feed, self.coll_anchor, lazer)
    }
    pub fn liquidatable(&self, lazer: &HashMap<u32, f64>) -> bool {
        self.complete && self.bf_debt(lazer) >= self.unhealthy(lazer)
    }
    /// bf_debt / unhealthy — ≥ 1.0 = underwater; how close otherwise.
    pub fn ratio_now(&self, lazer: &HashMap<u32, f64>) -> f64 {
        let u = self.unhealthy(lazer);
        if u <= 0.0 { 0.0 } else { self.bf_debt(lazer) / u }
    }
    /// True unless a Lazer-mapped side is missing a live price (then the ratio
    /// would silently fall back to 1.0 and hide a move — don't trust it).
    pub fn feeds_ready(&self, lazer: &HashMap<u32, f64>) -> bool {
        let ok = |f: Option<u32>, a: Option<f64>| match (f, a) {
            (Some(feed), Some(_)) => lazer.contains_key(&feed),
            _ => true,
        };
        ok(self.coll_feed, self.coll_anchor) && ok(self.debt_feed, self.debt_anchor)
    }
}

/// In-memory watch-set, rebuilt on rescan, queried on every Lazer tick.
pub struct Engine {
    pub accounts: Vec<KaminoWatch>,
    pub min_debt: f64,
    /// Reject obligations whose ratio exceeds this — an absurd ratio (debt ≫
    /// threshold) means the collateral is mis-priced near zero (dust / dead
    /// feed), never a real opportunity. Without it, deficit-ranking would put
    /// these un-fireable accounts FIRST (huge debt − ~0 threshold) and starve
    /// the genuine near-threshold ones. Census-proven fix from the old poller.
    pub ratio_cap: f64,
}

impl Engine {
    pub fn new(min_debt: f64, ratio_cap: f64) -> Engine {
        Engine { accounts: Vec::new(), min_debt, ratio_cap }
    }

    /// Rebuild from decoded obligations. Keeps v1-shaped, ≥ min_debt (borrowed
    /// market value), complete obligations near threshold (watch_ratio ≤ ratio
    /// ≤ ratio_cap at build prices).
    pub fn rebuild(
        &mut self,
        obls: &[(Pubkey, Obligation)],
        reserve_feed: &HashMap<Pubkey, u32>,
        watch_ratio: f64,
        lazer_now: &HashMap<u32, f64>,
    ) -> usize {
        self.accounts.clear();
        for (pk, o) in obls {
            if o.borrowed_value < self.min_debt { continue; }
            let Some(w) = KaminoWatch::build(o, *pk, reserve_feed, lazer_now) else { continue };
            let r = w.ratio_now(lazer_now);
            if w.complete && r >= watch_ratio && r <= self.ratio_cap {
                self.accounts.push(w);
            }
        }
        self.accounts.len()
    }

    /// Liquidatable obligations (fire_ratio ≤ ratio ≤ ratio_cap) at these prices.
    pub fn crossed(&self, lazer: &HashMap<u32, f64>, fire_ratio: f64) -> Vec<Pubkey> {
        self.accounts.iter()
            .filter(|w| w.complete && w.feeds_ready(lazer))
            .filter(|w| { let r = w.ratio_now(lazer); r >= fire_ratio && r <= self.ratio_cap })
            .map(|w| w.obligation).collect()
    }

    /// Same, ranked by USD deficit (bf_debt − unhealthy) desc — biggest real
    /// opportunity first, with the mis-priced-dust tail excluded by ratio_cap.
    pub fn crossed_ranked(&self, lazer: &HashMap<u32, f64>, fire_ratio: f64) -> Vec<(Pubkey, f64)> {
        let mut v: Vec<(Pubkey, f64)> = self.accounts.iter().filter_map(|w| {
            if !(w.complete && w.feeds_ready(lazer)) { return None; }
            let r = w.ratio_now(lazer);
            (r >= fire_ratio && r <= self.ratio_cap).then_some((w.obligation, w.bf_debt(lazer) - w.unhealthy(lazer)))
        }).collect();
        v.sort_by(|a, b| b.1.partial_cmp(&a.1).unwrap_or(std::cmp::Ordering::Equal));
        v
    }

    /// Look up a watched obligation's reserves (for building the fire/refresh).
    pub fn reserves_of(&self, obligation: &Pubkey) -> Option<(Pubkey, Pubkey)> {
        self.accounts.iter().find(|w| &w.obligation == obligation).map(|w| (w.coll_reserve, w.debt_reserve))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A v1 obligation with the given stored health, one collateral reserve and
    /// one debt reserve. Feed ids are wired by the returned reserve_feed map.
    fn mk_obligation(
        coll_reserve: Pubkey, debt_reserve: Pubkey,
        borrowed_value: f64, bf_adjusted_debt: f64, unhealthy_borrow_value: f64,
    ) -> Obligation {
        Obligation {
            owner: Pubkey::default(), lending_market: Pubkey::default(),
            last_update_slot: 0, stale: false,
            deposited_value: 1000.0, bf_adjusted_debt, borrowed_value,
            allowed_borrow_value: 0.0, unhealthy_borrow_value,
            elevation_group: 0,
            deposits: vec![(coll_reserve, 10)],
            borrows: vec![(debt_reserve, borrowed_value)],
        }
    }

    // SOL collateral (feed 6), USDC debt (feed 7).
    fn fixture() -> (Obligation, HashMap<Pubkey, u32>, Pubkey, Pubkey) {
        let coll = Pubkey::new_unique();
        let debt = Pubkey::new_unique();
        // Healthy at build: bf_debt 700 < unhealthy 800.
        let o = mk_obligation(coll, debt, 700.0, 700.0, 800.0);
        let reserve_feed = HashMap::from([(coll, 6u32), (debt, 7u32)]);
        (o, reserve_feed, coll, debt)
    }

    #[test]
    fn reproduces_stored_health_at_rescan() {
        let (o, reserve_feed, _, _) = fixture();
        let anchor = HashMap::from([(6u32, 100.0), (7u32, 1.0)]);
        let w = KaminoWatch::build(&o, Pubkey::new_unique(), &reserve_feed, &anchor).unwrap();
        // At the anchor prices, bf_debt/unhealthy == the stored values.
        assert!((w.bf_debt(&anchor) - 700.0).abs() < 1e-9);
        assert!((w.unhealthy(&anchor) - 800.0).abs() < 1e-9);
        assert!(!w.liquidatable(&anchor)); // 700 < 800
    }

    #[test]
    fn sol_drop_flips_liquidatable() {
        let (o, reserve_feed, _, _) = fixture();
        let anchor = HashMap::from([(6u32, 100.0), (7u32, 1.0)]);
        let w = KaminoWatch::build(&o, Pubkey::new_unique(), &reserve_feed, &anchor).unwrap();
        // SOL (collateral) drops 20% → unhealthy 800→640 < bf_debt 700 → liquidatable.
        let moved = HashMap::from([(6u32, 80.0), (7u32, 1.0)]);
        assert!((w.unhealthy(&moved) - 640.0).abs() < 1e-6);
        assert!(w.liquidatable(&moved));
    }

    #[test]
    fn ratio_cap_excludes_mispriced_dust() {
        // A dust obligation: $500 bf_debt, ~$1 threshold → ratio ~500. ratio_cap
        // keeps it OUT of the watch-set so it can't starve real near-threshold ones.
        let coll = Pubkey::new_unique();
        let debt = Pubkey::new_unique();
        let reserve_feed = HashMap::from([(coll, 6u32), (debt, 7u32)]);
        let dust = (Pubkey::new_unique(), mk_obligation(coll, debt, 500.0, 500.0, 1.0));
        let real = (Pubkey::new_unique(), mk_obligation(coll, debt, 810.0, 810.0, 800.0));
        let anchor = HashMap::from([(6u32, 100.0), (7u32, 1.0)]);
        let mut engine = Engine::new(100.0, 3.0);
        engine.rebuild(&[dust.clone(), real.clone()], &reserve_feed, 0.85, &anchor);
        let crossed = engine.crossed(&anchor, 1.0);
        assert_eq!(crossed, vec![real.0], "only the real near-threshold obligation, not the mis-priced dust");
    }

    #[test]
    fn lst_anchor_is_the_feed_not_reserve_price() {
        // jitoSOL collateral maps to the SOL feed (id 6). The stored
        // unhealthy_borrow_value was computed on-chain from jitoSOL's Scope price
        // (which carries the staking premium), but the ratio anchors on the FEED
        // price — so ratio = 1.0 at rescan regardless of the premium, no false
        // liquidation. (If we anchored on the reserve price we'd need it here at
        // all; we don't — the engine never sees it.)
        let coll = Pubkey::new_unique();
        let debt = Pubkey::new_unique();
        let reserve_feed = HashMap::from([(coll, 6u32), (debt, 7u32)]);
        let o = mk_obligation(coll, debt, 700.0, 700.0, 800.0);
        let anchor = HashMap::from([(6u32, 100.0), (7u32, 1.0)]);
        let w = KaminoWatch::build(&o, Pubkey::new_unique(), &reserve_feed, &anchor).unwrap();
        assert!((w.unhealthy(&anchor) - 800.0).abs() < 1e-9, "ratio must be 1.0 at rescan (feed-anchored)");
        assert!(!w.liquidatable(&anchor));
        // A 10% SOL-feed drop tracks the collateral even though the reserve price
        // is a premium jitoSOL price we never touch.
        let moved = HashMap::from([(6u32, 90.0), (7u32, 1.0)]);
        assert!((w.unhealthy(&moved) - 720.0).abs() < 1e-6);
    }
}
