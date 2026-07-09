//! Jupiter Lend (Fluid) flash-loan instructions — 0 bp. Ported from the exact
//! output of the @jup-ag/lend SDK's getFlashloanIx (captured on mainnet, then
//! diffed across signers): for the USDC "main" market only accounts 0 (signer)
//! and 2 (signer's USDC ATA) vary; the other twelve are market constants. The
//! program matches borrow↔payback via the instructions sysvar, so we just place
//! borrow before payback in the tx — no index math. Verified by flashloan_probe
//! (borrow → payback simulates clean).

use solana_instruction::{AccountMeta, Instruction};
use solana_pubkey::Pubkey;
use std::str::FromStr;

pub const JUP_LEND_PROGRAM: &str = "jupgfSgfuAXv4B6R2Uxu85Z1qdzgju79s6MfZekN6XS";
pub const USDC_MINT: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
pub const USDT_MINT: &str = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB";
pub const WSOL_MINT: &str = "So11111111111111111111111111111111111111112";
pub const TOKEN_PROGRAM: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
pub const TOKEN_2022_PROGRAM: &str = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb";
const ATA_PROGRAM: &str = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL";
const SYS_PROGRAM: &str = "11111111111111111111111111111111";
const INSTRUCTIONS_SYSVAR: &str = "Sysvar1nstructions1111111111111111111111111";

// Anchor discriminators (first 8 bytes of each ix's data).
const DISC_BORROW: [u8; 8] = [0x67, 0x13, 0x4e, 0x18, 0xf0, 0x09, 0x87, 0x3f];
const DISC_PAYBACK: [u8; 8] = [0xd5, 0x2f, 0x99, 0x89, 0x54, 0xf3, 0x5e, 0xe8];

// Accounts shared by EVERY market (indices 1,8,9 in the ix). The lending admin
// (idx 1) has no per-mint field; idx 8 = PDA(["liquidity"], vault-program);
// idx 9 = the vault program id itself.
const M_LENDING: &str = "ALXWtv2P4GqH1B7Lq731joag52yRBRqmHV4naiXPTYWL"; // idx 1  (W)  global
const M_LIQUIDITY: &str = "7s1da8DduuBFqGra5bJBjpnvL5E9mGzCuMk1Qkh4or2Z"; // idx 8  (r)  global
const M_VAULT_PROGRAM: &str = "jupeiUmn818Jg1ekPURTpr4mFo29p46vygyykFJ3wZC"; // idx 9  (r)  global

/// The four per-asset flash-market accounts (indices 4,5,6,7). Verified against
/// the live USDC set (self-check reproduces the SDK-captured constants) and
/// derived the same way for USDT/wSOL:
///   idx4 reserve   = PDA(["reserve", mint], vault-program)
///   idx6 rate_model= PDA(["rate_model", mint], vault-program)
///   idx7 vault     = ATA(M_LIQUIDITY, mint, classic token program)
///   idx5 "token"   = the 120B account linking M_LENDING↔mint (looked up live;
///                    not a clean [seed,mint] PDA, so pinned as a constant).
struct FlashMarket {
    reserve: &'static str,    // idx 4 (W)
    token: &'static str,      // idx 5 (W)
    rate_model: &'static str, // idx 6 (r)
    vault: &'static str,      // idx 7 (W)
}

/// Look up the flash-loan market for a debt mint. Only the three assets the
/// liquidation fire path repays are wired; anything else returns None (caller
/// must reject — a wrong account set would just revert).
fn market_for(mint: &Pubkey) -> Option<(FlashMarket, &'static str)> {
    let m = mint.to_string();
    if m == USDC_MINT {
        Some((FlashMarket {
            reserve: "94vK29npVbyRHXH63rRcTiSr26SFhrQTzbpNJuhQEDu",
            token: "J9dyC4pBTBPvzzPh7J9rhFhg8RvgerDNKkUH9kEwGMsj",
            rate_model: "5pjzT5dFTsXcwixoab1QDLvZQvpYJxJeBphkyfHGn688",
            vault: "BmkUoKMFYBxNSzWXyUjyMJjMAaVz4d8ZnxwwmhDCUXFB",
        }, USDC_MINT))
    } else if m == USDT_MINT {
        Some((FlashMarket {
            reserve: "Enao27EWUV2fv3rUqwknJ1eRaM5aAeN5ijeCrM9tayRX",
            token: "FmFLvD6X1zHh6gXGCgiB3zRiV1zoEKPgHqfUpXL9EvKu",
            rate_model: "6sAbVeSvEfjQGRAGg9W4PAfhB5qNhYiGdx6Fh9uVEsEC",
            vault: "4HTRHjdgy4VSVRcsumuzVFCgWywNhjGsD5oG3kqAt5vo",
        }, USDT_MINT))
    } else if m == WSOL_MINT {
        Some((FlashMarket {
            reserve: "4Y66HtUEqbbbpZdENGtFdVhUMS3tnagffn3M4do59Nfy",
            token: "BZZKgXxhxVkzx3NN8RfBPwU7ZmnQbDtp3ezcsXbiALL6",
            rate_model: "Acvyi9HBGmqh3Exe1N4PjBVyY8fokq2AdC6fSLqV6KSo",
            vault: "5JP5zgYCb9W37QQLgAHRHuinFLrKt87akDY1CgZoTPzr",
        }, WSOL_MINT))
    } else {
        None
    }
}

