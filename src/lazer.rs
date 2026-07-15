//! Pyth Lazer pre-positioning for the liquidation executors.
//!
//! Liquidation eligibility is gated by the protocol's ON-CHAIN oracle, so Lazer
//! can't make an account liquidatable sooner than the chain sees it. Its value
//! is a LEADING signal: Lazer ticks at ms latency, minutes ahead of the
//! reserve/bank oracle crank. Blending Lazer prices into the health recompute
//! lets the executor ARM (pre-select + keep hot) exactly the accounts about to
//! cross the threshold, so when the on-chain oracle catches up the fire tx is
//! already built and only needs sign+submit. The FIRE decision itself stays
//! gated by full on-chain simulation — Lazer never affects safety, only which
//! accounts we spend sim budget on.
//!
//! Feed ids are Lazer's numeric ids (SOL=6, BTC=1, ETH=2, USDC=7). Only the
//! volatile majors matter for arming — a borrower crosses the threshold when
//! its volatile collateral drops or volatile debt rises; stables don't move.

use crate::liquidation::{BankMap, PriceMap};
use crate::pyth::{self, PriceTable};
use solana_pubkey::Pubkey;
use std::collections::HashMap;
use std::str::FromStr;

// Lazer numeric feed ids (VERIFIED live in pyth_probe: SOL=6, USDC=7, BTC=1,
// ETH=2; the rest verified against the Lazer symbol registry
// history.pyth-lazer.dourolabs.app/history/v1/symbols).
pub const LAZER_SOL: u32 = 6;
pub const LAZER_BTC: u32 = 1;
pub const LAZER_ETH: u32 = 2;
pub const LAZER_USDC: u32 = 7;
pub const LAZER_USDT: u32 = 8;
pub const LAZER_BONK: u32 = 9;
pub const LAZER_WIF: u32 = 10;
pub const LAZER_PYTH: u32 = 3;
// JUP/W exist on Lazer but do NOT support the `real_time` channel — including
// them errors the ENTIRE subscription ("Feeds do not support channel
// real_time: 92, 102", verified live 2026-07-14) and the stream goes dark.
// Their banks stay baseline-priced; do not add them to arm_feed_ids unless
// the channel support changes or a separate fixed-rate subscription is wired.
pub const LAZER_JUP: u32 = 92;
pub const LAZER_W: u32 = 102;

/// Every feed the executors subscribe to and arm on. The list is CENSUS-DRIVEN:
/// a 7-day scan of landed marginfi liquidations (2026-07-14) showed BONK
/// collateral in 91% of them, with PYTH/WIF next among Lazer-covered assets —
/// while the old majors-only list (SOL/BTC/ETH/USDC) missed all of them
/// between 300s rescans. Stables (USDC/USDT) included so stable-debt accounts
/// are fully priced by Lazer. Known gaps (baseline-priced): HNT (no Lazer
/// feed), JUP/W (no real_time channel — see above).
pub fn arm_feed_ids() -> Vec<u32> {
    vec![LAZER_SOL, LAZER_BTC, LAZER_ETH, LAZER_USDC, LAZER_USDT,
         LAZER_BONK, LAZER_WIF, LAZER_PYTH]
}

