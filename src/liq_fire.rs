//! The atomic liquidation FIRE path — one flashloan-wrapped v0 tx:
//!
//!   [cu_limit, cu_price, create ATAs, start_flashloan,
//!    liquidate → withdraw_all seized collateral → Jupiter swap collateral→USDC
//!    → repay_all liability, end_flashloan, tip]
//!
//! Profit-or-revert with NO external capital: `liquidate` moves internal shares
//! (liquidator gains asset shares + takes on the matching liability), so no
//! tokens are needed up front; `repay_all` fails unless the swap produced
//! enough USDC to cover the full liability, and `end_flashloan` re-checks
//! account health — either the whole tx lands net-positive (surplus USDC stays
//! in the wallet ATA) or it reverts and costs nothing but the fee on a miss.
//!
//! v1 restriction: the liability bank must be USDC (the dominant marginfi debt
//! asset) — the swap leg targets USDC and `payback_usdc` closes it.

use crate::arb::{cu_limit_ix, cu_price_ix, transfer_ix};
use crate::flashloan::{ata_for, create_ata_idempotent_for};
use crate::jup;
use crate::marginfi;
use anyhow::{anyhow, Result};
use solana_hash::Hash;
use solana_instruction::AccountMeta;
use solana_message::{v0, VersionedMessage};
use solana_pubkey::Pubkey;
use solana_transaction::versioned::VersionedTransaction;
use std::str::FromStr;

pub const FIRE_CU_LIMIT: u32 = 1_400_000;

/// Dedicated ALT holding the 18 accounts common to every marginfi-USDC
/// liquidation (see liq_alt_print for the set + recreate instructions).
/// Override via LIQ_ALT.
pub const LIQ_ALT: &str = "DEMhLvSJbSZQfCdiH7YicYNopo3EhhapjfoEjt2kJVij";

/// Everything the executor knows about one liquidation opportunity.
pub struct FireCandidate {
    pub liquidatee: Pubkey,
    pub asset_bank: Pubkey,
    pub asset_mint: Pubkey,
    pub asset_token_program: Pubkey,
    /// Collateral native units to seize (sized by the caller via simulation).
    pub asset_amount: u64,
    /// The liability (debt) bank the liquidator absorbs and must repay. Any of
    /// USDC/USDT/wSOL in v1.5.
    pub liab_bank: Pubkey,
    /// The debt asset's mint + token program — the swap target and payback token
    /// (was hardcoded USDC; now the actual absorbed-liability asset).
    pub debt_mint: Pubkey,
    pub debt_token_program: Pubkey,
    pub asset_oracle: Pubkey,
    pub liab_oracle: Pubkey,
    /// The liquidatee's observation list: [bank(ro), oracle(ro)] per active
    /// balance, in balance order.
    pub liquidatee_obs: Vec<AccountMeta>,
}

pub struct FireTx {
    /// Unsigned (sign before sending; default signature placeholder).
    pub tx: VersionedTransaction,
    /// Jupiter's quoted DEBT-asset out (native) for the seized collateral —
    /// compare against the absorbed liability to decide whether firing is worth
    /// it. (Named `quoted_usdc_out` historically; now the debt asset, which may
    /// be USDC/USDT/wSOL.)
    pub quoted_usdc_out: u64,
    pub tx_bytes: usize,
}

