// Port of src/liq_fire.rs
//
// The atomic liquidation FIRE path — one flashloan-wrapped v0 tx:
//
//   [cuLimit, cuPrice, create ATAs, startFlashloan,
//    liquidate -> withdrawAll seized collateral -> Jupiter/direct-DEX swap
//    collateral->USDC -> repayAll liability, endFlashloan, tip]
//
// Profit-or-revert with NO external capital: `liquidate` moves internal
// shares (liquidator gains asset shares + takes on the matching liability),
// so no tokens are needed up front; `repayAll` fails unless the swap
// produced enough USDC to cover the full liability, and `endFlashloan`
// re-checks account health — either the whole tx lands net-positive
// (surplus USDC stays in the wallet ATA) or it reverts and costs nothing
// but the fee on a miss.
//
// v1 restriction: the liability bank must be USDC (the dominant marginfi
// debt asset) — the swap leg targets USDC and `paybackUsdc` closes it.

import {
  type AccountMeta,
  type AddressLookupTableAccount,
  PublicKey,
  TransactionInstruction,
  TransactionMessage,
  VersionedTransaction,
} from '@solana/web3.js';
import { cuLimitIx, cuPriceIx, orcaAccounts, pkAt, transferIx } from './arb.js';
import { ataFor, createAtaIdempotentFor } from './flashloan.js';
import * as jup from './jup.js';
import * as marginfi from './marginfi.js';
import { applySwap as clmmApplySwap, clmmFromOrca } from './clmm.js';
import { orcaSwapIx, orcaSwapV2Ix, sqrtLimit } from './swap.js';

export const FIRE_CU_LIMIT = 1_400_000;

/**
 * Dedicated ALT holding the 18 accounts common to every marginfi-USDC
 * liquidation (see liqAltPrint for the set + recreate instructions).
 * Override via LIQ_ALT.
 */
export const LIQ_ALT = 'DEMhLvSJbSZQfCdiH7YicYNopo3EhhapjfoEjt2kJVij';

/** Everything the executor knows about one liquidation opportunity. */
export interface FireCandidate {
  liquidatee: PublicKey;
  assetBank: PublicKey;
  assetMint: PublicKey;
  assetTokenProgram: PublicKey;
  /** Collateral native units to seize (sized by the caller via simulation). */
  assetAmount: bigint;
  /** The liability (debt) bank the liquidator absorbs and must repay. Any of USDC/USDT/wSOL in v1.5. */
  liabBank: PublicKey;
  /** The debt asset's mint + token program — the swap target and payback token. */
  debtMint: PublicKey;
  debtTokenProgram: PublicKey;
  assetOracle: PublicKey;
  liabOracle: PublicKey;
  /** The liquidatee's observation list: [bank(ro), oracle(ro)] per active balance, in balance order. */
  liquidateeObs: AccountMeta[];
}

export interface FireTx {
  /** Unsigned (sign before sending; default signature placeholder). */
  tx: VersionedTransaction;
  /**
   * Jupiter's quoted DEBT-asset out (native) for the seized collateral —
   * compare against the absorbed liability to decide whether firing is worth
   * it. (Named `quotedUsdcOut` historically; now the debt asset, which may
   * be USDC/USDT/wSOL.)
   */
  quotedUsdcOut: bigint;
  txBytes: number;
}

/**
 * A direct-DEX route for the collateral->debt swap (bypasses Jupiter/lite-api,
 * which is rate-limited to death). Orca Whirlpool only for now. v1 targets
 * BONK -> USDC — the dominant marginfi liquidation (BONK = 91% of collateral,
 * USDC = 100% of debt in the census). Override the pool via DEX_POOL_BONK_USDC.
 */
