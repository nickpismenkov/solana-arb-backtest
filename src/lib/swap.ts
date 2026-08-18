// Port of src/swap.rs
//
// Swap instruction builders for Orca Whirlpool and Raydium CLMM, built
// directly (no aggregator, no network hop) so they fit the shred-reaction
// budget. Account orders follow each program's on-chain layout; the exact
// metas are VERIFIED against mainnet by `swap_probe` (simulate a real swap)
// before any of this is wired into a live bundle — same discipline as
// tickarray_probe. Until that probe passes, treat these as unverified.

import { AccountMeta, PublicKey, TransactionInstruction } from '@solana/web3.js';
import { ORCA_PROGRAM, RAY_CLMM_PROGRAM } from './decode.js';

// Anchor "global:swap" sighash — shared by both programs (disambiguated by
// program id, matching decode.ts).
const DISC_SWAP = Uint8Array.from([0xf8, 0xc6, 0x9e, 0x91, 0xe1, 0x75, 0x87, 0xc8]);
// "global:swap_v2" sighash — shared by Orca + Raydium CLMM. Handles Token-2022
// mints (passes both token programs + mint accounts). Verified vs both IDLs.
const DISC_SWAP_V2 = Uint8Array.from([0x2b, 0x04, 0xed, 0x0b, 0x1a, 0xc9, 0x1e, 0x62]);

const TOKEN_PROGRAM = 'TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA';
const TOKEN_2022_PROGRAM = 'TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb';
const MEMO_PROGRAM = 'MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr';

function writeU64LE(buf: Buffer, offset: number, v: bigint): void {
  buf.writeBigUInt64LE(v, offset);
}
function writeU128LE(buf: Buffer, offset: number, v: bigint): void {
  buf.writeBigUInt64LE(v & 0xffffffffffffffffn, offset);
  buf.writeBigUInt64LE(v >> 64n, offset + 8);
}

/** Accounts the caller resolves once (from pool state + our ATAs) and reuses. */
export interface OrcaSwapAccounts {
  whirlpool: PublicKey;
  tokenAuthority: PublicKey; // our wallet (signer)
  tokenOwnerA: PublicKey; // our ATA for mintA
  tokenVaultA: PublicKey;
  tokenOwnerB: PublicKey; // our ATA for mintB
  tokenVaultB: PublicKey;
  tickArrays: [PublicKey, PublicKey, PublicKey];
  oracle: PublicKey; // PDA ["oracle", whirlpool]
}

/**
 * Orca `swap`: data = disc + amount + other_amount_threshold + sqrt_price_limit
 * + amount_specified_is_input + a_to_b. `exactIn`: true → amount is input,
 * threshold is min-out; false → amount is desired output, threshold is max-in.
 */
export function orcaSwapIx(
  a: OrcaSwapAccounts,
  amount: bigint,
  threshold: bigint,
  sqrtPriceLimit: bigint,
  exactIn: boolean,
  aToB: boolean,
): TransactionInstruction {
  const data = Buffer.alloc(8 + 8 + 8 + 16 + 1 + 1);
  Buffer.from(DISC_SWAP).copy(data, 0);
  writeU64LE(data, 8, amount);
  writeU64LE(data, 16, threshold);
  writeU128LE(data, 24, sqrtPriceLimit);
  data.writeUInt8(exactIn ? 1 : 0, 40); // amount_specified_is_input
  data.writeUInt8(aToB ? 1 : 0, 41);

  const tok = new PublicKey(TOKEN_PROGRAM);
  const keys: AccountMeta[] = [
    { pubkey: tok, isSigner: false, isWritable: false },
    { pubkey: a.tokenAuthority, isSigner: true, isWritable: false },
    { pubkey: a.whirlpool, isSigner: false, isWritable: true },
    { pubkey: a.tokenOwnerA, isSigner: false, isWritable: true },
    { pubkey: a.tokenVaultA, isSigner: false, isWritable: true },
    { pubkey: a.tokenOwnerB, isSigner: false, isWritable: true },
    { pubkey: a.tokenVaultB, isSigner: false, isWritable: true },
    { pubkey: a.tickArrays[0], isSigner: false, isWritable: true },
    { pubkey: a.tickArrays[1], isSigner: false, isWritable: true },
    { pubkey: a.tickArrays[2], isSigner: false, isWritable: true },
    { pubkey: a.oracle, isSigner: false, isWritable: true },
  ];
  return new TransactionInstruction({ programId: new PublicKey(ORCA_PROGRAM), keys, data });
}

