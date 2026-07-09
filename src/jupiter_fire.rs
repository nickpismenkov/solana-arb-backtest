//! Jupiter Lend (Fluid) liquidate instruction builder + fire-path scaffold.
//!
//! ⚠️ HONESTY BANNER — what is VERIFIED vs INFERRED (read before trusting this).
//!
//! VERIFIED (from a real mainnet liquidate tx 5nLVofDj… and the IDL):
//! - the instruction discriminator + the exact borsh arg encoding (debt_amt u64,
//!   col_per_unit_debt u128, absorb bool, transfer_type Option<enum>,
//!   remaining_accounts_indices Vec<u8>), unit-tested to reproduce the captured
//!   tx's arg bytes;
//! - the 26 named account ORDER (matched account-for-account to that tx).
//!
//! SOLVED (see src/jupiter_math.rs + jupiter_fire_probe, reversed from the
//! on-chain program source and the published SDK, verified against 8 real txs):
//! - `remaining_accounts_indices` layout = `[oracle_sources, branches, ticks,
//!   tick_has_debt]` and the tick/branch account SELECTION → `build_remaining_
//!   accounts` derives the exact PDA set from live vault state;
//! - `col_per_unit_debt` — reversed as a *minimum-acceptable slippage floor*
//!   (1e15), NOT the price: the program computes the actual price from the
//!   vault oracle itself. Real liquidators pass 0 (accept oracle price — 2/8
//!   txs) or a computed floor. `jupiter_math::compute_col_per_debt` reproduces
//!   the on-chain formula; the resolver revert (to=ADDRESS_DEAD) yields the exact
//!   live ratio.
//!
//! STILL derive-from-truth (by design; seeds not in the vaults IDL): the per-vault
//! Liquidity-program PDAs (reserves/positions/token accounts/rate models/claim)
//! and the oracle `sources` are lifted from a recent on-chain tx for the vault.
//! The liquidate sim remains the ground-truth gate before any fire.

use crate::jupiter::{Vault, VAULTS_PROGRAM};
use crate::jupiter_math::{
    self, branch_pda, index_for_tick, tick_has_debt_pda, tick_pda, BranchLite,
};
use solana_instruction::{AccountMeta, Instruction};
use solana_pubkey::Pubkey;
use std::str::FromStr;

/// liquidate discriminator (sha256("global:liquidate")[..8]) — VERIFIED against
/// the on-chain tx.
pub const LIQUIDATE_DISC: [u8; 8] = [223, 179, 226, 125, 48, 46, 39, 74];

const SYSTEM: &str = "11111111111111111111111111111111";
const ATA_PROGRAM: &str = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL";

/// The full account set a liquidate ix needs for one vault. The vault-derived
/// fields come from `VaultConfig`; the Liquidity-program PDAs are captured from
/// a recent on-chain tx via `resolve_from_tx_accounts` (see the honesty banner).
#[derive(Clone, Debug)]
pub struct LiquidateAccounts {
    // liquidator-side (we fill these)
    pub signer: Pubkey,
    pub signer_token_account: Pubkey, // liquidator's borrow-token (debt) ATA
    pub to: Pubkey,
    pub to_token_account: Pubkey, // liquidator's supply-token (collateral) ATA
    // vault-derived (from VaultConfig)
    pub vault_config: Pubkey,
    pub vault_state: Pubkey,
    pub supply_token: Pubkey,
    pub borrow_token: Pubkey,
    pub oracle: Pubkey,
    pub oracle_program: Pubkey,
    // Liquidity-program per-vault accounts (captured from a real tx)
    pub new_branch: Pubkey,
    pub supply_token_reserves_liquidity: Pubkey,
    pub borrow_token_reserves_liquidity: Pubkey,
    pub vault_supply_position_on_liquidity: Pubkey,
    pub vault_borrow_position_on_liquidity: Pubkey,
    pub supply_rate_model: Pubkey,
    pub borrow_rate_model: Pubkey,
    pub supply_token_claim_account: Pubkey,
    pub liquidity: Pubkey,
    pub liquidity_program: Pubkey,
    pub vault_supply_token_account: Pubkey,
    pub vault_borrow_token_account: Pubkey,
    pub supply_token_program: Pubkey,
    pub borrow_token_program: Pubkey,
    /// tick/branch accounts referenced by `remaining_accounts_indices`.
    pub remaining: Vec<Pubkey>,
}

