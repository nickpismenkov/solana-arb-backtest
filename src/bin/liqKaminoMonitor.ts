// Port of src/bin/liq_kamino_monitor.rs
//
// Kamino continuous liquidation-opportunity monitor (read-only, long-run).
//
// Full-scans obligations occasionally to build a watch-set of near-liquidation
// positions, then polls only that set + its reserves frequently — recomputing
// health at each reserve's latest cached price. Logs when a position crosses
// liquidatable (APPEARED) and when it's taken/recovers (RESOLVED, with survival
// seconds). Over hours this measures real opportunity flow + how fast the
// competition takes them = the go/no-go for a Kamino liquidation executor.
//
// Usage: HELIUS_RPC=<url> [POLL_SECS=15] [FULL_SCAN_SECS=300] [WATCH_RATIO=0.92]
//        [MIN_COLLATERAL_USD=100] tsx src/bin/liqKaminoMonitor.ts

import 'dotenv/config';
import * as fs from 'node:fs';
import { PublicKey } from '@solana/web3.js';
import {
  decodeObligation,
  decodeReserve,
  KLEND_PROGRAM,
  OBLIGATION_SIZE,
  recompute,
  recomputedLiquidatable,
  recomputedRatio,
  recomputedTrustworthy,
  type Obligation,
  type ReserveMap,
} from '../lib/kamino.js';

function nowTs(): number {
  return Math.floor(Date.now() / 1000);
}

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

/** Fetch reserves referenced by a set of obligations, decode -> map. */
async function fetchReserves(endpoint: string, obs: Array<[PublicKey, Obligation]>): Promise<ReserveMap> {
  const pkSet = new Set<string>();
  const pks: PublicKey[] = [];
  for (const [, o] of obs) {
    for (const [r] of o.deposits) {
      if (!pkSet.has(r.toBase58())) {
        pkSet.add(r.toBase58());
        pks.push(r);
      }
    }
    for (const [r] of o.borrows) {
      if (!pkSet.has(r.toBase58())) {
        pkSet.add(r.toBase58());
        pks.push(r);
      }
    }
  }
  const raw = await getMultiple(endpoint, pks, 5016);
  const out: ReserveMap = new Map();
  for (const [pk, bytes] of raw) {
    const r = decodeReserve(bytes);
    if (r !== null) out.set(pk, r);
  }
  return out;
}

