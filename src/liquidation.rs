//! marginfi v2 liquidation engine — Stage 1: read-only health finder.
//!
//! Decodes on-chain `Bank` and `MarginfiAccount` state, computes each
//! borrower's maintenance health, and flags who is liquidatable (weighted
//! assets < weighted liabilities). No money moves here — this is the finder
//! that feeds the liquidate-tx builder (Stage 2).
//!
//! Every byte offset below is VERIFIED: the Bank layout is checked field-by-
//! field against real mainnet USDC-bank bytes (share values, weights, oracle
//! setup all sane), and the MarginfiAccount/Balance layout is size-asserted
//! against the marginfi-v2 source (Balance=104, MarginfiAccount=2312,
//! Bank=1864) with the head (group@8, authority@40, lending_account@72)
//! confirmed on-chain. `WrappedI80F48` is a 16-byte LE i128 fixed-point
//! (value / 2^48), repr(C, align(8)).

use solana_pubkey::Pubkey;
use std::collections::HashMap;

/// Convert a WrappedI80F48 (16-byte LE i128, 48 fractional bits) to f64.
pub fn i80f48_to_f64(bytes: &[u8]) -> f64 {
    debug_assert!(bytes.len() >= 16);
    let mut buf = [0u8; 16];
    buf.copy_from_slice(&bytes[..16]);
    i128::from_le_bytes(buf) as f64 / (1u64 << 48) as f64
}

fn read_pubkey(data: &[u8], off: usize) -> Pubkey {
    let mut b = [0u8; 32];
    b.copy_from_slice(&data[off..off + 32]);
    Pubkey::new_from_array(b)
}

// ── Bank (VERIFIED against mainnet bytes; total account size 1864) ──────────
pub const BANK_DISC: [u8; 8] = [142, 49, 166, 242, 50, 66, 97, 188]; // sha256("account:Bank")[..8]

/// One emode (elevation-mode) rule on a *liability* bank: when this bank is
/// borrowed and the account holds collateral whose bank has `collateral_tag`,
/// the collateral's asset weights are REPLACED by these boosted values. VERIFIED
/// against USDC bank 2s37akK2 (tag 619 → maint 0.99, tag 871 → maint 0.92) which
/// is exactly what flips ratio-1.30 accounts healthy per marginfi.
#[derive(Clone, Copy, Debug)]
pub struct EmodeEntry {
    pub collateral_tag: u16,
    pub asset_weight_init: f64,
    pub asset_weight_maint: f64,
}

/// The risk parameters we need from a Bank to price a position's health.
#[derive(Clone, Debug)]
pub struct Bank {
    pub mint: Pubkey,
    pub mint_decimals: u8,
    pub asset_share_value: f64,
    pub liability_share_value: f64,
    pub asset_weight_init: f64,
    pub asset_weight_maint: f64,
    pub liability_weight_init: f64,
    pub liability_weight_maint: f64,
    /// OracleSetup enum: 1=PythLegacy, 2=SwitchboardV2, 3=PythPushOracle, …
    pub oracle_setup: u8,
    /// oracle_keys[0]: for Pyth Push this is the feed id / PriceUpdateV2 ref.
    pub oracle_key: Pubkey,
    /// This bank's emode class (0 = not emode-eligible as collateral). VERIFIED
    /// @920: Amtw3n7G→619, USDC/other stables→57481.
    pub emode_tag: u16,
    /// Boosts this bank grants to collateral when borrowed as a liability.
    pub emode_entries: Vec<EmodeEntry>,
}

// Emode layout in the Bank account (VERIFIED against mainnet USDC + collateral
// banks): the bank's own tag is a u16 @920; the boost-entry array starts @1264,
// each entry 40 bytes (collateral_tag u16 @0, asset_weight_init @8,
// asset_weight_maint @24). Unused/garbage slots are rejected by weight range.
const BANK_EMODE_TAG: usize = 920;
const BANK_EMODE_ENTRIES: usize = 1264;
const EMODE_ENTRY_SIZE: usize = 40;

