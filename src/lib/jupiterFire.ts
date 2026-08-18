// Port of src/jupiter_fire.rs
//
// Jupiter Lend (Fluid) liquidate instruction builder + fire-path scaffold.
//
// HONESTY BANNER — what is VERIFIED vs INFERRED (read before trusting this).
//
// VERIFIED (from a real mainnet liquidate tx 5nLVofDj... and the IDL):
// - the instruction discriminator + the exact borsh arg encoding (debtAmt u64,
//   colPerUnitDebt u128, absorb bool, transferType Option<enum>,
//   remainingAccountsIndices Vec<u8>), unit-tested to reproduce the captured
//   tx's arg bytes;
// - the 26 named account ORDER (matched account-for-account to that tx).
//
// SOLVED (see jupiterMath.ts + jupiterFireProbe, reversed from the on-chain
// program source and the published SDK, verified against 8 real txs):
// - `remainingAccountsIndices` layout = `[oracleSources, branches, ticks,
//   tickHasDebt]` and the tick/branch account SELECTION -> `buildRemaining
//   Accounts` derives the exact PDA set from live vault state;
// - `colPerUnitDebt` — reversed as a *minimum-acceptable slippage floor*
//   (1e15), NOT the price: the program computes the actual price from the
//   vault oracle itself. Real liquidators pass 0 (accept oracle price — 2/8
//   txs) or a computed floor. `jupiterMath.computeColPerDebt` reproduces the
//   on-chain formula; the resolver revert (to=ADDRESS_DEAD) yields the exact
//   live ratio.
//
// SEED-DERIVED (the former tx-replay dependency is GONE — see
// `deriveLiquidateAccounts`): the per-vault Liquidity-program PDAs
// (reserves/positions/token accounts/rate models), `newBranch`, and the oracle
// `sources` are now derived PURELY from seeds + on-chain vault/oracle state.
// Seeds reversed from the on-chain Anchor source (audit repo
// code-423n4/2026-02-jupiter-lend) and validated against live accounts
// (jupiterSeedProbe PROOF A = 159/159 across 14 vaults) + the on-chain
// program's own `#[account(seeds=...)]` re-derivation at sim (jupiterFireProbe
// STAGE 5: the fully seed-derived set gates at VaultInvalidLiquidation 6027 on a
// vault with NO recent liquidate tx). The liquidate sim is still the ground-truth
// gate before any fire.

import { type AccountMeta, type Blockhash, PublicKey, TransactionInstruction, TransactionMessage, VersionedTransaction } from '@solana/web3.js';
import { LIQUIDITY_PROGRAM, VAULTS_PROGRAM, type Vault } from './jupiter.js';
import {
  branchPda,
  indexForTick,
  findNextTickWithDebt,
  liquidityPda,
  newBranchId,
  rateModelPda,
  reservePda,
  tickHasDebtPda,
  tickPda,
  userBorrowPositionPda,
  userSupplyPositionPda,
  BranchLite,
} from './jupiterMath.js';
import { cuLimitIx, cuPriceIx, transferIx } from './arb.js';
import { ataFor, createAtaIdempotentFor } from './flashloan.js';
import * as jup from './jup.js';
import * as marginfi from './marginfi.js';

/** liquidate discriminator (sha256("global:liquidate")[..8]) — VERIFIED against the on-chain tx. */
export const LIQUIDATE_DISC = Buffer.from([223, 179, 226, 125, 48, 46, 39, 74]);

const SYSTEM = '11111111111111111111111111111111';
const ATA_PROGRAM = 'ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL';

function pk(s: string): PublicKey {
  return new PublicKey(s);
}
function meta(pubkey: PublicKey, isSigner: boolean, isWritable: boolean): AccountMeta {
  return { pubkey, isSigner, isWritable };
}

/**
 * The full account set a liquidate ix needs for one vault. The vault-derived
 * fields come from `VaultConfig`; the Liquidity-program PDAs are captured from
 * a recent on-chain tx via `accountsFromCaptured` (see the honesty banner).
 */
