//! Kamino Lend (klend) liquidation finder — Stage 1, read-only.
//!
//! Unlike marginfi (where we decode Pyth oracles and compute health), Kamino's
//! `Obligation` STORES pre-computed USD health values as `Fraction` fixed-point
//! (u128, 60 fractional bits). So we read liquidatability straight from the
//! obligation — no oracle needed:
//!
//!     liquidatable  ⟺  borrow_factor_adjusted_debt_value ≥ unhealthy_borrow_value
//!
//! Caveat: these values are as of the obligation's last on-chain refresh; the
//! `stale` flag + last_update.slot tell us how fresh they are. Good enough for a
//! finder; a live trigger would re-price via Scope (Kamino's oracle, not Pyth).
//!
//! All offsets VERIFIED against a real 3344-byte main-market obligation: the
//! stored allowed/unhealthy values equal deposited_value × the init/liq LTVs
//! (0.80 / 0.90), which only holds if every offset is correct.

use solana_pubkey::Pubkey;
use std::collections::HashMap;

pub const KLEND_PROGRAM: &str = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD";
pub const KAMINO_MAIN_MARKET: &str = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF";

/// account:Obligation Anchor discriminator, and total account size.
pub const OBLIGATION_DISC: [u8; 8] = [168, 206, 141, 106, 88, 76, 172, 167];
pub const OBLIGATION_SIZE: usize = 3344;

/// account:Reserve Anchor discriminator, and total account size.
pub const RESERVE_DISC: [u8; 8] = [43, 242, 204, 202, 26, 247, 59, 127];
pub const RESERVE_SIZE: usize = 8624;

// Kamino `Fraction` = u128 with 60 fractional bits (value / 2^60).
const FRACTION_BITS: u32 = 60;

// VERIFIED offsets (offset 0 = 8-byte discriminator).
const O_LAST_UPDATE_SLOT: usize = 16;
const O_STALE: usize = 24;
const O_LENDING_MARKET: usize = 32;
const O_OWNER: usize = 64;
const O_DEPOSITS: usize = 96; // [ObligationCollateral; 8], 136B each
const O_DEPOSITED_VALUE: usize = 1192;
const O_BORROWS: usize = 1208; // [ObligationLiquidity; 5], 200B each
const O_BF_ADJ_DEBT_VALUE: usize = 2208;
const O_BORROWED_VALUE: usize = 2224;
const O_ALLOWED_BORROW_VALUE: usize = 2240;
const O_UNHEALTHY_BORROW_VALUE: usize = 2256;
const O_ELEVATION_GROUP: usize = 2285;
const O_HAS_DEBT: usize = 2287;
const COLLATERAL_STRIDE: usize = 136;
const COLL_DEPOSITED_AMOUNT: usize = 32; // u64 cTokens
const LIQUIDITY_STRIDE: usize = 200;
const LIQ_BORROWED_AMOUNT_SF: usize = 88; // u128 Fraction, native smallest units

fn frac_at(data: &[u8], off: usize) -> f64 {
    let mut b = [0u8; 16];
    b.copy_from_slice(&data[off..off + 16]);
    u128::from_le_bytes(b) as f64 / (2f64).powi(FRACTION_BITS as i32)
}

fn u64_at(data: &[u8], off: usize) -> u64 {
    u64::from_le_bytes(data[off..off + 8].try_into().unwrap())
}

fn pubkey_at(data: &[u8], off: usize) -> Pubkey {
    let mut b = [0u8; 32];
    b.copy_from_slice(&data[off..off + 32]);
    Pubkey::new_from_array(b)
}

/// A decoded Kamino borrower position with its stored USD health values.
#[derive(Clone, Debug)]
pub struct Obligation {
    pub owner: Pubkey,
    pub lending_market: Pubkey,
    pub last_update_slot: u64,
    pub stale: bool,
    /// Total deposited collateral value (USD) — what's seizable.
    pub deposited_value: f64,
    /// Borrow-factor-adjusted debt — the value compared against the threshold.
    pub bf_adjusted_debt: f64,
    /// Raw borrowed market value (USD).
    pub borrowed_value: f64,
    /// Borrow allowed at init LTV (USD).
    pub allowed_borrow_value: f64,
    /// Liquidation threshold (USD) — cross it and you're liquidatable.
    pub unhealthy_borrow_value: f64,
    /// elevation group (0 = none; nonzero overrides reserve LTV/liq/bf params).
    pub elevation_group: u8,
    /// Raw collateral positions: (deposit_reserve, deposited_amount cTokens).
    pub deposits: Vec<(Pubkey, u64)>,
    /// Raw debt positions: (borrow_reserve, borrowed_amount native smallest units).
    pub borrows: Vec<(Pubkey, f64)>,
}

