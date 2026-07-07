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
}

impl Bank {
    pub fn decode(data: &[u8]) -> Option<Bank> {
        if data.len() < 1864 || data[..8] != BANK_DISC {
            return None;
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
        })
    }
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
            wa += ui * price * bank.asset_weight_maint;
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
}