export interface LiquidateAccounts {
  // liquidator-side (we fill these)
  signer: PublicKey;
  signerTokenAccount: PublicKey; // liquidator's borrow-token (debt) ATA
  to: PublicKey;
  toTokenAccount: PublicKey; // liquidator's supply-token (collateral) ATA
  // vault-derived (from VaultConfig)
  vaultConfig: PublicKey;
  vaultState: PublicKey;
  supplyToken: PublicKey;
  borrowToken: PublicKey;
  oracle: PublicKey;
  oracleProgram: PublicKey;
  // Liquidity-program per-vault accounts (captured from a real tx)
  newBranch: PublicKey;
  supplyTokenReservesLiquidity: PublicKey;
  borrowTokenReservesLiquidity: PublicKey;
  vaultSupplyPositionOnLiquidity: PublicKey;
  vaultBorrowPositionOnLiquidity: PublicKey;
  supplyRateModel: PublicKey;
  borrowRateModel: PublicKey;
  supplyTokenClaimAccount: PublicKey;
  liquidity: PublicKey;
  liquidityProgram: PublicKey;
  vaultSupplyTokenAccount: PublicKey;
  vaultBorrowTokenAccount: PublicKey;
  supplyTokenProgram: PublicKey;
  borrowTokenProgram: PublicKey;
  /** tick/branch accounts referenced by `remainingAccountsIndices`. */
  remaining: PublicKey[];
}

function cloneLiquidateAccounts(a: LiquidateAccounts): LiquidateAccounts {
  return { ...a, remaining: [...a.remaining] };
}

/**
 * Index positions of the vault's Liquidity-program accounts inside a captured
 * liquidate ix's account list (VERIFIED order from tx 5nLVofDj...). Lets us lift
 * the hard-to-derive PDAs straight from a real tx for the same vault.
 */
export const ACCT_ORDER: readonly string[] = [
  'signer', 'signerTokenAccount', 'to', 'toTokenAccount', 'vaultConfig',
  'vaultState', 'supplyToken', 'borrowToken', 'oracle', 'newBranch',
  'supplyTokenReservesLiquidity', 'borrowTokenReservesLiquidity',
  'vaultSupplyPositionOnLiquidity', 'vaultBorrowPositionOnLiquidity',
  'supplyRateModel', 'borrowRateModel', 'supplyTokenClaimAccount',
  'liquidity', 'liquidityProgram', 'vaultSupplyTokenAccount',
  'vaultBorrowTokenAccount', 'supplyTokenProgram', 'borrowTokenProgram',
  'systemProgram', 'associatedTokenProgram', 'oracleProgram',
];

/**
 * borsh-encode the liquidate instruction data. VERIFIED: reproduces the arg
 * bytes of the captured tx (see tests). `transferType` is the inner enum
 * discriminant when present (the real tx used 1).
 */
export function buildLiquidateData(debtAmt: bigint, colPerUnitDebt: bigint, absorb: boolean, transferType: number | undefined, remainingAccountsIndices: Uint8Array): Buffer {
  const fixedLen = 8 + 8 + 16 + 1 + (transferType !== undefined ? 2 : 1) + 4;
  const d = Buffer.alloc(fixedLen + remainingAccountsIndices.length);
  let o = 0;
  LIQUIDATE_DISC.copy(d, o);
  o += 8;
  d.writeBigUInt64LE(debtAmt, o);
  o += 8;
  d.writeBigUInt64LE(colPerUnitDebt & 0xffffffffffffffffn, o);
  d.writeBigUInt64LE(colPerUnitDebt >> 64n, o + 8);
  o += 16;
  d.writeUInt8(absorb ? 1 : 0, o);
  o += 1;
  if (transferType !== undefined) {
    d.writeUInt8(1, o);
    o += 1;
    d.writeUInt8(transferType, o);
    o += 1;
  } else {
    d.writeUInt8(0, o);
    o += 1;
  }
  d.writeUInt32LE(remainingAccountsIndices.length, o);
  o += 4;
  Buffer.from(remainingAccountsIndices).copy(d, o);
  return d;
}

/**
 * Assemble the liquidate instruction. Account order is VERIFIED; the caller is
 * responsible for supplying correct `remaining` (tick/branch) accounts +
 * indices (see honesty banner — currently INFERRED).
 */