impl Obligation {
    pub fn decode(data: &[u8]) -> Option<Obligation> {
        if data.len() < O_HAS_DEBT + 1 || data[..8] != OBLIGATION_DISC {
            return None;
        }
        // Raw positions (skip empty slots = zeroed reserve pubkey).
        let mut deposits = Vec::new();
        for i in 0..8 {
            let base = O_DEPOSITS + i * COLLATERAL_STRIDE;
            let reserve = pubkey_at(data, base);
            if reserve == Pubkey::default() { continue; }
            let amt = u64_at(data, base + COLL_DEPOSITED_AMOUNT);
            if amt == 0 { continue; }
            deposits.push((reserve, amt));
        }
        let mut borrows = Vec::new();
        for i in 0..5 {
            let base = O_BORROWS + i * LIQUIDITY_STRIDE;
            let reserve = pubkey_at(data, base);
            if reserve == Pubkey::default() { continue; }
            let amt = frac_at(data, base + LIQ_BORROWED_AMOUNT_SF); // native smallest units
            if amt == 0.0 { continue; }
            borrows.push((reserve, amt));
        }
        Some(Obligation {
            owner: pubkey_at(data, O_OWNER),
            lending_market: pubkey_at(data, O_LENDING_MARKET),
            last_update_slot: u64_at(data, O_LAST_UPDATE_SLOT),
            stale: data[O_STALE] != 0,
            deposited_value: frac_at(data, O_DEPOSITED_VALUE),
            bf_adjusted_debt: frac_at(data, O_BF_ADJ_DEBT_VALUE),
            borrowed_value: frac_at(data, O_BORROWED_VALUE),
            allowed_borrow_value: frac_at(data, O_ALLOWED_BORROW_VALUE),
            unhealthy_borrow_value: frac_at(data, O_UNHEALTHY_BORROW_VALUE),
            elevation_group: data[O_ELEVATION_GROUP],
            deposits,
            borrows,
        })
    }

    /// Kamino's own liquidation gate.
    pub fn liquidatable(&self) -> bool {
        self.bf_adjusted_debt >= self.unhealthy_borrow_value && self.unhealthy_borrow_value > 0.0
    }

    /// debt / threshold — ≥ 1.0 means liquidatable; how close otherwise.
    pub fn ratio(&self) -> f64 {
        if self.unhealthy_borrow_value == 0.0 {
            0.0
        } else {
            self.bf_adjusted_debt / self.unhealthy_borrow_value
        }
    }
}

// ── Reserve (8624 bytes) — cached price + params to recompute CURRENT health ──
// Offsets VERIFIED against the mainnet SOL reserve (price $82.20, dec 9).
const R_LAST_UPDATE_SLOT: usize = 16;
const R_STALE: usize = 24;
const R_AVAILABLE_AMOUNT: usize = 224; // u64 native
const R_BORROWED_AMOUNT_SF: usize = 232; // u128 Fraction native
const R_MARKET_PRICE_SF: usize = 248; // u128 Fraction USD/whole-token
const R_MINT_DECIMALS: usize = 272; // u64
const R_ACC_PROTOCOL_FEES_SF: usize = 344;
const R_ACC_REFERRER_FEES_SF: usize = 360;
const R_PENDING_REFERRER_FEES_SF: usize = 376;
const R_COLL_MINT_TOTAL_SUPPLY: usize = 2592; // u64 cToken supply
const R_LTV_PCT: usize = 4872; // u8
const R_LIQ_THRESHOLD_PCT: usize = 4873; // u8
const R_BORROW_FACTOR_PCT: usize = 5008; // u64

/// A reserve's cached price + params needed to value obligation positions.
#[derive(Clone, Debug)]
pub struct Reserve {
    pub mint_decimals: u32,
    /// USD per whole token (from the reserve's cached, refresh_reserve'd oracle).
    pub market_price: f64,
    pub price_slot: u64,
    pub price_stale: bool,
    /// underlying liquidity tokens per cToken (≥ 1, grows with interest).
    pub exchange_rate: f64,
    pub ltv_pct: u8,
    pub liq_threshold_pct: u8,
    pub borrow_factor_pct: u64,
}

impl Reserve {
    pub fn decode(data: &[u8]) -> Option<Reserve> {
        if data.len() < R_BORROW_FACTOR_PCT + 8 || data[..8] != RESERVE_DISC {
            return None;
        }
        // total_supply() (native units): available + borrowed − fees.
        let total_supply = u64_at(data, R_AVAILABLE_AMOUNT) as f64
            + frac_at(data, R_BORROWED_AMOUNT_SF)
            - frac_at(data, R_ACC_PROTOCOL_FEES_SF)
            - frac_at(data, R_ACC_REFERRER_FEES_SF)
            - frac_at(data, R_PENDING_REFERRER_FEES_SF);
        let ctoken_supply = u64_at(data, R_COLL_MINT_TOTAL_SUPPLY) as f64;
        let exchange_rate = if ctoken_supply > 0.0 && total_supply > 0.0 {
            total_supply / ctoken_supply
        } else {
            1.0 // INITIAL_COLLATERAL_RATE
        };
        Some(Reserve {
            mint_decimals: u64_at(data, R_MINT_DECIMALS) as u32,
            market_price: frac_at(data, R_MARKET_PRICE_SF),
            price_slot: u64_at(data, R_LAST_UPDATE_SLOT),
            price_stale: data[R_STALE] != 0,
            exchange_rate,
            ltv_pct: data[R_LTV_PCT],
            liq_threshold_pct: data[R_LIQ_THRESHOLD_PCT],
            borrow_factor_pct: u64_at(data, R_BORROW_FACTOR_PCT),
        })
    }
}

