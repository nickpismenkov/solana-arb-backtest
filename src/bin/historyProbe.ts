// Port of src/bin/history_probe.rs
//
// Backward-looking residual scan: replay the last N hours of landed swaps on
// both pools from chain history and reconstruct the cross-venue gap timeline.
// Complements backrun_probe (live, sub-slot) with slot-level coverage over a
// full day — including hours we weren't listening.
//
// Method: getSignaturesForAddress on each pool → getTransaction (jsonParsed)
// → the pool's vault balance deltas give each swap's execution price. A CLMM
// price only moves on swaps, so the last execution price on a venue ≈ its
// current price until the next swap. Caveat: execution price is the swap's
// average (mid of pre/post marginal price), so large swaps read ~half their
// own price impact as "gap" — treat counts near the floor as upper bounds.
//
// Usage: RPC_ENDPOINT=<url> HOURS=24 [pair env vars] \
//   tsx src/bin/historyProbe.ts

import 'dotenv/config';
import bs58 from 'bs58';
import { pair } from '../lib/pools.js';

const TIP_CUSHION_BPS = 1.0;
// Backstop for very active pools (liquid controls). When hit, the window is
// truncated and the report says so — never silently.
const MAX_TX_PER_POOL = 8000;

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function rpcCall(rpc: string, body: unknown): Promise<any | undefined> {
  for (let attempt = 0; attempt < 5; attempt++) {
    try {
      const res = await fetch(rpc, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (res.status === 429) {
        // Back off hard on rate limits — a 429 storm is slower than pacing.
        await sleep(1500 * (attempt + 1));
        continue;
      }
      return await res.json();
    } catch (e) {
      if (attempt < 4) {
        await sleep(400 << attempt);
      } else {
        console.error(`rpc error (giving up): ${e}`);
        return undefined;
      }
    }
  }
  return undefined;
}

async function accountData(rpc: string, addr: string): Promise<Uint8Array | undefined> {
  const v = await rpcCall(rpc, { jsonrpc: '2.0', id: 1, method: 'getAccountInfo', params: [addr, { encoding: 'base64' }] });
  const b64 = v?.result?.value?.data?.[0];
  if (typeof b64 !== 'string') return undefined;
  return new Uint8Array(Buffer.from(b64, 'base64'));
}

function pkAt(d: Uint8Array, o: number): string {
  return bs58.encode(d.subarray(o, o + 32));
}

interface Swap {
  slot: number;
  blockTime: number;
  venue: string;
  price: number; // quote per base (execution price)
  baseUi: number; // |base delta| of the swap
}

/** All pool signatures newer than `cutoff` (unix secs), oldest capped by MAX_TX_PER_POOL. */
async function signaturesSince(rpc: string, pool: string, cutoff: number): Promise<[string[], boolean]> {
  const sigs: string[] = [];
  let before: string | undefined;
  while (true) {
    const params: any = { limit: 1000 };
    if (before !== undefined) params.before = before;
    const v = await rpcCall(rpc, { jsonrpc: '2.0', id: 1, method: 'getSignaturesForAddress', params: [pool, params] });
    if (v === undefined) break;
    const arr: any[] = Array.isArray(v?.result) ? v.result : [];
    if (arr.length === 0) break;
    let reachedCutoff = false;
    for (const e of arr) {
      const bt = typeof e?.blockTime === 'number' ? e.blockTime : 0;
      if (bt !== 0 && bt < cutoff) {
        reachedCutoff = true;
        break;
      }
      if (e?.err === null || e?.err === undefined) {
        sigs.push(e?.signature ?? '');
      }
    }
    if (reachedCutoff) return [sigs, false];
    if (sigs.length >= MAX_TX_PER_POOL) return [sigs, true];
    before = arr[arr.length - 1]?.signature;
    if (before === undefined) break;
  }
  return [sigs, false];
}

/** Vault deltas → execution price for one landed tx. */
async function swapFromTx(
  rpc: string,
  sig: string,
  venue: string,
  baseVault: string,
  quoteVault: string,
  baseDec: number,
  quoteDec: number,
): Promise<Swap | undefined> {
  const v = await rpcCall(rpc, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getTransaction',
    params: [sig, { encoding: 'jsonParsed', maxSupportedTransactionVersion: 0, commitment: 'confirmed' }],
  });
  const r = v?.result;
  if (r === null || r === undefined) return undefined;
  if (r?.meta?.err !== null && r?.meta?.err !== undefined) return undefined;
  const keys: string[] = Array.isArray(r?.transaction?.message?.accountKeys)
    ? r.transaction.message.accountKeys.map((k: any) => k?.pubkey).filter((s: unknown): s is string => typeof s === 'string')
    : [];
  const balance = (bals: any, vault: string): bigint => {
    const arr: any[] = Array.isArray(bals) ? bals : [];
    const b = arr.find((b) => {
      const idx = b?.accountIndex;
      return typeof idx === 'number' && keys[idx] === vault;
    });
    const amt = b?.uiTokenAmount?.amount;
    if (typeof amt !== 'string') return 0n;
    try {
      return BigInt(amt);
    } catch {
      return 0n;
    }
  };
  const meta = r?.meta;
  const dBase = balance(meta?.postTokenBalances, baseVault) - balance(meta?.preTokenBalances, baseVault);
  const dQuote = balance(meta?.postTokenBalances, quoteVault) - balance(meta?.preTokenBalances, quoteVault);
  if (dBase === 0n || dQuote === 0n) return undefined; // not a swap on this pool
  const baseUi = Number(dBase < 0n ? -dBase : dBase) / 10 ** baseDec;
  const quoteUi = Number(dQuote < 0n ? -dQuote : dQuote) / 10 ** quoteDec;
  const slot = r?.slot;
  if (typeof slot !== 'number') return undefined;
  return {
    slot,
    blockTime: typeof r?.blockTime === 'number' ? r.blockTime : 0,
    venue,
    price: quoteUi / baseUi,
    baseUi,
  };
}