/// Index positions of the vault's Liquidity-program accounts inside a captured
/// liquidate ix's account list (VERIFIED order from tx 5nLVofDj…). Lets us lift
/// the hard-to-derive PDAs straight from a real tx for the same vault.
pub const ACCT_ORDER: &[&str] = &[
    "signer", "signer_token_account", "to", "to_token_account", "vault_config",
    "vault_state", "supply_token", "borrow_token", "oracle", "new_branch",
    "supply_token_reserves_liquidity", "borrow_token_reserves_liquidity",
    "vault_supply_position_on_liquidity", "vault_borrow_position_on_liquidity",
    "supply_rate_model", "borrow_rate_model", "supply_token_claim_account",
    "liquidity", "liquidity_program", "vault_supply_token_account",
    "vault_borrow_token_account", "supply_token_program", "borrow_token_program",
    "system_program", "associated_token_program", "oracle_program",
];

/// borsh-encode the liquidate instruction data. VERIFIED: reproduces the arg
/// bytes of the captured tx (see tests). `transfer_type` is the inner enum
/// discriminant when Some (the real tx used Some(1)).
pub fn build_liquidate_data(
    debt_amt: u64,
    col_per_unit_debt: u128,
    absorb: bool,
    transfer_type: Option<u8>,
    remaining_accounts_indices: &[u8],
) -> Vec<u8> {
    let mut d = Vec::with_capacity(43 + remaining_accounts_indices.len());
    d.extend_from_slice(&LIQUIDATE_DISC);
    d.extend_from_slice(&debt_amt.to_le_bytes());
    d.extend_from_slice(&col_per_unit_debt.to_le_bytes());
    d.push(absorb as u8);
    match transfer_type {
        Some(v) => { d.push(1); d.push(v); }
        None => d.push(0),
    }
    d.extend_from_slice(&(remaining_accounts_indices.len() as u32).to_le_bytes());
    d.extend_from_slice(remaining_accounts_indices);
    d
}

/// Assemble the liquidate instruction. Account order is VERIFIED; the caller is
/// responsible for supplying correct `remaining` (tick/branch) accounts +
/// indices (see honesty banner — currently INFERRED).
#[allow(clippy::too_many_arguments)]
pub fn build_liquidate_ix(
    a: &LiquidateAccounts,
    debt_amt: u64,
    col_per_unit_debt: u128,
    absorb: bool,
    transfer_type: Option<u8>,
    remaining_accounts_indices: &[u8],
) -> Instruction {
    let mut accounts = vec![
        AccountMeta::new(a.signer, true),
        AccountMeta::new(a.signer_token_account, false),
        AccountMeta::new_readonly(a.to, false),
        AccountMeta::new(a.to_token_account, false),
        AccountMeta::new_readonly(a.vault_config, false),
        AccountMeta::new(a.vault_state, false),
        AccountMeta::new_readonly(a.supply_token, false),
        AccountMeta::new_readonly(a.borrow_token, false),
        AccountMeta::new_readonly(a.oracle, false),
        AccountMeta::new(a.new_branch, false),
        AccountMeta::new(a.supply_token_reserves_liquidity, false),
        AccountMeta::new(a.borrow_token_reserves_liquidity, false),
        AccountMeta::new(a.vault_supply_position_on_liquidity, false),
        AccountMeta::new(a.vault_borrow_position_on_liquidity, false),
        AccountMeta::new_readonly(a.supply_rate_model, false),
        AccountMeta::new_readonly(a.borrow_rate_model, false),
        AccountMeta::new(a.supply_token_claim_account, false),
        AccountMeta::new_readonly(a.liquidity, false),
        AccountMeta::new_readonly(a.liquidity_program, false),
        AccountMeta::new(a.vault_supply_token_account, false),
        AccountMeta::new(a.vault_borrow_token_account, false),
        AccountMeta::new_readonly(a.supply_token_program, false),
        AccountMeta::new_readonly(a.borrow_token_program, false),
        AccountMeta::new_readonly(Pubkey::from_str(SYSTEM).unwrap(), false),
        AccountMeta::new_readonly(Pubkey::from_str(ATA_PROGRAM).unwrap(), false),
        AccountMeta::new_readonly(a.oracle_program, false),
    ];
    // Fluid tick/branch remaining accounts (writable — they mutate on liquidation).
    for r in &a.remaining {
        accounts.push(AccountMeta::new(*r, false));
    }
    Instruction {
        program_id: Pubkey::from_str(VAULTS_PROGRAM).unwrap(),
        accounts,
        data: build_liquidate_data(debt_amt, col_per_unit_debt, absorb, transfer_type, remaining_accounts_indices),
    }
}

