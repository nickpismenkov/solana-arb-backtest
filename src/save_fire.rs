//! The atomic Save (Solend) liquidation FIRE path — one flash-loan-wrapped v0 tx:
//!
//!   [cu_limit, cu_price, create ATAs,
//!    marginfi start_flashloan → borrow_usdc  (capital, no inventory)
//!    save refresh_reserve(repay=USDC) · refresh_reserve(withdraw=collateral)
//!      · refresh_obligation
//!    save liquidate_obligation_and_redeem  (repay USDC, seize collateral,
//!      redeem cTokens → underlying into our ATA)
//!    Jupiter swap collateral-underlying → USDC
//!    marginfi payback_usdc (repay the flash loan) · end_flashloan
//!    tip]
//!
//! We wrap in MARGINFI's flash loan (not Solend's own) for two reasons: it
//! reuses the tested marginfi flashloan path, and it avoids Solend's flash-loan
//! reentrancy guard — our liquidate repays into the same USDC reserve we'd have
//! borrowed from, which a Solend flash loan forbids between borrow/repay.
//!
//! Profit-or-revert with NO capital: `payback_usdc(repay_all)` fails unless the
//! swap produced enough USDC to cover the borrowed amount, and `end_flashloan`
//! re-checks health — either the whole tx lands net-positive (the ~liq_bonus%
//! surplus USDC stays in the wallet ATA) or it reverts for just the base fee.

use crate::arb::{cu_limit_ix, cu_price_ix, transfer_ix};
use crate::flashloan::{ata_for, create_ata_idempotent_for};
use crate::jup;
use crate::marginfi;
use crate::save::{self, Reserve};
use anyhow::{anyhow, Result};
use solana_hash::Hash;
use solana_message::{v0, VersionedMessage};
use solana_pubkey::Pubkey;
use solana_transaction::versioned::VersionedTransaction;
use std::str::FromStr;

pub const FIRE_CU_LIMIT: u32 = 1_400_000;

/// Dedicated ALT holding the fixed Solend + marginfi-flashloan accounts common
/// to every Save-USDC liquidation (create with save_alt_print, analogous to
/// LIQ_ALT). Override via SAVE_ALT.
pub const SAVE_ALT: &str = "11111111111111111111111111111111"; // placeholder until created on-chain

/// One Save liquidation opportunity, sized by the caller (via simulation).
pub struct SaveFireCandidate {
    pub obligation: Pubkey,
    /// The borrow (USDC) reserve being repaid.
    pub repay_reserve: Reserve,
    /// The collateral reserve being seized (its liquidity mint is what we swap).
    pub withdraw_reserve: Reserve,
    pub collateral_token_program: Pubkey,
    /// USDC debt to repay (native, 6dp).
    pub repay_amount: u64,
    /// Expected collateral-underlying out of liquidate+redeem, to size the swap.
    pub seize_underlying: u64,
    /// The obligation's deposit + borrow reserves, in obligation order, for
    /// refresh_obligation.
    pub deposit_reserves: Vec<Pubkey>,
    pub borrow_reserves: Vec<Pubkey>,
}

pub struct SaveFireTx {
    pub tx: VersionedTransaction,
    pub quoted_usdc_out: u64,
    pub tx_bytes: usize,
}

