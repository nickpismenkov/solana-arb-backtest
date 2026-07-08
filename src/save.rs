//! Save (formerly Solend) liquidation data layer + instruction builders.
//!
//! Save is the original SPL token-lending model — a NATIVE program, so each
//! instruction is a one-byte tag (not an Anchor discriminator). Every layout
//! below is derived from CAPTURED mainnet truth (the marginfi/Kamino lesson):
//! the Reserve/Obligation packed layouts are cross-checked against the canonical
//! Solend source AND real on-chain accounts, and the liquidate/refresh account
//! orders are taken verbatim from real liquidation txs
//! (4tQm9zcd… and 2inNexup…, both identical).
//!
//! ★ Save's USDC reserve reads the SAME Pyth sponsored feed (Dpw1EAVr…) that our
//! self-crank pipeline already refreshes — so the crank front-run edge applies
//! here too on Pyth-priced collateral, with no extra crank work.

use solana_instruction::{AccountMeta, Instruction};
use solana_pubkey::Pubkey;
use std::str::FromStr;

pub const SOLEND_PROGRAM: &str = "So1endDq2YkqhipRh3WViPa8hdiSpxWy6z3Z6tMCpAo";
pub const MAIN_POOL: &str = "4UpD2fh7xH3VP9QQaXtsS1YY3bxzWhtfpks7FatyKvdY";
pub const USDC_RESERVE: &str = "BgxfHJDzm44T7XG68MYKx7YisTjZu73tVovyZSjJMpmw";
pub const USDC_MINT: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const TOKEN_PROGRAM: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";

// Instruction tags (Solend LendingInstruction enum).
const TAG_REFRESH_RESERVE: u8 = 3;
const TAG_REFRESH_OBLIGATION: u8 = 7;
const TAG_LIQUIDATE_AND_REDEEM: u8 = 17; // LiquidateObligationAndRedeemReserveCollateral

fn pk(s: &str) -> Pubkey { Pubkey::from_str(s).unwrap() }
fn read_pk(d: &[u8], o: usize) -> Option<Pubkey> { Some(Pubkey::new_from_array(d.get(o..o + 32)?.try_into().ok()?)) }
fn u64le(d: &[u8], o: usize) -> Option<u64> { Some(u64::from_le_bytes(d.get(o..o + 8)?.try_into().ok()?)) }
/// Solend Decimal / scaled values are u128 with WAD = 1e18.
fn wad(d: &[u8], o: usize) -> Option<f64> {
    Some(u128::from_le_bytes(d.get(o..o + 16)?.try_into().ok()?) as f64 / 1e18)
}

// ── Reserve (619 bytes, layout from Solend reserve.rs Pack + verified on the
//    USDC reserve). ───────────────────────────────────────────────────────────
const R_LENDING_MARKET: usize = 10;
const R_LIQ_MINT: usize = 42;
const R_MINT_DECIMALS: usize = 74;
const R_LIQ_SUPPLY: usize = 75;
const R_PYTH_ORACLE: usize = 107;
const R_SB_ORACLE: usize = 139;
const R_MARKET_PRICE: usize = 211; // Decimal (WAD): USD price per whole token
const R_COLL_MINT: usize = 227;
const R_COLL_SUPPLY: usize = 267;
const R_LTV: usize = 300;
const R_LIQ_BONUS: usize = 301;
const R_LIQ_THRESHOLD: usize = 302;
const R_FEE_RECEIVER: usize = 339;

/// Every account a refresh/liquidate touches for one reserve, pulled from the
/// reserve bytes — mirrors Kamino's ReserveAccounts pattern.
#[derive(Clone, Debug)]
pub struct Reserve {
    pub reserve: Pubkey,
    pub lending_market: Pubkey,
    pub liquidity_mint: Pubkey,
    pub mint_decimals: u8,
    pub liquidity_supply: Pubkey,
    pub pyth_oracle: Pubkey,
    pub switchboard_oracle: Pubkey,
    pub collateral_mint: Pubkey,
    pub collateral_supply: Pubkey,
    pub fee_receiver: Pubkey,
    pub market_price: f64,
    pub liquidation_threshold_pct: u8,
    pub liquidation_bonus_pct: u8,
    pub loan_to_value_pct: u8,
}

impl Reserve {
    pub fn decode(reserve: Pubkey, d: &[u8]) -> Option<Reserve> {
        if d.len() < 619 || d[0] != 1 { return None; }
        Some(Reserve {
            reserve,
            lending_market: read_pk(d, R_LENDING_MARKET)?,
            liquidity_mint: read_pk(d, R_LIQ_MINT)?,
            mint_decimals: d[R_MINT_DECIMALS],
            liquidity_supply: read_pk(d, R_LIQ_SUPPLY)?,
            pyth_oracle: read_pk(d, R_PYTH_ORACLE)?,
            switchboard_oracle: read_pk(d, R_SB_ORACLE)?,
            collateral_mint: read_pk(d, R_COLL_MINT)?,
            collateral_supply: read_pk(d, R_COLL_SUPPLY)?,
            fee_receiver: read_pk(d, R_FEE_RECEIVER)?,
            market_price: wad(d, R_MARKET_PRICE)?,
            loan_to_value_pct: d[R_LTV],
            liquidation_bonus_pct: d[R_LIQ_BONUS],
            liquidation_threshold_pct: d[R_LIQ_THRESHOLD],
        })
    }
}