export function buildLiquidateIx(a: LiquidateAccounts, debtAmt: bigint, colPerUnitDebt: bigint, absorb: boolean, transferType: number | undefined, remainingAccountsIndices: Uint8Array): TransactionInstruction {
  const keys: AccountMeta[] = [
    meta(a.signer, true, true),
    meta(a.signerTokenAccount, false, true),
    meta(a.to, false, false),
    meta(a.toTokenAccount, false, true),
    meta(a.vaultConfig, false, false),
    meta(a.vaultState, false, true),
    meta(a.supplyToken, false, false),
    meta(a.borrowToken, false, false),
    meta(a.oracle, false, false),
    meta(a.newBranch, false, true),
    meta(a.supplyTokenReservesLiquidity, false, true),
    meta(a.borrowTokenReservesLiquidity, false, true),
    meta(a.vaultSupplyPositionOnLiquidity, false, true),
    meta(a.vaultBorrowPositionOnLiquidity, false, true),
    meta(a.supplyRateModel, false, false),
    meta(a.borrowRateModel, false, false),
    meta(a.supplyTokenClaimAccount, false, true),
    meta(a.liquidity, false, false),
    meta(a.liquidityProgram, false, false),
    meta(a.vaultSupplyTokenAccount, false, true),
    meta(a.vaultBorrowTokenAccount, false, true),
    meta(a.supplyTokenProgram, false, false),
    meta(a.borrowTokenProgram, false, false),
    meta(pk(SYSTEM), false, false),
    meta(pk(ATA_PROGRAM), false, false),
    meta(a.oracleProgram, false, false),
  ];
  // Fluid tick/branch remaining accounts (writable — they mutate on liquidation).
  for (const r of a.remaining) {
    keys.push(meta(r, false, true));
  }
  return new TransactionInstruction({
    programId: pk(VAULTS_PROGRAM),
    keys,
    data: buildLiquidateData(debtAmt, colPerUnitDebt, absorb, transferType, remainingAccountsIndices),
  });
}

/**
 * Lift the per-vault Liquidity-program accounts out of a captured liquidate ix
 * account list (the `ACCT_ORDER` positions), so a fresh liquidate for the same
 * vault reuses the exact PDAs it used before. `txAccounts` = the ordered
 * account pubkeys of a real liquidate ix for this vault; `remaining` = anything
 * past index 26. Returns the vault-fixed accounts (signer/token-accounts left
 * to the caller). This is the derive-from-truth account resolver.
 */
export function accountsFromCaptured(v: Vault, txAccounts: PublicKey[]): LiquidateAccounts | undefined {
  if (txAccounts.length < 26) return undefined;
  const g = (i: number): PublicKey => txAccounts[i] as PublicKey;
  return {
    signer: g(0), signerTokenAccount: g(1), to: g(2), toTokenAccount: g(3),
    vaultConfig: v.configPubkey, vaultState: v.statePubkey,
    supplyToken: v.config.supplyToken, borrowToken: v.config.borrowToken,
    oracle: v.config.oracle, oracleProgram: v.config.oracleProgram,
    newBranch: g(9),
    supplyTokenReservesLiquidity: g(10), borrowTokenReservesLiquidity: g(11),
    vaultSupplyPositionOnLiquidity: g(12), vaultBorrowPositionOnLiquidity: g(13),
    supplyRateModel: g(14), borrowRateModel: g(15), supplyTokenClaimAccount: g(16),
    liquidity: g(17), liquidityProgram: g(18),
    vaultSupplyTokenAccount: g(19), vaultBorrowTokenAccount: g(20),
    supplyTokenProgram: g(21), borrowTokenProgram: g(22),
    remaining: txAccounts.slice(26),
  };
}

/**
 * Build the full vault-fixed + Liquidity-program account set for a `liquidate`
 * PURELY FROM SEEDS + on-chain vault state — no captured liquidate tx required.
 * This is the standalone resolver that unblocks arming for vaults with no recent
 * liquidate (the previous `accountsFromCaptured` path). Every Liquidity PDA is
 * re-derived by the liquidity program's own `#[account(seeds=...)]` on the CPI,
 * so a wrong seed reverts at sim (ConstraintSeeds) — the program is the check.
 *
 * Signer-side accounts (signer + our debt/collateral ATAs, `to`) are left default;
 * set them via `setLiquidatorSide`. `remaining` is filled by
 * `buildRemainingAccounts`. `supplyTokenProgram`/`borrowTokenProgram` are the
 * mints' owning token programs (SPL-Token or Token-2022) — resolve from
 * `getAccountInfo(mint).owner`.
 */