const DEX_POOLS: Array<[string, string]> = [
  ['DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263', '5P6n5omLbLbP4kaPGL8etqQAHEx2UCkaUyvjLDnwV4EY'], // BONK
  ['7vfCXTUXx5WJV5JADk17DUJ4ksgau7utNKj4b963voxs', 'AU971DrPyhhrpRnmEBp5pDTWL2ny7nofb5vYBjDJkR2E'],
  ['85VBFQZC9TZkfaptBWjvUw7YbZjy52A6mjtPGjstQAmQ', '91E61RiGhH9b9Ns8wrb4E3oBNdtkQx2k4xb33pSqt5am'],
  ['HZ1JovNiVvGrGNiiYvEozEVgZ58xaU3RKwX8eACQBCt3', 'Fra9rBL1F5eAgtoqjXsBzZocD1UKbxXoERKVs6e23ixn'],
  ['2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo', '9tXiuRRw7kbejLhZXtxDxYs2REe43uH2e7k1kocgdM9B'],
  ['EKpQGSJtjMFqKZ9KQanSqYXRcF8fBopzLHYxdM65zcjm', 'CN8M75cH57DuZNzW5wSUpTXtMrSfXBFScJoQxVCgAXes'],
  ['pumpCmXqMfrsAkQ5r49WcJnRayYRqmXz6ae8H7H9Dfn', '4AFAkCSkSNmra64irggEFd8ZtF4WCtFe51qVaFFNBL2D'], // PUMP
  ['3NZ9JMVBmGAqocybic2c7LQCJScmgsAZ6vQqTDzcqmJh', '55BrDTCLWayM16GwrMEQU57o4PTm6ceF9wavSdNZcEiy'],
  ['USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA', 'AxqAWNZqozhTn2pkDPgpf5kc5DeBuhLKKNWnt3dLrxdi'], // USDS
  ['JUPyiwrYJFskUPiHa7hkeR8VUtAeFoSYbKedZNsDvCN', '4Ui9QdDNuUaAGqCPcDSp191QrixLzQiLxJ1Gnqvz3szP'], // JUP
  ['2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH', '9RqDTfwCx2SgxsvKpspQHc38HUo3B6hRd3oR9JR966Ps'],
  ['27G8MtK7VtTcCHkpASjSDdkWWYfoqT6ggEuKidVJidD4', 'HD8i7qr1hd9ida6sN71RbkLxbWcbvZS4NA5CY6vfcDpj'],
  ['cbbtcf3aa214zXHbiAZQwf4122FBYbraNdFqgw4iMij', 'HxA6SKW5qA4o12fjVgTpXdq2YnZ5Zv1s7SB4FFomsyLM'], // cbBTC
  ['CASHx9KJUStyftLFWGvEVf59SGeG9sh5FfcnZMVPCASH', '3wijQvPKm6jHQrAkfPpok5o8WjCWPm1DGG17NmeW8q1w'], // CASH
  ['hntyVP6YFm1Hg25TN9WGLqM12b8TQmcknKrdu1oxWux', '5LnAsMfjG32kdUauAzEuzANT6YmM3TSRpL1rWsCUDKus'], // HNT
];

const USDC_MINT = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v';

export function directDexPool(collateral: PublicKey, debt: PublicKey): PublicKey | null {
  if (debt.toBase58() !== USDC_MINT) return null; // every pool here routes to USDC
  const c = collateral.toBase58();
  const found = DEX_POOLS.find(([m]) => m === c);
  return found ? new PublicKey(found[1]) : null;
}

/**
 * Shared, live pool-state cache. The streaming executor gRPC-subscribes the
 * DEX pools and pushes their bytes here, so the fire path reads pool state
 * from RAM instead of a ~45ms getAccountInfo — critical for the burst "tail"
 * (fires that weren't pre-armed). Empty by default -> falls back to RPC
 * (polling executor).
 */
const poolCache = new Map<string, Buffer>();

/** Push fresh pool bytes (called from the gRPC stream on each pool update). */
export function updatePoolCache(pool: PublicKey, bytes: Buffer): void {
  poolCache.set(pool.toBase58(), bytes);
}