/// Lift the per-vault Liquidity-program accounts out of a captured liquidate ix
/// account list (the `ACCT_ORDER` positions), so a fresh liquidate for the same
/// vault reuses the exact PDAs it used before. `tx_accounts` = the ordered
/// account pubkeys of a real liquidate ix for this vault; `remaining` = anything
/// past index 26. Returns the vault-fixed accounts (signer/token-accounts left
/// to the caller). This is the derive-from-truth account resolver.
pub fn accounts_from_captured(v: &Vault, tx_accounts: &[Pubkey]) -> Option<LiquidateAccounts> {
    if tx_accounts.len() < 26 { return None; }
    let g = |i: usize| tx_accounts[i];
    Some(LiquidateAccounts {
        signer: g(0), signer_token_account: g(1), to: g(2), to_token_account: g(3),
        vault_config: v.config_pubkey, vault_state: v.state_pubkey,
        supply_token: v.config.supply_token, borrow_token: v.config.borrow_token,
        oracle: v.config.oracle, oracle_program: v.config.oracle_program,
        new_branch: g(9),
        supply_token_reserves_liquidity: g(10), borrow_token_reserves_liquidity: g(11),
        vault_supply_position_on_liquidity: g(12), vault_borrow_position_on_liquidity: g(13),
        supply_rate_model: g(14), borrow_rate_model: g(15), supply_token_claim_account: g(16),
        liquidity: g(17), liquidity_program: g(18),
        vault_supply_token_account: g(19), vault_borrow_token_account: g(20),
        supply_token_program: g(21), borrow_token_program: g(22),
        remaining: tx_accounts.get(26..).unwrap_or(&[]).to_vec(),
    })
}

/// The all-zero pubkey. Passing it as `to` triggers the program's built-in
/// resolver: it runs the full liquidation math and REVERTS with
/// `VaultLiquidationResult: [actual_col, actual_debt, topmost_tick]` — exact
/// ground truth for pricing/sizing, computed by the program itself.
pub const ADDRESS_DEAD: Pubkey = Pubkey::new_from_array([0u8; 32]);

/// Overwrite the liquidator-side accounts (signer + our debt/collateral ATAs)
/// on a captured account set, so a fresh liquidate seizes to OUR wallet.
pub fn set_liquidator_side(
    a: &mut LiquidateAccounts,
    signer: Pubkey,
    signer_token_account: Pubkey,
    to: Pubkey,
    to_token_account: Pubkey,
) {
    a.signer = signer;
    a.signer_token_account = signer_token_account;
    a.to = to;
    a.to_token_account = to_token_account;
}

/// Build the `remaining_accounts` + `remaining_accounts_indices` for a liquidate,
/// derived from CURRENT on-chain state — the layout is
/// `[oracle_sources, branches, ticks, tick_has_debt]` (verified against 8 real
/// mainnet txs; see jupiter_fire_probe). Ported from the SDK
/// `getRemainingAccountsLiquidate`.
///
/// `oracle_sources` are lifted from a recent tx (deterministic per vault oracle).
/// `fetch` reads raw account bytes (None if the account does not exist / not
/// owned by the program). `liquidation_tick` is `jupiter_math`'s value (bit-
/// identical to what the program computes) — pass what you compute from the live
/// oracle price via `jupiter_math::liquidation_tick_from_price_1e8`, or a low
/// bound to include more ticks.
pub fn build_remaining_accounts(
    vault_id: u16,
    topmost_tick: i32,
    current_branch_id: u32,
    liquidation_tick: i32,
    oracle_sources: &[Pubkey],
    fetch: &dyn Fn(&Pubkey) -> Option<Vec<u8>>,
) -> (Vec<Pubkey>, [u8; 4]) {
    // ── branches: current branch, then walk connected_branch_id, always incl 0 ──
    let mut branch_ids: Vec<u32> = Vec::new();
    let mut connected = 0u32;
    if current_branch_id > 0 {
        if let Some(raw) = fetch(&branch_pda(vault_id, current_branch_id)) {
            if let Some(b) = BranchLite::decode(&raw) {
                branch_ids.push(current_branch_id);
                connected = b.connected_branch_id;
            }
        }
    }
    while connected > 0 && !branch_ids.contains(&connected) {
        let pda = branch_pda(vault_id, connected);
        match fetch(&pda).as_deref().and_then(BranchLite::decode) {
            Some(b) => { branch_ids.push(connected); connected = b.connected_branch_id; }
            None => break,
        }
    }
    if !branch_ids.contains(&0) { branch_ids.push(0); }

    // ── ticks: topmost (if a real perfect tick exists) then walk down to liq_tick ─
    let array_fetch = |idx: u8| -> Option<Vec<u8>> { fetch(&tick_has_debt_pda(vault_id, idx)) };
    let mut ticks: Vec<i32> = Vec::new();
    if topmost_tick > liquidation_tick && fetch(&tick_pda(vault_id, topmost_tick)).is_some() {
        ticks.push(topmost_tick);
    }
    let mut next_tick = jupiter_math::find_next_tick_with_debt(topmost_tick, &array_fetch);
    while next_tick > liquidation_tick && !ticks.contains(&next_tick) {
        if fetch(&tick_pda(vault_id, next_tick)).is_some() {
            ticks.push(next_tick);
        }
        let n = jupiter_math::find_next_tick_with_debt(next_tick, &array_fetch);
        if n == next_tick { break; }
        next_tick = n;
    }

    // ── tick_has_debt arrays: index(topmost) down to index(next_tick) ──
    let top_idx = index_for_tick(topmost_tick);
    let next_idx = index_for_tick(next_tick);
    let (hi, lo) = (top_idx.max(next_idx), top_idx.min(next_idx));
    let thd_indices: Vec<u8> = (lo..=hi).rev().collect();

    // ── assemble in the exact program order ──
    let mut remaining: Vec<Pubkey> = Vec::new();
    remaining.extend_from_slice(oracle_sources);
    for b in &branch_ids { remaining.push(branch_pda(vault_id, *b)); }
    for t in &ticks { remaining.push(tick_pda(vault_id, *t)); }
    for i in &thd_indices { remaining.push(tick_has_debt_pda(vault_id, *i)); }

    let indices = [
        oracle_sources.len() as u8,
        branch_ids.len() as u8,
        ticks.len() as u8,
        thd_indices.len() as u8,
    ];
    (remaining, indices)
}

