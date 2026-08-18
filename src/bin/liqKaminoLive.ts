// Port of src/bin/liq_kamino_live.rs
//
// Kamino LIVE-health finder — recomputes each obligation's health from CURRENT
// reserve prices (replicating refresh_obligation), instead of trusting the
// obligation's stored (possibly stale) values.
//
// Two outputs:
//   1. VALIDATION — for fresh obligations, recomputed vs stored aggregates
//      should match (proves the recompute math against on-chain truth).
//   2. ALPHA — obligations that are liquidatable at current prices, especially
//      ones whose STORED values still say healthy (stale -> a refresh_obligation
//      would flag them; catching these ahead of the crank is the only edge).
//
// Reserve prices come from each reserve's cached market_price (refresh_reserve),
// which stays fresh because reserves are cranked constantly — so we sidestep
// Scope. Freshness of those prices is reported.
//
// Usage: HELIUS_RPC=<url> [MARKET=all] [MIN_COLLATERAL_USD=50] [NEAR=25]
//        tsx src/bin/liqKaminoLive.ts

import 'dotenv/config';
import { PublicKey } from '@solana/web3.js';
import {
  decodeObligation,
  decodeReserve,
  KLEND_PROGRAM,
  obligationLiquidatable,
  recompute,
  recomputedLiquidatable,
  recomputedRatio,
  recomputedTrustworthy,
  type Obligation,
  type Recomputed,
  type ReserveMap,
} from '../lib/kamino.js';

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
async function rpc(endpoint: string, body: unknown): Promise<any | undefined> {
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
    await sleep(400 << attempt);
  }
  return undefined;
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
function b64(data: any): Buffer | undefined {
  const s = data?.[0];
  if (typeof s !== 'string') return undefined;
  try {
    return Buffer.from(s, 'base64');
  } catch {
    return undefined;
  }
}

async function getMultiple(endpoint: string, keys: PublicKey[], sliceLen: number): Promise<Map<string, Buffer>> {
  const out = new Map<string, Buffer>();
  for (let i = 0; i < keys.length; i += 100) {
    const chunk = keys.slice(i, i + 100);
    const strs = chunk.map((k) => k.toBase58());
    const v = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'getMultipleAccounts',
      params: [strs, { encoding: 'base64', dataSlice: { offset: 0, length: sliceLen } }],
    });
    const arr: unknown[] = Array.isArray(v?.result?.value) ? v.result.value : [];
    arr.forEach((acc, j) => {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const bytes = (acc as any)?.data !== undefined ? b64((acc as any).data) : undefined;
      if (bytes !== undefined) out.set(chunk[j].toBase58(), bytes);
    });
    await sleep(40);
  }
  return out;
}

interface Hit {
  o: Obligation;
  r: Recomputed;
  storedLiq: boolean;
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (!endpoint) throw new Error('HELIUS_RPC in .env');
  const market = process.env.MARKET ?? 'all';
  const minCollateral = Number.parseFloat(process.env.MIN_COLLATERAL_USD ?? '') || 50.0;
  const nearN = Number.parseInt(process.env.NEAR ?? '', 10) || 25;

