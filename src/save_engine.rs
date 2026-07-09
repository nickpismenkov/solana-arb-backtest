//! Off-chain, Lazer-driven health for Solend obligations — the event-driven
//! trigger that replaces liq_save_executor's 30s stored-health poll. That poll
//! lost every race (census: 45 USDC-debt Solend liquidations in 48h, 0 caught)
//! because competitors react to the oracle in ms while we looked every 30s.
//!
//! Design (robust, avoids Solend's fiddly absolute price/amount scaling): ANCHOR
//! on the obligation's own on-chain health — the STORED `borrowed_value` and
//! `unhealthy_borrow_value`, correct as of Solend's last refresh — and TRACK it
//! by the Lazer price RATIO. At rescan we snapshot each side's Lazer feed price;
//! on every tick we scale the stored values by `lazer_now / lazer_at_rescan`
//! (exactly 1.0 at rescan, so it reproduces the on-chain values, and tracks
//! ms-latency moves between rescans with ZERO RPC). Anchoring on the *Lazer*
//! price (not the reserve `market_price`) is what makes LST collateral correct:
//! mSOL/jitoSOL map to the SOL feed but their reserve price carries the staking
//! premium — the ratio only cares about the feed's relative move.
//!
//! v1 scope: single deposit + single borrow (matches the fire path). The full
//! on-chain simulateBundle remains the authoritative fire gate; this engine only
//! decides WHO to spend that sim budget on, fast.

use crate::save::{Obligation, Reserve};
use solana_pubkey::Pubkey;
use std::collections::HashMap;

/// One watched Solend obligation reduced to price-ratio tracking.
#[derive(Clone, Debug)]
pub struct SolendWatch {
    pub obligation: Pubkey,
    pub coll_reserve: Pubkey,
    pub debt_reserve: Pubkey,
    borrowed_stored: f64,
    unhealthy_stored: f64,
    /// Lazer feed for each side (None = priced off a non-Lazer/baseline oracle,
    /// so it can't move between rescans → ratio stays 1.0).
    coll_feed: Option<u32>,
    debt_feed: Option<u32>,
    /// Lazer feed price captured at rescan (the ratio anchor). None if the feed
    /// had no live price at rescan.
    coll_anchor: Option<f64>,
    debt_anchor: Option<f64>,
    /// unhealthy_borrow_value > 0 and both reserves priced — else never trusted.
    pub complete: bool,
}

fn ratio(feed: Option<u32>, anchor: Option<f64>, lazer: &HashMap<u32, f64>) -> f64 {
    match (feed, anchor) {
        (Some(f), Some(a)) if a > 0.0 => lazer.get(&f).map(|p| p / a).unwrap_or(1.0),
        _ => 1.0,
    }
}

impl SolendWatch {
    /// Build for a v1 obligation (1 deposit, 1 borrow). `lazer_now` is the Lazer
    /// snapshot at rescan, used to anchor the ratios.
    pub fn build(
        o: &Obligation,
        obligation: Pubkey,
        reserves: &HashMap<Pubkey, Reserve>,
        mint_feed: &HashMap<Pubkey, u32>,
        lazer_now: &HashMap<u32, f64>,
    ) -> Option<SolendWatch> {
        if o.deposits.len() != 1 || o.borrows.len() != 1 { return None; }
        let coll = reserves.get(&o.deposits[0].reserve)?;
        let debt = reserves.get(&o.borrows[0].reserve)?;
        let coll_feed = mint_feed.get(&coll.liquidity_mint).copied();
        let debt_feed = mint_feed.get(&debt.liquidity_mint).copied();
        let coll_anchor = coll_feed.and_then(|f| lazer_now.get(&f).copied());
        let debt_anchor = debt_feed.and_then(|f| lazer_now.get(&f).copied());
        Some(SolendWatch {
            obligation,
            coll_reserve: coll.reserve,
            debt_reserve: debt.reserve,
            borrowed_stored: o.borrowed_value,
            unhealthy_stored: o.unhealthy_borrow_value,
            coll_feed,
            debt_feed,
            coll_anchor,
            debt_anchor,
            complete: o.unhealthy_borrow_value > 0.0 && coll.market_price > 0.0 && debt.market_price > 0.0,
        })
    }