// ── Obligation (1300 bytes, layout from Solend obligation.rs Pack + verified
//    on a real main-pool obligation). ────────────────────────────────────────
const O_LENDING_MARKET: usize = 10;
const O_OWNER: usize = 42;
const O_DEPOSITED_VALUE: usize = 74;
const O_BORROWED_VALUE: usize = 90;
const O_UNHEALTHY_BORROW_VALUE: usize = 122;
const O_DEPOSITS_LEN: usize = 202;
const O_BORROWS_LEN: usize = 203;
const O_DATA_FLAT: usize = 204;
const COLLATERAL_LEN: usize = 88;  // reserve(32) + deposited_amount u64(8) + market_value(16) + pad(32)
const LIQUIDITY_LEN: usize = 112;  // reserve(32) + cum_rate(16) + borrowed_wads(16) + market_value(16) + pad(32)

#[derive(Clone, Debug)]
pub struct Deposit {
    pub reserve: Pubkey,
    /// cToken (collateral) amount deposited.
    pub deposited_amount: u64,
    pub market_value: f64,
}
#[derive(Clone, Debug)]
pub struct Borrow {
    pub reserve: Pubkey,
    pub borrowed_amount_wads: f64,
    pub market_value: f64,
}

#[derive(Clone, Debug)]
pub struct Obligation {
    pub lending_market: Pubkey,
    pub owner: Pubkey,
    pub deposited_value: f64,
    pub borrowed_value: f64,
    pub unhealthy_borrow_value: f64,
    pub deposits: Vec<Deposit>,
    pub borrows: Vec<Borrow>,
}

impl Obligation {
    pub fn decode(d: &[u8]) -> Option<Obligation> {
        if d.len() < 1300 || d[0] != 1 { return None; }
        let n_dep = d[O_DEPOSITS_LEN] as usize;
        let n_bor = d[O_BORROWS_LEN] as usize;
        let mut deposits = Vec::with_capacity(n_dep);
        let mut off = O_DATA_FLAT;
        for _ in 0..n_dep {
            deposits.push(Deposit {
                reserve: read_pk(d, off)?,
                deposited_amount: u64le(d, off + 32)?,
                market_value: wad(d, off + 40)?,
            });
            off += COLLATERAL_LEN;
        }
        let mut borrows = Vec::with_capacity(n_bor);
        for _ in 0..n_bor {
            borrows.push(Borrow {
                reserve: read_pk(d, off)?,
                borrowed_amount_wads: wad(d, off + 48)?,
                market_value: wad(d, off + 64)?,
            });
            off += LIQUIDITY_LEN;
        }
        Some(Obligation {
            lending_market: read_pk(d, O_LENDING_MARKET)?,
            owner: read_pk(d, O_OWNER)?,
            deposited_value: wad(d, O_DEPOSITED_VALUE)?,
            borrowed_value: wad(d, O_BORROWED_VALUE)?,
            unhealthy_borrow_value: wad(d, O_UNHEALTHY_BORROW_VALUE)?,
            deposits,
            borrows,
        })
    }

    /// Liquidatable per Solend's own on-chain math: borrowed value has crossed
    /// the (deposit-weighted) unhealthy threshold. Both fields are refreshed
    /// on-chain, so this is the protocol's verdict — the fire is still sim-gated.
    pub fn liquidatable(&self) -> bool {
        self.unhealthy_borrow_value > 0.0 && self.borrowed_value > self.unhealthy_borrow_value
    }
    /// How far over the threshold (>1.0 = underwater), for ranking.
    pub fn health_ratio(&self) -> f64 {
        if self.unhealthy_borrow_value == 0.0 { 0.0 } else { self.borrowed_value / self.unhealthy_borrow_value }
    }
}

/// lending_market_authority PDA — seed = the lending market pubkey (VERIFIED:
/// derives DdZR6zR… for the main pool, matching the captured liquidation tx).
pub fn lending_market_authority(lending_market: &Pubkey) -> Pubkey {
    Pubkey::find_program_address(&[lending_market.as_ref()], &pk(SOLEND_PROGRAM)).0
}

// ── Instruction builders (tags + account orders from the captured txs) ────────

/// refresh_reserve (tag 3): [reserve(w), pyth_oracle, switchboard_oracle].
pub fn refresh_reserve(r: &Reserve) -> Instruction {
    Instruction {
        program_id: pk(SOLEND_PROGRAM),
        accounts: vec![
            AccountMeta::new(r.reserve, false),
            AccountMeta::new_readonly(r.pyth_oracle, false),
            AccountMeta::new_readonly(r.switchboard_oracle, false),
        ],
        data: vec![TAG_REFRESH_RESERVE],
    }
}

