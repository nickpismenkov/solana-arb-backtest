//! Swap instruction builders for Orca Whirlpool and Raydium CLMM, built
//! directly (no aggregator, no network hop) so they fit the shred-reaction
//! budget. Account orders follow each program's on-chain layout; the exact
//! metas are VERIFIED against mainnet by `swap_probe` (simulate a real swap)
//! before any of this is wired into a live bundle — same discipline as
//! tickarray_probe. Until that probe passes, treat these as unverified.

use solana_instruction::{AccountMeta, Instruction};
use solana_pubkey::Pubkey;
use std::str::FromStr;

use crate::decode::{ORCA_PROGRAM, RAY_CLMM_PROGRAM};

// Anchor "global:swap" sighash — shared by both programs (disambiguated by
// program id, matching decode.rs).
const DISC_SWAP: [u8; 8] = [0xf8, 0xc6, 0x9e, 0x91, 0xe1, 0x75, 0x87, 0xc8];
// "global:swap_v2" sighash — shared by Orca + Raydium CLMM. Handles Token-2022
// mints (passes both token programs + mint accounts). Verified vs both IDLs.
const DISC_SWAP_V2: [u8; 8] = [0x2b, 0x04, 0xed, 0x0b, 0x1a, 0xc9, 0x1e, 0x62];

const TOKEN_PROGRAM: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
const TOKEN_2022_PROGRAM: &str = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb";
const MEMO_PROGRAM: &str = "MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr";

/// Accounts the caller resolves once (from pool state + our ATAs) and reuses.
pub struct OrcaSwapAccounts {
    pub whirlpool: Pubkey,
    pub token_authority: Pubkey, // our wallet (signer)
    pub token_owner_a: Pubkey,   // our ATA for mintA
    pub token_vault_a: Pubkey,
    pub token_owner_b: Pubkey, // our ATA for mintB
    pub token_vault_b: Pubkey,
    pub tick_arrays: [Pubkey; 3],
    pub oracle: Pubkey, // PDA ["oracle", whirlpool]
}

/// Orca `swap`: data = disc + amount + other_amount_threshold + sqrt_price_limit
/// + amount_specified_is_input + a_to_b. `exact_in`: true → amount is input,
/// threshold is min-out; false → amount is desired output, threshold is max-in.
pub fn orca_swap_ix(
    a: &OrcaSwapAccounts,
    amount: u64,
    threshold: u64,
    sqrt_price_limit: u128,
    exact_in: bool,
    a_to_b: bool,
) -> Instruction {
    let mut data = Vec::with_capacity(8 + 8 + 8 + 16 + 1 + 1);
    data.extend_from_slice(&DISC_SWAP);
    data.extend_from_slice(&amount.to_le_bytes());
    data.extend_from_slice(&threshold.to_le_bytes());
    data.extend_from_slice(&sqrt_price_limit.to_le_bytes());
    data.push(exact_in as u8); // amount_specified_is_input
    data.push(a_to_b as u8);

    let tok = Pubkey::from_str(TOKEN_PROGRAM).unwrap();
    let metas = vec![
        AccountMeta::new_readonly(tok, false),
        AccountMeta::new_readonly(a.token_authority, true),
        AccountMeta::new(a.whirlpool, false),
        AccountMeta::new(a.token_owner_a, false),
        AccountMeta::new(a.token_vault_a, false),
        AccountMeta::new(a.token_owner_b, false),
        AccountMeta::new(a.token_vault_b, false),
        AccountMeta::new(a.tick_arrays[0], false),
        AccountMeta::new(a.tick_arrays[1], false),
        AccountMeta::new(a.tick_arrays[2], false),
        AccountMeta::new(a.oracle, false),
    ];
    Instruction {
        program_id: Pubkey::from_str(ORCA_PROGRAM).unwrap(),
        accounts: metas,
        data,
    }
}

