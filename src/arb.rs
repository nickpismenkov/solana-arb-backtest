//! The guarded flash-loan arb transaction, as a reusable builder (extracted
//! from the verified arb_probe). Resolves both pools' accounts, assembles
//! [CU, create ATAs, flash-borrow USDC, leg1 USDC→base exact-in, leg2
//! base→USDC EXACT-OUT=repay, flash-payback] in either direction, and compiles
//! a v0 tx against the ALT so it fits 1232 bytes. Leg 2 exact-out for exactly
//! the repay amount is the profit-or-revert guard. Verified on mainnet (PR #16).

use anyhow::{anyhow, Result};
use solana_hash::Hash;
use solana_instruction::Instruction;
use solana_message::{v0, AddressLookupTableAccount, VersionedMessage};
use solana_pubkey::Pubkey;
use solana_transaction::versioned::VersionedTransaction;
use std::str::FromStr;

use crate::execute::{
    decode_orca_state, decode_ray_state, orca_start_index, orca_tick_array, ray_start_index,
    ray_tick_array,
};
use crate::flashloan::{ata_for, borrow_usdc, create_ata_idempotent_for, payback_usdc, USDC_MINT};
use crate::pools::pair;
use crate::swap::{orca_oracle, orca_swap_ix, orca_swap_v2_ix, ray_swap_ix, ray_swap_v2_ix, sqrt_limit, OrcaSwapAccounts, RaySwapAccounts};

const COMPUTE_BUDGET: &str = "ComputeBudget111111111111111111111111111111";

fn pk(s: &str) -> Pubkey {
    Pubkey::from_str(s).unwrap()
}
pub fn pk_at(d: &[u8], o: usize) -> Pubkey {
    Pubkey::try_from(&d[o..o + 32]).unwrap()
}

pub fn cu_limit_ix(units: u32) -> Instruction {
    let mut data = vec![0x02];
    data.extend_from_slice(&units.to_le_bytes());
    Instruction { program_id: pk(COMPUTE_BUDGET), accounts: vec![], data }
}

pub fn cu_price_ix(micro_lamports: u64) -> Instruction {
    let mut data = vec![0x03];
    data.extend_from_slice(&micro_lamports.to_le_bytes());
    Instruction { program_id: pk(COMPUTE_BUDGET), accounts: vec![], data }
}

const SYS_PROGRAM: &str = "11111111111111111111111111111111";

/// System-program transfer (Jito tip). Inside the arb tx so a revert pays no tip.
pub fn transfer_ix(from: Pubkey, to: Pubkey, lamports: u64) -> Instruction {
    let mut data = vec![2u8, 0, 0, 0]; // SystemInstruction::Transfer
    data.extend_from_slice(&lamports.to_le_bytes());
    Instruction {
        program_id: pk(SYS_PROGRAM),
        accounts: vec![
            solana_instruction::AccountMeta::new(from, true),
            solana_instruction::AccountMeta::new(to, false),
        ],
        data,
    }
}

/// Both pools' raw account data, fetched once per build.
pub struct PoolData {
    pub orca: Vec<u8>,
    pub ray: Vec<u8>,
}

/// Token program owning `mint`: base_tp if it's the base mint, else quote_tp.
pub fn tp_of(mint: &Pubkey, base: &Pubkey, base_tp: Pubkey, quote_tp: Pubkey) -> Pubkey {
    if mint == base { base_tp } else { quote_tp }
}