/// refresh_obligation (tag 7): [obligation(w), then each deposit reserve, then
/// each borrow reserve — in obligation order].
pub fn refresh_obligation(obligation: &Pubkey, deposit_reserves: &[Pubkey], borrow_reserves: &[Pubkey]) -> Instruction {
    let mut accounts = vec![AccountMeta::new(*obligation, false)];
    for r in deposit_reserves.iter().chain(borrow_reserves.iter()) {
        accounts.push(AccountMeta::new(*r, false));
    }
    Instruction { program_id: pk(SOLEND_PROGRAM), accounts, data: vec![TAG_REFRESH_OBLIGATION] }
}

/// liquidate_obligation_and_redeem_reserve_collateral (tag 17). Repays
/// `liquidity_amount` of the borrow (USDC) and seizes+redeems the withdraw
/// reserve's collateral to underlying, into the liquidator's accounts. Account
/// order is verbatim from the captured txs (15 accounts).
#[allow(clippy::too_many_arguments)]
pub fn liquidate_and_redeem(
    liquidity_amount: u64,
    source_liquidity: &Pubkey,      // liquidator USDC (repay)
    dest_collateral: &Pubkey,       // liquidator cToken ATA
    dest_liquidity: &Pubkey,        // liquidator underlying ATA (redeemed collateral)
    repay_reserve: &Reserve,        // the borrow (USDC) reserve
    withdraw_reserve: &Reserve,     // the collateral reserve being seized
    obligation: &Pubkey,
    lending_market: &Pubkey,
    user_transfer_authority: &Pubkey, // signer
) -> Instruction {
    let mut data = Vec::with_capacity(9);
    data.push(TAG_LIQUIDATE_AND_REDEEM);
    data.extend_from_slice(&liquidity_amount.to_le_bytes());
    Instruction {
        program_id: pk(SOLEND_PROGRAM),
        accounts: vec![
            AccountMeta::new(*source_liquidity, false),                 // 0
            AccountMeta::new(*dest_collateral, false),                  // 1
            AccountMeta::new(*dest_liquidity, false),                   // 2
            AccountMeta::new(repay_reserve.reserve, false),             // 3
            AccountMeta::new(repay_reserve.liquidity_supply, false),    // 4
            AccountMeta::new(withdraw_reserve.reserve, false),          // 5
            AccountMeta::new(withdraw_reserve.collateral_mint, false),  // 6
            AccountMeta::new(withdraw_reserve.collateral_supply, false),// 7
            AccountMeta::new(withdraw_reserve.liquidity_supply, false), // 8
            AccountMeta::new(withdraw_reserve.fee_receiver, false),     // 9
            AccountMeta::new(*obligation, false),                       // 10
            AccountMeta::new_readonly(*lending_market, false),          // 11
            AccountMeta::new_readonly(lending_market_authority(lending_market), false), // 12
            AccountMeta::new(*user_transfer_authority, true),           // 13 signer
            AccountMeta::new_readonly(pk(TOKEN_PROGRAM), false),        // 14
        ],
        data,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn lending_market_authority_matches_captured_tx() {
        // Captured liquidation txs had lending_market_authority = DdZR6zR… for
        // the main pool. This pins the PDA seed derivation.
        assert_eq!(
            lending_market_authority(&pk(MAIN_POOL)).to_string(),
            "DdZR6zRFiUt4S5mg7AV1uKB2z1f1WzcNYCaTEEWPAuby"
        );
    }

    #[test]
    fn liquidate_ix_shape() {
        let dummy = |seed: &str| Reserve {
            reserve: pk(seed), lending_market: pk(MAIN_POOL), liquidity_mint: pk(USDC_MINT),
            mint_decimals: 6, liquidity_supply: pk(USDC_MINT), pyth_oracle: pk(USDC_MINT),
            switchboard_oracle: pk(USDC_MINT), collateral_mint: pk(USDC_MINT), collateral_supply: pk(USDC_MINT),
            fee_receiver: pk(USDC_MINT), market_price: 1.0, liquidation_threshold_pct: 77,
            liquidation_bonus_pct: 3, loan_to_value_pct: 70,
        };
        let ix = liquidate_and_redeem(
            1_000_000, &pk(USDC_MINT), &pk(USDC_MINT), &pk(USDC_MINT),
            &dummy(USDC_RESERVE), &dummy(USDC_RESERVE), &pk(MAIN_POOL), &pk(MAIN_POOL), &pk(USDC_MINT));
        assert_eq!(ix.accounts.len(), 15);
        assert_eq!(ix.data[0], TAG_LIQUIDATE_AND_REDEEM);
        assert_eq!(u64::from_le_bytes(ix.data[1..9].try_into().unwrap()), 1_000_000);
        assert!(ix.accounts[13].is_signer);
    }
}
