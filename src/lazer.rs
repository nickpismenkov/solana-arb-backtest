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

// Lazer numeric feed ids (VERIFIED live in pyth_probe: SOL=6, USDC=7, BTC=1, ETH=2).
pub const LAZER_SOL: u32 = 6;
pub const LAZER_BTC: u32 = 1;
pub const LAZER_ETH: u32 = 2;
pub const LAZER_USDC: u32 = 7;

/// The volatile majors worth arming on. Stables (USDC) included so a USDC-debt
/// account is fully priced by Lazer when its collateral is also a major.
pub fn arm_feed_ids() -> Vec<u32> {
    vec![LAZER_SOL, LAZER_BTC, LAZER_ETH, LAZER_USDC]
}

/// Mint → Lazer feed id for assets whose price Lazer leads. SOL-correlated LSTs
/// map to SOL (marginfi's staked-SOL banks price off the SOL feed too, so this
/// matches the on-chain valuation's leading edge). Extend as needed.
pub fn mint_feed_map() -> HashMap<Pubkey, u32> {
    let e = |s: &str, id: u32| (Pubkey::from_str(s).unwrap(), id);
    HashMap::from([
        e("So11111111111111111111111111111111111111112", LAZER_SOL), // wSOL
        e("mSoLzYCxHdYgdzU16g5QSh3i5K3z3KZK7ytfqcJm7So", LAZER_SOL),  // mSOL
        e("J1toso1uCk3RLmjorhTtrVwY9HJ7X8V9yYac6Y7kGCPn", LAZER_SOL), // jitoSOL
        e("bSo13r4TkiE4KumL71LsHTPpL2euBYLFx6h9HP3piy1", LAZER_SOL),  // bSOL
        e("7dHbWXmci3dT8UFYWYZweBLXgycu7Y3iL6trKn1Y7ARj", LAZER_SOL), // stSOL
        e("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", LAZER_USDC),// USDC
        e("cbbtcf3aa214zXHbiAZQwf4122FBYbraNdFqgw4iMij", LAZER_BTC),  // cbBTC
        e("3NZ9JMVBmGAqocybic2c7LQCJScmgsAZ6vQqTDzcqmJh", LAZER_BTC), // wBTC (Wormhole)
        e("7vfCXTUXx5WJV5JADk17DUJ4ksgau7utNKj4b963voxs", LAZER_ETH), // wETH (Wormhole)
    ])
}

/// Blend Lazer prices over an on-chain baseline: start from the on-chain
/// PriceMap (authoritative for anything Lazer doesn't cover) and override a
/// bank's price with the fresher Lazer tick when we have both a mint mapping
/// and a recent tick. Returns the blended map + how many banks Lazer led.
pub fn blend(
    banks: &BankMap,
    on_chain: &PriceMap,
    table: &PriceTable,
    map: &HashMap<Pubkey, u32>,
) -> (PriceMap, usize) {
    let mut out = on_chain.clone();
    let mut led = 0;
    for (bank_pk, bank) in banks {
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
    [f(LAZER_SOL, "SOL"), f(LAZER_BTC, "BTC"), f(LAZER_ETH, "ETH"), f(LAZER_USDC, "USDC")]
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
            oracle_setup: 3, oracle_key: Pubkey::default(),
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