  const slotResp = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getSlot',
    params: [{ commitment: 'confirmed' }],
  });
  const curSlot: bigint = typeof slotResp?.result === 'number' ? BigInt(slotResp.result) : 0n;

  // 1) Obligations (dataSlice through has_debt @2287).
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const filters: any[] = [{ dataSize: 3344 }];
  if (market !== 'all') filters.push({ memcmp: { offset: 32, bytes: market } });
  console.error('[live] getProgramAccounts obligations …');
  const resp = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getProgramAccounts',
    params: [KLEND_PROGRAM, { encoding: 'base64', dataSlice: { offset: 0, length: 2288 }, filters }],
  });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const entries: any[] = Array.isArray(resp?.result) ? resp.result : [];
  const obs: Obligation[] = [];
  for (const e of entries) {
    const bytes = e?.account?.data !== undefined ? b64(e.account.data) : undefined;
    if (bytes === undefined) continue;
    const o = decodeObligation(bytes);
    if (o !== null && o.borrows.length > 0) obs.push(o);
  }
  console.error(`[live] ${obs.length} obligations with debt, current slot ${curSlot}`);
  if (obs.length === 0) return;

  // 2) Fetch + decode every referenced reserve (need through borrow_factor @5008).
  const reservePkSet = new Set<string>();
  const reservePks: PublicKey[] = [];
  for (const o of obs) {
    for (const [r] of o.deposits) {
      if (!reservePkSet.has(r.toBase58())) {
        reservePkSet.add(r.toBase58());
        reservePks.push(r);
      }
    }
    for (const [r] of o.borrows) {
      if (!reservePkSet.has(r.toBase58())) {
        reservePkSet.add(r.toBase58());
        reservePks.push(r);
      }
    }
  }
  console.error(`[live] fetching ${reservePks.length} reserves …`);
  const reserveRaw = await getMultiple(endpoint, reservePks, 5016);
  const reserves: ReserveMap = new Map();
  for (const [pk, raw] of reserveRaw) {
    const r = decodeReserve(raw);
    if (r !== null) reserves.set(pk, r);
  }
  // Reserve price freshness (these cached prices drive the recompute).
  const ages: bigint[] = Array.from(reserves.values()).map((r) => (curSlot > r.priceSlot ? curSlot - r.priceSlot : 0n));
  ages.sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));
  const medAge = ages.length > 0 ? ages[Math.floor(ages.length / 2)] : 0n;
  console.error(
    `[live] decoded ${reserves.size} reserves; cached-price age median ${medAge}sl (~${(medAge * 2n) / 5n}s), max ${ages.length > 0 ? ages[ages.length - 1] : 0n}sl`,
  );

  // Population diagnostics.
  const nElev = obs.filter((o) => o.elevationGroup !== 0).length;
  const nStale = obs.filter((o) => o.stale).length;
  const nTrust = obs.filter((o) => recomputedTrustworthy(recompute(o, reserves))).length;
  console.error(
    `[live] population: ${obs.length} debt obs | ${nElev} elevation-group | ${nStale} stale | ${nTrust} trustworthy(non-elev, fully priced)`,
  );

  // 3) Validation: recomputed vs stored. Compare on trustworthy obligations
  // whose recompute used fresh-enough reserve prices AND that were refreshed
  // recently (so stored ~= current). Match => recompute math is correct.
  const valErr: number[] = [];
  let shown = 0;
  console.log('\n──── VALIDATION: recomputed vs stored ────');
  for (const o of obs) {
    const r = recompute(o, reserves);
    if (!recomputedTrustworthy(r) || o.unhealthyBorrowValue < 100.0) continue;
    // both the obligation and the reserve prices it uses must be recent.
    if (curSlot - o.lastUpdateSlot > 300n) continue;
    if (curSlot - r.oldestPriceSlot > 300n) continue;
    const err = Math.abs(r.unhealthyBorrowValue - o.unhealthyBorrowValue) / o.unhealthyBorrowValue;
    valErr.push(err);
    if (shown < 10) {
      console.log(
        `  stored unhealthy=$${o.unhealthyBorrowValue.toFixed(2)} debt=$${o.bfAdjustedDebt.toFixed(2)} depos=$${o.depositedValue.toFixed(2)}  |  recomp unhealthy=$${r.unhealthyBorrowValue.toFixed(2)} debt=$${r.bfAdjustedDebt.toFixed(2)} depos=$${r.depositedValue.toFixed(2)}  (err ${(err * 100.0).toFixed(2)}%)`,
      );
      shown += 1;
    }
  }
  valErr.sort((a, b) => a - b);
  if (valErr.length === 0) {
    console.log('  (no obligation with both itself + its reserve prices fresh enough to validate)');
  } else {
    const p90idx = Math.min(Math.floor((valErr.length * 9) / 10), valErr.length - 1);
    console.log(
      `  → ${valErr.length} validated, median error ${(valErr[Math.floor(valErr.length / 2)] * 100.0).toFixed(3)}%, p90 ${(valErr[p90idx] * 100.0).toFixed(3)}%`,
    );
  }

  // ALPHA: liquidatable at current prices, ranked by seizable collateral.
  const hits: Hit[] = [];
  const near: Array<[number, Obligation, Recomputed]> = [];
  for (const o of obs) {
    const r = recompute(o, reserves);
    if (!recomputedTrustworthy(r) || r.depositedValue < minCollateral) continue;
    if (recomputedLiquidatable(r)) {
      hits.push({ o, r, storedLiq: obligationLiquidatable(o) });
    } else if (recomputedRatio(r) > 0.9) {
      near.push([recomputedRatio(r), o, r]);
    }
  }
  hits.sort((a, b) => b.r.depositedValue - a.r.depositedValue);
  const hiddenAlpha = hits.filter((h) => !h.storedLiq).length;

  console.log('\n════ Kamino LIVE liquidatable (recomputed at current prices) ════');
  console.log(`obligations w/ debt: ${obs.length}   collateral ≥ $${minCollateral.toFixed(0)}`);
  console.log(
    `LIQUIDATABLE NOW: ${hits.length}   [${hits.length - hiddenAlpha} already flagged by stored values, ${hiddenAlpha} HIDDEN (stored says healthy = stale alpha)]`,
  );
  for (const h of hits.slice(0, 40)) {
    const age = curSlot - h.o.lastUpdateSlot;
    console.log(
      `  ${h.storedLiq ? 'known' : 'ALPHA'} ${h.o.owner.toBase58().slice(0, 8)}…  collateral=$${h.r.depositedValue.toFixed(2)}  debt=$${h.r.bfAdjustedDebt.toFixed(2)}  thresh=$${h.r.unhealthyBorrowValue.toFixed(2)}  ratio=${recomputedRatio(h.r).toFixed(4)}  (obl age ${age}sl${h.o.stale ? ', stale' : ''})`,
    );
  }

  near.sort((a, b) => b[0] - a[0]);
  console.log('\nclosest to liquidation at current prices (ratio→1.0):');
  for (const [ratio, o, r] of near.slice(0, nearN)) {
    console.log(
      `  ${o.owner.toBase58().slice(0, 8)}…  ratio=${ratio.toFixed(4)}  debt=$${r.bfAdjustedDebt.toFixed(2)}  thresh=$${r.unhealthyBorrowValue.toFixed(2)}  collateral=$${r.depositedValue.toFixed(2)}`,
    );
  }
  console.log();
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