/// Build the unsigned fire tx. Quotes the collateral→USDC swap live (Jupiter),
/// so call this only for a sim-confirmed candidate. `blockhash` = real recent
/// hash for live submission, or default for replace-blockhash simulation.
#[allow(clippy::too_many_arguments)]
pub fn build_fire_tx(
    rpc_endpoint: &str,
    c: &FireCandidate,
    liquidator_ma: &Pubkey,
    authority: &Pubkey,
    tip_account: Option<Pubkey>,
    tip_lamports: u64,
    priority_micro_lamports: u64,
    slippage_bps: u32,
    max_swap_accounts: usize,
    blockhash: Hash,
) -> Result<FireTx> {
    let token_program = Pubkey::from_str("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA").unwrap();

    // Swap leg: ExactIn the seized collateral → DEBT asset (USDC/USDT/wSOL).
    // Haircut 0.05%: the seize→withdraw round-trip goes through marginfi share
    // math and can round down a few native units, and an ExactIn of the full
    // amount would fail on insufficient funds — the dust stays in the asset ATA.
    // wrap_sol=false — a SOL collateral withdraw lands wSOL in the wSOL ATA, and
    // a wSOL-debt swap output also lands as wSOL, which payback spends directly.
    //
    // Same-mint case (collateral mint == debt mint, e.g. SOL collateral / SOL
    // debt): no swap — the withdrawn collateral IS the debt asset (same ATA), so
    // repay spends it directly. Jupiter rejects equal in/out mints, so we must
    // skip the quote entirely. quoted_out ≈ the withdrawn amount.
    let swap_in = c.asset_amount.saturating_sub(c.asset_amount / 2000 + 1);
    let same_mint = c.asset_mint == c.debt_mint;
    let (swap_ixs, quoted_out, swap_alts): (Vec<_>, u64, Vec<Pubkey>) = if same_mint {
        (Vec::new(), swap_in, Vec::new())
    } else {
        let quote = jup::quote(&c.asset_mint, &c.debt_mint, swap_in, slippage_bps, max_swap_accounts)?;
        let plan = jup::swap_instructions(&quote, authority, false)?;
        (plan.instructions, plan.quoted_out, plan.alt_addresses)
    };
    // Jupiter's route ALTs + our liquidation ALT (the fixed marginfi accounts).
    let liq_alt = Pubkey::from_str(
        &std::env::var("LIQ_ALT").unwrap_or_else(|_| LIQ_ALT.into()))?;
    let mut alt_addrs = swap_alts.clone();
    alt_addrs.push(liq_alt);
    let alts = jup::fetch_alts(rpc_endpoint, &alt_addrs)?;

    let asset_ata = ata_for(authority, &c.asset_mint, &c.asset_token_program);
    let debt_ata = ata_for(authority, &c.debt_mint, &c.debt_token_program);

    let mut ixs = vec![
        cu_limit_ix(FIRE_CU_LIMIT),
        cu_price_ix(priority_micro_lamports),
        create_ata_idempotent_for(authority, &c.asset_mint, &c.asset_token_program),
        create_ata_idempotent_for(authority, &c.debt_mint, &c.debt_token_program),
    ];
    let _ = token_program;
    let start_idx = ixs.len();
    ixs.push(marginfi::start_flashloan(liquidator_ma, authority, 0)); // end_index patched below
    ixs.push(marginfi::lending_account_liquidate(
        &c.asset_bank, &c.liab_bank, liquidator_ma, authority, &c.liquidatee,
        &token_program, c.asset_amount, &c.asset_oracle, &c.liab_oracle, &c.liquidatee_obs,
    ));
    ixs.push(marginfi::lending_account_withdraw(
        liquidator_ma, authority, &c.asset_bank, &asset_ata, &c.asset_token_program, c.asset_amount, true,
    ));
    ixs.extend(swap_ixs);
    // repay_all clears the entire liability regardless of amount (verified in
    // marginfi_probe); pass the quoted swap output as a plausible amount. Uses
    // the generic payback for the actual debt bank (USDC/USDT/wSOL).
    ixs.push(marginfi::payback_asset(liquidator_ma, authority, &c.liab_bank, &debt_ata, quoted_out, true));
    // withdraw_all + repay_all close both balances → end_flashloan health check
    // runs over zero active balances (empty observation list).
    let end_index = ixs.len() as u64;
    ixs[start_idx] = marginfi::start_flashloan(liquidator_ma, authority, end_index);
    ixs.push(marginfi::end_flashloan(liquidator_ma, authority, &[]));
    // Tip last, in-tx → only paid when the liquidation lands.
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
    Ok(FireTx { tx, quoted_usdc_out: quoted_out, tx_bytes })
}