/** The DEX pool addresses to subscribe/stream (so the executor knows what to watch). */
export function dexPoolAddresses(): PublicKey[] {
  return DEX_POOLS.map(([, p]) => new PublicKey(p));
}

/** Fetch one account's raw bytes via RPC (off the hot path) — live pool state for the direct-DEX swap. */
async function fetchAccount(endpoint: string, key: PublicKey): Promise<Buffer> {
  const res = await fetch(endpoint, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({
      jsonrpc: '2.0',
      id: 1,
      method: 'getAccountInfo',
      params: [key.toBase58(), { encoding: 'base64' }],
    }),
  });
  const v = (await res.json()) as { result?: { value?: { data?: [string, string] } } };
  const d = v.result?.value?.data?.[0];
  if (!d) throw new Error(`no account data for ${key.toBase58()}`);
  return Buffer.from(d, 'base64');
}

/**
 * Build the Orca Whirlpool swap ix for the seized collateral -> debt asset,
 * entirely from live pool bytes (no network aggregator). Returns (ixs, quotedOut).
 */
async function orcaDirectSwap(
  rpcEndpoint: string,
  poolPk: PublicKey,
  c: FireCandidate,
  authority: PublicKey,
  swapIn: bigint,
  slippageBps: number,
): Promise<[TransactionInstruction[], bigint]> {
  // Streamed pool state from RAM (µs) if present; else RPC fetch (~45ms fallback).
  const pb = poolCache.get(poolPk.toBase58()) ?? (await fetchAccount(rpcEndpoint, poolPk));
  if (pb.length < 213) throw new Error('orca pool too small');
  // Direction: input is token0 (a_to_b / zero_for_one) iff assetMint == mint0@101.
  const mint0 = pkAt(pb, 101);
  const aToB = c.assetMint.equals(mint0);
  const feeRate = pb.readUInt16LE(45); // Orca feeRate (1e-6) -> bps = /100
  const cl = clmmFromOrca(pb, 0, 0, feeRate / 100.0);
  if (!cl) throw new Error('orca state');
  const quoted = BigInt(Math.max(0, Math.trunc(clmmApplySwap(cl, aToB, Number(swapIn)))));
  const minOut = BigInt(Math.trunc(Number(quoted) * (1.0 - slippageBps / 1e4)));
  const accts = orcaAccounts(pb, poolPk, authority, aToB, c.assetMint, c.assetTokenProgram, c.debtTokenProgram);
  // Token-2022 mints (e.g. cbBTC) need swapV2 (passes token programs + mints).
  const T22 = 'TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb';
  const needsV2 = c.assetTokenProgram.toBase58() === T22 || c.debtTokenProgram.toBase58() === T22;
  let ix: TransactionInstruction;
  if (needsV2) {
    const mintA = pkAt(pb, 101);
    const mintB = pkAt(pb, 181);
    const [tpA, tpB] = aToB ? [c.assetTokenProgram, c.debtTokenProgram] : [c.debtTokenProgram, c.assetTokenProgram];
    ix = orcaSwapV2Ix(accts, mintA, mintB, tpA, tpB, swapIn, minOut, sqrtLimit(aToB), true, aToB);
  } else {
    ix = orcaSwapIx(accts, swapIn, minOut, sqrtLimit(aToB), true, aToB);
  }
  return [[ix], quoted];
}