impl Bank {
    pub fn decode(data: &[u8]) -> Option<Bank> {
        if data.len() < 1864 || data[..8] != BANK_DISC {
            return None;
        }
        // Emode entries: scan slots, keep only sane weight pairs (a real boost
        // has 0 < init ≤ maint ≤ 1.5; leftover/other-field bytes fail this).
        let mut emode_entries = Vec::new();
        let mut e = 0;
        while BANK_EMODE_ENTRIES + (e + 1) * EMODE_ENTRY_SIZE <= data.len() && e < 16 {
            let base = BANK_EMODE_ENTRIES + e * EMODE_ENTRY_SIZE;
            let tag = u16::from_le_bytes(data[base..base + 2].try_into().unwrap());
            let init = i80f48_to_f64(&data[base + 8..]);
            let maint = i80f48_to_f64(&data[base + 24..]);
            if tag != 0 && init > 0.0 && init <= maint && maint <= 1.5 {
                emode_entries.push(EmodeEntry { collateral_tag: tag, asset_weight_init: init, asset_weight_maint: maint });
            }
            e += 1;
        }
        Some(Bank {
            mint: read_pubkey(data, 8),
            mint_decimals: data[40],
            // group @41 (verified, unused here)
            asset_share_value: i80f48_to_f64(&data[80..]),
            liability_share_value: i80f48_to_f64(&data[96..]),
            // BankConfig @296
            asset_weight_init: i80f48_to_f64(&data[296..]),
            asset_weight_maint: i80f48_to_f64(&data[312..]),
            liability_weight_init: i80f48_to_f64(&data[328..]),
            liability_weight_maint: i80f48_to_f64(&data[344..]),
            oracle_setup: data[609],
            oracle_key: read_pubkey(data, 610),
            emode_tag: u16::from_le_bytes(data[BANK_EMODE_TAG..BANK_EMODE_TAG + 2].try_into().unwrap()),
            emode_entries,
        })
    }

    /// Boosted maint weight this liability bank grants to collateral of `tag`,
    /// if any. Used by the emode intersection rule.
    fn emode_boost(&self, tag: u16) -> Option<f64> {
        self.emode_entries.iter().find(|e| e.collateral_tag == tag).map(|e| e.asset_weight_maint)
    }
}

/// The effective maintenance asset weight for one collateral bank, applying
/// marginfi's emode rule: emode boosts the collateral's weight ONLY if the
/// collateral is emode-tagged AND *every* liability bank the account borrows
/// grants a boost for that tag (intersection); then the boost is the min across
/// those liabilities (most conservative). Otherwise the base maint weight.
/// Falls back to base weight whenever emode doesn't cleanly apply, so we
/// over-flag rather than under-flag (the sim gate is the fire backstop).
pub fn effective_asset_weight_maint(collateral: &Bank, liability_banks: &[&Bank]) -> f64 {
    let base = collateral.asset_weight_maint;
    if collateral.emode_tag == 0 || liability_banks.is_empty() {
        return base;
    }
    let mut boost = f64::INFINITY;
    for l in liability_banks {
        match l.emode_boost(collateral.emode_tag) {
            Some(w) => boost = boost.min(w),
            None => return base, // a borrowed liability doesn't grant emode → no boost
        }
    }
    // Emode replaces the base weight; guard against a pathological lower value.
    if boost.is_finite() { boost.max(base) } else { base }
}

// ── MarginfiAccount / Balance (size-asserted; head verified on-chain) ───────
pub const MARGINFI_ACCOUNT_DISC: [u8; 8] = [67, 178, 130, 109, 126, 114, 28, 42]; // sha256("account:MarginfiAccount")[..8]
pub const MA_SIZE: usize = 2312;
const MA_GROUP: usize = 8;
const MA_AUTHORITY: usize = 40;
const LENDING_ACCOUNT: usize = 72; // balances[0] start
const BALANCE_SIZE: usize = 104;
const MAX_BALANCES: usize = 16;
// within a Balance:
const BAL_ACTIVE: usize = 0;
const BAL_BANK_PK: usize = 1;
const BAL_ASSET_SHARES: usize = 40;
const BAL_LIABILITY_SHARES: usize = 56;

