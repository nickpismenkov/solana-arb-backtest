//! Kamino atomic liquidation FIRE path — one flashloan-wrapped v0 tx:
//!
//!   [cu, cu_price, create ATAs, JupLend borrow USDC,
//!    refresh_reserve(repay), refresh_reserve(withdraw), refresh_obligation,
//!    liquidate_and_redeem_v2, Jupiter swap seized→USDC, JupLend payback, tip]
//!
//! Profit-or-revert, NO external capital: the flash-borrowed USDC repays the
//! obligation's debt inside `liquidate`, which seizes discounted collateral and
//! redeems it to the underlying liquidity token. The swap turns that back into
//! USDC; the fixed-amount JupLend payback then fails unless the swap produced
//! at least the borrowed amount — so a landed tx is always net-positive (the
//! liquidation bonus), and an unprofitable one reverts for just the base fee.
//!
//! v1 restriction: the debt (repay reserve's liquidity) must be USDC — that's
//! what JupLend flash-borrows and what the swap targets.

use crate::arb::{cu_limit_ix, cu_price_ix, transfer_ix};
use crate::flashloan::{ata_for, borrow_usdc, create_ata_idempotent_for, payback_usdc};
use crate::jup;
use crate::kamino_ix::{self, ReserveAccounts};
use anyhow::{anyhow, Result};
use solana_hash::Hash;
use solana_message::{v0, VersionedMessage};
use solana_pubkey::Pubkey;
use solana_transaction::versioned::VersionedTransaction;
use std::str::FromStr;

pub const FIRE_CU_LIMIT: u32 = 1_400_000;
pub const USDC_MINT: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const TOKEN_PROGRAM: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";

/// Dedicated Kamino liquidation ALT (main-market static accounts + programs).
/// Override via KAMINO_ALT. Set after liq_kamino_alt_print + on-chain create.
pub const KAMINO_ALT: &str = "6X77KtDupVYqU4SBjWsY93ycFW2bPm3AWpAuPWfxraKo";

pub struct KaminoFireCandidate {
    pub obligation: Pubkey,
    pub lending_market: Pubkey,
    pub repay_reserve: ReserveAccounts,
    pub withdraw_reserve: ReserveAccounts,
    /// obligation's reserves in refresh order (deposits then borrows).
    pub obligation_reserves: Vec<Pubkey>,
    /// seized-collateral underlying mint (= withdraw reserve liquidity) + its program.
    pub withdraw_liquidity_mint: Pubkey,
    pub withdraw_liquidity_token_program: Pubkey,
    pub withdraw_collateral_token_program: Pubkey,
    /// repay side token program (USDC = classic SPL in v1).
    pub repay_liquidity_token_program: Pubkey,
    /// USDC to flash-borrow and repay into the obligation (the close amount).
    pub repay_amount: u64,
    /// Native underlying units to swap → USDC. Computed by the caller from the
    /// seized-collateral value and underlying price (with a haircut so the
    /// ExactIn never exceeds the redeemed balance); dust stays in the ATA.
    pub swap_in_amount: u64,
}

pub struct KaminoFireTx {
    pub tx: VersionedTransaction,
    pub quoted_usdc_out: u64,
    pub tx_bytes: usize,
}

/// Build the unsigned Kamino fire tx. Quotes the seized-underlying→USDC swap
/// live (Jupiter), so call only for a sim-confirmed candidate.
#[allow(clippy::too_many_arguments)]
pub fn build_fire_tx(
    rpc_endpoint: &str,
    c: &KaminoFireCandidate,
    authority: &Pubkey,
    tip_account: Option<Pubkey>,
    tip_lamports: u64,
    priority_micro_lamports: u64,
    slippage_bps: u32,
    max_swap_accounts: usize,
    blockhash: Hash,
) -> Result<KaminoFireTx> {
    let usdc = Pubkey::from_str(USDC_MINT).unwrap();
    let usdc_tp = Pubkey::from_str(TOKEN_PROGRAM).unwrap();
    if c.repay_reserve.liquidity_mint != usdc {
        return Err(anyhow!("v1 Kamino fire requires USDC debt, got {}", c.repay_reserve.liquidity_mint));
    }

    // ATAs: USDC (borrow dest + repay source + swap out), seized underlying
    // (swap in), collateral cToken (transient redeem target).
    let usdc_ata = ata_for(authority, &usdc, &usdc_tp);
    let seized_ata = ata_for(authority, &c.withdraw_liquidity_mint, &c.withdraw_liquidity_token_program);
    let coll_ata = ata_for(authority, &c.withdraw_reserve.collateral_mint, &c.withdraw_collateral_token_program);

    // Swap the redeemed underlying → USDC. swap_in_amount is the caller's
    // native-unit estimate of the seized collateral (with a haircut so the
    // ExactIn stays within the redeemed balance). The fixed payback is the
    // real profit-or-revert guard regardless of any swap-sizing slack.
    let quote = jup::quote(&c.withdraw_liquidity_mint, &usdc, c.swap_in_amount, slippage_bps, max_swap_accounts)?;
    let plan = jup::swap_instructions(&quote, authority, false)?;

    let mut alt_addrs = plan.alt_addresses.clone();
    if let Ok(a) = std::env::var("KAMINO_ALT").or_else(|_| if KAMINO_ALT.is_empty() { Err(std::env::VarError::NotPresent) } else { Ok(KAMINO_ALT.to_string()) }) {
        if let Ok(pk) = Pubkey::from_str(&a) { alt_addrs.push(pk); }
    }
    let alts = jup::fetch_alts(rpc_endpoint, &alt_addrs)?;

    let mut ixs = vec![
        cu_limit_ix(FIRE_CU_LIMIT),
        cu_price_ix(priority_micro_lamports),
        create_ata_idempotent_for(authority, &usdc, &usdc_tp),
        create_ata_idempotent_for(authority, &c.withdraw_liquidity_mint, &c.withdraw_liquidity_token_program),
        create_ata_idempotent_for(authority, &c.withdraw_reserve.collateral_mint, &c.withdraw_collateral_token_program),
        borrow_usdc(authority, c.repay_amount),
        kamino_ix::refresh_reserve(&c.repay_reserve),
        kamino_ix::refresh_reserve(&c.withdraw_reserve),
        kamino_ix::refresh_obligation(&c.lending_market, &c.obligation, &c.obligation_reserves),
        kamino_ix::liquidate_and_redeem_v2(
            authority, &c.obligation, &c.lending_market, &c.repay_reserve, &c.withdraw_reserve,
            &coll_ata, &seized_ata, &usdc_ata,
            &c.withdraw_collateral_token_program, &c.repay_liquidity_token_program, &c.withdraw_liquidity_token_program,
            c.repay_amount, 0, 0,
        ),
    ];
    ixs.extend(plan.instructions);
    // Fixed-amount payback = the guard: reverts unless the swap covered it.
    ixs.push(payback_usdc(authority, c.repay_amount));
    if let (Some(tip_to), true) = (tip_account, tip_lamports > 0) {
        ixs.push(transfer_ix(*authority, tip_to, tip_lamports));
    }

    let msg = v0::Message::try_compile(authority, &ixs, &alts, blockhash)
        .map_err(|e| anyhow!("compile v0: {e}"))?;
    let tx = VersionedTransaction {
        signatures: vec![solana_signature::Signature::default()],
        message: VersionedMessage::V0(msg),
    };
    let tx_bytes = bincode::serialize(&tx)?.len();
    Ok(KaminoFireTx { tx, quoted_usdc_out: plan.quoted_out, tx_bytes })
}