/// Build the unsigned Save fire tx. Quotes the collateral→USDC swap live, so
/// call only for a sim-confirmed candidate. `blockhash` = real recent hash for
/// live submission, or default for replace-blockhash simulation.
#[allow(clippy::too_many_arguments)]
pub fn build_save_fire_tx(
    rpc_endpoint: &str,
    c: &SaveFireCandidate,
    liquidator_ma: &Pubkey,
    authority: &Pubkey,
    tip_account: Option<Pubkey>,
    tip_lamports: u64,
    priority_micro_lamports: u64,
    slippage_bps: u32,
    max_swap_accounts: usize,
    blockhash: Hash,
) -> Result<SaveFireTx> {
    let usdc = Pubkey::from_str(save::USDC_MINT).unwrap();
    let token_program = Pubkey::from_str("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA").unwrap();
    if c.repay_reserve.liquidity_mint != usdc {
        return Err(anyhow!("v1 Save fire path requires USDC debt, got {}", c.repay_reserve.liquidity_mint));
    }
    let underlying = c.withdraw_reserve.liquidity_mint;

    // Swap leg: ExactIn the redeemed collateral underlying → USDC. Haircut 0.05%
    // to absorb redeem rounding (same as the marginfi path). wrap_sol=false — a
    // wSOL underlying lands in the wSOL ATA which Jupiter spends directly.
    let swap_in = c.seize_underlying.saturating_sub(c.seize_underlying / 2000 + 1);
    let quote = jup::quote(&underlying, &usdc, swap_in, slippage_bps, max_swap_accounts)?;
    let plan = jup::swap_instructions(&quote, authority, false)?;
    let save_alt = Pubkey::from_str(&std::env::var("SAVE_ALT").unwrap_or_else(|_| SAVE_ALT.into()))?;
    let mut alt_addrs = plan.alt_addresses.clone();
    // The marginfi-flashloan ALT (fixed marginfi USDC-bank accounts) + our Save ALT.
    if let Ok(liq_alt) = std::env::var("LIQ_ALT").or_else(|_| Ok::<_, std::env::VarError>(crate::liq_fire::LIQ_ALT.to_string())) {
        if let Ok(pk) = Pubkey::from_str(&liq_alt) { alt_addrs.push(pk); }
    }
    if save_alt != Pubkey::from_str("11111111111111111111111111111111").unwrap() {
        alt_addrs.push(save_alt);
    }
    let alts = jup::fetch_alts(rpc_endpoint, &alt_addrs)?;

    let usdc_ata = ata_for(authority, &usdc, &token_program);
    let underlying_ata = ata_for(authority, &underlying, &c.collateral_token_program);
    let ctoken_ata = ata_for(authority, &c.withdraw_reserve.collateral_mint, &token_program);

    let mut ixs = vec![
        cu_limit_ix(FIRE_CU_LIMIT),
        cu_price_ix(priority_micro_lamports),
        create_ata_idempotent_for(authority, &usdc, &token_program),
        create_ata_idempotent_for(authority, &underlying, &c.collateral_token_program),
        create_ata_idempotent_for(authority, &c.withdraw_reserve.collateral_mint, &token_program),
    ];
    let start_idx = ixs.len();
    ixs.push(marginfi::start_flashloan(liquidator_ma, authority, 0)); // end_index patched below
    // Flash-borrow the USDC we need to repay the liquidatee's debt.
    ixs.push(marginfi::borrow_usdc(liquidator_ma, authority, &usdc_ata, c.repay_amount));
    // Refresh Save state, then liquidate+redeem.
    ixs.push(save::refresh_reserve(&c.repay_reserve));
    ixs.push(save::refresh_reserve(&c.withdraw_reserve));
    ixs.push(save::refresh_obligation(&c.obligation, &c.deposit_reserves, &c.borrow_reserves));
    ixs.push(save::liquidate_and_redeem(
        c.repay_amount,
        &usdc_ata,          // source_liquidity (repay)
        &ctoken_ata,        // destination_collateral (transient cTokens)
        &underlying_ata,    // destination_liquidity (redeemed underlying)
        &c.repay_reserve,
        &c.withdraw_reserve,
        &c.obligation,
        &c.repay_reserve.lending_market,
        authority,          // user_transfer_authority (signer)
    ));
    // Sell the seized underlying for USDC.
    ixs.extend(plan.instructions);
    // Repay the flash loan (repay_all clears the borrowed USDC exactly).
    ixs.push(marginfi::payback_usdc(liquidator_ma, authority, &usdc_ata, c.repay_amount, true));
    let end_index = ixs.len() as u64;
    ixs[start_idx] = marginfi::start_flashloan(liquidator_ma, authority, end_index);
    ixs.push(marginfi::end_flashloan(liquidator_ma, authority, &[]));
    if let (Some(tip_to), true) = (tip_account, tip_lamports > 0) {
        ixs.push(transfer_ix(*authority, tip_to, tip_lamports));
    }

    let msg = v0::Message::try_compile(authority, &ixs, &alts, blockhash)
        .map_err(|e| anyhow!("compile save fire v0: {e}"))?;
    let tx = VersionedTransaction {
        signatures: vec![solana_signature::Signature::default()],
        message: VersionedMessage::V0(msg),
    };
    let tx_bytes = bincode::serialize(&tx)?.len();
    Ok(SaveFireTx { tx, quoted_usdc_out: plan.quoted_out, tx_bytes })
}
