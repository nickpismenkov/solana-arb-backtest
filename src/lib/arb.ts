// Port of src/arb.rs
//
// The guarded flash-loan arb transaction, as a reusable builder (extracted
// from the verified arb_probe). Resolves both pools' accounts, assembles
// [CU, create ATAs, flash-borrow USDC, leg1 USDC→base exact-in, leg2
// base→USDC EXACT-OUT=repay, flash-payback] in either direction, and compiles
// a v0 tx against the ALT so it fits 1232 bytes. Leg 2 exact-out for exactly
// the repay amount is the profit-or-revert guard. Verified on mainnet (PR #16).

import {
  AddressLookupTableAccount,
  ComputeBudgetProgram,
  PublicKey,
  SystemProgram,
  TransactionInstruction,
  TransactionMessage,
  VersionedTransaction,
} from '@solana/web3.js';

import { decodeOrcaState, decodeRayState, orcaStartIndex, orcaTickArray, rayStartIndex, rayTickArray } from './execute.js';
import { ataFor, borrowUsdc, createAtaIdempotentFor, paybackUsdc, USDC_MINT } from './flashloan.js';
import { pair } from './pools.js';
import {
  orcaOracle,
  orcaSwapIx,
  orcaSwapV2Ix,
  raySwapIx,
  raySwapV2Ix,
  sqrtLimit,
  OrcaSwapAccounts,
  RaySwapAccounts,
} from './swap.js';

function pk(s: string): PublicKey {
  return new PublicKey(s);
}
export function pkAt(d: Uint8Array, o: number): PublicKey {
  return new PublicKey(d.subarray(o, o + 32));
}

export function cuLimitIx(units: number): TransactionInstruction {
  return ComputeBudgetProgram.setComputeUnitLimit({ units });
}

export function cuPriceIx(microLamports: number | bigint): TransactionInstruction {
  return ComputeBudgetProgram.setComputeUnitPrice({ microLamports });
}

/** System-program transfer (Jito tip). Inside the arb tx so a revert pays no tip. */
export function transferIx(from: PublicKey, to: PublicKey, lamports: number | bigint): TransactionInstruction {
  return SystemProgram.transfer({ fromPubkey: from, toPubkey: to, lamports });
}

/** Both pools' raw account data, fetched once per build. */
export interface PoolData {
  orca: Uint8Array;
  ray: Uint8Array;
}

/** Token program owning `mint`: baseTp if it's the base mint, else quoteTp. */
export function tpOf(mint: PublicKey, base: PublicKey, baseTp: PublicKey, quoteTp: PublicKey): PublicKey {
  return mint.equals(base) ? baseTp : quoteTp;
}

/**
 * Resolve the Orca swap accounts for our wallet at the current tick.
 * `aToB` selects the tick-array traversal direction. ATAs are derived under
 * each mint's owning token program (classic or Token-2022).
 */
export function orcaAccounts(
  od: Uint8Array,
  orcaPk: PublicKey,
  signer: PublicKey,
  aToB: boolean,
  base: PublicKey,
  baseTp: PublicKey,
  quoteTp: PublicKey,
): OrcaSwapAccounts {
  const ost = decodeOrcaState(od);
  if (ost === undefined) throw new Error('orca state');
  const mintA = pkAt(od, 101);
  const mintB = pkAt(od, 181);
  const n = 88 * ost.tickSpacing;
  const start = orcaStartIndex(ost.tick, ost.tickSpacing);
  const starts = aToB ? [start, start - n, start - 2 * n] : [start, start + n, start + 2 * n];
  return {
    whirlpool: orcaPk,
    tokenAuthority: signer,
    tokenOwnerA: ataFor(signer, mintA, tpOf(mintA, base, baseTp, quoteTp)),
    tokenVaultA: pkAt(od, 133),
    tokenOwnerB: ataFor(signer, mintB, tpOf(mintB, base, baseTp, quoteTp)),
    tokenVaultB: pkAt(od, 213),
    tickArrays: [
      orcaTickArray(orcaPk, starts[0]),
      orcaTickArray(orcaPk, starts[1]),
      orcaTickArray(orcaPk, starts[2]),
    ],
    oracle: orcaOracle(orcaPk),
  };
}

function rayAccounts(
  rd: Uint8Array,
  rayPk: PublicKey,
  signer: PublicKey,
  base: PublicKey,
  usdc: PublicKey,
  baseIn: boolean,
  baseTp: PublicKey,
  quoteTp: PublicKey,
): RaySwapAccounts {
  const rst = decodeRayState(rd);
  if (rst === undefined) throw new Error('ray state');
  const mint0 = pkAt(rd, 73);
  const baseIs0 = mint0.equals(base);
  const [baseVault, quoteVault] = baseIs0 ? [pkAt(rd, 137), pkAt(rd, 169)] : [pkAt(rd, 169), pkAt(rd, 137)];
  // baseIn = leg spends base (sell); else spends USDC (buy). ATAs under the
  // owning token program per mint.
  const baseAta = ataFor(signer, base, baseTp);
  const usdcAta = ataFor(signer, usdc, quoteTp);
  const [inputVault, outputVault, inputAta, outputAta] = baseIn
    ? [baseVault, quoteVault, baseAta, usdcAta]
    : [quoteVault, baseVault, usdcAta, baseAta];
  // Tick-array traversal: input mint == token0 → price/tick decreases
  // (zero-for-one) → arrays descend from the current one; else ascend.
  const zeroForOne = baseIn ? baseIs0 : !baseIs0;
  const n = 60 * rst.tickSpacing;
  const rstart = rayStartIndex(rst.tick, rst.tickSpacing);
  const starts = zeroForOne ? [rstart, rstart - n, rstart - 2 * n] : [rstart, rstart + n, rstart + 2 * n];
  return {
    payer: signer,
    ammConfig: pkAt(rd, 9),
    poolState: rayPk,
    inputTokenAccount: inputAta,
    outputTokenAccount: outputAta,
    inputVault,
    outputVault,
    observationState: pkAt(rd, 201),
    tickArrays: [rayTickArray(rayPk, starts[0]), rayTickArray(rayPk, starts[1]), rayTickArray(rayPk, starts[2])],
  };
}