// ── flash-loan-wrapped fire tx (USDC-debt vaults; mirrors save_fire.rs) ──────
use crate::arb::{cu_limit_ix, cu_price_ix, transfer_ix};
use crate::flashloan::{ata_for, create_ata_idempotent_for};
use crate::jup;
use crate::marginfi;
use anyhow::{anyhow, Result};
use solana_hash::Hash;
use solana_message::{v0, VersionedMessage};
use solana_transaction::versioned::VersionedTransaction;

pub const FIRE_CU_LIMIT: u32 = 1_400_000;
const USDC_MINT: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const TOKEN_PROGRAM: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";

/// One sized Jupiter-Lend liquidation opportunity (USDC debt).
pub struct JupiterFireCandidate {
    /// Fully-resolved liquidate accounts (Liquidity PDAs lifted from a real tx;
    /// liquidator-side + `remaining` set to OURS via the builder).
    pub accts: LiquidateAccounts,
    /// Debt (USDC, 6dp) we repay.
    pub debt_amt: u64,
    /// Slippage floor (1e15). 0 = accept the oracle price (proven safe by real
    /// txs); or a `jupiter_math::compute_col_per_debt` / resolver-derived value.
    pub col_per_unit_debt: u128,
    /// remaining accounts + indices from `build_remaining_accounts`.
    pub remaining: Vec<Pubkey>,
    pub remaining_indices: [u8; 4],
    /// Collateral (supply_token) underlying we expect to seize, to size the swap.
    pub seize_underlying: u64,
    /// Collateral mint + its token program (for the swap-back ATA).
    pub collateral_mint: Pubkey,
    pub collateral_token_program: Pubkey,
}

pub struct JupiterFireTx {
    pub tx: VersionedTransaction,
    pub quoted_usdc_out: u64,
    pub tx_bytes: usize,
}