export async function buildFireTx(
  rpcEndpoint: string,
  c: FireCandidate,
  liquidatorMa: PublicKey,
  authority: PublicKey,
  tipAccount: PublicKey | null,
  tipLamports: bigint,
  priorityMicroLamports: bigint,
  slippageBps: number,
  maxSwapAccounts: number,
  blockhash: string,
): Promise<FireTx> {
  const tokenProgram = new PublicKey('TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA');

  // Swap leg: ExactIn the seized collateral -> DEBT asset (USDC/USDT/wSOL).
  // Haircut 0.05%: the seize->withdraw round-trip goes through marginfi share
  // math and can round down a few native units, and an ExactIn of the full
  // amount would fail on insufficient funds — the dust stays in the asset ATA.
  //
  // Same-mint case (collateral mint == debt mint, e.g. SOL collateral / SOL
  // debt): no swap — the withdrawn collateral IS the debt asset (same ATA), so
  // repay spends it directly.
  const swapIn = c.assetAmount - (c.assetAmount / 2000n + 1n);
  const sameMint = c.assetMint.equals(c.debtMint);
  let swapIxs: TransactionInstruction[];
  let quotedOut: bigint;
  const swapAlts: PublicKey[] = [];
  if (sameMint) {
    swapIxs = [];
    quotedOut = swapIn;
  } else {
    const pool = directDexPool(c.assetMint, c.debtMint);
    if (pool) {
      // Direct-DEX (Orca Whirlpool) — no Jupiter, no HTTP quote, no rate limit.
      const [ixs, quoted] = await orcaDirectSwap(rpcEndpoint, pool, c, authority, swapIn, slippageBps);
      swapIxs = ixs;
      quotedOut = quoted;
    } else {
      // NO Jupiter on the fire path. A Jupiter HTTP quote is 100s of ms (+429
      // backoff) — it can never be a 1ms fire, and its multi-second hangs
      // starve the MAX_INFLIGHT slots and block the fast direct-DEX fires. A
      // pair with no direct-DEX pool fast-fails here (µs); recapture it by
      // ADDING a pool.
      void maxSwapAccounts;
      throw new Error(`no direct-DEX pool for ${c.assetMint.toBase58()}->${c.debtMint.toBase58()} — add a pool`);
    }
  }
  // Jupiter's route ALTs + our liquidation ALT (the fixed marginfi accounts).
  const liqAlt = new PublicKey(process.env.LIQ_ALT ?? LIQ_ALT);
  const altAddrs = [...swapAlts, liqAlt];
  const alts: AddressLookupTableAccount[] = await jup.fetchAlts(rpcEndpoint, altAddrs);

  const assetAta = ataFor(authority, c.assetMint, c.assetTokenProgram);
  const debtAta = ataFor(authority, c.debtMint, c.debtTokenProgram);

  const ixs: TransactionInstruction[] = [
    cuLimitIx(FIRE_CU_LIMIT),
    cuPriceIx(priorityMicroLamports),
    createAtaIdempotentFor(authority, c.assetMint, c.assetTokenProgram),
    createAtaIdempotentFor(authority, c.debtMint, c.debtTokenProgram),
  ];
  void tokenProgram;
  const startIdx = ixs.length;
  ixs.push(marginfi.startFlashloan(liquidatorMa, authority, 0n)); // end_index patched below
  ixs.push(
    marginfi.lendingAccountLiquidate(
      c.assetBank,
      c.liabBank,
      liquidatorMa,
      authority,
      c.liquidatee,
      tokenProgram,
      c.assetAmount,
      c.assetOracle,
      c.liabOracle,
      c.liquidateeObs,
    ),
  );
  ixs.push(marginfi.lendingAccountWithdraw(liquidatorMa, authority, c.assetBank, assetAta, c.assetTokenProgram, c.assetAmount, true));
  ixs.push(...swapIxs);
  // repayAll clears the entire liability regardless of amount (verified in
  // marginfiProbe); pass the quoted swap output as a plausible amount. Uses
  // the generic payback for the actual debt bank (USDC/USDT/wSOL).
  ixs.push(marginfi.paybackAsset(liquidatorMa, authority, c.liabBank, debtAta, quotedOut, true));
  // withdrawAll + repayAll close both balances -> endFlashloan health check
  // runs over zero active balances (empty observation list).
  const endIndex = BigInt(ixs.length);
  ixs[startIdx] = marginfi.startFlashloan(liquidatorMa, authority, endIndex);
  ixs.push(marginfi.endFlashloan(liquidatorMa, authority, []));
  // Tip last, in-tx -> only paid when the liquidation lands.
  if (tipAccount && tipLamports > 0n) {
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