export function deriveLiquidateAccounts(v: Vault, supplyTokenProgram: PublicKey, borrowTokenProgram: PublicKey): LiquidateAccounts {
  const vc = v.configPubkey;
  const supply = v.config.supplyToken;
  const borrow = v.config.borrowToken;
  const liq = liquidityPda();
  const nbId = newBranchId(v.state.branchLiquidated, v.state.currentBranchId, v.state.totalBranchId);
  return {
    signer: PublicKey.default,
    signerTokenAccount: PublicKey.default,
    to: PublicKey.default,
    toTokenAccount: PublicKey.default,
    vaultConfig: vc,
    vaultState: v.statePubkey,
    supplyToken: supply,
    borrowToken: borrow,
    oracle: v.config.oracle,
    oracleProgram: v.config.oracleProgram,
    newBranch: branchPda(v.config.vaultId, nbId),
    supplyTokenReservesLiquidity: reservePda(supply),
    borrowTokenReservesLiquidity: reservePda(borrow),
    vaultSupplyPositionOnLiquidity: userSupplyPositionPda(supply, vc),
    vaultBorrowPositionOnLiquidity: userBorrowPositionPda(borrow, vc),
    supplyRateModel: rateModelPda(supply),
    borrowRateModel: rateModelPda(borrow),
    // `supplyTokenClaimAccount` is `Option<UncheckedAccount>` on `Liquidate`
    // and is ONLY consumed for claim-type withdraws (`isClaimType`), which a
    // liquidation is NOT — so the real liquidator passes None. Anchor encodes a
    // None optional account as the invoked program's own id (the VAULTS program),
    // which is what we pass. (The would-be `UserClaim` PDA is `userClaimPda(vc,
    // supply)`; it isn't created for a vault, confirming None — see jupiterMath.)
    supplyTokenClaimAccount: pk(VAULTS_PROGRAM),
    liquidity: liq,
    liquidityProgram: pk(LIQUIDITY_PROGRAM),
    vaultSupplyTokenAccount: ataFor(liq, supply, supplyTokenProgram),
    vaultBorrowTokenAccount: ataFor(liq, borrow, borrowTokenProgram),
    supplyTokenProgram,
    borrowTokenProgram,
    remaining: [],
  };
}

/**
 * The all-zero pubkey. Passing it as `to` triggers the program's built-in
 * resolver: it runs the full liquidation math and REVERTS with
 * `VaultLiquidationResult: [actualCol, actualDebt, topmostTick]` — exact
 * ground truth for pricing/sizing, computed by the program itself.
 */
export const ADDRESS_DEAD = new PublicKey(new Uint8Array(32));

/**
 * Overwrite the liquidator-side accounts (signer + our debt/collateral ATAs)
 * on a captured account set, so a fresh liquidate seizes to OUR wallet.
 */
export function setLiquidatorSide(a: LiquidateAccounts, signer: PublicKey, signerTokenAccount: PublicKey, to: PublicKey, toTokenAccount: PublicKey): void {
  a.signer = signer;
  a.signerTokenAccount = signerTokenAccount;
  a.to = to;
  a.toTokenAccount = toTokenAccount;
}

/**
 * Build the `remainingAccounts` + `remainingAccountsIndices` for a liquidate,
 * derived from CURRENT on-chain state — the layout is
 * `[oracleSources, branches, ticks, tickHasDebt]` (verified against 8 real
 * mainnet txs; see jupiterFireProbe). Ported from the SDK
 * `getRemainingAccountsLiquidate`.
 *
 * `oracleSources` are lifted from a recent tx (deterministic per vault oracle).
 * `fetch` reads raw account bytes (undefined if the account does not exist /
 * not owned by the program). `liquidationTick` is `jupiterMath`'s value (bit-
 * identical to what the program computes) — pass what you compute from the live
 * oracle price via `liquidationTickFromPrice1e8`, or a low bound to include
 * more ticks.
 */
