// Port of src/bin/save_overflag_probe.rs
//
// Quantify the Save two-tier gating fix + calibrate the on-chain fire gate on
// LIVE mainnet data — read-only.
//
// The overflag bug: the executor flagged obligations "liquidatable" off the
// LAZER-projected ratio, then ran a full simulateTransaction/Bundle on each. But
// Solend settles at the ON-CHAIN oracle price, and Lazer leads/diverges — so the
// flagged set was dominated by phantoms (healthy on-chain), a per-cycle sim flood
// that starves a real opportunity's sim budget.
//
// The fix: Lazer NARROWS the watch-set; the ON-CHAIN price GATES the sim. Only
// obligations liquidatable at the on-chain oracle price earn a sim, ranked by USD
// deficit and capped top-K (MAX_FIRE_PER_CYCLE).
//
// CALIBRATION (task point 4): an obligation's STORED borrowed/unhealthy values
// are lazily updated by Solend (only when someone refresh_obligation's it), so a
// marginally-over-threshold obligation can sit "stored-liquidatable" while a fresh
// refresh_reserve (fresh Pyth price) shows it healthy — the "healthy at fresh
// price" sim rejects. This probe RE-COMPUTES each obligation's health from the
// freshly-fetched reserve prices + amounts (cToken exchange rate from the reserve
// bytes) and reports, for the stored-liquidatable set: (a) how many stay
// liquidatable at the fresh RESERVE price (the calibrated fire gate), and (b) the
// per-cycle sim reduction. If (a) is still hundreds, the residual phantoms are
// live-Pyth-vs-cranked-reserve drift the top-K cap must absorb.
//
// Usage: HELIUS_RPC=<url> [MIN_DEBT=100] [WATCH_RATIO=0.85] [ARM_RATIO=0.97]
//        [RATIO_CAP=3.0] [MAX_FIRE=4] npx tsx src/bin/saveOverflagProbe.ts

import 'dotenv/config';
import { PublicKey } from '@solana/web3.js';
import * as save from '../lib/save.js';
import {
  ctokenExchangeRate,
  decodeObligation,
  decodeReserve,
  obligationFreshHealth,
  obligationHealthRatio,
  obligationLiquidatable,
  type Obligation,
  type Reserve,
} from '../lib/save.js';
import { Engine } from '../lib/saveEngine.js';
import * as lazer from '../lib/lazer.js';

const LAZER_USDT = 8;

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

async function getMultiple(endpoint: string, keys: PublicKey[]): Promise<Map<string, Buffer>> {
  const out = new Map<string, Buffer>();
  for (let i = 0; i < keys.length; i += 100) {
    const chunk = keys.slice(i, i + 100);
    const strs = chunk.map((k) => k.toBase58());
    const v = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'getMultipleAccounts',
      params: [strs, { encoding: 'base64' }],
    });
    const arr: any[] = v?.result?.value ?? [];
    arr.forEach((acc, idx) => {
      const b = b64(acc?.data);
      if (b !== null) out.set(chunk[idx].toBase58(), b);
    });
  }
  return out;
}