/// Orca `swap_v2` (Token-2022-aware). Same args as `swap` plus a trailing
/// `remaining_accounts_info: Option<..>` (we send `None` = 0x00). Accounts add
/// token_program_a/b, memo, mint_a/b at the front/after-whirlpool; oracle is
/// writable. `tp_a`/`tp_b` are the token programs owning mintA/mintB.
#[allow(clippy::too_many_arguments)]
pub fn orca_swap_v2_ix(
    a: &OrcaSwapAccounts,
    mint_a: Pubkey,
    mint_b: Pubkey,
    tp_a: Pubkey,
    tp_b: Pubkey,
    amount: u64,
    threshold: u64,
    sqrt_price_limit: u128,
    exact_in: bool,
    a_to_b: bool,
) -> Instruction {
    let mut data = Vec::with_capacity(8 + 8 + 8 + 16 + 1 + 1 + 1);
    data.extend_from_slice(&DISC_SWAP_V2);
    data.extend_from_slice(&amount.to_le_bytes());
    data.extend_from_slice(&threshold.to_le_bytes());
    data.extend_from_slice(&sqrt_price_limit.to_le_bytes());
    data.push(exact_in as u8);
    data.push(a_to_b as u8);
    data.push(0); // remaining_accounts_info = None

    let memo = Pubkey::from_str(MEMO_PROGRAM).unwrap();
    let metas = vec![
        AccountMeta::new_readonly(tp_a, false),
        AccountMeta::new_readonly(tp_b, false),
        AccountMeta::new_readonly(memo, false),
        AccountMeta::new_readonly(a.token_authority, true),
        AccountMeta::new(a.whirlpool, false),
        AccountMeta::new_readonly(mint_a, false),
        AccountMeta::new_readonly(mint_b, false),
        AccountMeta::new(a.token_owner_a, false),
        AccountMeta::new(a.token_vault_a, false),
        AccountMeta::new(a.token_owner_b, false),
        AccountMeta::new(a.token_vault_b, false),
        AccountMeta::new(a.tick_arrays[0], false),
        AccountMeta::new(a.tick_arrays[1], false),
        AccountMeta::new(a.tick_arrays[2], false),
        AccountMeta::new(a.oracle, false),
    ];
    Instruction { program_id: Pubkey::from_str(ORCA_PROGRAM).unwrap(), accounts: metas, data }
}

/// Orca oracle PDA: seeds ["oracle", whirlpool].
pub fn orca_oracle(whirlpool: &Pubkey) -> Pubkey {
    Pubkey::find_program_address(
        &[b"oracle", whirlpool.as_ref()],
        &Pubkey::from_str(ORCA_PROGRAM).unwrap(),
    )
    .0
}

pub struct RaySwapAccounts {
    pub payer: Pubkey, // our wallet (signer)
    pub amm_config: Pubkey,
    pub pool_state: Pubkey,
    pub input_token_account: Pubkey,
    pub output_token_account: Pubkey,
    pub input_vault: Pubkey,
    pub output_vault: Pubkey,
    pub observation_state: Pubkey,
    /// Current tick array first, then the next two in traversal direction —
    /// Raydium walks them as remaining accounts and errors with
    /// NotEnoughTickArrayAccount (6023) if the walk runs past what's provided.
    pub tick_arrays: [Pubkey; 3],
}

