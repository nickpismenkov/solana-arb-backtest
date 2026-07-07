//! Kamino (KLend) liquidation instructions, derived from a REAL mainnet
//! liquidation tx (see kamino_liq_decode) — layouts are program-fixed, so one
//! captured sample pins them. Discriminators and the 25-account liquidate
//! layout are verified against tx pBAF8kTU… (bSoL liquidator, USDC→bSOL).
//!
//! A Kamino liquidation is a 3-instruction sequence, all in one tx:
//!   refresh_reserve(repay_reserve)  +  refresh_reserve(withdraw_reserve)
//!   refresh_obligation(obligation, [reserves…])
//!   liquidate_obligation_and_redeem_reserve_collateral_v2
//! The liquidate seizes collateral and immediately redeems the cTokens to the
//! underlying liquidity token into the liquidator's ATA (so the swap leg sells
//! the underlying, not a cToken).
//!
//! Oracle wiring: main-market reserves price via Scope (scope_prices account
//! at reserve offset 5112); the pyth/switchboard refresh slots are None
//! (KLend-program placeholders). ReserveAccounts::decode pulls every account a
//! refresh/liquidate needs straight out of the Reserve bytes.

use solana_instruction::{AccountMeta, Instruction};
use solana_pubkey::Pubkey;
use std::str::FromStr;

pub const KLEND_PROGRAM: &str = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD";
pub const FARMS_PROGRAM: &str = "FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr";
pub const SYSVAR_INSTRUCTIONS: &str = "Sysvar1nstructions1111111111111111111111111";

// VERIFIED discriminators (from the captured tx).
const DISC_REFRESH_RESERVE: [u8; 8] = [0x02, 0xda, 0x8a, 0xeb, 0x4f, 0xc9, 0x19, 0x66];
const DISC_REFRESH_OBLIGATION: [u8; 8] = [0x21, 0x84, 0x93, 0xe4, 0x97, 0xc0, 0x48, 0x59];
const DISC_LIQUIDATE_V2: [u8; 8] = [0xa2, 0xa1, 0x23, 0x8f, 0x1e, 0xbb, 0xb9, 0x67];

// Reserve account-field offsets (VERIFIED by locating known pubkeys in a real
// 8624-byte reserve).
const R_LENDING_MARKET: usize = 32;
const R_LIQ_MINT: usize = 128;
const R_LIQ_SUPPLY: usize = 160;
const R_FEE_RECEIVER: usize = 192;
const R_COLL_MINT: usize = 2560;
const R_COLL_SUPPLY: usize = 2600;
const R_SCOPE_PRICES: usize = 5112;

fn pk(s: &str) -> Pubkey { Pubkey::from_str(s).unwrap() }
fn pk_at(d: &[u8], off: usize) -> Pubkey {
    Pubkey::try_from(&d[off..off + 32]).unwrap()
}

/// Every account of one reserve that a refresh/liquidate touches, pulled from
/// the reserve account bytes.
#[derive(Clone, Debug)]
pub struct ReserveAccounts {
    pub reserve: Pubkey,
    pub lending_market: Pubkey,
    pub liquidity_mint: Pubkey,
    pub liquidity_supply: Pubkey,
    pub fee_receiver: Pubkey,
    pub collateral_mint: Pubkey,
    pub collateral_supply: Pubkey,
    pub scope_prices: Pubkey,
}

impl ReserveAccounts {
    pub fn decode(reserve: Pubkey, data: &[u8]) -> Option<ReserveAccounts> {
        if data.len() < R_SCOPE_PRICES + 32 { return None; }
        Some(ReserveAccounts {
            reserve,
            lending_market: pk_at(data, R_LENDING_MARKET),
            liquidity_mint: pk_at(data, R_LIQ_MINT),
            liquidity_supply: pk_at(data, R_LIQ_SUPPLY),
            fee_receiver: pk_at(data, R_FEE_RECEIVER),
            collateral_mint: pk_at(data, R_COLL_MINT),
            collateral_supply: pk_at(data, R_COLL_SUPPLY),
            scope_prices: pk_at(data, R_SCOPE_PRICES),
        })
    }
}

/// lending_market_authority PDA (seed "lma"), VERIFIED against the captured tx.
pub fn lending_market_authority(lending_market: &Pubkey) -> Pubkey {
    Pubkey::find_program_address(&[b"lma", lending_market.as_ref()], &pk(KLEND_PROGRAM)).0
}

/// refresh_reserve. Scope-priced reserves pass the scope_prices account and
/// leave pyth/switchboard slots as the KLend program (Anchor `None`).
/// Accounts: [reserve(W), lending_market, pyth?, sb_price?, sb_twap?, scope?].
pub fn refresh_reserve(r: &ReserveAccounts) -> Instruction {
    let klend = pk(KLEND_PROGRAM);
    Instruction {
        program_id: klend,
        accounts: vec![
            AccountMeta::new(r.reserve, false),
            AccountMeta::new_readonly(r.lending_market, false),
            AccountMeta::new_readonly(klend, false),          // pyth = None
            AccountMeta::new_readonly(klend, false),          // switchboard price = None
            AccountMeta::new_readonly(klend, false),          // switchboard twap = None
            AccountMeta::new_readonly(r.scope_prices, false), // scope
        ],
        data: DISC_REFRESH_RESERVE.to_vec(),
    }
}