// The cToken exchange rate + fresh-price health now live on lib/save.ts
// (ctokenExchangeRate, obligationFreshHealth), so this probe just exercises
// those directly — no local layout duplication.

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (endpoint === undefined) throw new Error('HELIUS_RPC');
  const minDebt = Number.parseFloat(process.env.MIN_DEBT ?? '') || 100.0;
  const watchRatio = Number.parseFloat(process.env.WATCH_RATIO ?? '') || 0.85;
  const armRatio = Number.parseFloat(process.env.ARM_RATIO ?? '') || 0.97;
  const ratioCap = Number.parseFloat(process.env.RATIO_CAP ?? '') || 3.0;
  const maxFire = Number.parseInt(process.env.MAX_FIRE ?? '', 10) || 4;

  // Debt reserves (USDC/USDT/wSOL) — the accepted debt set.
  const reserves = new Map<string, Reserve>();
  for (const res of [save.USDC_RESERVE, save.USDT_RESERVE, save.WSOL_RESERVE]) {
    const pk = new PublicKey(res);
    const d = await getAcct(endpoint, pk);
    if (d !== null) {
      const r = decodeReserve(pk, d);
      if (r !== null) reserves.set(pk.toBase58(), r);
    }
  }
  const debtReserves = new Set<string>(reserves.keys());

  console.error('[overflag] scanning main-pool obligations …');
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

  const obls: Array<[PublicKey, Obligation]> = [];
  for (const e of entries) {
    const pkStr = e?.pubkey;
    if (typeof pkStr !== 'string') continue;
    const d = b64(e?.account?.data);
    if (d === null) continue;
    const o = decodeObligation(d);
    if (o === null) continue;
    if (o.deposits.length !== 1 || o.borrows.length !== 1) continue;
    if (!debtReserves.has(o.borrows[0].reserve.toBase58())) continue;
    if (o.borrowedValue < minDebt) continue;
    obls.push([new PublicKey(pkStr), o]);
  }

  // Load collateral reserves.
  const collSeen = new Set<string>();
  const collPks: PublicKey[] = [];
  for (const [, o] of obls) {
    const s = o.deposits[0].reserve.toBase58();
    if (!collSeen.has(s)) {
      collSeen.add(s);
      collPks.push(o.deposits[0].reserve);
    }
  }
  const collData = await getMultiple(endpoint, collPks);
  for (const [pkStr, raw] of collData) {
    const r = decodeReserve(new PublicKey(pkStr), raw);
    if (r !== null) reserves.set(pkStr, r);
  }

  const mintFeed = lazer.mintFeedMap();
  mintFeed.set(save.USDT_MINT, LAZER_USDT);

  const snap = new Map<number, number>();
  const engine = new Engine(minDebt, ratioCap);
  engine.rebuild(obls, reserves, mintFeed, watchRatio, snap);

  const armTier = engine.crossed(snap, armRatio).length;
  // BEFORE (old gate): the obligation's own STORED borrowedValue >
  // unhealthyBorrowValue (Solend's lazily-refreshed verdict, which the executor
  // used to flag + sim). AFTER (new gate): the engine's FRESH-price fire tier —
  // borrowed/unhealthy recomputed at the current reserve prices via the cToken
  // exchange rate (obligationFreshHealth), the value Solend's `liquidate`
  // recomputes at settle time.
  const oblsByPk = new Map<string, Obligation>(obls.map(([pk, o]) => [pk.toBase58(), o]));
  const storedLiq: Array<[PublicKey, number, number]> = [];
  for (const [pk, o] of obls) {
    const r = obligationHealthRatio(o);
    if (obligationLiquidatable(o) && r <= ratioCap) {
      storedLiq.push([pk, o.borrowedValue - o.unhealthyBorrowValue, r]);
    }
  }
  const freshFire = engine.onchainLiquidatableRanked();

  // How many of the STORED-liquidatable set are phantoms (healthy at fresh price)?
  let phantom = 0;
  for (const [pk] of storedLiq) {
    const o = oblsByPk.get(pk.toBase58())!;
    const fh = obligationFreshHealth(o, reserves);
    const isLive = fh !== null && fh[1] > 0.0 && fh[0] > fh[1];
    if (!isLive) phantom += 1;
  }

  console.log('\n=== Save fire-tier gate: STORED verdict vs FRESH cToken health — live mainnet ===');
  console.log(`scanned obligations (main-pool, 1300B) ........ ${entries.length}`);
  console.log(`v1 / accepted-debt / ≥ $${minDebt.toFixed(0)} ............... ${obls.length}`);
  console.log(`engine watch-set (${watchRatio} ≤ ratio ≤ ${ratioCap}) ...... ${engine.accounts.length}  (NEVER simulated)`);
  console.log(`within arm(${armRatio}) — Lazer near-threshold ...... ${armTier}`);
  console.log(`BEFORE — on-chain liquidatable (STORED verdict) . ${storedLiq.length}  ← the phantom flood`);
  console.log(`AFTER  — on-chain liquidatable (FRESH cToken)  .. ${freshFire.length}  ← NEW fire gate`);
  console.log(`  stored-liquidatable that are phantoms @ fresh . ${phantom}  (dropped by the fresh gate)`);
  console.log(`fire cap (MAX_FIRE_PER_CYCLE) ................. ${maxFire}`);

  console.log('\nDIAGNOSTIC — stored deposit/borrow market_value vs FRESH recompute @ current reserve px');
  console.log('(the collateral gap is the staleness that left the stored health stale-high):');
  for (const [pk] of storedLiq.slice(0, 6)) {
    const o = oblsByPk.get(pk.toBase58())!;
    const d = o.deposits[0];
    const b = o.borrows[0];
    const coll = reserves.get(d.reserve.toBase58())!;
    const debt = reserves.get(b.reserve.toBase58())!;
    const rate = ctokenExchangeRate(coll);
    const fh = obligationFreshHealth(o, reserves) ?? [0.0, 0.0];
    const [freshBor, freshUnh] = fh;
    const freshDep = (Number(d.depositedAmount) * rate) / 10 ** coll.mintDecimals * coll.marketPrice;
    console.log(`  ${pk.toBase58()}`);
    console.log(`     borrow  stored mv $${b.marketValue.toFixed(2)}  fresh $${freshBor.toFixed(2)}  (debt px $${debt.marketPrice.toFixed(4)})`);
    console.log(
      `     deposit stored mv $${d.marketValue.toFixed(2)}  fresh $${freshDep.toFixed(2)}  (coll px $${coll.marketPrice.toFixed(4)}, cToken rate ${rate.toFixed(5)}, liq_thr ${coll.liquidationThresholdPct}% → fresh unhealthy $${freshUnh.toFixed(2)})`,
    );
  }

  console.log('\ntop fresh fire-tier candidates (deficit desc), fresh ratio:');
  for (const [pk, deficit] of freshFire.slice(0, 10)) {
    const fr = engine.onchainRatioOf(pk) ?? 0.0;
    console.log(`  ${pk.toBase58()}  fresh deficit $${deficit.toFixed(0)}  fresh r${fr.toFixed(4)}`);
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