function median(v: number[]): number {
  if (v.length === 0) return 0.0;
  const sorted = [...v].sort((a, b) => a - b);
  return sorted[Math.floor(sorted.length / 2)];
}

/** Tiny UTC formatter (no chrono dep): unix secs → "HH:MM:SSZ". */
function chronoLite(secs: number): string {
  const s = ((secs % 86_400) + 86_400) % 86_400;
  const pad = (n: number): string => n.toString().padStart(2, '0');
  return `${pad(Math.floor(s / 3600))}:${pad(Math.floor((s % 3600) / 60))}:${pad(s % 60)}Z`;
}

async function main(): Promise<void> {
  const rpc = process.env.RPC_ENDPOINT;
  if (rpc === undefined) throw new Error('set RPC_ENDPOINT');
  const hours = Number.parseFloat(process.env.HOURS ?? '') || 24.0;
  const cfg = pair();
  const feeBps = cfg.roundTripFeeBps();
  const cutoff = Math.floor(Date.now() / 1000) - Math.floor(hours * 3600.0);

  // Vault addresses from the pool accounts (offsets verified on mainnet).
  const orca = await accountData(rpc, cfg.orcaPool);
  if (orca === undefined) throw new Error('fetch orca pool');
  const ray = await accountData(rpc, cfg.rayPool);
  if (ray === undefined) throw new Error('fetch ray pool');
  const orcaBaseIsA = pkAt(orca, 101) === cfg.baseMint;
  const [orcaBaseV, orcaQuoteV] = orcaBaseIsA ? [pkAt(orca, 133), pkAt(orca, 213)] : [pkAt(orca, 213), pkAt(orca, 133)];
  const rayBaseIs0 = pkAt(ray, 73) === cfg.baseMint;
  const [rayBaseV, rayQuoteV] = rayBaseIs0 ? [pkAt(ray, 137), pkAt(ray, 169)] : [pkAt(ray, 169), pkAt(ray, 137)];

  console.log(`history-probe — pair ${cfg.label}, floor ${feeBps}bp (+${TIP_CUSHION_BPS}bp cushion), last ${hours}h\n`);

  const swaps: Swap[] = [];
  const venues: [string, string, string, string][] = [
    ['Orca', cfg.orcaPool, orcaBaseV, orcaQuoteV],
    ['Raydium', cfg.rayPool, rayBaseV, rayQuoteV],
  ];
  for (const [venue, pool, bv, qv] of venues) {
    const [sigs, truncated] = await signaturesSince(rpc, pool, cutoff);
    if (truncated) {
      console.log(`⚠ ${venue}: hit the ${MAX_TX_PER_POOL}-tx cap — window truncated, report covers less than ${hours}h on this venue.`);
    }
    console.log(`${venue}: ${sigs.length} landed txs in window, fetching…`);
    let n = 0;
    for (let i = 0; i < sigs.length; i++) {
      const s = await swapFromTx(rpc, sigs[i], venue, bv, qv, cfg.baseDec, cfg.quoteDec);
      if (s !== undefined) {
        swaps.push(s);
        n += 1;
      }
      if ((i + 1) % 200 === 0) {
        console.error(`  ${venue}: ${i + 1}/${sigs.length} fetched…`);
      }
      await sleep(120); // ~8 rps, under RPC rate limits
    }
    console.log(`${venue}: ${n} swaps decoded`);
  }

  swaps.sort((a, b) => a.slot - b.slot);

  // Replay: last exec price per venue is that venue's standing price (CLMM
  // price only moves on swaps). On every swap, measure the cross-venue gap.
  let lastOrca = Number.NaN;
  let lastRay = Number.NaN;
  const gaps: number[] = [];
  const clears: [number, number, number, number][] = []; // slot, time, gap, swap size
  let openSlot: number | undefined;
  const lifetimes: number[] = [];
  const byHour = new Map<number, number>();
  for (const s of swaps) {
    if (s.venue === 'Orca') {
      lastOrca = s.price;
    } else {
      lastRay = s.price;
    }
    if (!(Number.isFinite(lastOrca) && Number.isFinite(lastRay))) continue;
    const gap = Math.abs(((lastRay - lastOrca) / Math.min(lastOrca, lastRay)) * 10_000.0);
    gaps.push(gap);
    if (gap > feeBps) {
      if (openSlot === undefined) {
        openSlot = s.slot;
        clears.push([s.slot, s.blockTime, gap, s.baseUi]);
        const h = ((Math.floor(s.blockTime / 3600) % 24) + 24) % 24;
        byHour.set(h, (byHour.get(h) ?? 0) + 1);
      }
    } else if (openSlot !== undefined) {
      lifetimes.push(s.slot - openSlot);
      openSlot = undefined;
    }
  }

  console.log(`\n──────── history-probe report (${hours}h) ────────`);
  console.log(`swaps decoded: ${swaps.length} (both venues)`);
  if (gaps.length === 0) {
    console.log('no overlapping price data — one venue had no swaps in the window.');
    return;
  }
  console.log(
    `cross-venue gap at each swap: median ${median(gaps).toFixed(1)} bp, max ${gaps.reduce((a, b) => Math.max(a, b), 0.0).toFixed(1)} bp`,
  );
  console.log(`fee-clearing episodes (>${feeBps}bp): ${clears.length}`);
  const strong = clears.filter((c) => c[2] > feeBps + TIP_CUSHION_BPS);
  console.log(`  above floor+cushion (>${(feeBps + TIP_CUSHION_BPS).toFixed(0)}bp): ${strong.length}`);
  if (clears.length > 0) {
    console.log(
      `  gap at open: median ${median(clears.map((c) => c[2])).toFixed(1)} bp, max ${clears.reduce((a, c) => Math.max(a, c[2]), 0.0).toFixed(1)} bp`,
    );
    if (lifetimes.length > 0) {
      const lt = [...lifetimes].sort((a, b) => a - b);
      console.log(
        `  episode lifetime: median ${lt[Math.floor(lt.length / 2)]} slots (~${(lt[Math.floor(lt.length / 2)] * 0.4).toFixed(1)}s), max ${lt[lt.length - 1]} slots`,
      );
    }
    const hoursSorted = Array.from(byHour.entries()).sort((a, b) => a[0] - b[0]);
    const hist = hoursSorted.map(([h, n]) => `${h.toString().padStart(2, '0')}h:${n}`).join(' ');
    console.log(`  episodes by UTC hour: ${hist}`);
    console.log('\nlast 10 episodes:');
    for (const [slot, bt, gap, size] of clears.slice(-10).reverse()) {
      const t = chronoLite(bt);
      console.log(`  slot ${slot} ${t} gap ${gap.toFixed(1)}bp (trigger swap ${size.toFixed(3)} base)`);
    }
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
