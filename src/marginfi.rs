//! marginfi v2 flash-loan instructions (alternative to Jupiter Lend, which
//! Jito silently drops — see land_probe bisection). marginfi brackets the
//! borrow/repay between start_flashloan and end_flashloan and enforces an
//! account-health check at the end; a MarginfiAccount owned by the signer is
//! required (one-time create, plain keypair account — NOT a PDA).
//!
//! Discriminators are VERIFIED (sha256("global:<name>")[..8], cross-checked vs
//! the marginfi IDL). Group + USDC bank are the authoritative mainnet values.
//! Vault/authority PDAs use the conventional seeds but are env-overridable
//! (MARGINFI_USDC_VAULT / _VAULT_AUTH) in case the deployed build differs —
//! marginfi_probe verifies the whole thing by mainnet simulation before trust.

use solana_instruction::{AccountMeta, Instruction};
use solana_pubkey::Pubkey;
use std::str::FromStr;

pub const MARGINFI_PROGRAM: &str = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA";
pub const MARGINFI_GROUP: &str = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8";
pub const USDC_BANK: &str = "2s37akK2eyBbp8DZgCm7RtsaEz8eJP3Nxd4urLHQv7yB";
pub const USDC_MINT: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const TOKEN_PROGRAM: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
const SYS_PROGRAM: &str = "11111111111111111111111111111111";
const INSTRUCTIONS_SYSVAR: &str = "Sysvar1nstructions1111111111111111111111111";

// VERIFIED Anchor discriminators (sha256("global:<name>")[..8]).
const DISC_START_FLASHLOAN: [u8; 8] = [14, 131, 33, 220, 81, 186, 180, 107];
const DISC_END_FLASHLOAN: [u8; 8] = [105, 124, 201, 106, 153, 2, 8, 156];
const DISC_BORROW: [u8; 8] = [4, 126, 116, 53, 48, 5, 212, 31];
// Closing a borrow requires `repay`, not `deposit` (deposit refuses a bank you
// owe to → OperationDepositOnly 6019). repay_all=Some(true) clears the whole
// liability (principal + dust interest) and leaves any surplus USDC as profit.
const DISC_REPAY: [u8; 8] = [79, 209, 172, 177, 222, 51, 173, 151];
const DISC_ACCOUNT_INIT: [u8; 8] = [43, 78, 61, 255, 148, 52, 249, 154];
// Liquidation ixs. VERIFIED against 3 real mainnet liquidations: liquidate data
// is 18 bytes = disc(8) + asset_amount(u64) + liquidatee_accounts(u8) +
// liquidator_accounts(u8) — the deployed build is the 3-arg version.
const DISC_LIQUIDATE: [u8; 8] = [214, 169, 151, 213, 251, 167, 86, 219];
const DISC_WITHDRAW: [u8; 8] = [36, 72, 74, 19, 210, 210, 192, 192];

fn pk(s: &str) -> Pubkey {
    Pubkey::from_str(s).unwrap()
}

/// Generic bank-vault PDAs (any bank, not just USDC).
pub fn bank_liquidity_vault(bank: &Pubkey) -> Pubkey {
    Pubkey::find_program_address(&[b"liquidity_vault", bank.as_ref()], &pk(MARGINFI_PROGRAM)).0
}
pub fn bank_liquidity_vault_auth(bank: &Pubkey) -> Pubkey {
    Pubkey::find_program_address(&[b"liquidity_vault_auth", bank.as_ref()], &pk(MARGINFI_PROGRAM)).0
}
pub fn bank_insurance_vault(bank: &Pubkey) -> Pubkey {
    Pubkey::find_program_address(&[b"insurance_vault", bank.as_ref()], &pk(MARGINFI_PROGRAM)).0
}

/// Bank liquidity vault + its authority. Conventional PDA seeds; overridable via
/// env if the deployed build diverged (verified by simulation).
pub fn usdc_vault() -> Pubkey {
    std::env::var("MARGINFI_USDC_VAULT")
        .ok()
        .and_then(|s| Pubkey::from_str(&s).ok())
        .unwrap_or_else(|| {
            Pubkey::find_program_address(&[b"liquidity_vault", pk(USDC_BANK).as_ref()], &pk(MARGINFI_PROGRAM)).0
        })
}
pub fn usdc_vault_authority() -> Pubkey {
    std::env::var("MARGINFI_USDC_VAULT_AUTH")
        .ok()
        .and_then(|s| Pubkey::from_str(&s).ok())
        .unwrap_or_else(|| {
            Pubkey::find_program_address(&[b"liquidity_vault_auth", pk(USDC_BANK).as_ref()], &pk(MARGINFI_PROGRAM)).0
        })
}