/// Health recomputed from CURRENT reserve prices (replicates refresh_obligation).
#[derive(Clone, Copy, Debug)]
pub struct Recomputed {
    pub deposited_value: f64,
    pub allowed_borrow_value: f64,
    pub unhealthy_borrow_value: f64,
    pub bf_adjusted_debt: f64,
    /// positions whose reserve we couldn't resolve — result is INCOMPLETE if > 0.
    pub missing: usize,
    /// obligation uses an elevation group → reserve-config LTV/liq/bf are WRONG here.
    pub elevation: bool,
    /// worst (oldest) reserve-price slot used — how fresh the recompute is.
    pub oldest_price_slot: u64,
}

impl Recomputed {
    pub fn liquidatable(&self) -> bool {
        self.bf_adjusted_debt >= self.unhealthy_borrow_value && self.unhealthy_borrow_value > 0.0
    }
    pub fn ratio(&self) -> f64 {
        if self.unhealthy_borrow_value == 0.0 { 0.0 } else { self.bf_adjusted_debt / self.unhealthy_borrow_value }
    }
    /// Trust only when fully priced and not elevation-group-dependent.
    pub fn trustworthy(&self) -> bool {
        self.missing == 0 && !self.elevation
    }
}

/// Recompute an obligation's health at current reserve prices. Caveat: uses the
/// stored borrowed_amount without re-accruing interest to the current slot
/// (slightly under-counts debt → conservative, won't false-positive from this).
pub fn recompute(ob: &Obligation, reserves: &HashMap<Pubkey, Reserve>) -> Recomputed {
    let mut deposited = 0.0;
    let mut allowed = 0.0;
    let mut unhealthy = 0.0;
    let mut bf_debt = 0.0;
    let mut missing = 0;
    let mut oldest = u64::MAX;
    for (res, camt) in &ob.deposits {
        let Some(r) = reserves.get(res) else { missing += 1; continue };
        oldest = oldest.min(r.price_slot);
        let underlying = *camt as f64 * r.exchange_rate;
        let val = underlying * r.market_price / 10f64.powi(r.mint_decimals as i32);
        deposited += val;
        allowed += val * r.ltv_pct as f64 / 100.0;
        unhealthy += val * r.liq_threshold_pct as f64 / 100.0;
    }
    for (res, bamt) in &ob.borrows {
        let Some(r) = reserves.get(res) else { missing += 1; continue };
        oldest = oldest.min(r.price_slot);
        let val = (*bamt / 10f64.powi(r.mint_decimals as i32)) * r.market_price;
        bf_debt += val * r.borrow_factor_pct as f64 / 100.0;
    }
    Recomputed {
        deposited_value: deposited,
        allowed_borrow_value: allowed,
        unhealthy_borrow_value: unhealthy,
        bf_adjusted_debt: bf_debt,
        missing,
        elevation: ob.elevation_group != 0,
        oldest_price_slot: if oldest == u64::MAX { 0 } else { oldest },
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn healthy_when_debt_below_threshold() {
        let o = Obligation {
            owner: Pubkey::default(), lending_market: Pubkey::default(),
            last_update_slot: 0, stale: false,
            deposited_value: 2513.33, bf_adjusted_debt: 900.49, borrowed_value: 720.39,
            allowed_borrow_value: 2010.66, unhealthy_borrow_value: 2262.0,
            elevation_group: 0, deposits: vec![], borrows: vec![],
        };
        assert!(!o.liquidatable());
        assert!(o.ratio() < 1.0);
    }

    #[test]
    fn liquidatable_when_debt_crosses() {
        let o = Obligation {
            owner: Pubkey::default(), lending_market: Pubkey::default(),
            last_update_slot: 0, stale: false,
            deposited_value: 1000.0, bf_adjusted_debt: 910.0, borrowed_value: 900.0,
            allowed_borrow_value: 800.0, unhealthy_borrow_value: 900.0,
            elevation_group: 0, deposits: vec![], borrows: vec![],
        };
        assert!(o.liquidatable());
        assert!(o.ratio() >= 1.0);
    }
}
