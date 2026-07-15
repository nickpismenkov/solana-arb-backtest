//! Event-driven liquidation engine: an in-memory watch-set that recomputes
//! account health on every Pyth Lazer tick with ZERO RPC in the hot path,
//! replacing the 5-second poll as the trigger.
//!
//! marginfi maintenance health is LINEAR in each bank's price:
//!   weighted_assets      = Σ_b (asset_shares·asset_share_value/scale·w_maint_a)·price_b
//!   weighted_liabilities = Σ_b (liab_shares ·liab_share_value /scale·w_maint_l)·price_b
//! so per account we precompute, once per rescan, the price COEFFICIENTS and
//! split them by whether the bank's price comes from a Lazer feed (fast) or the
//! on-chain baseline (slow-moving). A tick then costs O(#mapped feeds) per
//! account — a few multiply-adds — instead of an RPC round-trip. The result is
//! bit-for-bit the same number as liquidation::maintenance_health (unit-tested).

use crate::liquidation::{BankMap, Health, MarginfiAccount, PriceMap};
use solana_pubkey::Pubkey;
use std::collections::HashMap;

/// One account, reduced to price coefficients. `const_*` fold in the banks
/// priced off the on-chain baseline (captured at build time); `feed_*` are the
/// per-Lazer-feed coefficients applied to live tick prices.
#[derive(Clone, Debug)]
pub struct WatchAccount {
    pub pubkey: Pubkey,
    /// weighted-assets contribution from baseline-priced banks (USD).
    const_a: f64,
    /// weighted-liabilities contribution from baseline-priced banks (USD).
    const_l: f64,
    /// Lazer feed id → summed asset coefficient (multiply by live price → USD).
    feed_a: HashMap<u32, f64>,
    feed_l: HashMap<u32, f64>,
    /// Total weighted assets at build-time prices — for the min-collateral gate.
    pub weighted_assets_snapshot: f64,
    /// A balance whose bank or baseline price was unresolved → health INCOMPLETE,
    /// never trusted for a fire decision (mirrors maintenance_health.missing).
    pub complete: bool,
}

impl WatchAccount {
    /// Build the coefficient form. `baseline` = on-chain prices (authoritative
    /// for anything not on a Lazer feed); `mint_feed` maps a bank's mint to its
    /// Lazer feed id; `direct` is the subset of mints priced 1:1 by their feed
    /// (everything else feed-mapped is anchor-scaled to baseline — see below).
    /// `lazer_now` are the Lazer prices at build time, used to anchor non-1:1
    /// banks and to seed weighted_assets_snapshot consistently.
    pub fn build(
        acct: &MarginfiAccount,
        banks: &BankMap,
        baseline: &PriceMap,
        mint_feed: &HashMap<Pubkey, u32>,
        direct: &std::collections::HashSet<Pubkey>,
        lazer_now: &HashMap<u32, f64>,
    ) -> WatchAccount {
        let mut const_a = 0.0;
        let mut const_l = 0.0;
        let mut feed_a: HashMap<u32, f64> = HashMap::new();
        let mut feed_l: HashMap<u32, f64> = HashMap::new();
        let mut complete = true;
        // Liability banks for the emode intersection rule (matches maintenance_health).
        let liab_banks: Vec<&crate::liquidation::Bank> = acct.balances.iter()
            .filter(|b| b.liability_shares > 0.0)
            .filter_map(|b| banks.get(&b.bank_pk))
            .collect();
        for b in &acct.balances {
            let Some(bank) = banks.get(&b.bank_pk) else {
                if b.asset_shares > 0.0 || b.liability_shares > 0.0 { complete = false; }
                continue;
            };
            let scale = 10f64.powi(bank.mint_decimals as i32);
            let coef_a = if b.asset_shares > 0.0 {
                let w = crate::liquidation::effective_asset_weight_maint(bank, &liab_banks);
                b.asset_shares * bank.asset_share_value / scale * w
            } else { 0.0 };
            let coef_l = if b.liability_shares > 0.0 {
                b.liability_shares * bank.liability_share_value / scale * bank.liability_weight_maint
            } else { 0.0 };
            if coef_a == 0.0 && coef_l == 0.0 { continue; }

            match mint_feed.get(&bank.mint) {
                // 1:1 bank (its on-chain price IS the feed's asset): raw feed
                // coefficient. The feed's absolute level is the true price, so
                // the engine deliberately LEADS a stale on-chain oracle — the
                // crank edge depends on this.
                Some(&feed) if direct.contains(&bank.mint) => {
                    *feed_a.entry(feed).or_default() += coef_a;
                    *feed_l.entry(feed).or_default() += coef_l;
                }
                // Feed-mapped but NOT 1:1 (LSTs → SOL feed): anchor-scale to
                // the on-chain baseline. k = baseline/feed makes the bank's
                // effective price equal its oracle price at build time and
                // track the feed's relative movement afterwards. Unscaled, an
                // LST reads 15–35% under its real value (the raw SOL price)
                // and healthy accounts look deep underwater — the phantom-
                // candidate bug. Without a live feed price to anchor against,
                // fold the baseline into the const part (correct level, just
                // not tick-driven until the next rescan).
                Some(&feed) => {
                    match (baseline.get(&b.bank_pk), lazer_now.get(&feed)) {
                        (Some(&bp), Some(&lp)) if lp > 0.0 => {
                            let k = bp / lp;
                            *feed_a.entry(feed).or_default() += coef_a * k;
                            *feed_l.entry(feed).or_default() += coef_l * k;
                        }
                        (Some(&bp), _) => { const_a += coef_a * bp; const_l += coef_l * bp; }
                        (None, _) => { complete = false; }
                    }
                }
                // Bank priced off the on-chain baseline: fold its price in now.
                None => match baseline.get(&b.bank_pk) {
                    Some(&price) => { const_a += coef_a * price; const_l += coef_l * price; }
                    None => { complete = false; }
                },
            }
        }
        // Snapshot weighted assets at build-time prices (baseline already folded;
        // add the Lazer-priced part at current Lazer prices).
        let mut wa = const_a;
        for (feed, ca) in &feed_a {
            wa += ca * lazer_now.get(feed).copied().unwrap_or(0.0);
        }
        WatchAccount {
            pubkey: acct.authority, // display only; keyed externally by account pk
            const_a, const_l, feed_a, feed_l,
            weighted_assets_snapshot: wa,
            complete,
        }
    }