/// One-time: initialize a fresh MarginfiAccount (a plain keypair account, which
/// must sign THIS ix only). fee_payer usually == authority.
/// Accounts: [group, marginfi_account (W+signer), authority (signer),
///            fee_payer (W+signer), system_program].
pub fn account_initialize(marginfi_account: &Pubkey, authority: &Pubkey, fee_payer: &Pubkey) -> Instruction {
    Instruction {
        program_id: pk(MARGINFI_PROGRAM),
        accounts: vec![
            AccountMeta::new_readonly(pk(MARGINFI_GROUP), false),
            AccountMeta::new(*marginfi_account, true),
            AccountMeta::new_readonly(*authority, true),
            AccountMeta::new(*fee_payer, true),
            AccountMeta::new_readonly(pk(SYS_PROGRAM), false),
        ],
        data: DISC_ACCOUNT_INIT.to_vec(),
    }
}

/// start_flashloan: sets ACCOUNT_IN_FLASHLOAN; `end_index` = the tx-relative ix
/// index of the matching end_flashloan (validated via the instructions sysvar).
/// Accounts: [marginfi_account (W), authority (signer), ixs_sysvar].
pub fn start_flashloan(marginfi_account: &Pubkey, authority: &Pubkey, end_index: u64) -> Instruction {
    let mut data = Vec::with_capacity(16);
    data.extend_from_slice(&DISC_START_FLASHLOAN);
    data.extend_from_slice(&end_index.to_le_bytes());
    Instruction {
        program_id: pk(MARGINFI_PROGRAM),
        accounts: vec![
            AccountMeta::new(*marginfi_account, false),
            AccountMeta::new_readonly(*authority, true),
            AccountMeta::new_readonly(pk(INSTRUCTIONS_SYSVAR), false),
        ],
        data,
    }
}

/// end_flashloan: unsets the flag then runs the real health check. Pass one
/// `[bank, oracle…]` group per still-active balance as `remaining`. If the
/// flashloan nets to zero balances, `remaining` can be empty.
/// Accounts: [marginfi_account (W), authority (signer)] + remaining.
pub fn end_flashloan(marginfi_account: &Pubkey, authority: &Pubkey, remaining: &[AccountMeta]) -> Instruction {
    let mut accounts = vec![
        AccountMeta::new(*marginfi_account, false),
        AccountMeta::new_readonly(*authority, true),
    ];
    accounts.extend_from_slice(remaining);
    Instruction {
        program_id: pk(MARGINFI_PROGRAM),
        accounts,
        data: DISC_END_FLASHLOAN.to_vec(),
    }
}

/// Flash-borrow `amount` USDC base units into `dest_ata`. Inside a flashloan the
/// risk engine is skipped, so no oracle remaining-accounts here.
/// Accounts: [group, marginfi_account (W), authority (signer), bank (W),
///            dest_ata (W), vault_authority, liquidity_vault (W), token_program].
pub fn borrow_usdc(marginfi_account: &Pubkey, authority: &Pubkey, dest_ata: &Pubkey, amount: u64) -> Instruction {
    let mut data = Vec::with_capacity(16);
    data.extend_from_slice(&DISC_BORROW);
    data.extend_from_slice(&amount.to_le_bytes());
    Instruction {
        program_id: pk(MARGINFI_PROGRAM),
        accounts: vec![
            AccountMeta::new_readonly(pk(MARGINFI_GROUP), false),
            AccountMeta::new(*marginfi_account, false),
            AccountMeta::new_readonly(*authority, true),
            AccountMeta::new(pk(USDC_BANK), false),
            AccountMeta::new(*dest_ata, false),
            AccountMeta::new_readonly(usdc_vault_authority(), false),
            AccountMeta::new(usdc_vault(), false),
            AccountMeta::new_readonly(pk(TOKEN_PROGRAM), false),
        ],
        data,
    }
}

/// Repay the USDC borrow from `source_ata`. `repay_all=true` clears the entire
/// liability (principal + dust interest) regardless of `amount` and leaves any
/// surplus USDC in the ATA as profit — the correct close for a flashloan.
/// Accounts: [group, marginfi_account (W), authority (signer), bank (W),
///            source_ata (W), liquidity_vault (W), token_program].
pub fn payback_usdc(marginfi_account: &Pubkey, authority: &Pubkey, source_ata: &Pubkey, amount: u64, repay_all: bool) -> Instruction {
    let mut data = Vec::with_capacity(18);
    data.extend_from_slice(&DISC_REPAY);
    data.extend_from_slice(&amount.to_le_bytes());
    data.push(1); // Borsh Option<bool>::Some
    data.push(repay_all as u8);
    Instruction {
        program_id: pk(MARGINFI_PROGRAM),
        accounts: vec![
            AccountMeta::new_readonly(pk(MARGINFI_GROUP), false),
            AccountMeta::new(*marginfi_account, false),
            AccountMeta::new_readonly(*authority, true),
            AccountMeta::new(pk(USDC_BANK), false),
            AccountMeta::new(*source_ata, false),
            AccountMeta::new(usdc_vault(), false),
            AccountMeta::new_readonly(pk(TOKEN_PROGRAM), false),
        ],
        data,
    }
}

