// Port of src/flashloan.rs
//
// Jupiter Lend (Fluid) flash-loan instructions — 0 bp. Ported from the exact
// output of the @jup-ag/lend SDK's getFlashloanIx (captured on mainnet, then
// diffed across signers): for the USDC "main" market only accounts 0 (signer)
// and 2 (signer's USDC ATA) vary; the other twelve are market constants. The
// program matches borrow<->payback via the instructions sysvar, so we just place
// borrow before payback in the tx — no index math. Verified by flashloanProbe
// (borrow -> payback simulates clean).

import { type AccountMeta, PublicKey, TransactionInstruction } from '@solana/web3.js';

export const JUP_LEND_PROGRAM = 'jupgfSgfuAXv4B6R2Uxu85Z1qdzgju79s6MfZekN6XS';
export const USDC_MINT = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v';
export const USDT_MINT = 'Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB';
export const WSOL_MINT = 'So11111111111111111111111111111111111111112';
export const TOKEN_PROGRAM = 'TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA';
export const TOKEN_2022_PROGRAM = 'TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb';
const ATA_PROGRAM = 'ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL';
const SYS_PROGRAM = '11111111111111111111111111111111';
const INSTRUCTIONS_SYSVAR = 'Sysvar1nstructions1111111111111111111111111';

// Anchor discriminators (first 8 bytes of each ix's data).
const DISC_BORROW = Buffer.from([0x67, 0x13, 0x4e, 0x18, 0xf0, 0x09, 0x87, 0x3f]);
const DISC_PAYBACK = Buffer.from([0xd5, 0x2f, 0x99, 0x89, 0x54, 0xf3, 0x5e, 0xe8]);

// Accounts shared by EVERY market (indices 1,8,9 in the ix). The lending admin
// (idx 1) has no per-mint field; idx 8 = PDA(["liquidity"], vault-program);
// idx 9 = the vault program id itself.
const M_LENDING = 'ALXWtv2P4GqH1B7Lq731joag52yRBRqmHV4naiXPTYWL'; // idx 1  (W)  global
const M_LIQUIDITY = '7s1da8DduuBFqGra5bJBjpnvL5E9mGzCuMk1Qkh4or2Z'; // idx 8  (r)  global
const M_VAULT_PROGRAM = 'jupeiUmn818Jg1ekPURTpr4mFo29p46vygyykFJ3wZC'; // idx 9  (r)  global

/**
 * The four per-asset flash-market accounts (indices 4,5,6,7). Verified against
 * the live USDC set (self-check reproduces the SDK-captured constants) and
 * derived the same way for USDT/wSOL:
 *   idx4 reserve   = PDA(["reserve", mint], vault-program)
 *   idx6 rateModel = PDA(["rate_model", mint], vault-program)
 *   idx7 vault     = ATA(M_LIQUIDITY, mint, classic token program)
 *   idx5 "token"   = the 120B account linking M_LENDING<->mint (looked up live;
 *                    not a clean [seed,mint] PDA, so pinned as a constant).
 */
interface FlashMarket {
  reserve: string; // idx 4 (W)
  token: string; // idx 5 (W)
  rateModel: string; // idx 6 (r)
  vault: string; // idx 7 (W)
}

/**
 * Look up the flash-loan market for a debt mint. Only the three assets the
 * liquidation fire path repays are wired; anything else returns undefined
 * (caller must reject — a wrong account set would just revert).
 */
function marketFor(mint: PublicKey): [FlashMarket, string] | undefined {
  const m = mint.toBase58();
  if (m === USDC_MINT) {
    return [
      {
        reserve: '94vK29npVbyRHXH63rRcTiSr26SFhrQTzbpNJuhQEDu',
        token: 'J9dyC4pBTBPvzzPh7J9rhFhg8RvgerDNKkUH9kEwGMsj',
        rateModel: '5pjzT5dFTsXcwixoab1QDLvZQvpYJxJeBphkyfHGn688',
        vault: 'BmkUoKMFYBxNSzWXyUjyMJjMAaVz4d8ZnxwwmhDCUXFB',
      },
      USDC_MINT,
    ];
  } else if (m === USDT_MINT) {
    return [
      {
        reserve: 'Enao27EWUV2fv3rUqwknJ1eRaM5aAeN5ijeCrM9tayRX',
        token: 'FmFLvD6X1zHh6gXGCgiB3zRiV1zoEKPgHqfUpXL9EvKu',
        rateModel: '6sAbVeSvEfjQGRAGg9W4PAfhB5qNhYiGdx6Fh9uVEsEC',
        vault: '4HTRHjdgy4VSVRcsumuzVFCgWywNhjGsD5oG3kqAt5vo',
      },
      USDT_MINT,
    ];
  } else if (m === WSOL_MINT) {
    return [
      {
        reserve: '4Y66HtUEqbbbpZdENGtFdVhUMS3tnagffn3M4do59Nfy',
        token: 'BZZKgXxhxVkzx3NN8RfBPwU7ZmnQbDtp3ezcsXbiALL6',
        rateModel: 'Acvyi9HBGmqh3Exe1N4PjBVyY8fokq2AdC6fSLqV6KSo',
        vault: '5JP5zgYCb9W37QQLgAHRHuinFLrKt87akDY1CgZoTPzr',
      },
      WSOL_MINT,
    ];
  }
  return undefined;
}