/// Resolve the Orca swap accounts for our wallet at the current tick.
/// `a_to_b` selects the tick-array traversal direction. ATAs are derived under
/// each mint's owning token program (classic or Token-2022).
pub fn orca_accounts(od: &[u8], orca_pk: Pubkey, signer: Pubkey, a_to_b: bool, base: Pubkey, base_tp: Pubkey, quote_tp: Pubkey) -> OrcaSwapAccounts {
    let ost = decode_orca_state(od).expect("orca state");
    let mint_a = pk_at(od, 101);
    let mint_b = pk_at(od, 181);
    let n = 88 * ost.tick_spacing as i32;
    let start = orca_start_index(ost.tick, ost.tick_spacing);
    let starts = if a_to_b {
        [start, start - n, start - 2 * n]
    } else {
        [start, start + n, start + 2 * n]
    };
    OrcaSwapAccounts {
        whirlpool: orca_pk,
        token_authority: signer,
        token_owner_a: ata_for(&signer, &mint_a, &tp_of(&mint_a, &base, base_tp, quote_tp)),
        token_vault_a: pk_at(od, 133),
        token_owner_b: ata_for(&signer, &mint_b, &tp_of(&mint_b, &base, base_tp, quote_tp)),
        token_vault_b: pk_at(od, 213),
        tick_arrays: [
            orca_tick_array(&orca_pk, starts[0]),
            orca_tick_array(&orca_pk, starts[1]),
            orca_tick_array(&orca_pk, starts[2]),
        ],
        oracle: orca_oracle(&orca_pk),
    }
}

fn ray_accounts(rd: &[u8], ray_pk: Pubkey, signer: Pubkey, base: Pubkey, usdc: Pubkey, base_in: bool, base_tp: Pubkey, quote_tp: Pubkey) -> RaySwapAccounts {
    let rst = decode_ray_state(rd).expect("ray state");
    let mint0 = pk_at(rd, 73);
    let base_is_0 = mint0 == base;
    let (base_vault, quote_vault) = if base_is_0 {
        (pk_at(rd, 137), pk_at(rd, 169))
    } else {
        (pk_at(rd, 169), pk_at(rd, 137))
    };
    // base_in = leg spends base (sell); else spends USDC (buy). ATAs under the
    // owning token program per mint.
    let base_ata = ata_for(&signer, &base, &base_tp);
    let usdc_ata = ata_for(&signer, &usdc, &quote_tp);
    let (input_vault, output_vault, input_ata, output_ata) = if base_in {
        (base_vault, quote_vault, base_ata, usdc_ata)
    } else {
        (quote_vault, base_vault, usdc_ata, base_ata)
    };
    // Tick-array traversal: input mint == token0 → price/tick decreases
    // (zero-for-one) → arrays descend from the current one; else ascend.
    let zero_for_one = if base_in { base_is_0 } else { !base_is_0 };
    let n = 60 * rst.tick_spacing as i32;
    let rstart = ray_start_index(rst.tick, rst.tick_spacing);
    let starts = if zero_for_one {
        [rstart, rstart - n, rstart - 2 * n]
    } else {
        [rstart, rstart + n, rstart + 2 * n]
    };
    RaySwapAccounts {
        payer: signer,
        amm_config: pk_at(rd, 9),
        pool_state: ray_pk,
        input_token_account: input_ata,
        output_token_account: output_ata,
        input_vault,
        output_vault,
        observation_state: pk_at(rd, 201),
        tick_arrays: [
            ray_tick_array(&ray_pk, starts[0]),
            ray_tick_array(&ray_pk, starts[1]),
            ray_tick_array(&ray_pk, starts[2]),
        ],
    }
}