export function buildRemainingAccounts(
  vaultId: number,
  topmostTick: number,
  currentBranchId: number,
  liquidationTick: number,
  oracleSources: PublicKey[],
  fetch: (addr: PublicKey) => Buffer | undefined,
): [PublicKey[], [number, number, number, number]] {
  // ── branches: current branch, then walk connected_branch_id, always incl 0 ──
  const branchIds: number[] = [];
  let connected = 0;
  if (currentBranchId > 0) {
    const raw = fetch(branchPda(vaultId, currentBranchId));
    if (raw !== undefined) {
      const b = BranchLite.decode(raw);
      if (b !== undefined) {
        branchIds.push(currentBranchId);
        connected = b.connectedBranchId;
      }
    }
  }
  while (connected > 0 && !branchIds.includes(connected)) {
    const pda = branchPda(vaultId, connected);
    const raw = fetch(pda);
    const b = raw !== undefined ? BranchLite.decode(raw) : undefined;
    if (b !== undefined) {
      branchIds.push(connected);
      connected = b.connectedBranchId;
    } else {
      break;
    }
  }
  if (!branchIds.includes(0)) branchIds.push(0);

  // ── ticks: topmost (if a real perfect tick exists) then walk down to liqTick ─
  const arrayFetch = (idx: number): Buffer | undefined => fetch(tickHasDebtPda(vaultId, idx));
  const ticks: number[] = [];
  if (topmostTick > liquidationTick && fetch(tickPda(vaultId, topmostTick)) !== undefined) {
    ticks.push(topmostTick);
  }
  let nextTick = findNextTickWithDebt(topmostTick, arrayFetch);
  while (nextTick > liquidationTick && !ticks.includes(nextTick)) {
    if (fetch(tickPda(vaultId, nextTick)) !== undefined) {
      ticks.push(nextTick);
    }
    const n = findNextTickWithDebt(nextTick, arrayFetch);
    if (n === nextTick) break;
    nextTick = n;
  }

  // ── tick_has_debt arrays: index(topmost) down to index(nextTick) ──
  const topIdx = indexForTick(topmostTick);
  const nextIdx = indexForTick(nextTick);
  const hi = Math.max(topIdx, nextIdx);
  const lo = Math.min(topIdx, nextIdx);
  const thdIndices: number[] = [];
  for (let i = hi; i >= lo; i--) thdIndices.push(i);

  // ── assemble in the exact program order ──
  const remaining: PublicKey[] = [];
  remaining.push(...oracleSources);
  for (const b of branchIds) remaining.push(branchPda(vaultId, b));
  for (const t of ticks) remaining.push(tickPda(vaultId, t));
  for (const i of thdIndices) remaining.push(tickHasDebtPda(vaultId, i));

  const indices: [number, number, number, number] = [oracleSources.length, branchIds.length, ticks.length, thdIndices.length];
  return [remaining, indices];
}

// ── flash-loan-wrapped fire tx (USDC-debt vaults; mirrors saveFire.ts) ───────
export const FIRE_CU_LIMIT = 1_400_000;
const USDC_MINT = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v';
const TOKEN_PROGRAM = 'TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA';
/**
 * All-1s system-program pubkey = the "JUP_ALT not deployed yet" sentinel; skip
 * it rather than fetch a table that doesn't exist (mirrors saveFire's SAVE_ALT).
 */
const ALT_PLACEHOLDER = '11111111111111111111111111111111';

/** One sized Jupiter-Lend liquidation opportunity (USDC debt). */
export interface JupiterFireCandidate {
  /**
   * Fully-resolved liquidate accounts (Liquidity PDAs lifted from a real tx;
   * liquidator-side + `remaining` set to OURS via the builder).
   */
  accts: LiquidateAccounts;
  /** Debt (USDC, 6dp) we repay. */
  debtAmt: bigint;
  /**
   * Slippage floor (1e15). 0 = accept the oracle price (proven safe by real
   * txs); or a `computeColPerDebt` / resolver-derived value.
   */
  colPerUnitDebt: bigint;
  /** remaining accounts + indices from `buildRemainingAccounts`. */
  remaining: PublicKey[];
  remainingIndices: [number, number, number, number];
  /** Collateral (supplyToken) underlying we expect to seize, to size the swap. */
  seizeUnderlying: bigint;
  /** Collateral mint + its token program (for the swap-back ATA). */
  collateralMint: PublicKey;
  collateralTokenProgram: PublicKey;
}

export interface JupiterFireTx {
  tx: VersionedTransaction;
  quotedUsdcOut: bigint;
  txBytes: number;
}