/// Mint → Lazer feed id for assets whose price Lazer leads. SOL-correlated LSTs
/// map to SOL (their on-chain valuation moves with SOL, scaled by the LST
/// exchange rate — see `one_to_one_mints` for why that scale matters).
pub fn mint_feed_map() -> HashMap<Pubkey, u32> {
    let e = |s: &str, id: u32| (Pubkey::from_str(s).unwrap(), id);
    HashMap::from([
        e("So11111111111111111111111111111111111111112", LAZER_SOL), // wSOL
        e("mSoLzYCxHdYgdzU16g5QSh3i5K3z3KZK7ytfqcJm7So", LAZER_SOL),  // mSOL
        e("J1toso1uCk3RLmjorhTtrVwY9HJ7X8V9yYac6Y7kGCPn", LAZER_SOL), // jitoSOL
        e("bSo13r4TkiE4KumL71LsHTPpL2euBYLFx6h9HP3piy1", LAZER_SOL),  // bSOL
        e("7dHbWXmci3dT8UFYWYZweBLXgycu7Y3iL6trKn1Y7ARj", LAZER_SOL), // stSOL
        e("LSTxxxnJzKDFSLr4dUkPcmCf5VyryEqzPLz5j4bpxFp", LAZER_SOL),  // LST (Marinade)
        e("5oVNBeEEQvYi1cX3ir8Dx5n1P7pdxydbGF2X4TxVusJm", LAZER_SOL), // INF
        e("jupSoLaHXQiZZTSfEWMTRRgpnyFm8f6sZdosWBjx93v", LAZER_SOL),  // jupSOL
        e("he1iusmfkpAdwvxLNGV8Y1iSbj4rUy6yMhEA3fotn9A", LAZER_SOL),  // hSOL (Helius)
        e("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", LAZER_USDC),// USDC
        e("Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", LAZER_USDT),// USDT
        e("cbbtcf3aa214zXHbiAZQwf4122FBYbraNdFqgw4iMij", LAZER_BTC),  // cbBTC
        e("3NZ9JMVBmGAqocybic2c7LQCJScmgsAZ6vQqTDzcqmJh", LAZER_BTC), // wBTC (Wormhole)
        e("7vfCXTUXx5WJV5JADk17DUJ4ksgau7utNKj4b963voxs", LAZER_ETH), // wETH (Wormhole)
        e("DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263", LAZER_BONK),// BONK
        e("EKpQGSJtjMFqKZ9KQanSqYXRcF8fBopzLHYxdM65zcjm", LAZER_WIF), // WIF
        e("HZ1JovNiVvGrGNiiYvEozEVgZ58xaU3RKwX8eACQBCt3", LAZER_PYTH),// PYTH
        // JUP/W deliberately unmapped: their feeds aren't in arm_feed_ids (no
        // real_time channel), and a mapped-but-unsubscribed feed would leave
        // accounts permanently !feeds_ready instead of baseline-priced.
    ])
}

/// Mints whose on-chain price IS the feed price (1 token = 1 feed unit). LSTs
/// are deliberately absent: an LST is worth (exchange rate)× SOL — pricing it
/// at the RAW SOL feed undervalues the collateral by 15–35% and makes healthy
/// LST-collateral accounts look deep underwater (the phantom-candidate bug
/// found 2026-07-14). Consumers that substitute the feed price directly
/// (`blend`) must restrict themselves to this set; coefficient-based consumers
/// (liq_engine) anchor-scale mapped banks to the on-chain baseline instead.
pub fn one_to_one_mints() -> std::collections::HashSet<Pubkey> {
    [
        "So11111111111111111111111111111111111111112",  // wSOL
        "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", // USDC
        "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", // USDT
        "cbbtcf3aa214zXHbiAZQwf4122FBYbraNdFqgw4iMij",  // cbBTC
        "3NZ9JMVBmGAqocybic2c7LQCJScmgsAZ6vQqTDzcqmJh", // wBTC
        "7vfCXTUXx5WJV5JADk17DUJ4ksgau7utNKj4b963voxs", // wETH
        "DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263", // BONK
        "EKpQGSJtjMFqKZ9KQanSqYXRcF8fBopzLHYxdM65zcjm", // WIF
        "HZ1JovNiVvGrGNiiYvEozEVgZ58xaU3RKwX8eACQBCt3", // PYTH
    ].into_iter().map(|s| Pubkey::from_str(s).unwrap()).collect()
}

/// Blend Lazer prices over an on-chain baseline: start from the on-chain
/// PriceMap (authoritative for anything Lazer doesn't cover) and override a
/// bank's price with the fresher Lazer tick when we have both a mint mapping
/// and a recent tick. Only 1:1 mints are overridden — an LST priced at the raw
/// SOL feed would be undervalued by its exchange rate, so LST banks keep the
/// on-chain baseline here (the tick-driven engine anchor-scales them instead).
/// Returns the blended map + how many banks Lazer led.
pub fn blend(
    banks: &BankMap,
    on_chain: &PriceMap,
    table: &PriceTable,
    map: &HashMap<Pubkey, u32>,
) -> (PriceMap, usize) {
    let direct = one_to_one_mints();
    let mut out = on_chain.clone();
    let mut led = 0;
    for (bank_pk, bank) in banks {
        if !direct.contains(&bank.mint) { continue; }
        if let Some(feed) = map.get(&bank.mint) {
            if let Some(p) = pyth::get(table, *feed) {
                out.insert(*bank_pk, p.price);
                led += 1;
            }
        }
    }
    (out, led)
}

