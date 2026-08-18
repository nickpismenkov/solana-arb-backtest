// Port of src/save_fire.rs
//
// The atomic Save (Solend) liquidation FIRE path — one flash-loan-wrapped v0 tx:
//
//   [cuLimit, cuPrice, create ATAs,
//    JupLend borrow(debt)               (capital, no inventory)
//    save refreshReserve(repay=debt) · refreshReserve(withdraw=collateral)
//      · refreshObligation
//    save liquidateAndRedeem  (repay debt, seize collateral,
//      redeem cTokens → underlying into our ATA)
//    Jupiter swap collateral-underlying → debt  (skipped when they're the same
//      mint — Jupiter rejects equal in/out)
//    JupLend payback(debt)              (repay the flash loan)
//    tip]
//
// v1.5: the debt (repay reserve's liquidity) may be any asset with a wired
// JupLend flash market — USDC/USDT/wSOL. That mint is what JupLend flash-borrows,
// what the seized-collateral swap targets, and what the fixed payback repays.
//
// We wrap in JUPLEND's 0-bp flash loan (not Solend's own) for the same reason
// Kamino does: it's a different program, so it sidesteps Solend's flash-loan
// reentrancy guard (our liquidate repays into the very reserve a Solend flash
// loan would forbid touching between borrow/repay), and JupLend matches
// borrow↔payback via the instructions sysvar so no start/end wrapper is needed.
//
// Profit-or-revert with NO capital: the fixed-amount `payback(debt)` fails
// unless the swap produced at least the borrowed amount, so a landed tx is
// always net-positive (the ~liq_bonus% surplus stays in the wallet ATA) and an
// unprofitable one reverts for just the base fee.

import {
  type AddressLookupTableAccount,
  PublicKey,
  TransactionInstruction,
  TransactionMessage,
  VersionedTransaction,
} from '@solana/web3.js';
import { cuLimitIx, cuPriceIx, transferIx } from './arb.js';
import { ataFor, borrow, createAtaIdempotentFor, hasMarket, payback } from './flashloan.js';
import * as jup from './jup.js';
import * as save from './save.js';
import type { Reserve } from './save.js';

export const FIRE_CU_LIMIT = 1_400_000;

/**
 * Dedicated ALT holding the fixed Solend + JupLend-flashloan accounts common to
 * every Save liquidation (create with saveAltPrint, analogous to LIQ_ALT).
 * Override via SAVE_ALT.
 */
export const SAVE_ALT = '11111111111111111111111111111111'; // placeholder until created on-chain

/** One Save liquidation opportunity, sized by the caller (via simulation). */
export interface SaveFireCandidate {
  obligation: PublicKey;
  /** The borrow reserve being repaid. Its `liquidityMint` is the debt asset
   * (USDC/USDT/wSOL) — flash-borrowed, swapped into, and repaid. */
  repayReserve: Reserve;
  /** The collateral reserve being seized (its liquidity mint is what we swap). */
  withdrawReserve: Reserve;
  /** Token program owning the collateral-underlying mint (redeem ATA). */
  collateralTokenProgram: PublicKey;
  /** Token program owning the debt mint (USDC/USDT/wSOL are all classic SPL). */
  debtTokenProgram: PublicKey;
  /** Debt to repay, in the debt asset's native units. */
  repayAmount: bigint;
  /** Expected collateral-underlying out of liquidate+redeem, to size the swap. */
  seizeUnderlying: bigint;
  /** The obligation's deposit + borrow reserves, in obligation order, for
   * refreshObligation. */
  depositReserves: PublicKey[];
  borrowReserves: PublicKey[];
}

export interface SaveFireTx {
  tx: VersionedTransaction;
  /** Debt-asset units the collateral→debt swap is quoted to produce (native).
   * In the same-mint case (collateral underlying == debt) this is the seized
   * amount itself, since there is no swap. */
  quotedDebtOut: bigint;
  txBytes: number;
}

const SYSTEM_PROGRAM_ID = '11111111111111111111111111111111';
const CLASSIC_TOKEN_PROGRAM = 'TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA';

/**
 * Build the unsigned Save fire tx. Quotes the collateral→debt swap live, so
 * call only for a sim-confirmed candidate. `blockhash` = real recent hash for
 * live submission, or default for replace-blockhash simulation.
 */