/// Raydium CLMM `swap`: data = disc + amount + other_amount_threshold
/// + sqrt_price_limit_x64 + is_base_input. `is_base_input`: true → amount is
/// input (exact-in), threshold is min-out; false → amount is output (exact-out),
/// threshold is max-in.
pub fn ray_swap_ix(
    a: &RaySwapAccounts,
    amount: u64,
    threshold: u64,
    sqrt_price_limit_x64: u128,
    is_base_input: bool,
) -> Instruction {
    let mut data = Vec::with_capacity(8 + 8 + 8 + 16 + 1);
    data.extend_from_slice(&DISC_SWAP);
    data.extend_from_slice(&amount.to_le_bytes());
    data.extend_from_slice(&threshold.to_le_bytes());
    data.extend_from_slice(&sqrt_price_limit_x64.to_le_bytes());
    data.push(is_base_input as u8);

    let tok = Pubkey::from_str(TOKEN_PROGRAM).unwrap();
    let metas = vec![
        AccountMeta::new_readonly(a.payer, true),
        AccountMeta::new_readonly(a.amm_config, false),
        AccountMeta::new(a.pool_state, false),
        AccountMeta::new(a.input_token_account, false),
        AccountMeta::new(a.output_token_account, false),
        AccountMeta::new(a.input_vault, false),
        AccountMeta::new(a.output_vault, false),
        AccountMeta::new(a.observation_state, false),
        AccountMeta::new_readonly(tok, false),
        AccountMeta::new(a.tick_arrays[0], false),
        AccountMeta::new(a.tick_arrays[1], false),
        AccountMeta::new(a.tick_arrays[2], false),
    ];
    Instruction {
        program_id: Pubkey::from_str(RAY_CLMM_PROGRAM).unwrap(),
        accounts: metas,
        data,
    }
}

/// Raydium CLMM `swap_v2` (Token-2022-aware). Args identical to `swap`. Accounts
/// add token_program_2022, memo, and input/output vault MINTS; there is NO named
/// tick_array — all tick arrays are remaining accounts (writable). Both token
/// programs (classic + 2022) are always passed. `input_mint`/`output_mint` must
/// match input_vault/output_vault.
#[allow(clippy::too_many_arguments)]
pub fn ray_swap_v2_ix(
    a: &RaySwapAccounts,
    input_mint: Pubkey,
    output_mint: Pubkey,
    amount: u64,
    threshold: u64,
    sqrt_price_limit_x64: u128,
    is_base_input: bool,
) -> Instruction {
    let mut data = Vec::with_capacity(8 + 8 + 8 + 16 + 1);
    data.extend_from_slice(&DISC_SWAP_V2);
    data.extend_from_slice(&amount.to_le_bytes());
    data.extend_from_slice(&threshold.to_le_bytes());
    data.extend_from_slice(&sqrt_price_limit_x64.to_le_bytes());
    data.push(is_base_input as u8);

    let tok = Pubkey::from_str(TOKEN_PROGRAM).unwrap();
    let tok22 = Pubkey::from_str(TOKEN_2022_PROGRAM).unwrap();
    let memo = Pubkey::from_str(MEMO_PROGRAM).unwrap();
    let metas = vec![
        AccountMeta::new_readonly(a.payer, true),
        AccountMeta::new_readonly(a.amm_config, false),
        AccountMeta::new(a.pool_state, false),
        AccountMeta::new(a.input_token_account, false),
        AccountMeta::new(a.output_token_account, false),
        AccountMeta::new(a.input_vault, false),
        AccountMeta::new(a.output_vault, false),
        AccountMeta::new(a.observation_state, false),
        AccountMeta::new_readonly(tok, false),
        AccountMeta::new_readonly(tok22, false),
        AccountMeta::new_readonly(memo, false),
        AccountMeta::new_readonly(input_mint, false),
        AccountMeta::new_readonly(output_mint, false),
        // Tick arrays as remaining accounts (writable).
        AccountMeta::new(a.tick_arrays[0], false),
        AccountMeta::new(a.tick_arrays[1], false),
        AccountMeta::new(a.tick_arrays[2], false),
    ];
    Instruction {
        program_id: Pubkey::from_str(RAY_CLMM_PROGRAM).unwrap(),
        accounts: metas,
        data,
    }
}

/// Price-limit sentinels: no slippage cap at the swap level (we guard on the
/// flash-repay min-out instead). Orca uses Q64.64 bounds; Raydium Q64.64 too.
pub const MIN_SQRT_PRICE: u128 = 4295048016;
pub const MAX_SQRT_PRICE: u128 = 79226673515401279992447579055;

/// For an a_to_b (price-decreasing) swap the limit is the min; else the max.
pub fn sqrt_limit(a_to_b: bool) -> u128 {
    if a_to_b {
        MIN_SQRT_PRICE
    } else {
        MAX_SQRT_PRICE
    }
}
