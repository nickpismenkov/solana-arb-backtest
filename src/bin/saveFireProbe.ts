// Port of src/bin/save_fire_probe.rs
//
// Verify the Save fire path composes across debt assets: find real liquidatable
// v1 obligations (1 collateral deposit + 1 borrow, debt ∈ {USDC,USDT,wSOL}),
// build the flash-loan-wrapped liquidate+redeem+swap+repay tx, and
// simulateTransaction. Success = a live profitable liquidation (CLEAN sim); a
// revert at the Solend liquidate/health gate (custom err 29 LiquidationTooSmall
// = healthy at the fresh price) proves every upstream leg (JupLend flash borrow,
// refresh, liquidate wiring, Jupiter swap, payback) composes. Reports tx byte
// size (flags if a SAVE_ALT is needed to fit 1232B). Read-only — never submits.
//
// Usage: HELIUS_RPC=<url> [DEBT=all|usdc|usdt|wsol] [TRIES=25] [MIN_DEBT=50]
//        [REPAY_FRAC=0.2] [RATIO_CAP=3.0] [MAX_SWAP_ACCOUNTS=18]
//        npx tsx src/bin/saveFireProbe.ts

import 'dotenv/config';
import { PublicKey } from '@solana/web3.js';
import * as save from '../lib/save.js';
import {
  decodeObligation,
  decodeReserve,
  obligationHealthRatio,
  obligationLiquidatable,
  type Obligation,
  type Reserve,
} from '../lib/save.js';
import { buildSaveFireTx, type SaveFireCandidate } from '../lib/saveFire.js';

const CLASSIC_TOKEN_PROGRAM = 'TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA';
// solana_hash::Hash::default() is 32 zero bytes, which base58-encodes to 32 '1's.
const DEFAULT_BLOCKHASH = '11111111111111111111111111111111';

async function rpc(endpoint: string, body: unknown): Promise<any | null> {
  for (let attempt = 0; attempt < 4; attempt++) {
    try {
      const res = await fetch(endpoint, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(body),
      });
      return await res.json();
    } catch {
      // fall through to retry
    }
    await new Promise((r) => setTimeout(r, 400 << attempt));
  }
  return null;
}

function b64(d: any): Buffer | null {
  const s = d?.[0];
  if (typeof s !== 'string') return null;
  try {
    return Buffer.from(s, 'base64');
  } catch {
    return null;
  }
}

async function getAcct(endpoint: string, pk: PublicKey): Promise<Buffer | null> {
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getAccountInfo',
    params: [pk.toBase58(), { encoding: 'base64' }],
  });
  return b64(v?.result?.value?.data);
}

async function mintOwner(endpoint: string, mint: PublicKey): Promise<PublicKey | null> {
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getAccountInfo',
    params: [mint.toBase58(), { encoding: 'base64' }],
  });
  const owner = v?.result?.value?.owner;
  if (typeof owner !== 'string') return null;
  try {
    return new PublicKey(owner);
  } catch {
    return null;
  }
}

async function load(endpoint: string, reserves: Map<string, Reserve>, pk: PublicKey): Promise<Reserve | null> {
  const existing = reserves.get(pk.toBase58());
  if (existing !== undefined) return existing;
  const raw = await getAcct(endpoint, pk);
  if (raw === null) return null;
  const r = decodeReserve(pk, raw);
  if (r === null) return null;
  reserves.set(pk.toBase58(), r);
  return r;
}

/** Run one debt asset: rank its liquidatable v1 candidates near threshold, build
 * + sim the top TRIES, and tally CLEAN / too-small-at-fresh / other. */