fn pk(s: &str) -> Pubkey {
    Pubkey::from_str(s).unwrap()
}

/// ATA for a mint under a specific token program (classic or Token-2022). The
/// token program id is part of the ATA derivation seeds, so a Token-2022 mint's
/// ATA differs from a classic one.
pub fn ata_for(owner: &Pubkey, mint: &Pubkey, token_program: &Pubkey) -> Pubkey {
    Pubkey::find_program_address(
        &[owner.as_ref(), token_program.as_ref(), mint.as_ref()],
        &pk(ATA_PROGRAM),
    )
    .0
}

/// Signer's ATA for a mint (classic SPL Token program — the common case).
pub fn ata(owner: &Pubkey, mint: &Pubkey) -> Pubkey {
    ata_for(owner, mint, &pk(TOKEN_PROGRAM))
}

/// Create-idempotent the signer's ATA under a given token program.
pub fn create_ata_idempotent_for(signer: &Pubkey, mint: &Pubkey, token_program: &Pubkey) -> Instruction {
    let ata = ata_for(signer, mint, token_program);
    Instruction {
        program_id: pk(ATA_PROGRAM),
        accounts: vec![
            AccountMeta::new(*signer, true),
            AccountMeta::new(ata, false),
            AccountMeta::new_readonly(*signer, false),
            AccountMeta::new_readonly(*mint, false),
            AccountMeta::new_readonly(pk(SYS_PROGRAM), false),
            AccountMeta::new_readonly(*token_program, false),
        ],
        data: vec![1], // createIdempotent
    }
}

/// Create-idempotent the signer's ATA (classic SPL Token program).
pub fn create_ata_idempotent(signer: &Pubkey, mint: &Pubkey) -> Instruction {
    create_ata_idempotent_for(signer, mint, &pk(TOKEN_PROGRAM))
}

fn market_metas(signer: &Pubkey, mint: &Pubkey) -> Option<Vec<AccountMeta>> {
    let (m, mint_str) = market_for(mint)?;
    let ata = ata(signer, mint);
    Some(vec![
        AccountMeta::new(*signer, true),                      // 0 signer
        AccountMeta::new(pk(M_LENDING), false),               // 1 lending admin (global)
        AccountMeta::new(ata, false),                         // 2 signer's debt ATA
        AccountMeta::new_readonly(pk(mint_str), false),       // 3 mint
        AccountMeta::new(pk(m.reserve), false),               // 4
        AccountMeta::new(pk(m.token), false),                 // 5
        AccountMeta::new_readonly(pk(m.rate_model), false),   // 6
        AccountMeta::new(pk(m.vault), false),                 // 7
        AccountMeta::new_readonly(pk(M_LIQUIDITY), false),    // 8 (global)
        AccountMeta::new_readonly(pk(M_VAULT_PROGRAM), false),// 9 (global)
        AccountMeta::new_readonly(pk(TOKEN_PROGRAM), false),  // 10
        AccountMeta::new_readonly(pk(ATA_PROGRAM), false),    // 11
        AccountMeta::new_readonly(pk(SYS_PROGRAM), false),    // 12
        AccountMeta::new_readonly(pk(INSTRUCTIONS_SYSVAR), false), // 13
    ])
}

fn ix(signer: &Pubkey, mint: &Pubkey, disc: [u8; 8], amount: u64) -> Option<Instruction> {
    let mut data = Vec::with_capacity(16);
    data.extend_from_slice(&disc);
    data.extend_from_slice(&amount.to_le_bytes());
    Some(Instruction {
        program_id: pk(JUP_LEND_PROGRAM),
        accounts: market_metas(signer, mint)?,
        data,
    })
}

/// True if the JupLend flash-loan market for `mint` is wired (USDC/USDT/wSOL).
pub fn has_market(mint: &Pubkey) -> bool {
    market_for(mint).is_some()
}

/// Flash-borrow `amount` base units of `mint` (USDC/USDT/wSOL) to `signer`.
/// None if the mint has no wired flash market.
pub fn borrow(signer: &Pubkey, mint: &Pubkey, amount: u64) -> Option<Instruction> {
    ix(signer, mint, DISC_BORROW, amount)
}

/// Repay `amount` base units of `mint` (must equal the borrow — 0 bp fee).
pub fn payback(signer: &Pubkey, mint: &Pubkey, amount: u64) -> Option<Instruction> {
    ix(signer, mint, DISC_PAYBACK, amount)
}

/// Flash-borrow `amount` base units of USDC to `signer` (back-compat wrapper).
pub fn borrow_usdc(signer: &Pubkey, amount: u64) -> Instruction {
    borrow(signer, &pk(USDC_MINT), amount).expect("USDC market is always wired")
}

/// Repay `amount` base units of USDC (must equal the borrow — 0 bp fee).
pub fn payback_usdc(signer: &Pubkey, amount: u64) -> Instruction {
    payback(signer, &pk(USDC_MINT), amount).expect("USDC market is always wired")
}