    pub fn borrowed(&self, lazer: &HashMap<u32, f64>) -> f64 {
        self.borrowed_stored * ratio(self.debt_feed, self.debt_anchor, lazer)
    }
    pub fn unhealthy(&self, lazer: &HashMap<u32, f64>) -> f64 {
        self.unhealthy_stored * ratio(self.coll_feed, self.coll_anchor, lazer)
    }
    pub fn liquidatable(&self, lazer: &HashMap<u32, f64>) -> bool {
        self.complete && self.borrowed(lazer) > self.unhealthy(lazer)
    }
    /// borrowed/unhealthy at the given prices (>1.0 = underwater).
    pub fn ratio_now(&self, lazer: &HashMap<u32, f64>) -> f64 {
        let u = self.unhealthy(lazer);
        if u <= 0.0 { 0.0 } else { self.borrowed(lazer) / u }
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
    pub accounts: Vec<SolendWatch>,
    pub min_debt: f64,
    /// Reject obligations whose ratio exceeds this — an absurd ratio (borrowed ≫
    /// unhealthy) means the collateral is mis-priced near zero (dust / dead
    /// feed), never a real opportunity. Without it, deficit-ranking would put
    /// these un-fireable accounts FIRST (huge borrowed − ~0 unhealthy) and starve
    /// the genuine near-threshold ones. Census-proven fix from the old poller.
    pub ratio_cap: f64,
}

impl Engine {
    pub fn new(min_debt: f64, ratio_cap: f64) -> Engine {
        Engine { accounts: Vec::new(), min_debt, ratio_cap }
    }