/// refresh_obligation. Accounts: [lending_market, obligation(W)] then each of
/// the obligation's reserves (deposits then borrows, in slot order) as
/// read-only remaining accounts.
pub fn refresh_obligation(lending_market: &Pubkey, obligation: &Pubkey, reserves: &[Pubkey]) -> Instruction {
    let mut accounts = vec![
        AccountMeta::new_readonly(*lending_market, false),
        AccountMeta::new(*obligation, false),
    ];
    for r in reserves { accounts.push(AccountMeta::new_readonly(*r, false)); }
    Instruction { program_id: pk(KLEND_PROGRAM), accounts, data: DISC_REFRESH_OBLIGATION.to_vec() }
}

/// liquidate_obligation_and_redeem_reserve_collateral_v2. Seizes and redeems in
/// one ix. data = disc + liquidity_amount(u64) + min_acceptable_received(u64) +
/// max_allowed_ltv_override_pct(u64). 25 accounts, VERIFIED layout:
///   [0] liquidator (signer)          [13] user_dest_collateral (W)
///   [1] obligation (W)               [14] user_dest_liquidity (W)
///   [2] lending_market               [15] user_source_liquidity (W, the repay)
///   [3] lending_market_authority     [16] collateral_token_program
///   [4] repay_reserve (W)            [17] repay_liquidity_token_program
///   [5] repay_liquidity_mint         [18] withdraw_liquidity_token_program
///   [6] repay_liquidity_supply (W)   [19] instructions sysvar
///   [7] withdraw_reserve (W)         [20..23] KLend placeholders (opt None)
///   [8] withdraw_liquidity_mint      [24] farms program
///   [9] withdraw_collateral_mint (W)
///   [10] withdraw_collateral_supply (W)
///   [11] withdraw_liquidity_supply (W)
///   [12] withdraw_fee_receiver (W)
#[allow(clippy::too_many_arguments)]
pub fn liquidate_and_redeem_v2(
    liquidator: &Pubkey,
    obligation: &Pubkey,
    lending_market: &Pubkey,
    repay: &ReserveAccounts,
    withdraw: &ReserveAccounts,
    user_dest_collateral: &Pubkey,
    user_dest_liquidity: &Pubkey,
    user_source_liquidity: &Pubkey,
    collateral_token_program: &Pubkey,
    repay_liquidity_token_program: &Pubkey,
    withdraw_liquidity_token_program: &Pubkey,
    liquidity_amount: u64,
    min_acceptable_received_liquidity: u64,
    max_allowed_ltv_override_pct: u64,
) -> Instruction {
    let klend = pk(KLEND_PROGRAM);
    let mut data = Vec::with_capacity(32);
    data.extend_from_slice(&DISC_LIQUIDATE_V2);
    data.extend_from_slice(&liquidity_amount.to_le_bytes());
    data.extend_from_slice(&min_acceptable_received_liquidity.to_le_bytes());
    data.extend_from_slice(&max_allowed_ltv_override_pct.to_le_bytes());
    let accounts = vec![
        AccountMeta::new_readonly(*liquidator, true),
        AccountMeta::new(*obligation, false),
        AccountMeta::new_readonly(*lending_market, false),
        AccountMeta::new_readonly(lending_market_authority(lending_market), false),
        AccountMeta::new(repay.reserve, false),
        AccountMeta::new_readonly(repay.liquidity_mint, false),
        AccountMeta::new(repay.liquidity_supply, false),
        AccountMeta::new(withdraw.reserve, false),
        AccountMeta::new_readonly(withdraw.liquidity_mint, false),
        AccountMeta::new(withdraw.collateral_mint, false),
        AccountMeta::new(withdraw.collateral_supply, false),
        AccountMeta::new(withdraw.liquidity_supply, false),
        AccountMeta::new(withdraw.fee_receiver, false),
        AccountMeta::new(*user_dest_collateral, false),
        AccountMeta::new(*user_dest_liquidity, false),
        AccountMeta::new(*user_source_liquidity, false),
        AccountMeta::new_readonly(*collateral_token_program, false),
        AccountMeta::new_readonly(*repay_liquidity_token_program, false),
        AccountMeta::new_readonly(*withdraw_liquidity_token_program, false),
        AccountMeta::new_readonly(pk(SYSVAR_INSTRUCTIONS), false),
        AccountMeta::new_readonly(klend, false),
        AccountMeta::new_readonly(klend, false),
        AccountMeta::new_readonly(klend, false),
        AccountMeta::new_readonly(klend, false),
        AccountMeta::new_readonly(pk(FARMS_PROGRAM), false),
    ];
    Instruction { program_id: klend, accounts, data }
}