/// One active position slot on a MarginfiAccount (raw shares, not yet priced).
#[derive(Clone, Debug)]
pub struct Balance {
    pub bank_pk: Pubkey,
    pub asset_shares: f64,
    pub liability_shares: f64,
}

/// A decoded borrower: authority + its active balances.
#[derive(Clone, Debug)]
pub struct MarginfiAccount {
    pub group: Pubkey,
    pub authority: Pubkey,
    pub balances: Vec<Balance>,
}

impl MarginfiAccount {
    /// Decode balances from account data. Accepts either the full 2312-byte
    /// account or a leading dataSlice that covers all balances (>= 1736 bytes).
    pub fn decode(data: &[u8]) -> Option<MarginfiAccount> {
        if data.len() < LENDING_ACCOUNT + MAX_BALANCES * BALANCE_SIZE
            || data[..8] != MARGINFI_ACCOUNT_DISC
        {
            return None;
        }
        let mut balances = Vec::new();
        for i in 0..MAX_BALANCES {
            let base = LENDING_ACCOUNT + i * BALANCE_SIZE;
            // active flag is a u8 bool; skip empty slots.
            if data[base + BAL_ACTIVE] == 0 {
                continue;
            }
            let asset_shares = i80f48_to_f64(&data[base + BAL_ASSET_SHARES..]);
            let liability_shares = i80f48_to_f64(&data[base + BAL_LIABILITY_SHARES..]);
            if asset_shares == 0.0 && liability_shares == 0.0 {
                continue;
            }
            balances.push(Balance {
                bank_pk: read_pubkey(data, base + BAL_BANK_PK),
                asset_shares,
                liability_shares,
            });
        }
        Some(MarginfiAccount {
            group: read_pubkey(data, MA_GROUP),
            authority: read_pubkey(data, MA_AUTHORITY),
            balances,
        })
    }
}

// ── Pyth PriceUpdateV2 (on-chain pull oracle — what marginfi reads) ─────────
// disc(8) · write_authority(32)@8 · verification_level@40 · price_message · …
// `verification_level` is a Borsh enum: tag@40 is 1 for Full (1 byte total) or
// 0 for Partial (2 bytes: tag + num_signatures). So price_message starts at 41
// (Full) or 42 (Partial). VERIFIED against the USDC oracle (Full → $0.9998).
// price_message: feed_id(32) · price:i64 · conf:u64 · exponent:i32 · publish_time:i64.
pub const PRICE_UPDATE_V2_DISC: [u8; 8] = [34, 241, 35, 99, 157, 126, 244, 205]; // sha256("account:PriceUpdateV2")[..8]

/// Decode a Pyth PriceUpdateV2 account → (feed_id, usd_price, publish_time).
/// Price is scaled by its exponent to whole-token USD.
pub fn decode_price_update_v2(data: &[u8]) -> Option<([u8; 32], f64, i64)> {
    if data.len() < 134 || data[..8] != PRICE_UPDATE_V2_DISC {
        return None;
    }
    // Branch on verification_level tag to locate price_message.
    let pm = match data[40] {
        1 => 41, // Full
        0 => 42, // Partial (tag + num_signatures)
        _ => return None,
    };
    let mut feed = [0u8; 32];
    feed.copy_from_slice(&data[pm..pm + 32]);
    let price = i64::from_le_bytes(data[pm + 32..pm + 40].try_into().ok()?);
    let exponent = i32::from_le_bytes(data[pm + 48..pm + 52].try_into().ok()?);
    let publish_time = i64::from_le_bytes(data[pm + 52..pm + 60].try_into().ok()?);
    let usd = price as f64 * 10f64.powi(exponent);
    Some((feed, usd, publish_time))
}

