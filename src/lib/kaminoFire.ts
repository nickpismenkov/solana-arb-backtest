// Port of src/kamino_fire.rs
//
// Kamino atomic liquidation FIRE path — one flashloan-wrapped v0 tx:
//
//   [cu, cu_price, create ATAs, JupLend borrow USDC,
//    refresh_reserve(repay), refresh_reserve(withdraw), refresh_obligation,
//    liquidate_and_redeem_v2, Jupiter swap seized->USDC, JupLend payback, tip]
//
// Profit-or-revert, NO external capital: the flash-borrowed USDC repays the
// obligation's debt inside `liquidate`, which seizes discounted collateral and
// redeems it to the underlying liquidity token. The swap turns that back into
// USDC; the fixed-amount JupLend payback then fails unless the swap produced
// at least the borrowed amount — so a landed tx is always net-positive (the
// liquidation bonus), and an unprofitable one reverts for just the base fee.
//
// v1.5: the debt (repay reserve's liquidity) may be any asset with a wired
// JupLend flash market — USDC/USDT/wSOL. That mint is what JupLend flash-borrows
// and what the seized-collateral swap targets.

import {
  type AddressLookupTableAccount,
  PublicKey,
  TransactionMessage,
  VersionedTransaction,
} from '@solana/web3.js';
import { cuLimitIx, cuPriceIx, transferIx } from './arb.js';
import { ataFor, borrow, createAtaIdempotentFor, hasMarket, payback } from './flashloan.js';
import * as jup from './jup.js';
import * as kaminoIx from './kaminoIx.js';
import type { ReserveAccounts } from './kaminoIx.js';

export const FIRE_CU_LIMIT = 1_400_000;
export const USDC_MINT = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v';

/**
 * Dedicated Kamino liquidation ALT (main-market static accounts + programs).
 * Override via KAMINO_ALT. Set after liqKaminoAltPrint + on-chain create.
 */
export const KAMINO_ALT = '6X77KtDupVYqU4SBjWsY93ycFW2bPm3AWpAuPWfxraKo';

export interface KaminoFireCandidate {
  obligation: PublicKey;
  lendingMarket: PublicKey;
  repayReserve: ReserveAccounts;
  withdrawReserve: ReserveAccounts;
  /** obligation's reserves in refresh order (deposits then borrows). */
  obligationReserves: PublicKey[];
  /** seized-collateral underlying mint (= withdraw reserve liquidity) + its program. */
  withdrawLiquidityMint: PublicKey;
  withdrawLiquidityTokenProgram: PublicKey;
  withdrawCollateralTokenProgram: PublicKey;
  /** repay side token program (USDC = classic SPL in v1). */
  repayLiquidityTokenProgram: PublicKey;
  /** USDC to flash-borrow and repay into the obligation (the close amount). */
  repayAmount: bigint;
  /**
   * Native underlying units to swap -> USDC. Computed by the caller from the
   * seized-collateral value and underlying price (with a haircut so the
   * ExactIn never exceeds the redeemed balance); dust stays in the ATA.
   */
  swapInAmount: bigint;
}

export interface KaminoFireTx {
  tx: VersionedTransaction;
  quotedUsdcOut: bigint;
  txBytes: number;
}

/**
 * Build the unsigned Kamino fire tx. Quotes the seized-underlying->USDC swap
 * live (Jupiter), so call only for a sim-confirmed candidate.
 */
