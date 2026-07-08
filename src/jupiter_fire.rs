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
//! INFERRED / NOT SOLVED HERE (do not fire on this without the sim as gate):
//! - `col_per_unit_debt` pricing (the collateral-per-debt limit) — needs the
//!   vault oracle + Fluid curve math;
//! - `remaining_accounts_indices` + which tick/branch accounts to pass — the
//!   captured tx used indices [2,1,1,1] over 5 remaining accounts; the selection
//!   rule is Fluid-internal and NOT reversed;
//! - the per-vault Liquidity-program PDAs (reserves/positions/token accounts/rate
//!   models/claim/new_branch) — this module RESOLVES them by capturing a recent
//!   on-chain tx for the vault (derive-from-truth), rather than deriving PDAs
//!   whose seeds aren't in the vaults IDL.
//!
//! Consequence: a live fire needs (a) the tick/branch remaining accounts +
//! indices and (b) a correct col_per_unit_debt. Until those are solved, the
//! executor SIMULATES only (the liquidate sim is the ground-truth gate, exactly
//! as with the other protocols).

use crate::jupiter::{Vault, VAULTS_PROGRAM};
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