/// lending_account_liquidate (3-arg, VERIFIED live). Seizes `asset_amount` of
/// `asset_bank` collateral from `liquidatee` into the liquidator's account and
/// takes on the matching liability — a 2.5% liquidator bonus (+2.5% insurance).
/// Wrap this in start/end_flashloan so the liquidator init-health check is
/// skipped (that's why real liquidators pass liquidator_accounts=0).
///
/// `liquidatee_obs` = the liquidatee's observation list: for each of its active
/// balances, in balance order, `[bank(ro), oracle(ro), …]` (2 metas per normal
/// Pyth/SB bank). The vaults are all derived from `liab_bank`.
/// Fixed accounts: [group, asset_bank(W), liab_bank(W), liquidator_ma(W),
///   authority(signer), liquidatee_ma(W), liab_vault_auth, liab_vault(W),
///   liab_insurance_vault(W), token_program] then remaining
///   [asset_oracle, liab_oracle, liquidatee_obs…].
#[allow(clippy::too_many_arguments)]
pub fn lending_account_liquidate(
    asset_bank: &Pubkey,
    liab_bank: &Pubkey,
    liquidator_account: &Pubkey,
    authority: &Pubkey,
    liquidatee_account: &Pubkey,
    liab_token_program: &Pubkey,
    asset_amount: u64,
    asset_oracle: &Pubkey,
    liab_oracle: &Pubkey,
    liquidatee_obs: &[AccountMeta],
) -> Instruction {
    let mut data = Vec::with_capacity(18);
    data.extend_from_slice(&DISC_LIQUIDATE);
    data.extend_from_slice(&asset_amount.to_le_bytes());
    data.push(liquidatee_obs.len() as u8); // liquidatee_accounts
    data.push(0u8); // liquidator_accounts = 0 (init-health skipped in flashloan)

    let mut accounts = vec![
        AccountMeta::new_readonly(pk(MARGINFI_GROUP), false),
        AccountMeta::new(*asset_bank, false),
        AccountMeta::new(*liab_bank, false),
        AccountMeta::new(*liquidator_account, false),
        AccountMeta::new_readonly(*authority, true),
        AccountMeta::new(*liquidatee_account, false),
        AccountMeta::new_readonly(bank_liquidity_vault_auth(liab_bank), false),
        AccountMeta::new(bank_liquidity_vault(liab_bank), false),
        AccountMeta::new(bank_insurance_vault(liab_bank), false),
        AccountMeta::new_readonly(*liab_token_program, false),
        // remaining: front oracle block (asset then liab), then liquidatee obs.
        AccountMeta::new_readonly(*asset_oracle, false),
        AccountMeta::new_readonly(*liab_oracle, false),
    ];
    accounts.extend_from_slice(liquidatee_obs);
    Instruction { program_id: pk(MARGINFI_PROGRAM), accounts, data }
}

/// lending_account_withdraw. `withdraw_all=Some(true)` closes the balance and
/// takes everything (amount ignored). Inside a flashloan no observation list is
/// needed (init-health skipped). Accounts: [group, marginfi_account(W),
///   authority(signer), bank(W), dest_ata(W), vault_auth, vault(W), token_program].
#[allow(clippy::too_many_arguments)]
pub fn lending_account_withdraw(
    marginfi_account: &Pubkey,
    authority: &Pubkey,
    bank: &Pubkey,
    dest_ata: &Pubkey,
    token_program: &Pubkey,
    amount: u64,
    withdraw_all: bool,
) -> Instruction {
    let mut data = Vec::with_capacity(18);
    data.extend_from_slice(&DISC_WITHDRAW);
    data.extend_from_slice(&amount.to_le_bytes());
    data.push(1); // Option::Some
    data.push(withdraw_all as u8);
    Instruction {
        program_id: pk(MARGINFI_PROGRAM),
        accounts: vec![
            AccountMeta::new_readonly(pk(MARGINFI_GROUP), false),
            AccountMeta::new(*marginfi_account, false),
            AccountMeta::new_readonly(*authority, true),
            AccountMeta::new(*bank, false),
            AccountMeta::new(*dest_ata, false),
            AccountMeta::new_readonly(bank_liquidity_vault_auth(bank), false),
            AccountMeta::new(bank_liquidity_vault(bank), false),
            AccountMeta::new_readonly(*token_program, false),
        ],
        data,
    }
}