function pk(s: string): PublicKey {
  return new PublicKey(s);
}

function meta(pubkey: PublicKey, isSigner: boolean, isWritable: boolean): AccountMeta {
  return { pubkey, isSigner, isWritable };
}

/**
 * ATA for a mint under a specific token program (classic or Token-2022). The
 * token program id is part of the ATA derivation seeds, so a Token-2022 mint's
 * ATA differs from a classic one.
 */
export function ataFor(owner: PublicKey, mint: PublicKey, tokenProgram: PublicKey): PublicKey {
  return PublicKey.findProgramAddressSync([owner.toBuffer(), tokenProgram.toBuffer(), mint.toBuffer()], pk(ATA_PROGRAM))[0];
}

/** Signer's ATA for a mint (classic SPL Token program — the common case). */
export function ata(owner: PublicKey, mint: PublicKey): PublicKey {
  return ataFor(owner, mint, pk(TOKEN_PROGRAM));
}

/** Create-idempotent the signer's ATA under a given token program. */
export function createAtaIdempotentFor(signer: PublicKey, mint: PublicKey, tokenProgram: PublicKey): TransactionInstruction {
  const ataAddr = ataFor(signer, mint, tokenProgram);
  return new TransactionInstruction({
    programId: pk(ATA_PROGRAM),
    keys: [
      meta(signer, true, true),
      meta(ataAddr, false, true),
      meta(signer, false, false),
      meta(mint, false, false),
      meta(pk(SYS_PROGRAM), false, false),
      meta(tokenProgram, false, false),
    ],
    data: Buffer.from([1]), // createIdempotent
  });
}

/** Create-idempotent the signer's ATA (classic SPL Token program). */
export function createAtaIdempotent(signer: PublicKey, mint: PublicKey): TransactionInstruction {
  return createAtaIdempotentFor(signer, mint, pk(TOKEN_PROGRAM));
}

function marketMetas(signer: PublicKey, mint: PublicKey): AccountMeta[] | undefined {
  const found = marketFor(mint);
  if (!found) return undefined;
  const [m, mintStr] = found;
  const ataAddr = ata(signer, mint);
  return [
    meta(signer, true, true), // 0 signer
    meta(pk(M_LENDING), false, true), // 1 lending admin (global)
    meta(ataAddr, false, true), // 2 signer's debt ATA
    meta(pk(mintStr), false, false), // 3 mint
    meta(pk(m.reserve), false, true), // 4
    meta(pk(m.token), false, true), // 5
    meta(pk(m.rateModel), false, false), // 6
    meta(pk(m.vault), false, true), // 7
    meta(pk(M_LIQUIDITY), false, false), // 8 (global)
    meta(pk(M_VAULT_PROGRAM), false, false), // 9 (global)
    meta(pk(TOKEN_PROGRAM), false, false), // 10
    meta(pk(ATA_PROGRAM), false, false), // 11
    meta(pk(SYS_PROGRAM), false, false), // 12
    meta(pk(INSTRUCTIONS_SYSVAR), false, false), // 13
  ];
}

function ix(signer: PublicKey, mint: PublicKey, disc: Buffer, amount: bigint): TransactionInstruction | undefined {
  const keys = marketMetas(signer, mint);
  if (!keys) return undefined;
  const data = Buffer.alloc(16);
  disc.copy(data, 0);
  data.writeBigUInt64LE(amount, 8);
  return new TransactionInstruction({ programId: pk(JUP_LEND_PROGRAM), keys, data });
}

/** True if the JupLend flash-loan market for `mint` is wired (USDC/USDT/wSOL). */
export function hasMarket(mint: PublicKey): boolean {
  return marketFor(mint) !== undefined;
}

/**
 * Flash-borrow `amount` base units of `mint` (USDC/USDT/wSOL) to `signer`.
 * undefined if the mint has no wired flash market.
 */
export function borrow(signer: PublicKey, mint: PublicKey, amount: bigint): TransactionInstruction | undefined {
  return ix(signer, mint, DISC_BORROW, amount);
}

/** Repay `amount` base units of `mint` (must equal the borrow — 0 bp fee). */
export function payback(signer: PublicKey, mint: PublicKey, amount: bigint): TransactionInstruction | undefined {
  return ix(signer, mint, DISC_PAYBACK, amount);
}

/** Flash-borrow `amount` base units of USDC to `signer` (back-compat wrapper). */
export function borrowUsdc(signer: PublicKey, amount: bigint): TransactionInstruction {
  const out = borrow(signer, pk(USDC_MINT), amount);
  if (!out) throw new Error('USDC market is always wired');
  return out;
}

/** Repay `amount` base units of USDC (must equal the borrow — 0 bp fee). */
export function paybackUsdc(signer: PublicKey, amount: bigint): TransactionInstruction {
  const out = payback(signer, pk(USDC_MINT), amount);
  if (!out) throw new Error('USDC market is always wired');
  return out;
}