// Switchboard On-Demand PullFeed (owner SBondMDrcV3K…, 3208 bytes). marginfi
// oracle_setup 4 (SwitchboardPull) and 7 use these. The feed result is an
// i128 fixed-point (÷ 1e18) at offset 56 — VERIFIED on-chain: SOL feed reads
// $80.98, a USDS feed $0.9999. disc = sha256("account:PullFeedAccountData")[..8].
pub const SWITCHBOARD_PULL_DISC: [u8; 8] = [0xc4, 0x1b, 0x6c, 0xc4, 0x0a, 0xd7, 0xdb, 0x28];
const SB_PULL_RESULT: usize = 56;

pub fn decode_switchboard_pull(data: &[u8]) -> Option<f64> {
    if data.len() < SB_PULL_RESULT + 16 || data[..8] != SWITCHBOARD_PULL_DISC {
        return None;
    }
    let v = i128::from_le_bytes(data[SB_PULL_RESULT..SB_PULL_RESULT + 16].try_into().ok()?);
    let usd = v as f64 / 1e18;
    (usd.is_finite() && usd > 0.0).then_some(usd)
}

/// USD price from any oracle account we can decode, dispatching on disc:
/// Pyth PriceUpdateV2 (setups 1/3/5/6) or Switchboard On-Demand PullFeed
/// (setups 4/7). Staked-SOL setups (5) resolve to the SOL Pyth feed, which
/// under-values the LST slightly — safe here because the finder only builds the
/// watch-set; the fire decision is gated by full on-chain simulation, which
/// uses marginfi's exact pricing.
pub fn decode_oracle_price(data: &[u8]) -> Option<f64> {
    decode_price_update_v2(data).map(|(_, usd, _)| usd).or_else(|| decode_switchboard_pull(data))
}

// ── Health ──────────────────────────────────────────────────────────────────

/// The USD price of one whole token for a bank's mint (mid; confidence bounds
/// are a later refinement). Keyed by bank pubkey.
pub type PriceMap = HashMap<Pubkey, f64>;
pub type BankMap = HashMap<Pubkey, Bank>;

#[derive(Clone, Copy, Debug)]
pub struct Health {
    pub weighted_assets: f64,
    pub weighted_liabilities: f64,
}

impl Health {
    /// Maintenance health value. Liquidatable when < 0.
    pub fn value(&self) -> f64 {
        self.weighted_assets - self.weighted_liabilities
    }
    pub fn liquidatable(&self) -> bool {
        self.value() < 0.0
    }
    /// weighted_liabs / weighted_assets — ratio >= 1.0 means underwater.
    pub fn ratio(&self) -> f64 {
        if self.weighted_assets == 0.0 {
            f64::INFINITY
        } else {
            self.weighted_liabilities / self.weighted_assets
        }
    }
}

/// Compute maintenance-weighted health for an account. `missing` counts
/// balances whose bank or price we couldn't resolve — if it's > 0 the health is
/// INCOMPLETE and must NOT be trusted for a liquidation decision (skipping an
/// unpriced collateral bank makes a healthy account look underwater).
#[derive(Clone, Copy, Debug)]
pub struct HealthResult {
    pub health: Health,
    pub missing: usize,
}

