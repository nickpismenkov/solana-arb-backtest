//! The atomic Save (Solend) liquidation FIRE path — one flash-loan-wrapped v0 tx:
//!
//!   [cu_limit, cu_price, create ATAs,
//!    JupLend borrow(debt)               (capital, no inventory)
//!    save refresh_reserve(repay=debt) · refresh_reserve(withdraw=collateral)
//!      · refresh_obligation
//!    save liquidate_obligation_and_redeem  (repay debt, seize collateral,
//!      redeem cTokens → underlying into our ATA)
//!    Jupiter swap collateral-underlying → debt  (skipped when they're the same
//!      mint — Jupiter rejects equal in/out)
//!    JupLend payback(debt)              (repay the flash loan)
//!    tip]
//!
//! v1.5: the debt (repay reserve's liquidity) may be any asset with a wired
//! JupLend flash market — USDC/USDT/wSOL. That mint is what JupLend flash-borrows,
//! what the seized-collateral swap targets, and what the fixed payback repays.
//!
//! We wrap in JUPLEND's 0-bp flash loan (not Solend's own) for the same reason
//! Kamino does: it's a different program, so it sidesteps Solend's flash-loan
//! reentrancy guard (our liquidate repays into the very reserve a Solend flash
//! loan would forbid touching between borrow/repay), and JupLend matches
//! borrow↔payback via the instructions sysvar so no start/end wrapper is needed.
//!
//! Profit-or-revert with NO capital: the fixed-amount `payback(debt)` fails
//! unless the swap produced at least the borrowed amount, so a landed tx is
//! always net-positive (the ~liq_bonus% surplus stays in the wallet ATA) and an
//! unprofitable one reverts for just the base fee.

use crate::arb::{cu_limit_ix, cu_price_ix, transfer_ix};
use crate::flashloan::{ata_for, borrow, create_ata_idempotent_for, has_market, payback};
use crate::jup;
use crate::save::{self, Reserve};
use anyhow::{anyhow, Result};
use solana_hash::Hash;
use solana_message::{v0, VersionedMessage};
use solana_pubkey::Pubkey;
use solana_transaction::versioned::VersionedTransaction;
use std::str::FromStr;

pub const FIRE_CU_LIMIT: u32 = 1_400_000;

/// Dedicated ALT holding the fixed Solend + JupLend-flashloan accounts common to
/// every Save liquidation (create with save_alt_print, analogous to LIQ_ALT).
/// Override via SAVE_ALT.
pub const SAVE_ALT: &str = "11111111111111111111111111111111"; // placeholder until created on-chain

/// One Save liquidation opportunity, sized by the caller (via simulation).
pub struct SaveFireCandidate {
    pub obligation: Pubkey,
    /// The borrow reserve being repaid. Its `liquidity_mint` is the debt asset
    /// (USDC/USDT/wSOL) — flash-borrowed, swapped into, and repaid.
    pub repay_reserve: Reserve,
    /// The collateral reserve being seized (its liquidity mint is what we swap).
    pub withdraw_reserve: Reserve,
    /// Token program owning the collateral-underlying mint (redeem ATA).
    pub collateral_token_program: Pubkey,
    /// Token program owning the debt mint (USDC/USDT/wSOL are all classic SPL).
    pub debt_token_program: Pubkey,
    /// Debt to repay, in the debt asset's native units.
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
    /// Debt-asset units the collateral→debt swap is quoted to produce (native).
    /// In the same-mint case (collateral underlying == debt) this is the seized
    /// amount itself, since there is no swap.
    pub quoted_debt_out: u64,
    pub tx_bytes: usize,
}