/// Run the Lazer WS feed on its own tokio runtime in a background thread,
/// writing into `table`. Lets a synchronous executor loop read fresh Lazer
/// prices without adopting an async runtime. Returns immediately.
pub fn spawn_lazer_thread(token: String, feed_ids: Vec<u32>, table: PriceTable) {
    std::thread::spawn(move || {
        let rt = match tokio::runtime::Runtime::new() {
            Ok(rt) => rt,
            Err(e) => { eprintln!("[lazer] runtime: {e}"); return; }
        };
        rt.block_on(async move {
            pyth::spawn_lazer(token, feed_ids, table);
            // Keep the runtime alive for the spawned task.
            loop { tokio::time::sleep(std::time::Duration::from_secs(3600)).await; }
        });
    });
}

/// Compact log line describing which Lazer majors are live (for boot output).
pub fn status(table: &PriceTable) -> String {
    let f = |id: u32, name: &str| pyth::get(table, id).map(|p| format!("{name}=${:.2}", p.price));
    let g = |id: u32, name: &str| pyth::get(table, id).map(|p| format!("{name}=${:.6}", p.price));
    [f(LAZER_SOL, "SOL"), f(LAZER_BTC, "BTC"), f(LAZER_ETH, "ETH"), f(LAZER_USDC, "USDC"),
     g(LAZER_BONK, "BONK"), f(LAZER_JUP, "JUP")]
        .into_iter().flatten().collect::<Vec<_>>().join(" ")
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::liquidation::Bank;

    fn bank(mint: &str) -> Bank {
        Bank {
            mint: Pubkey::from_str(mint).unwrap(), mint_decimals: 9,
            asset_share_value: 1.0, liability_share_value: 1.0,
            asset_weight_init: 0.8, asset_weight_maint: 0.9,
            liability_weight_init: 1.2, liability_weight_maint: 1.1,
            oracle_setup: 3, oracle_key: Pubkey::default(), oracle_max_age: 0,
            emode_tag: 0, emode_entries: vec![],
        }
    }

    #[test]
    fn lazer_overrides_on_chain_for_mapped_mint() {
        let sol_bank = Pubkey::new_unique();
        let mut banks: BankMap = HashMap::new();
        banks.insert(sol_bank, bank("So11111111111111111111111111111111111111112"));
        let mut on_chain: PriceMap = HashMap::new();
        on_chain.insert(sol_bank, 100.0); // stale on-chain price

        let table = pyth::new_table();
        table.write().unwrap().insert(LAZER_SOL, pyth::PricePoint { price: 92.5, ts_us: 1 });

        let (blended, led) = blend(&banks, &on_chain, &table, &mint_feed_map());
        assert_eq!(led, 1);
        assert_eq!(blended[&sol_bank], 92.5); // Lazer led the on-chain price
    }

    #[test]
    fn lst_bank_keeps_on_chain_baseline() {
        // mSOL is mapped to the SOL feed but is NOT 1:1 — blending the raw SOL
        // price would undervalue it by the exchange rate. It must keep baseline.
        let msol_bank = Pubkey::new_unique();
        let mut banks: BankMap = HashMap::new();
        banks.insert(msol_bank, bank("mSoLzYCxHdYgdzU16g5QSh3i5K3z3KZK7ytfqcJm7So"));
        let mut on_chain: PriceMap = HashMap::new();
        on_chain.insert(msol_bank, 123.0); // oracle values the LST above raw SOL

        let table = pyth::new_table();
        table.write().unwrap().insert(LAZER_SOL, pyth::PricePoint { price: 92.5, ts_us: 1 });

        let (blended, led) = blend(&banks, &on_chain, &table, &mint_feed_map());
        assert_eq!(led, 0);
        assert_eq!(blended[&msol_bank], 123.0);
    }

    #[test]
    fn unmapped_bank_keeps_on_chain() {
        let odd = Pubkey::new_unique();
        let mut banks: BankMap = HashMap::new();
        banks.insert(odd, bank("2b1kV6Dku6WsWyBxJSKR14pC7sURHfWGrChjjBWY6vfz")); // not mapped
        let mut on_chain: PriceMap = HashMap::new();
        on_chain.insert(odd, 3.5);
        let table = pyth::new_table();
        let (blended, led) = blend(&banks, &on_chain, &table, &mint_feed_map());
        assert_eq!(led, 0);
        assert_eq!(blended[&odd], 3.5);
    }
}