/** Full scan -> all debt obligations (with their account pubkey). */
async function fullScan(endpoint: string): Promise<Array<[PublicKey, Obligation]>> {
  const resp = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getProgramAccounts',
    params: [
      KLEND_PROGRAM,
      { encoding: 'base64', dataSlice: { offset: 0, length: 2288 }, filters: [{ dataSize: OBLIGATION_SIZE }] },
    ],
  });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const entries: any[] = Array.isArray(resp?.result) ? resp.result : [];
  const out: Array<[PublicKey, Obligation]> = [];
  for (const e of entries) {
    let pk: PublicKey;
    try {
      pk = new PublicKey(e?.pubkey);
    } catch {
      continue;
    }
    const bytes = e?.account?.data !== undefined ? b64(e.account.data) : undefined;
    if (bytes === undefined) continue;
    const o = decodeObligation(bytes);
    if (o !== null && o.borrows.length > 0) out.push([pk, o]);
  }
  return out;
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (!endpoint) throw new Error('HELIUS_RPC');
  const pollSecs = Number.parseInt(process.env.POLL_SECS ?? '', 10) || 15;
  const fullScanSecs = Number.parseInt(process.env.FULL_SCAN_SECS ?? '', 10) || 300;
  const watchRatio = Number.parseFloat(process.env.WATCH_RATIO ?? '') || 0.92;
  const minCollateral = Number.parseFloat(process.env.MIN_COLLATERAL_USD ?? '') || 100.0;

  fs.mkdirSync('runs', { recursive: true });
  const logFd = fs.openSync('runs/kamino_opportunities.jsonl', 'a');
  const emit = (v: unknown): void => {
    fs.writeSync(logFd, `${JSON.stringify(v)}\n`);
  };

  console.error(`[kmon] poll=${pollSecs}s full_scan=${fullScanSecs}s watch_ratio=${watchRatio} min_collateral=$${minCollateral}`);

  // watch-set: account pubkey (base58) -> obligation (positions refreshed each poll)
  let watch = new Map<string, Obligation>();
  let lastFull = 0;
  // acct (base58) -> (first_ts, peak_collateral, owner)
  const open = new Map<string, { t0: number; peak: number; owner: string }>();
  let appeared = 0;
  let resolved = 0;
  const durations: number[] = [];

  for (;;) {
    // Periodic full scan -> refresh watch-set (near-liquidation + trustworthy).
    if (nowTs() - lastFull >= fullScanSecs) {
      console.error(`[kmon ${nowTs()}] full scan …`);
      const all = await fullScan(endpoint);
      const reserves = await fetchReserves(endpoint, all);
      const newWatch = new Map<string, Obligation>();
      for (const [pk, o] of all) {
        const r = recompute(o, reserves);
        if (recomputedTrustworthy(r) && r.depositedValue >= minCollateral && recomputedRatio(r) >= watchRatio) {
          newWatch.set(pk.toBase58(), o);
        }
      }
      console.error(`[kmon ${nowTs()}] watch-set: ${newWatch.size} obligations (ratio ≥ ${watchRatio}, collateral ≥ $${minCollateral})`);
      watch = newWatch;
      lastFull = nowTs();
    }

    // Poll: fresh obligation data + fresh reserve prices -> recompute.
    const watchPks = Array.from(watch.keys()).map((k) => new PublicKey(k));
    const raw = await getMultiple(endpoint, watchPks, 2288);
    for (const [pkStr, bytes] of raw) {
      const o = decodeObligation(bytes);
      if (o !== null) watch.set(pkStr, o);
    }
    const obs: Array<[PublicKey, Obligation]> = Array.from(watch.entries()).map(([k, v]) => [new PublicKey(k), v]);
    const reserves = await fetchReserves(endpoint, obs);

    const ts = nowTs();
    let curLiq = 0;
    let tightest = 0.0;
    for (const [pk, o] of obs) {
      const r = recompute(o, reserves);
      if (!recomputedTrustworthy(r)) continue;
      tightest = Math.max(tightest, recomputedRatio(r));
      const pkStr = pk.toBase58();
      if (recomputedLiquidatable(r) && r.depositedValue >= minCollateral) {
        curLiq += 1;
        let e = open.get(pkStr);
        if (e === undefined) {
          appeared += 1;
          emit({
            ts,
            event: 'appeared',
            protocol: 'kamino',
            account: pkStr,
            owner: o.owner.toBase58(),
            collateral_usd: Math.round(r.depositedValue * 100.0) / 100.0,
            debt_usd: Math.round(r.bfAdjustedDebt * 100.0) / 100.0,
            ratio: Math.round(recomputedRatio(r) * 10000.0) / 10000.0,
          });
          console.error(`[APPEARED ${ts}] ${o.owner.toBase58().slice(0, 8)}… collateral=$${r.depositedValue.toFixed(0)} ratio=${recomputedRatio(r).toFixed(4)}`);
          e = { t0: ts, peak: r.depositedValue, owner: o.owner.toBase58() };
          open.set(pkStr, e);
        }
        e.peak = Math.max(e.peak, r.depositedValue);
      } else {
        const e = open.get(pkStr);
        if (e !== undefined) {
          open.delete(pkStr);
          resolved += 1;
          const dur = Math.max(0, ts - e.t0);
          durations.push(dur);
          emit({
            ts,
            event: 'resolved',
            protocol: 'kamino',
            account: pkStr,
            owner: e.owner,
            seen_secs: dur,
            peak_collateral_usd: Math.round(e.peak * 100.0) / 100.0,
          });
          console.error(`[RESOLVED ${ts}] ${e.owner.slice(0, 8)}… after ${dur}s (peak $${e.peak.toFixed(0)})`);
        }
      }
    }
    durations.sort((a, b) => a - b);
    const med = durations.length > 0 ? durations[Math.floor(durations.length / 2)] : 0;
    console.error(
      `[kmon ${ts}] watch=${watch.size} liq_now=${curLiq} open=${open.size} | appeared=${appeared} resolved=${resolved} med_survival=${med}s | tightest ${tightest.toFixed(4)}`,
    );

    await sleep(pollSecs * 1000);
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