    /// Health at the given Lazer prices (baseline banks are already folded).
    /// A feed with no live price falls back to 0 → treated as unresolved, so
    /// callers should only trust `complete` accounts with all feeds present.
    pub fn health(&self, lazer: &HashMap<u32, f64>) -> Health {
        let mut wa = self.const_a;
        let mut wl = self.const_l;
        for (feed, ca) in &self.feed_a {
            wa += ca * lazer.get(feed).copied().unwrap_or(0.0);
        }
        for (feed, cl) in &self.feed_l {
            wl += cl * lazer.get(feed).copied().unwrap_or(0.0);
        }
        Health { weighted_assets: wa, weighted_liabilities: wl }
    }

    /// True once we have a live Lazer price for every feed this account depends
    /// on (else the linear health is missing a term and must not be trusted).
    pub fn feeds_ready(&self, lazer: &HashMap<u32, f64>) -> bool {
        self.feed_a.keys().chain(self.feed_l.keys()).all(|f| lazer.contains_key(f))
    }
}

/// The in-memory watch-set. Rebuilt on rescan; queried on every tick.
pub struct Engine {
    pub accounts: Vec<(Pubkey, WatchAccount)>,
    pub min_collateral: f64,
}

impl Engine {
    pub fn new(min_collateral: f64) -> Engine {
        Engine { accounts: Vec::new(), min_collateral }
    }

    /// Rebuild from decoded accounts. `watch_ratio` pre-filters to accounts
    /// already near threshold (at build-time prices) so ticks evaluate a small
    /// hot set. Returns how many were kept.
    #[allow(clippy::too_many_arguments)]
    pub fn rebuild(
        &mut self,
        accts: &[(Pubkey, MarginfiAccount)],
        banks: &BankMap,
        baseline: &PriceMap,
        mint_feed: &HashMap<Pubkey, u32>,
        direct: &std::collections::HashSet<Pubkey>,
        lazer_now: &HashMap<u32, f64>,
        watch_ratio: f64,
    ) -> usize {
        self.accounts.clear();
        for (pk, a) in accts {
            let wa = WatchAccount::build(a, banks, baseline, mint_feed, direct, lazer_now);
            if !wa.complete || wa.weighted_assets_snapshot < self.min_collateral { continue; }
            let h = wa.health(lazer_now);
            if h.ratio() >= watch_ratio {
                self.accounts.push((*pk, wa));
            }
        }
        self.accounts.len()
    }