/// Build the unsigned Save fire tx. Quotes the collateral→debt swap live, so
/// call only for a sim-confirmed candidate. `blockhash` = real recent hash for
/// live submission, or default for replace-blockhash simulation.
#[allow(clippy::too_many_arguments)]
pub fn build_save_fire_tx(
    rpc_endpoint: &str,
    c: &SaveFireCandidate,
    authority: &Pubkey,
    tip_account: Option<Pubkey>,
    tip_lamports: u64,
    priority_micro_lamports: u64,
    slippage_bps: u32,
    max_swap_accounts: usize,
    blockhash: Hash,
) -> Result<SaveFireTx> {
    // Debt asset = the repay reserve's liquidity mint (USDC/USDT/wSOL). It's the
    // flash-borrow asset, the swap target, and the payback token.
    let debt_mint = c.repay_reserve.liquidity_mint;
    if !has_market(&debt_mint) {
        return Err(anyhow!("no JupLend flash market for Save debt mint {debt_mint}"));
    }
    let underlying = c.withdraw_reserve.liquidity_mint;

    // Same-mint case (seized underlying == debt): no swap — the redeemed
    // liquidity IS the debt asset. Jupiter rejects equal in/out mints, so skip
    // the swap leg (the fixed payback still guards profit).
    let same_mint = underlying == debt_mint;
    let (swap_ixs, quoted_debt_out, swap_alts): (Vec<_>, u64, Vec<Pubkey>) = if same_mint {
        (Vec::new(), c.seize_underlying, Vec::new())
    } else {
        // ExactIn the redeemed collateral underlying → debt asset. Haircut 0.05%
        // to absorb redeem rounding (same as the marginfi/Kamino paths).
        let swap_in = c.seize_underlying.saturating_sub(c.seize_underlying / 2000 + 1);
        let quote = jup::quote(&underlying, &debt_mint, swap_in, slippage_bps, max_swap_accounts)?;
        let plan = jup::swap_instructions(&quote, authority, false)?;
        (plan.instructions, plan.quoted_out, plan.alt_addresses)
    };

    let save_alt = Pubkey::from_str(&std::env::var("SAVE_ALT").unwrap_or_else(|_| SAVE_ALT.into()))?;
    let mut alt_addrs = swap_alts.clone();
    if save_alt != Pubkey::from_str("11111111111111111111111111111111").unwrap() {
        alt_addrs.push(save_alt);
    }
    let alts = jup::fetch_alts(rpc_endpoint, &alt_addrs)?;

    // Solend cTokens are always classic SPL (the program predates Token-2022 and
    // its liquidate ix passes the classic token program for every transfer).
    let token_program = Pubkey::from_str("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA").unwrap();
    let debt_ata = ata_for(authority, &debt_mint, &c.debt_token_program);
    let underlying_ata = ata_for(authority, &underlying, &c.collateral_token_program);
    let ctoken_ata = ata_for(authority, &c.withdraw_reserve.collateral_mint, &token_program);

    let borrow_ix = borrow(authority, &debt_mint, c.repay_amount)
        .ok_or_else(|| anyhow!("no JupLend flash market for Save debt mint {debt_mint}"))?;
    let payback_ix = payback(authority, &debt_mint, c.repay_amount)
        .ok_or_else(|| anyhow!("no JupLend flash market for Save debt mint {debt_mint}"))?;

    let mut ixs = vec![
        cu_limit_ix(FIRE_CU_LIMIT),
        cu_price_ix(priority_micro_lamports),
        create_ata_idempotent_for(authority, &debt_mint, &c.debt_token_program),
        create_ata_idempotent_for(authority, &underlying, &c.collateral_token_program),
        create_ata_idempotent_for(authority, &c.withdraw_reserve.collateral_mint, &token_program),
    ];
    // Flash-borrow the debt asset we need to repay the liquidatee's debt.
    ixs.push(borrow_ix);
    // Refresh Save state, then liquidate+redeem.
    ixs.push(save::refresh_reserve(&c.repay_reserve));
    ixs.push(save::refresh_reserve(&c.withdraw_reserve));
    ixs.push(save::refresh_obligation(&c.obligation, &c.deposit_reserves, &c.borrow_reserves));
    ixs.push(save::liquidate_and_redeem(
        c.repay_amount,
        &debt_ata,          // source_liquidity (repay)
        &ctoken_ata,        // destination_collateral (transient cTokens)
        &underlying_ata,    // destination_liquidity (redeemed underlying)
        &c.repay_reserve,
        &c.withdraw_reserve,
        &c.obligation,
        &c.repay_reserve.lending_market,
        authority,          // user_transfer_authority (signer)
    ));
    // Sell the seized underlying for the debt asset (unless it already is it).
    ixs.extend(swap_ixs);
    // Fixed-amount payback = the profit-or-revert guard: reverts unless the swap
    // (or the same-mint redeem) covered the borrowed debt exactly.
    ixs.push(payback_ix);
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
    Ok(SaveFireTx { tx, quoted_debt_out, tx_bytes })
}