/// Build the unsigned guarded arb v0 tx. `orca_first = true` buys base on Orca
/// (leg1) and sells on Raydium (leg2); false is the reverse. `blockhash` should
/// be a real recent hash for live submission, or default for replace-blockhash
/// simulation. Returns the unsigned tx (sign before sending).
#[allow(clippy::too_many_arguments)]
pub fn build_arb_tx(
    pools: &PoolData,
    signer: Pubkey,
    alt: &AddressLookupTableAccount,
    borrow_amount: u64,
    orca_first: bool,
    tip_account: Option<Pubkey>,
    tip_lamports: u64,
    priority_micro_lamports: u64,
    blockhash: Hash,
    repay_buffer: u64,
) -> Result<VersionedTransaction> {
    let cfg = pair();
    let usdc = pk(USDC_MINT);
    let base = pk(&cfg.base_mint);
    let orca_pk = pk(&cfg.orca_pool);
    let ray_pk = pk(&cfg.ray_pool);
    let base_tp = pk(&cfg.base_token_program);
    let quote_tp = pk(&cfg.quote_token_program);
    let v2 = cfg.needs_swap_v2(); // Token-2022 leg → swapV2
    let mint_a = pk_at(&pools.orca, 101);
    let mint_b = pk_at(&pools.orca, 181);
    let base_is_a_orca = mint_a == base;
    let tp_a = tp_of(&mint_a, &base, base_tp, quote_tp);
    let tp_b = tp_of(&mint_b, &base, base_tp, quote_tp);

    // Orca leg helper: swapV2 when the pair has a Token-2022 side, else classic.
    let orca_leg = |a_to_b: bool, amount: u64, threshold: u64, exact_in: bool| -> Instruction {
        let oa = orca_accounts(&pools.orca, orca_pk, signer, a_to_b, base, base_tp, quote_tp);
        if v2 {
            orca_swap_v2_ix(&oa, mint_a, mint_b, tp_a, tp_b, amount, threshold, sqrt_limit(a_to_b), exact_in, a_to_b)
        } else {
            orca_swap_ix(&oa, amount, threshold, sqrt_limit(a_to_b), exact_in, a_to_b)
        }
    };
    // Ray leg helper: base_in = the leg spends base (sell). input/output mints
    // follow the direction so swapV2 gets the right vault mints.
    let ray_leg = |base_in: bool, amount: u64, threshold: u64, is_base_input: bool| -> Instruction {
        let ra = ray_accounts(&pools.ray, ray_pk, signer, base, usdc, base_in, base_tp, quote_tp);
        let (in_mint, out_mint) = if base_in { (base, usdc) } else { (usdc, base) };
        if v2 {
            ray_swap_v2_ix(&ra, in_mint, out_mint, amount, threshold, 0, is_base_input)
        } else {
            ray_swap_ix(&ra, amount, threshold, 0, is_base_input)
        }
    };

    // leg1 = buy base with USDC (exact-in borrow_amount); leg2 = sell base for
    // USDC (exact-out = borrow + repay_buffer) — the guard. The buffer forces
    // the tx to produce enough surplus USDC to cover the tip + fees, so a landed
    // trade is always net-positive; if the gap is too small, leg2 can't produce
    // borrow+buffer and reverts → bundle fails for free.
    let leg2_out = borrow_amount.saturating_add(repay_buffer);
    let (leg1, leg2) = if orca_first {
        // Orca buy (input USDC → a_to_b = !base_is_a); Ray sell base exact-out.
        let l1 = orca_leg(!base_is_a_orca, borrow_amount, 0, true);
        let l2 = ray_leg(true, leg2_out, u64::MAX, false);
        (l1, l2)
    } else {
        // Ray buy base exact-in; Orca sell base exact-out (a_to_b = base_is_a).
        let l1 = ray_leg(false, borrow_amount, 0, true);
        let l2 = orca_leg(base_is_a_orca, leg2_out, u64::MAX, false);
        (l1, l2)
    };

    let mut ixs = vec![
        cu_limit_ix(600_000),
        cu_price_ix(priority_micro_lamports),
        create_ata_idempotent_for(&signer, &usdc, &quote_tp),
        create_ata_idempotent_for(&signer, &base, &base_tp),
        borrow_usdc(&signer, borrow_amount),
        leg1,
        leg2,
        payback_usdc(&signer, borrow_amount),
    ];
    // Tip transfer to a Jito tip account, inside the tx → only pays if it lands.
    if let (Some(tip_to), true) = (tip_account, tip_lamports > 0) {
        ixs.push(transfer_ix(signer, tip_to, tip_lamports));
    }

    let msg = v0::Message::try_compile(&signer, &ixs, std::slice::from_ref(alt), blockhash)
        .map_err(|e| anyhow!("compile v0: {e}"))?;
    Ok(VersionedTransaction {
        signatures: vec![solana_signature::Signature::default()],
        message: VersionedMessage::V0(msg),
    })
}

/// Load an ALT account into the form v0 message compilation needs.
pub fn load_alt(alt_addr: &str, alt_account_data: &[u8]) -> AddressLookupTableAccount {
    let addresses: Vec<Pubkey> = alt_account_data[56..]
        .chunks_exact(32)
        .map(|c| Pubkey::try_from(c).unwrap())
        .collect();
    AddressLookupTableAccount { key: pk(alt_addr), addresses }
}