/**
 * Build the unsigned guarded arb v0 tx. `orcaFirst = true` buys base on Orca
 * (leg1) and sells on Raydium (leg2); false is the reverse. `blockhash` should
 * be a real recent hash for live submission, or a placeholder for
 * replace-blockhash simulation. Returns the unsigned tx (sign before sending).
 */
export function buildArbTx(
  pools: PoolData,
  signer: PublicKey,
  alt: AddressLookupTableAccount,
  borrowAmount: bigint,
  orcaFirst: boolean,
  tipAccount: PublicKey | undefined,
  tipLamports: bigint,
  priorityMicroLamports: bigint,
  blockhash: string,
  repayBuffer: bigint,
): VersionedTransaction {
  const cfg = pair();
  const usdc = pk(USDC_MINT);
  const base = pk(cfg.baseMint);
  const orcaPk = pk(cfg.orcaPool);
  const rayPk = pk(cfg.rayPool);
  const baseTp = pk(cfg.baseTokenProgram);
  const quoteTp = pk(cfg.quoteTokenProgram);
  const v2 = cfg.needsSwapV2(); // Token-2022 leg → swapV2
  const mintA = pkAt(pools.orca, 101);
  const mintB = pkAt(pools.orca, 181);
  const baseIsAOrca = mintA.equals(base);
  const tpA = tpOf(mintA, base, baseTp, quoteTp);
  const tpB = tpOf(mintB, base, baseTp, quoteTp);

  // Orca leg helper: swapV2 when the pair has a Token-2022 side, else classic.
  const orcaLeg = (aToB: boolean, amount: bigint, threshold: bigint, exactIn: boolean): TransactionInstruction => {
    const oa = orcaAccounts(pools.orca, orcaPk, signer, aToB, base, baseTp, quoteTp);
    if (v2) {
      return orcaSwapV2Ix(oa, mintA, mintB, tpA, tpB, amount, threshold, sqrtLimit(aToB), exactIn, aToB);
    } else {
      return orcaSwapIx(oa, amount, threshold, sqrtLimit(aToB), exactIn, aToB);
    }
  };
  // Ray leg helper: baseIn = the leg spends base (sell). input/output mints
  // follow the direction so swapV2 gets the right vault mints.
  const rayLeg = (baseIn: boolean, amount: bigint, threshold: bigint, isBaseInput: boolean): TransactionInstruction => {
    const ra = rayAccounts(pools.ray, rayPk, signer, base, usdc, baseIn, baseTp, quoteTp);
    const [inMint, outMint] = baseIn ? [base, usdc] : [usdc, base];
    if (v2) {
      return raySwapV2Ix(ra, inMint, outMint, amount, threshold, 0n, isBaseInput);
    } else {
      return raySwapIx(ra, amount, threshold, 0n, isBaseInput);
    }
  };

  // leg1 = buy base with USDC (exact-in borrowAmount); leg2 = sell base for
  // USDC (exact-out = borrow + repayBuffer) — the guard. The buffer forces
  // the tx to produce enough surplus USDC to cover the tip + fees, so a landed
  // trade is always net-positive; if the gap is too small, leg2 can't produce
  // borrow+buffer and reverts → bundle fails for free.
  const leg2Out = borrowAmount + repayBuffer;
  const U64_MAX = (1n << 64n) - 1n;
  let leg1: TransactionInstruction;
  let leg2: TransactionInstruction;
  if (orcaFirst) {
    // Orca buy (input USDC → aToB = !baseIsAOrca); Ray sell base exact-out.
    leg1 = orcaLeg(!baseIsAOrca, borrowAmount, 0n, true);
    leg2 = rayLeg(true, leg2Out, U64_MAX, false);
  } else {
    // Ray buy base exact-in; Orca sell base exact-out (aToB = baseIsAOrca).
    leg1 = rayLeg(false, borrowAmount, 0n, true);
    leg2 = orcaLeg(baseIsAOrca, leg2Out, U64_MAX, false);
  }

  const ixs: TransactionInstruction[] = [
    cuLimitIx(600_000),
    cuPriceIx(priorityMicroLamports),
    createAtaIdempotentFor(signer, usdc, quoteTp),
    createAtaIdempotentFor(signer, base, baseTp),
    borrowUsdc(signer, borrowAmount),
    leg1,
    leg2,
    paybackUsdc(signer, borrowAmount),
  ];
  // Tip transfer to a Jito tip account, inside the tx → only pays if it lands.
  if (tipAccount !== undefined && tipLamports > 0n) {
    ixs.push(transferIx(signer, tipAccount, tipLamports));
  }

  const msg = new TransactionMessage({
    payerKey: signer,
    recentBlockhash: blockhash,
    instructions: ixs,
  }).compileToV0Message([alt]);
  return new VersionedTransaction(msg);
}

/** Load an ALT account into the form v0 message compilation needs. */
export function loadAlt(altAddr: string, altAccountData: Uint8Array): AddressLookupTableAccount {
  const state = AddressLookupTableAccount.deserialize(altAccountData);
  return new AddressLookupTableAccount({ key: pk(altAddr), state });
}