/**
 * Orca `swap_v2` (Token-2022-aware). Same args as `swap` plus a trailing
 * `remaining_accounts_info: Option<..>` (we send `None` = 0x00). Accounts add
 * token_program_a/b, memo, mint_a/b at the front/after-whirlpool; oracle is
 * writable. `tpA`/`tpB` are the token programs owning mintA/mintB.
 */
export function orcaSwapV2Ix(
  a: OrcaSwapAccounts,
  mintA: PublicKey,
  mintB: PublicKey,
  tpA: PublicKey,
  tpB: PublicKey,
  amount: bigint,
  threshold: bigint,
  sqrtPriceLimit: bigint,
  exactIn: boolean,
  aToB: boolean,
): TransactionInstruction {
  const data = Buffer.alloc(8 + 8 + 8 + 16 + 1 + 1 + 1);
  Buffer.from(DISC_SWAP_V2).copy(data, 0);
  writeU64LE(data, 8, amount);
  writeU64LE(data, 16, threshold);
  writeU128LE(data, 24, sqrtPriceLimit);
  data.writeUInt8(exactIn ? 1 : 0, 40);
  data.writeUInt8(aToB ? 1 : 0, 41);
  data.writeUInt8(0, 42); // remaining_accounts_info = None

  const memo = new PublicKey(MEMO_PROGRAM);
  const keys: AccountMeta[] = [
    { pubkey: tpA, isSigner: false, isWritable: false },
    { pubkey: tpB, isSigner: false, isWritable: false },
    { pubkey: memo, isSigner: false, isWritable: false },
    { pubkey: a.tokenAuthority, isSigner: true, isWritable: false },
    { pubkey: a.whirlpool, isSigner: false, isWritable: true },
    { pubkey: mintA, isSigner: false, isWritable: false },
    { pubkey: mintB, isSigner: false, isWritable: false },
    { pubkey: a.tokenOwnerA, isSigner: false, isWritable: true },
    { pubkey: a.tokenVaultA, isSigner: false, isWritable: true },
    { pubkey: a.tokenOwnerB, isSigner: false, isWritable: true },
    { pubkey: a.tokenVaultB, isSigner: false, isWritable: true },
    { pubkey: a.tickArrays[0], isSigner: false, isWritable: true },
    { pubkey: a.tickArrays[1], isSigner: false, isWritable: true },
    { pubkey: a.tickArrays[2], isSigner: false, isWritable: true },
    { pubkey: a.oracle, isSigner: false, isWritable: true },
  ];
  return new TransactionInstruction({ programId: new PublicKey(ORCA_PROGRAM), keys, data });
}

/** Orca oracle PDA: seeds ["oracle", whirlpool]. */
export function orcaOracle(whirlpool: PublicKey): PublicKey {
  const [pda] = PublicKey.findProgramAddressSync(
    [Buffer.from('oracle'), whirlpool.toBuffer()],
    new PublicKey(ORCA_PROGRAM),
  );
  return pda;
}

export interface RaySwapAccounts {
  payer: PublicKey; // our wallet (signer)
  ammConfig: PublicKey;
  poolState: PublicKey;
  inputTokenAccount: PublicKey;
  outputTokenAccount: PublicKey;
  inputVault: PublicKey;
  outputVault: PublicKey;
  observationState: PublicKey;
  /**
   * Current tick array first, then the next two in traversal direction —
   * Raydium walks them as remaining accounts and errors with
   * NotEnoughTickArrayAccount (6023) if the walk runs past what's provided.
   */
  tickArrays: [PublicKey, PublicKey, PublicKey];
}

/**
 * Raydium CLMM `swap`: data = disc + amount + other_amount_threshold
 * + sqrt_price_limit_x64 + is_base_input. `isBaseInput`: true → amount is
 * input (exact-in), threshold is min-out; false → amount is output (exact-out),
 * threshold is max-in.
 */