async function runAsset(
  endpoint: string,
  label: string,
  debtMint: PublicKey,
  entries: any[],
  reserves: Map<string, Reserve>,
  authority: PublicKey,
  tries: number,
  minDebt: number,
  repayFrac: number,
  ratioCap: number,
  maxSwapAccounts: number,
  sameMintOnly: boolean,
): Promise<void> {
  // Candidates: v1, this debt mint, liquidatable, ≥ minDebt, ratio ≤ ratioCap
  // (the cap drops mis-priced-dust obligations — huge borrowed / ~0 unhealthy —
  // that would otherwise sort first and waste every try, matching the engine).
  const cands: Array<[number, PublicKey, Obligation]> = [];
  for (const e of entries) {
    const pkStr = e?.pubkey;
    if (typeof pkStr !== 'string') continue;
    let pk: PublicKey;
    try {
      pk = new PublicKey(pkStr);
    } catch {
      continue;
    }
    const bytes = b64(e?.account?.data);
    if (bytes === null) continue;
    const o = decodeObligation(bytes);
    if (o === null) continue;
    if (o.deposits.length !== 1 || o.borrows.length !== 1) continue;
    if (!obligationLiquidatable(o) || o.borrowedValue < minDebt) continue;
    const r = obligationHealthRatio(o);
    if (r > ratioCap) continue;
    // Keep only this debt mint — its reserve is pre-loaded, so this is free.
    const rv = reserves.get(o.borrows[0].reserve.toBase58());
    if (rv === undefined || !rv.liquidityMint.equals(debtMint)) continue;
    cands.push([r, pk, o]);
  }
  cands.sort((a, b) => b[0] - a[0]);
  console.log(`\n== ${label} debt: ${cands.length} liquidatable v1 candidates (≥ $${minDebt}, ratio ≤ ${ratioCap}); trying top ${tries} ==`);

  let clean = 0;
  let tooSmall = 0;
  let other = 0;
  let tried = 0;
  let maxBytes = 0;
  const debtTp = new PublicKey(CLASSIC_TOKEN_PROGRAM);
  for (const [ratio, pk, o] of cands) {
    if (tried >= tries) break;
    const repayReserve = await load(endpoint, reserves, o.borrows[0].reserve);
    if (repayReserve === null) continue;
    const withdrawReserve = await load(endpoint, reserves, o.deposits[0].reserve);
    if (withdrawReserve === null) continue;
    // sameMintOnly targets the sub-1232B path (no swap leg): collateral
    // underlying == debt mint. Skip others without spending a try.
    if (sameMintOnly && !withdrawReserve.liquidityMint.equals(debtMint)) continue;
    const ctp = await mintOwner(endpoint, withdrawReserve.liquidityMint);
    if (ctp === null) continue;
    tried += 1;
    const debtDec = 10 ** repayReserve.mintDecimals;
    const repayUsd = o.borrowedValue * repayFrac;
    const repayAmount = BigInt(Math.max(1, Math.trunc((repayUsd / Math.max(repayReserve.marketPrice, 1e-9)) * debtDec)));
    const seizedUsd = repayUsd * (1.0 + withdrawReserve.liquidationBonusPct / 100.0);
    const seizeUnderlying = BigInt(
      Math.trunc((seizedUsd / Math.max(withdrawReserve.marketPrice, 1e-9)) * 10 ** withdrawReserve.mintDecimals),
    );
    const cand: SaveFireCandidate = {
      obligation: pk,
      repayReserve,
      withdrawReserve,
      collateralTokenProgram: ctp,
      debtTokenProgram: debtTp,
      repayAmount,
      seizeUnderlying,
      depositReserves: [withdrawReserve.reserve],
      borrowReserves: [repayReserve.reserve],
    };
    const sameMint = withdrawReserve.liquidityMint.equals(repayReserve.liquidityMint);
    let fire;
    try {
      fire = await buildSaveFireTx(endpoint, cand, authority, null, 0n, 50_000n, 100, maxSwapAccounts, DEFAULT_BLOCKHASH);
    } catch (e) {
      console.log(`  ${pk.toBase58()} ratio ${ratio.toFixed(3)} $${o.borrowedValue.toFixed(0)}: build failed: ${e}`);
      other += 1;
      continue;
    }
    maxBytes = Math.max(maxBytes, fire.txBytes);
    const b64tx = Buffer.from(fire.tx.serialize()).toString('base64');
    const sim = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'simulateTransaction',
      params: [b64tx, { sigVerify: false, replaceRecentBlockhash: true, commitment: 'processed', encoding: 'base64' }],
    });
    const sm = sameMint ? ' same-mint(no-swap)' : '';
    const value = sim?.result?.value;
    if (value !== undefined && value !== null && value.err === null) {
      clean += 1;
      console.log(
        `  ★★ ${pk.toBase58()} ratio ${ratio.toFixed(3)} $${o.borrowedValue.toFixed(0)}: SIMULATES CLEAN — WOULD FIRE (${fire.txBytes}B${sm}, out ${fire.quotedDebtOut}, ${value.unitsConsumed} CU)`,
      );
    } else if (value !== undefined && value !== null) {
      const errStr = JSON.stringify(value.err);
      if (errStr.includes('29')) {
        tooSmall += 1;
        console.log(
          `  ·  ${pk.toBase58()} ratio ${ratio.toFixed(3)} $${o.borrowedValue.toFixed(0)}: GATED at Solend liquidate (err 29 = healthy/too-small at fresh price) (${fire.txBytes}B${sm}) — wiring composes`,
        );
      } else {
        other += 1;
        console.log(`  ${pk.toBase58()} ratio ${ratio.toFixed(3)} $${o.borrowedValue.toFixed(0)}: OTHER err ${errStr} (${fire.txBytes}B${sm})`);
        const logs: string[] = Array.isArray(value.logs) ? value.logs : [];
        for (const l of logs.slice(-5)) {
          console.log(`       ${l}`);
        }
      }
    } else {
      other += 1;
      // No result.value → the RPC rejected the tx pre-execution (most
      // commonly "too large" when > 1232B without a SAVE_ALT).
      const err = sim?.error?.message ?? 'no sim value';
      console.log(`  ${pk.toBase58()} ratio ${ratio.toFixed(3)} $${o.borrowedValue.toFixed(0)}: sim rejected (${fire.txBytes}B${sm}): ${err}`);
    }
  }
  console.log(`── ${label}: tried ${tried} · CLEAN(would-fire) ${clean} · gated-at-liquidate(composes) ${tooSmall} · other ${other} · max tx ${maxBytes}B ──`);
  if (maxBytes > 1232) {
    console.log('   ⚠ tx exceeds 1232B — a SAVE_ALT is required for live submission (set SAVE_ALT to a deployed ALT).');
  }
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (endpoint === undefined) throw new Error('HELIUS_RPC');
  const debt = (process.env.DEBT ?? 'all').toLowerCase();
  const tries = Number.parseInt(process.env.TRIES ?? '', 10) || 25;
  const minDebt = Number.parseFloat(process.env.MIN_DEBT ?? '') || 50.0;
  const repayFrac = Number.parseFloat(process.env.REPAY_FRAC ?? '') || 0.2;
  const ratioCap = Number.parseFloat(process.env.RATIO_CAP ?? '') || 3.0;
  const maxSwapAccounts = Number.parseInt(process.env.MAX_SWAP_ACCOUNTS ?? '', 10) || 18;
  const sameMintOnly = process.env.SAMEMINT !== undefined ? process.env.SAMEMINT !== '0' : false;
  const authority = new PublicKey(process.env.AUTHORITY ?? 'DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak');

  // Pre-load the three debt reserves so the mint match is free.
  const reserves = new Map<string, Reserve>();
  for (const res of [save.USDC_RESERVE, save.USDT_RESERVE, save.WSOL_RESERVE]) {
    const pk = new PublicKey(res);
    await load(endpoint, reserves, pk);
  }

  console.error('[save-fire] scanning main-pool obligations …');
  const resp = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getProgramAccounts',
    params: [
      save.SOLEND_PROGRAM,
      {
        encoding: 'base64',
        dataSize: 1300,
        filters: [{ dataSize: 1300 }, { memcmp: { offset: 10, bytes: save.MAIN_POOL } }],
      },
    ],
  });
  const entries: any[] = resp?.result ?? [];
  console.error(`[save-fire] ${entries.length} obligations; debt filter = ${debt}`);

  const assets: Array<[string, string]> =
    debt === 'usdc'
      ? [['USDC', save.USDC_MINT]]
      : debt === 'usdt'
        ? [['USDT', save.USDT_MINT]]
        : debt === 'wsol' || debt === 'sol'
          ? [['wSOL', save.WSOL_MINT]]
          : [
              ['USDC', save.USDC_MINT],
              ['USDT', save.USDT_MINT],
              ['wSOL', save.WSOL_MINT],
            ];
  for (const [label, mint] of assets) {
    const m = new PublicKey(mint);
    await runAsset(endpoint, label, m, entries, reserves, authority, tries, minDebt, repayFrac, ratioCap, maxSwapAccounts, sameMintOnly);
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