export async function buildSaveFireTx(
  rpcEndpoint: string,
  c: SaveFireCandidate,
  authority: PublicKey,
  tipAccount: PublicKey | null,
  tipLamports: bigint,
  priorityMicroLamports: bigint,
  slippageBps: number,
  maxSwapAccounts: number,
  blockhash: string,
): Promise<SaveFireTx> {
  // Debt asset = the repay reserve's liquidity mint (USDC/USDT/wSOL). It's the
  // flash-borrow asset, the swap target, and the payback token.
  const debtMint = c.repayReserve.liquidityMint;
  if (!hasMarket(debtMint)) {
    throw new Error(`no JupLend flash market for Save debt mint ${debtMint.toBase58()}`);
  }
  const underlying = c.withdrawReserve.liquidityMint;

  // Same-mint case (seized underlying == debt): no swap — the redeemed
  // liquidity IS the debt asset. Jupiter rejects equal in/out mints, so skip
  // the swap leg (the fixed payback still guards profit).
  const sameMint = underlying.equals(debtMint);
  let swapIxs: TransactionInstruction[];
  let quotedDebtOut: bigint;
  let swapAlts: PublicKey[];
  if (sameMint) {
    swapIxs = [];
    quotedDebtOut = c.seizeUnderlying;
    swapAlts = [];
  } else {
    // ExactIn the redeemed collateral underlying → debt asset. Haircut 0.05%
    // to absorb redeem rounding (same as the marginfi/Kamino paths).
    const swapIn = c.seizeUnderlying - (c.seizeUnderlying / 2000n + 1n);
    const quote = await jup.quote(underlying, debtMint, swapIn, slippageBps, maxSwapAccounts);
    const plan = await jup.swapInstructions(quote, authority, false);
    swapIxs = plan.instructions;
    quotedDebtOut = plan.quotedOut;
    swapAlts = plan.altAddresses;
  }

  const saveAltStr = process.env.SAVE_ALT ?? SAVE_ALT;
  const saveAlt = new PublicKey(saveAltStr);
  const altAddrs = [...swapAlts];
  if (!saveAlt.equals(new PublicKey(SYSTEM_PROGRAM_ID))) {
    altAddrs.push(saveAlt);
  }
  const alts: AddressLookupTableAccount[] = await jup.fetchAlts(rpcEndpoint, altAddrs);

  // Solend cTokens are always classic SPL (the program predates Token-2022 and
  // its liquidate ix passes the classic token program for every transfer).
  const tokenProgram = new PublicKey(CLASSIC_TOKEN_PROGRAM);
  const debtAta = ataFor(authority, debtMint, c.debtTokenProgram);
  const underlyingAta = ataFor(authority, underlying, c.collateralTokenProgram);
  const ctokenAta = ataFor(authority, c.withdrawReserve.collateralMint, tokenProgram);

  const borrowIx = borrow(authority, debtMint, c.repayAmount);
  if (!borrowIx) throw new Error(`no JupLend flash market for Save debt mint ${debtMint.toBase58()}`);
  const paybackIx = payback(authority, debtMint, c.repayAmount);
  if (!paybackIx) throw new Error(`no JupLend flash market for Save debt mint ${debtMint.toBase58()}`);

  const ixs: TransactionInstruction[] = [
    cuLimitIx(FIRE_CU_LIMIT),
    cuPriceIx(priorityMicroLamports),
    createAtaIdempotentFor(authority, debtMint, c.debtTokenProgram),
    createAtaIdempotentFor(authority, underlying, c.collateralTokenProgram),
    createAtaIdempotentFor(authority, c.withdrawReserve.collateralMint, tokenProgram),
  ];
  // Flash-borrow the debt asset we need to repay the liquidatee's debt.
  ixs.push(borrowIx);
  // Refresh Save state, then liquidate+redeem.
  ixs.push(save.refreshReserve(c.repayReserve));
  ixs.push(save.refreshReserve(c.withdrawReserve));
  ixs.push(save.refreshObligation(c.obligation, c.depositReserves, c.borrowReserves));
  ixs.push(
    save.liquidateAndRedeem(
      c.repayAmount,
      debtAta, // sourceLiquidity (repay)
      ctokenAta, // destCollateral (transient cTokens)
      underlyingAta, // destLiquidity (redeemed underlying)
      c.repayReserve,
      c.withdrawReserve,
      c.obligation,
      c.repayReserve.lendingMarket,
      authority, // userTransferAuthority (signer)
    ),
  );
  // Sell the seized underlying for the debt asset (unless it already is it).
  ixs.push(...swapIxs);
  // Fixed-amount payback = the profit-or-revert guard: reverts unless the swap
  // (or the same-mint redeem) covered the borrowed debt exactly.
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
  return { tx, quotedDebtOut, txBytes };
}