/**
 * Build the unsigned flash-loan-wrapped liquidate tx:
 *   [cu, ATAs, marginfi startFlashloan -> borrow USDC,
 *    jupiter LIQUIDATE (repay USDC, seize collateral),
 *    Jupiter swap collateral->USDC, marginfi payback + endFlashloan, tip]
 * Same profit-or-revert shape as the Save path. `blockhash` default for
 * replace-blockhash simulation.
 */
export async function buildJupiterFireTx(
  rpcEndpoint: string,
  c: JupiterFireCandidate,
  liquidatorMa: PublicKey,
  authority: PublicKey,
  tipAccount: PublicKey | undefined,
  tipLamports: bigint,
  priorityMicroLamports: bigint,
  slippageBps: number,
  maxSwapAccounts: number,
  blockhash: Blockhash,
): Promise<JupiterFireTx> {
  const usdc = pk(USDC_MINT);
  const tokenProgram = pk(TOKEN_PROGRAM);
  if (!c.accts.borrowToken.equals(usdc)) {
    throw new Error(`jupiter fire path currently wraps only USDC debt, got ${c.accts.borrowToken.toBase58()}`);
  }

  // Swap leg: sell the seized collateral underlying -> USDC (0.05% haircut for
  // seize rounding, as in saveFire).
  const swapIn = c.seizeUnderlying - (c.seizeUnderlying / 2000n + 1n);
  const quoteResponse = await jup.quote(c.collateralMint, usdc, swapIn, slippageBps, maxSwapAccounts);
  const plan = await jup.swapInstructions(quoteResponse, authority, false);
  const altAddrs = [...plan.altAddresses];
  // JUP_ALT holds the fixed liquidate accounts (see jupAltPrint); LIQ_ALT is
  // accepted too for fleet parity. Both are appended to Jupiter's own swap ALTs,
  // exactly like saveFire folds in SAVE_ALT. An unset var — or the all-1s
  // placeholder (= "not deployed") — is skipped so we never try to fetch a
  // non-existent table.
  for (const varName of ['JUP_ALT', 'LIQ_ALT']) {
    const alt = process.env[varName];
    if (alt === undefined) continue;
    if (alt === ALT_PLACEHOLDER) continue;
    try {
      altAddrs.push(new PublicKey(alt));
    } catch {
      // skip unparseable override
    }
  }
  const alts = await jup.fetchAlts(rpcEndpoint, altAddrs);

  const usdcAta = ataFor(authority, usdc, tokenProgram);
  const collatAta = ataFor(authority, c.collateralMint, c.collateralTokenProgram);

  const a = cloneLiquidateAccounts(c.accts);
  setLiquidatorSide(a, authority, usdcAta, authority, collatAta);
  a.remaining = [...c.remaining];

  const ixs: TransactionInstruction[] = [
    cuLimitIx(FIRE_CU_LIMIT),
    cuPriceIx(priorityMicroLamports),
    createAtaIdempotentFor(authority, usdc, tokenProgram),
    createAtaIdempotentFor(authority, c.collateralMint, c.collateralTokenProgram),
  ];
  const startIdx = ixs.length;
  ixs.push(marginfi.startFlashloan(liquidatorMa, authority, 0n)); // end_index patched below
  ixs.push(marginfi.borrowUsdc(liquidatorMa, authority, usdcAta, c.debtAmt));
  // The reversed liquidate ix — correctly priced + tick/branch accounts.
  ixs.push(buildLiquidateIx(a, c.debtAmt, c.colPerUnitDebt, false, 1, Uint8Array.from(c.remainingIndices)));
  ixs.push(...plan.instructions);
  ixs.push(marginfi.paybackUsdc(liquidatorMa, authority, usdcAta, c.debtAmt, true));
  const endIndex = BigInt(ixs.length);
  ixs[startIdx] = marginfi.startFlashloan(liquidatorMa, authority, endIndex);
  ixs.push(marginfi.endFlashloan(liquidatorMa, authority, []));
  if (tipAccount !== undefined && tipLamports > 0n) {
    ixs.push(transferIx(authority, tipAccount, tipLamports));
  }

  const msg = new TransactionMessage({
    payerKey: authority,
    recentBlockhash: blockhash,
    instructions: ixs,
  }).compileToV0Message(alts);
  const tx = new VersionedTransaction(msg);
  const txBytes = tx.serialize().length;
  return { tx, quotedUsdcOut: plan.quotedOut, txBytes };
}