    /// Accounts that are liquidatable (ratio ≥ fire_ratio, health complete, all
    /// feeds priced) at the given Lazer prices. Pure arithmetic, no RPC.
    pub fn crossed(&self, lazer: &HashMap<u32, f64>, fire_ratio: f64) -> Vec<Pubkey> {
        self.accounts.iter().filter_map(|(pk, wa)| {
            (wa.complete && wa.feeds_ready(lazer) && wa.health(lazer).ratio() >= fire_ratio).then_some(*pk)
        }).collect()
    }

    /// Same set as `crossed`, but each paired with a priority score (the USD
    /// deficit = weighted_liabilities − weighted_assets) and sorted most-urgent
    /// first. A larger score means deeper underwater / bigger position, so the
    /// caller can cap per-cycle sim work to the top-K without starving the
    /// biggest real opportunities. For the arm-set (score < 0, not yet crossed)
    /// this ranks the accounts closest to crossing first.
    pub fn crossed_ranked(&self, lazer: &HashMap<u32, f64>, fire_ratio: f64) -> Vec<(Pubkey, f64)> {
        let mut v: Vec<(Pubkey, f64)> = self.accounts.iter().filter_map(|(pk, wa)| {
            if !(wa.complete && wa.feeds_ready(lazer)) { return None; }
            let h = wa.health(lazer);
            (h.ratio() >= fire_ratio).then_some((*pk, h.weighted_liabilities - h.weighted_assets))
        }).collect();
        v.sort_by(|a, b| b.1.partial_cmp(&a.1).unwrap_or(std::cmp::Ordering::Equal));
        v
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::liquidation::{maintenance_health, Balance, Bank};

    fn mk_bank(mint: Pubkey, dec: u8, wa: f64, wl: f64) -> Bank {
        Bank {
            mint, mint_decimals: dec,
            asset_share_value: 1.0, liability_share_value: 1.0,
            asset_weight_init: wa, asset_weight_maint: wa,
            liability_weight_init: wl, liability_weight_maint: wl,
            oracle_setup: 3, oracle_key: Pubkey::default(), oracle_max_age: 0,
            emode_tag: 0, emode_entries: vec![],
        }
    }

    fn direct_of(map: &HashMap<Pubkey, u32>) -> std::collections::HashSet<Pubkey> {
        map.keys().copied().collect()
    }

    // An account with SOL collateral (Lazer feed 6) and USDC debt (baseline).
    fn fixture() -> (MarginfiAccount, BankMap, PriceMap, HashMap<Pubkey, u32>) {
        let sol_bank = Pubkey::new_unique();
        let usdc_bank = Pubkey::new_unique();
        let sol_mint = Pubkey::new_unique();
        let usdc_mint = Pubkey::new_unique();
        let mut banks = BankMap::new();
        banks.insert(sol_bank, mk_bank(sol_mint, 9, 0.8, 1.0));
        banks.insert(usdc_bank, mk_bank(usdc_mint, 6, 1.0, 1.1));
        let acct = MarginfiAccount {
            group: Pubkey::default(), authority: Pubkey::new_unique(),
            balances: vec![
                Balance { bank_pk: sol_bank, asset_shares: 10.0 * 1e9, liability_shares: 0.0 },
                Balance { bank_pk: usdc_bank, asset_shares: 0.0, liability_shares: 700.0 * 1e6 },
            ],
        };
        let mut baseline = PriceMap::new();
        baseline.insert(sol_bank, 100.0); // will be overridden by Lazer feed 6
        baseline.insert(usdc_bank, 1.0);
        let mint_feed = HashMap::from([(sol_mint, 6u32)]);
        (acct, banks, baseline, mint_feed)
    }

    #[test]
    fn engine_health_matches_maintenance_health() {
        let (acct, banks, baseline, mint_feed) = fixture();
        // Reference: maintenance_health with SOL @ $92.
        let mut prices = baseline.clone();
        let sol_bank = *banks.iter().find(|(_, b)| b.mint_decimals == 9).map(|(pk, _)| pk).unwrap();
        prices.insert(sol_bank, 92.0);
        let reference = maintenance_health(&acct, &banks, &prices).health;

        let lazer = HashMap::from([(6u32, 92.0)]);
        let wa = WatchAccount::build(&acct, &banks, &baseline, &mint_feed, &direct_of(&mint_feed), &lazer);
        let engine_h = wa.health(&lazer);
        assert!((engine_h.weighted_assets - reference.weighted_assets).abs() < 1e-6,
            "assets {} vs {}", engine_h.weighted_assets, reference.weighted_assets);
        assert!((engine_h.weighted_liabilities - reference.weighted_liabilities).abs() < 1e-6);
        assert!((engine_h.ratio() - reference.ratio()).abs() < 1e-9);
    }

    #[test]
    fn tick_flips_to_liquidatable() {
        let (acct, banks, baseline, mint_feed) = fixture();
        let mut engine = Engine::new(50.0);
        // Healthy at $100 (10 SOL × 0.8 × 100 = $800 assets vs 700×1.1=$770 → ratio .96).
        engine.rebuild(&[(Pubkey::new_unique(), acct.clone())], &banks, &baseline, &mint_feed,
            &direct_of(&mint_feed), &HashMap::from([(6u32, 100.0)]), 0.85);
        assert_eq!(engine.crossed(&HashMap::from([(6u32, 100.0)]), 1.0).len(), 0);
        // SOL drops to $90 → assets 10×0.8×90=$720 < $770 → liquidatable.
        assert_eq!(engine.crossed(&HashMap::from([(6u32, 90.0)]), 1.0).len(), 1);
    }

    #[test]
    fn lst_bank_is_anchored_to_baseline_not_raw_feed() {
        // LST collateral (mapped to feed 6 but NOT in `direct`): its oracle
        // values it at $130 while raw SOL trades at $100. Health must match
        // maintenance_health at the $130 baseline (not read 23% under), and a
        // SOL tick must move it proportionally.
        let lst_bank = Pubkey::new_unique();
        let usdc_bank = Pubkey::new_unique();
        let lst_mint = Pubkey::new_unique();
        let usdc_mint = Pubkey::new_unique();
        let mut banks = BankMap::new();
        banks.insert(lst_bank, mk_bank(lst_mint, 9, 0.8, 1.0));
        banks.insert(usdc_bank, mk_bank(usdc_mint, 6, 1.0, 1.1));
        let acct = MarginfiAccount {
            group: Pubkey::default(), authority: Pubkey::new_unique(),
            balances: vec![
                Balance { bank_pk: lst_bank, asset_shares: 10.0 * 1e9, liability_shares: 0.0 },
                Balance { bank_pk: usdc_bank, asset_shares: 0.0, liability_shares: 900.0 * 1e6 },
            ],
        };
        let mut baseline = PriceMap::new();
        baseline.insert(lst_bank, 130.0);
        baseline.insert(usdc_bank, 1.0);
        let mint_feed = HashMap::from([(lst_mint, 6u32)]);
        let direct = std::collections::HashSet::new(); // LST is not 1:1

        let lazer = HashMap::from([(6u32, 100.0)]);
        let wa = WatchAccount::build(&acct, &banks, &baseline, &mint_feed, &direct, &lazer);
        // At build-time prices the engine must agree with maintenance_health
        // at the ORACLE's $130 (assets 10×0.8×130 = $1040 vs liabs $990 →
        // healthy), not the raw feed's $100 (assets $800 → phantom-underwater).
        let reference = maintenance_health(&acct, &banks, &baseline).health;
        let h = wa.health(&lazer);
        assert!((h.weighted_assets - reference.weighted_assets).abs() < 1e-6,
            "assets {} vs {}", h.weighted_assets, reference.weighted_assets);
        assert!(!h.liquidatable(), "healthy LST account must not be flagged");
        // SOL −10% → LST tracks proportionally (130 × 0.9 = 117): 10×0.8×117
        // = $936 < $990 → now genuinely liquidatable.
        let dropped = wa.health(&HashMap::from([(6u32, 90.0)]));
        assert!((dropped.weighted_assets - 936.0).abs() < 1e-6);
        assert!(dropped.liquidatable());
    }

    #[test]
    fn incomplete_never_crosses() {
        let (mut acct, banks, baseline, mint_feed) = fixture();
        // Add a balance on an unknown bank → incomplete.
        acct.balances.push(Balance { bank_pk: Pubkey::new_unique(), asset_shares: 0.0, liability_shares: 1e6 });
        let wa = WatchAccount::build(&acct, &banks, &baseline, &mint_feed, &direct_of(&mint_feed), &HashMap::from([(6u32, 50.0)]));
        assert!(!wa.complete);
        let mut engine = Engine::new(50.0);
        engine.rebuild(&[(Pubkey::new_unique(), acct)], &banks, &baseline, &mint_feed,
            &direct_of(&mint_feed), &HashMap::from([(6u32, 50.0)]), 0.0);
        assert_eq!(engine.accounts.len(), 0); // incomplete filtered out
    }
}