export async function buildFireTx(
  rpcEndpoint: string,
  c: KaminoFireCandidate,
  authority: PublicKey,
  tipAccount: PublicKey | null,
  tipLamports: bigint,
  priorityMicroLamports: bigint,
  slippageBps: number,
  maxSwapAccounts: number,
  blockhash: string,
): Promise<KaminoFireTx> {
  // Debt asset = the repay reserve's liquidity mint (USDC/USDT/wSOL). It's the
  // flash-borrow asset, the swap target, and the payback token.
  const debtMint = c.repayReserve.liquidityMint;
  const debtTp = c.repayLiquidityTokenProgram;
  if (!hasMarket(debtMint)) {
    throw new Error(`no JupLend flash market for debt mint ${debtMint.toBase58()}`);
  }

  // ATAs: debt asset (borrow dest + repay source + swap out), seized underlying
  // (swap in), collateral cToken (transient redeem target).
  const debtAta = ataFor(authority, debtMint, debtTp);
  const seizedAta = ataFor(authority, c.withdrawLiquidityMint, c.withdrawLiquidityTokenProgram);
  const collAta = ataFor(authority, c.withdrawReserve.collateralMint, c.withdrawCollateralTokenProgram);

  // Swap the redeemed underlying -> debt asset. swapInAmount is the caller's
  // native-unit estimate of the seized collateral (with a haircut so the
  // ExactIn stays within the redeemed balance). The fixed payback is the
  // real profit-or-revert guard regardless of any swap-sizing slack.
  //
  // Same-mint case (seized underlying == debt mint): no swap — the redeemed
  // liquidity IS the debt asset. Jupiter rejects equal in/out mints, so skip.
  const sameMint = c.withdrawLiquidityMint.equals(debtMint);
  let swapIxs: import('@solana/web3.js').TransactionInstruction[];
  let quotedOut: bigint;
  let swapAlts: PublicKey[];
  if (sameMint) {
    swapIxs = [];
    quotedOut = c.swapInAmount;
    swapAlts = [];
  } else {
    const quote = await jup.quote(c.withdrawLiquidityMint, debtMint, c.swapInAmount, slippageBps, maxSwapAccounts);
    const plan = await jup.swapInstructions(quote, authority, false);
    swapIxs = plan.instructions;
    quotedOut = plan.quotedOut;
    swapAlts = plan.altAddresses;
  }

  const altAddrs = [...swapAlts];
  const kaminoAltStr = process.env.KAMINO_ALT ?? (KAMINO_ALT.length > 0 ? KAMINO_ALT : undefined);
  if (kaminoAltStr) {
    try {
      altAddrs.push(new PublicKey(kaminoAltStr));
    } catch {
      /* skip a bad override */
    }
  }
  const alts: AddressLookupTableAccount[] = await jup.fetchAlts(rpcEndpoint, altAddrs);

  const borrowIx = borrow(authority, debtMint, c.repayAmount);
  if (borrowIx === undefined) throw new Error(`no JupLend flash market for debt mint ${debtMint.toBase58()}`);
  const paybackIx = payback(authority, debtMint, c.repayAmount);
  if (paybackIx === undefined) throw new Error(`no JupLend flash market for debt mint ${debtMint.toBase58()}`);

  const ixs = [
    cuLimitIx(FIRE_CU_LIMIT),
    cuPriceIx(priorityMicroLamports),
    createAtaIdempotentFor(authority, debtMint, debtTp),
    createAtaIdempotentFor(authority, c.withdrawLiquidityMint, c.withdrawLiquidityTokenProgram),
    createAtaIdempotentFor(authority, c.withdrawReserve.collateralMint, c.withdrawCollateralTokenProgram),
    borrowIx,
    kaminoIx.refreshReserve(c.repayReserve),
    kaminoIx.refreshReserve(c.withdrawReserve),
    kaminoIx.refreshObligation(c.lendingMarket, c.obligation, c.obligationReserves),
    kaminoIx.liquidateAndRedeemV2(
      authority,
      c.obligation,
      c.lendingMarket,
      c.repayReserve,
      c.withdrawReserve,
      collAta,
      seizedAta,
      debtAta,
      c.withdrawCollateralTokenProgram,
      c.repayLiquidityTokenProgram,
      c.withdrawLiquidityTokenProgram,
      c.repayAmount,
      0n,
      0n,
    ),
  ];
  ixs.push(...swapIxs);
  // Fixed-amount payback = the guard: reverts unless the swap covered it.
  ixs.push(paybackIx);
  if (tipAccount !== null && tipLamports > 0n) {
    ixs.push(transferIx(authority, tipAccount, tipLamports));
  }

  const msg = new TransactionMessage({
    payerKey: authority,
    recentBlockhash: blockhash,
    instructions: ixs,
  }).compileToV0Message(alts);
  const tx = new VersionedTransaction(msg);
  const txBytes = tx.serialize().length;
  return { tx, quotedUsdcOut: quotedOut, txBytes };
}