    /// Rebuild from decoded obligations. Keeps v1-shaped, ≥ min_debt, complete
    /// obligations near threshold (watch_ratio ≤ ratio ≤ ratio_cap at build prices).
    pub fn rebuild(
        &mut self,
        obls: &[(Pubkey, Obligation)],
        reserves: &HashMap<Pubkey, Reserve>,
        mint_feed: &HashMap<Pubkey, u32>,
        watch_ratio: f64,
        lazer_now: &HashMap<u32, f64>,
    ) -> usize {
        self.accounts.clear();
        for (pk, o) in obls {
            if o.borrowed_value < self.min_debt { continue; }
            let Some(w) = SolendWatch::build(o, *pk, reserves, mint_feed, lazer_now) else { continue };
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

    /// Same, ranked by USD deficit (borrowed − unhealthy) desc — biggest real
    /// opportunity first, with the mis-priced-dust tail excluded by ratio_cap.
    pub fn crossed_ranked(&self, lazer: &HashMap<u32, f64>, fire_ratio: f64) -> Vec<(Pubkey, f64)> {
        let mut v: Vec<(Pubkey, f64)> = self.accounts.iter().filter_map(|w| {
            if !(w.complete && w.feeds_ready(lazer)) { return None; }
            let r = w.ratio_now(lazer);
            (r >= fire_ratio && r <= self.ratio_cap).then_some((w.obligation, w.borrowed(lazer) - w.unhealthy(lazer)))
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
    use crate::save::{Borrow, Deposit};

    use std::str::FromStr;

    fn mk_reserve(mint: &str, price: f64) -> Reserve {
        Reserve {
            reserve: Pubkey::new_unique(), lending_market: Pubkey::default(),
            liquidity_mint: Pubkey::from_str(mint).unwrap(), mint_decimals: 9,
            liquidity_supply: Pubkey::default(), pyth_oracle: Pubkey::default(),
            switchboard_oracle: Pubkey::default(), collateral_mint: Pubkey::default(),
            collateral_supply: Pubkey::default(), fee_receiver: Pubkey::default(),
            market_price: price, liquidation_threshold_pct: 80, liquidation_bonus_pct: 5,
            loan_to_value_pct: 75,
        }
    }

    fn fixture() -> (Obligation, HashMap<Pubkey, Reserve>, HashMap<Pubkey, u32>) {
        // SOL collateral (feed 6), USDC debt (feed 7). Healthy at build.
        let sol = mk_reserve("So11111111111111111111111111111111111111112", 100.0);
        let usdc = mk_reserve("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", 1.0);
        let mut reserves = HashMap::new();
        reserves.insert(sol.reserve, sol.clone());
        reserves.insert(usdc.reserve, usdc.clone());
        let obl = Obligation {
            lending_market: Pubkey::default(), owner: Pubkey::default(),
            deposited_value: 1000.0, borrowed_value: 700.0, unhealthy_borrow_value: 800.0,
            deposits: vec![Deposit { reserve: sol.reserve, deposited_amount: 10, market_value: 1000.0 }],
            borrows: vec![Borrow { reserve: usdc.reserve, borrowed_amount_wads: 700.0, market_value: 700.0 }],
        };
        let mint_feed = HashMap::from([
            (sol.liquidity_mint, 6u32), (usdc.liquidity_mint, 7u32),
        ]);
        (obl, reserves, mint_feed)
    }

    #[test]
    fn reproduces_stored_health_at_rescan() {
        let (o, reserves, mint_feed) = fixture();
        let anchor = HashMap::from([(6u32, 100.0), (7u32, 1.0)]);
        let w = SolendWatch::build(&o, Pubkey::new_unique(), &reserves, &mint_feed, &anchor).unwrap();
        // At the anchor prices, borrowed/unhealthy == the stored values.
        assert!((w.borrowed(&anchor) - 700.0).abs() < 1e-9);
        assert!((w.unhealthy(&anchor) - 800.0).abs() < 1e-9);
        assert!(!w.liquidatable(&anchor)); // 700 < 800
    }

    #[test]
    fn sol_drop_flips_liquidatable() {
        let (o, reserves, mint_feed) = fixture();
        let anchor = HashMap::from([(6u32, 100.0), (7u32, 1.0)]);
        let w = SolendWatch::build(&o, Pubkey::new_unique(), &reserves, &mint_feed, &anchor).unwrap();
        // SOL (collateral) drops 20% → unhealthy 800→640 < borrowed 700 → liquidatable.
        let moved = HashMap::from([(6u32, 80.0), (7u32, 1.0)]);
        assert!((w.unhealthy(&moved) - 640.0).abs() < 1e-6);
        assert!(w.liquidatable(&moved));
    }

    #[test]
    fn ratio_cap_excludes_mispriced_dust() {
        // A dust obligation: $500 debt, ~$0 collateral → unhealthy ~$1, ratio ~500.
        // ratio_cap keeps it OUT of the watch-set so it can't starve real ones.
        let sol = mk_reserve("So11111111111111111111111111111111111111112", 100.0);
        let usdc = mk_reserve("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", 1.0);
        let mut reserves = HashMap::new();
        reserves.insert(sol.reserve, sol.clone());
        reserves.insert(usdc.reserve, usdc.clone());
        let dust = (Pubkey::new_unique(), Obligation {
            lending_market: Pubkey::default(), owner: Pubkey::default(),
            deposited_value: 1.0, borrowed_value: 500.0, unhealthy_borrow_value: 1.0,
            deposits: vec![Deposit { reserve: sol.reserve, deposited_amount: 1, market_value: 1.0 }],
            borrows: vec![Borrow { reserve: usdc.reserve, borrowed_amount_wads: 500.0, market_value: 500.0 }],
        });
        let real = (Pubkey::new_unique(), Obligation {
            lending_market: Pubkey::default(), owner: Pubkey::default(),
            deposited_value: 1000.0, borrowed_value: 810.0, unhealthy_borrow_value: 800.0,
            deposits: vec![Deposit { reserve: sol.reserve, deposited_amount: 10, market_value: 1000.0 }],
            borrows: vec![Borrow { reserve: usdc.reserve, borrowed_amount_wads: 810.0, market_value: 810.0 }],
        });
        let mint_feed = HashMap::from([(sol.liquidity_mint, 6u32), (usdc.liquidity_mint, 7u32)]);
        let anchor = HashMap::from([(6u32, 100.0), (7u32, 1.0)]);
        let mut engine = Engine::new(100.0, 3.0);
        engine.rebuild(&[dust.clone(), real.clone()], &reserves, &mint_feed, 0.85, &anchor);
        let crossed = engine.crossed(&anchor, 1.0);
        assert_eq!(crossed, vec![real.0], "only the real near-threshold obligation, not the mis-priced dust");
    }

    #[test]
    fn lst_anchor_is_the_feed_not_reserve_price() {
        // jitoSOL collateral (reserve price $115, but maps to SOL feed @ $100).
        // Anchoring on the FEED (100) means ratio=1 at rescan despite the $115
        // reserve price — no false liquidation.
        let jito = mk_reserve("J1toso1uCk3RLmjorhTtrVwY9HJ7X8V9yYac6Y7kGCPn", 115.0);
        let usdc = mk_reserve("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", 1.0);
        let mut reserves = HashMap::new();
        reserves.insert(jito.reserve, jito.clone());
        reserves.insert(usdc.reserve, usdc.clone());
        let o = Obligation {
            lending_market: Pubkey::default(), owner: Pubkey::default(),
            deposited_value: 1150.0, borrowed_value: 700.0, unhealthy_borrow_value: 800.0,
            deposits: vec![Deposit { reserve: jito.reserve, deposited_amount: 10, market_value: 1150.0 }],
            borrows: vec![Borrow { reserve: usdc.reserve, borrowed_amount_wads: 700.0, market_value: 700.0 }],
        };
        let mint_feed = HashMap::from([(jito.liquidity_mint, 6u32), (usdc.liquidity_mint, 7u32)]);
        let anchor = HashMap::from([(6u32, 100.0), (7u32, 1.0)]);
        let w = SolendWatch::build(&o, Pubkey::new_unique(), &reserves, &mint_feed, &anchor).unwrap();
        assert!((w.unhealthy(&anchor) - 800.0).abs() < 1e-9, "ratio must be 1.0 at rescan despite $115 reserve price");
        assert!(!w.liquidatable(&anchor));
    }
}