pub fn maintenance_health(acct: &MarginfiAccount, banks: &BankMap, prices: &PriceMap) -> HealthResult {
    // Liability banks of this account (needed for the emode intersection rule).
    let liab_banks: Vec<&Bank> = acct.balances.iter()
        .filter(|b| b.liability_shares > 0.0)
        .filter_map(|b| banks.get(&b.bank_pk))
        .collect();
    let mut wa = 0.0;
    let mut wl = 0.0;
    let mut missing = 0usize;
    for b in &acct.balances {
        let (Some(bank), Some(&price)) = (banks.get(&b.bank_pk), prices.get(&b.bank_pk)) else {
            missing += 1;
            continue;
        };
        let scale = 10f64.powi(bank.mint_decimals as i32);
        if b.asset_shares > 0.0 {
            let ui = b.asset_shares * bank.asset_share_value / scale;
            // Emode: collateral weight may be boosted by the borrowed liabilities.
            let w = effective_asset_weight_maint(bank, &liab_banks);
            wa += ui * price * w;
        }
        if b.liability_shares > 0.0 {
            let ui = b.liability_shares * bank.liability_share_value / scale;
            wl += ui * price * bank.liability_weight_maint;
        }
    }
    HealthResult { health: Health { weighted_assets: wa, weighted_liabilities: wl }, missing }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn i80f48_one() {
        let one = (1i128 << 48).to_le_bytes();
        assert!((i80f48_to_f64(&one) - 1.0).abs() < 1e-12);
    }

    #[test]
    fn health_liquidatable_when_liabs_exceed_assets() {
        let h = Health { weighted_assets: 100.0, weighted_liabilities: 101.0 };
        assert!(h.liquidatable());
        assert!(h.value() < 0.0);
        let h2 = Health { weighted_assets: 100.0, weighted_liabilities: 90.0 };
        assert!(!h2.liquidatable());
    }

    fn bank(tag: u16, base_maint: f64, entries: Vec<EmodeEntry>) -> Bank {
        Bank {
            mint: Pubkey::default(), mint_decimals: 6,
            asset_share_value: 1.0, liability_share_value: 1.0,
            asset_weight_init: base_maint, asset_weight_maint: base_maint,
            liability_weight_init: 1.05, liability_weight_maint: 1.05,
            oracle_setup: 3, oracle_key: Pubkey::default(),
            emode_tag: tag, emode_entries: entries,
        }
    }

    // Reproduces the verified mainnet case: collateral tag 619, base maint 0.65,
    // borrowing USDC which grants tag 619 → 0.99. The boost must apply.
    #[test]
    fn emode_boost_applies_with_matching_liability() {
        let collat = bank(619, 0.65, vec![]);
        let usdc = bank(57481, 1.0, vec![EmodeEntry { collateral_tag: 619, asset_weight_init: 0.94, asset_weight_maint: 0.99 }]);
        assert!((effective_asset_weight_maint(&collat, &[&usdc]) - 0.99).abs() < 1e-9);
    }

    #[test]
    fn emode_no_boost_without_matching_entry() {
        let collat = bank(871, 0.65, vec![]); // tag not offered by this liability
        let usdc = bank(57481, 1.0, vec![EmodeEntry { collateral_tag: 619, asset_weight_init: 0.94, asset_weight_maint: 0.99 }]);
        assert_eq!(effective_asset_weight_maint(&collat, &[&usdc]), 0.65);
    }

    // Intersection rule: emode applies only if EVERY borrowed liability grants it.
    #[test]
    fn emode_requires_all_liabilities_to_grant() {
        let collat = bank(619, 0.65, vec![]);
        let usdc = bank(57481, 1.0, vec![EmodeEntry { collateral_tag: 619, asset_weight_init: 0.94, asset_weight_maint: 0.99 }]);
        let other = bank(42, 1.0, vec![]); // second borrow with no emode → disqualifies
        assert_eq!(effective_asset_weight_maint(&collat, &[&usdc, &other]), 0.65);
    }

    #[test]
    fn emode_untagged_collateral_never_boosts() {
        let collat = bank(0, 0.65, vec![]);
        let usdc = bank(57481, 1.0, vec![EmodeEntry { collateral_tag: 0, asset_weight_init: 0.94, asset_weight_maint: 0.99 }]);
        assert_eq!(effective_asset_weight_maint(&collat, &[&usdc]), 0.65);
    }
}