/// Build the unsigned flash-loan-wrapped liquidate tx:
///   [cu, ATAs, marginfi start_flashloan → borrow USDC,
///    jupiter LIQUIDATE (repay USDC, seize collateral),
///    Jupiter swap collateral→USDC, marginfi payback + end_flashloan, tip]
/// Same profit-or-revert shape as the Save path. `blockhash` default for
/// replace-blockhash simulation.
#[allow(clippy::too_many_arguments)]
pub fn build_jupiter_fire_tx(
    rpc_endpoint: &str,
    c: &JupiterFireCandidate,
    liquidator_ma: &Pubkey,
    authority: &Pubkey,
    tip_account: Option<Pubkey>,
    tip_lamports: u64,
    priority_micro_lamports: u64,
    slippage_bps: u32,
    max_swap_accounts: usize,
    blockhash: Hash,
) -> Result<JupiterFireTx> {
    let usdc = Pubkey::from_str(USDC_MINT).unwrap();
    let token_program = Pubkey::from_str(TOKEN_PROGRAM).unwrap();
    if c.accts.borrow_token != usdc {
        return Err(anyhow!("jupiter fire path currently wraps only USDC debt, got {}", c.accts.borrow_token));
    }

    // Swap leg: sell the seized collateral underlying → USDC (0.05% haircut for
    // seize rounding, as in save_fire).
    let swap_in = c.seize_underlying.saturating_sub(c.seize_underlying / 2000 + 1);
    let quote = jup::quote(&c.collateral_mint, &usdc, swap_in, slippage_bps, max_swap_accounts)?;
    let plan = jup::swap_instructions(&quote, authority, false)?;
    let mut alt_addrs = plan.alt_addresses.clone();
    if let Ok(liq_alt) = std::env::var("LIQ_ALT") {
        if let Ok(pk) = Pubkey::from_str(&liq_alt) { alt_addrs.push(pk); }
    }
    let alts = jup::fetch_alts(rpc_endpoint, &alt_addrs)?;

    let usdc_ata = ata_for(authority, &usdc, &token_program);
    let collat_ata = ata_for(authority, &c.collateral_mint, &c.collateral_token_program);

    let mut a = c.accts.clone();
    set_liquidator_side(&mut a, *authority, usdc_ata, *authority, collat_ata);
    a.remaining = c.remaining.clone();

    let mut ixs = vec![
        cu_limit_ix(FIRE_CU_LIMIT),
        cu_price_ix(priority_micro_lamports),
        create_ata_idempotent_for(authority, &usdc, &token_program),
        create_ata_idempotent_for(authority, &c.collateral_mint, &c.collateral_token_program),
    ];
    let start_idx = ixs.len();
    ixs.push(marginfi::start_flashloan(liquidator_ma, authority, 0)); // end_index patched below
    ixs.push(marginfi::borrow_usdc(liquidator_ma, authority, &usdc_ata, c.debt_amt));
    // The reversed liquidate ix — correctly priced + tick/branch accounts.
    ixs.push(build_liquidate_ix(
        &a, c.debt_amt, c.col_per_unit_debt, false, Some(1), &c.remaining_indices,
    ));
    ixs.extend(plan.instructions);
    ixs.push(marginfi::payback_usdc(liquidator_ma, authority, &usdc_ata, c.debt_amt, true));
    let end_index = ixs.len() as u64;
    ixs[start_idx] = marginfi::start_flashloan(liquidator_ma, authority, end_index);
    ixs.push(marginfi::end_flashloan(liquidator_ma, authority, &[]));
    if let (Some(tip_to), true) = (tip_account, tip_lamports > 0) {
        ixs.push(transfer_ix(*authority, tip_to, tip_lamports));
    }

    let msg = v0::Message::try_compile(authority, &ixs, &alts, blockhash)
        .map_err(|e| anyhow!("compile jupiter fire v0: {e}"))?;
    let tx = VersionedTransaction {
        signatures: vec![solana_signature::Signature::default()],
        message: VersionedMessage::V0(msg),
    };
    let tx_bytes = bincode::serialize(&tx)?.len();
    Ok(JupiterFireTx { tx, quoted_usdc_out: plan.quoted_out, tx_bytes })
}

#[cfg(test)]
mod tests {
    use super::*;

    // Reproduce the arg bytes of the captured mainnet liquidate (5nLVofDj…):
    // debt 15958229, col_per_unit_debt 14941855085548, absorb false,
    // transfer_type Some(1), remaining_accounts_indices [2,1,1,1].
    #[test]
    fn liquidate_data_matches_captured_tx() {
        let d = build_liquidate_data(15958229, 14941855085548, false, Some(1), &[2, 1, 1, 1]);
        assert_eq!(&d[..8], &LIQUIDATE_DISC);
        assert_eq!(u64::from_le_bytes(d[8..16].try_into().unwrap()), 15958229);
        assert_eq!(u128::from_le_bytes(d[16..32].try_into().unwrap()), 14941855085548);
        assert_eq!(d[32], 0); // absorb=false
        assert_eq!(d[33], 1); // transfer_type Some
        assert_eq!(d[34], 1); // enum discriminant
        assert_eq!(u32::from_le_bytes(d[35..39].try_into().unwrap()), 4); // indices len
        assert_eq!(&d[39..43], &[2, 1, 1, 1]);
        assert_eq!(d.len(), 43);
    }
}