export function raySwapIx(
  a: RaySwapAccounts,
  amount: bigint,
  threshold: bigint,
  sqrtPriceLimitX64: bigint,
  isBaseInput: boolean,
): TransactionInstruction {
  const data = Buffer.alloc(8 + 8 + 8 + 16 + 1);
  Buffer.from(DISC_SWAP).copy(data, 0);
  writeU64LE(data, 8, amount);
  writeU64LE(data, 16, threshold);
  writeU128LE(data, 24, sqrtPriceLimitX64);
  data.writeUInt8(isBaseInput ? 1 : 0, 40);

  const tok = new PublicKey(TOKEN_PROGRAM);
  const keys: AccountMeta[] = [
    { pubkey: a.payer, isSigner: true, isWritable: false },
    { pubkey: a.ammConfig, isSigner: false, isWritable: false },
    { pubkey: a.poolState, isSigner: false, isWritable: true },
    { pubkey: a.inputTokenAccount, isSigner: false, isWritable: true },
    { pubkey: a.outputTokenAccount, isSigner: false, isWritable: true },
    { pubkey: a.inputVault, isSigner: false, isWritable: true },
    { pubkey: a.outputVault, isSigner: false, isWritable: true },
    { pubkey: a.observationState, isSigner: false, isWritable: true },
    { pubkey: tok, isSigner: false, isWritable: false },
    { pubkey: a.tickArrays[0], isSigner: false, isWritable: true },
    { pubkey: a.tickArrays[1], isSigner: false, isWritable: true },
    { pubkey: a.tickArrays[2], isSigner: false, isWritable: true },
  ];
  return new TransactionInstruction({ programId: new PublicKey(RAY_CLMM_PROGRAM), keys, data });
}

/**
 * Raydium CLMM `swap_v2` (Token-2022-aware). Args identical to `swap`. Accounts
 * add token_program_2022, memo, and input/output vault MINTS; there is NO named
 * tick_array — all tick arrays are remaining accounts (writable). Both token
 * programs (classic + 2022) are always passed. `inputMint`/`outputMint` must
 * match input_vault/output_vault.
 */
export function raySwapV2Ix(
  a: RaySwapAccounts,
  inputMint: PublicKey,
  outputMint: PublicKey,
  amount: bigint,
  threshold: bigint,
  sqrtPriceLimitX64: bigint,
  isBaseInput: boolean,
): TransactionInstruction {
  const data = Buffer.alloc(8 + 8 + 8 + 16 + 1);
  Buffer.from(DISC_SWAP_V2).copy(data, 0);
  writeU64LE(data, 8, amount);
  writeU64LE(data, 16, threshold);
  writeU128LE(data, 24, sqrtPriceLimitX64);
  data.writeUInt8(isBaseInput ? 1 : 0, 40);

  const tok = new PublicKey(TOKEN_PROGRAM);
  const tok22 = new PublicKey(TOKEN_2022_PROGRAM);
  const memo = new PublicKey(MEMO_PROGRAM);
  const keys: AccountMeta[] = [
    { pubkey: a.payer, isSigner: true, isWritable: false },
    { pubkey: a.ammConfig, isSigner: false, isWritable: false },
    { pubkey: a.poolState, isSigner: false, isWritable: true },
    { pubkey: a.inputTokenAccount, isSigner: false, isWritable: true },
    { pubkey: a.outputTokenAccount, isSigner: false, isWritable: true },
    { pubkey: a.inputVault, isSigner: false, isWritable: true },
    { pubkey: a.outputVault, isSigner: false, isWritable: true },
    { pubkey: a.observationState, isSigner: false, isWritable: true },
    { pubkey: tok, isSigner: false, isWritable: false },
    { pubkey: tok22, isSigner: false, isWritable: false },
    { pubkey: memo, isSigner: false, isWritable: false },
    { pubkey: inputMint, isSigner: false, isWritable: false },
    { pubkey: outputMint, isSigner: false, isWritable: false },
    // Tick arrays as remaining accounts (writable).
    { pubkey: a.tickArrays[0], isSigner: false, isWritable: true },
    { pubkey: a.tickArrays[1], isSigner: false, isWritable: true },
    { pubkey: a.tickArrays[2], isSigner: false, isWritable: true },
  ];
  return new TransactionInstruction({ programId: new PublicKey(RAY_CLMM_PROGRAM), keys, data });
}

/**
 * Price-limit sentinels: no slippage cap at the swap level (we guard on the
 * flash-repay min-out instead). Orca uses Q64.64 bounds; Raydium Q64.64 too.
 */
export const MIN_SQRT_PRICE = 4295048016n;
export const MAX_SQRT_PRICE = 79226673515401279992447579055n;

/** For an a_to_b (price-decreasing) swap the limit is the min; else the max. */
export function sqrtLimit(aToB: boolean): bigint {
  return aToB ? MIN_SQRT_PRICE : MAX_SQRT_PRICE;
}
