// Port of src/bin/flow_probe.rs
//
// Flow probe — two go/no-go measurements before we build the co-bundler:
//   (1) DIRECT vs ROUTED: of the swaps hitting our pools, how many call the
//       DEX program top-level (decodable from a shred → co-bundlable) vs only
//       via CPI (opaque). This is the addressable direct-swap volume.
//   (2) WINNING TIPS: for cross-venue arbs that landed on our pools, how much
//       did the winner tip Jito (balance delta of a Jito tip account) and what
//       did they net (fee-payer USDC delta)? → what it costs to compete.
//
// Read-only, RPC-only (use Helius). No money.
//
// Usage: RPC_ENDPOINT=<helius> [LIMIT=800] tsx src/bin/flowProbe.ts

import 'dotenv/config';
import { ORCA_PROGRAM, RAY_CLMM_PROGRAM } from '../lib/decode.js';
import { defaultBlockEngine, getTipAccounts } from '../lib/jito.js';
import { pair } from '../lib/pools.js';

const USDC = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v';

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

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
    await sleep(300 << attempt);
  }
  return undefined;
}

async function recentSigs(endpoint: string, pool: string, limit: number): Promise<string[]> {
  const v = await rpc(endpoint, { jsonrpc: '2.0', id: 1, method: 'getSignaturesForAddress', params: [pool, { limit }] });
  const arr: any[] = Array.isArray(v?.result) ? v.result : [];
  return arr
    .filter((e) => e?.err === null || e?.err === undefined)
    .map((e) => e?.signature)
    .filter((s): s is string => typeof s === 'string');
}

function pct(sorted: number[], p: number): number {
  if (sorted.length === 0) return 0.0;
  const i = Math.round((sorted.length - 1) * p);
  return sorted[i]!;
}

async function main(): Promise<void> {
  const endpoint = process.env.RPC_ENDPOINT;
  if (endpoint === undefined) throw new Error('RPC_ENDPOINT (use Helius)');
  const limit = Number.parseInt(process.env.LIMIT ?? '', 10) || 800;
  const sleepMs = Number.parseInt(process.env.SLEEP_MS ?? '', 10) || 25;
  const cfg = pair();

  const tipAccounts = new Set<string>((await getTipAccounts(defaultBlockEngine()).catch(() => [])).map((p) => p.toBase58()));
  console.error(`flow-probe ${cfg.label} — ${tipAccounts.size} Jito tip accounts; scanning ~${limit} sigs/pool`);

  // Gather unique recent signatures across both pools.
  const sigs = new Set<string>();
  for (const pool of [cfg.orcaPool, cfg.rayPool]) {
    for (const s of await recentSigs(endpoint, pool, limit)) sigs.add(s);
  }
  console.error(`scanning ${sigs.size} unique txns…`);

  let direct = 0;
  let routed = 0;
  let arbs = 0;
  const arbTips: number[] = []; // lamports
  const arbProfits: number[] = []; // USDC
  const allTips: number[] = [];
  let scanned = 0;
  let fetchFail = 0;

  for (const sig of sigs) {
    await sleep(sleepMs);
    const v = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'getTransaction',
      params: [sig, { encoding: 'jsonParsed', maxSupportedTransactionVersion: 0, commitment: 'confirmed' }],
    });
    if (v === undefined) {
      fetchFail += 1;
      continue;
    }
    const r = v?.result;
    if (r === null || r === undefined) continue;
    if (r?.meta?.err !== null && r?.meta?.err !== undefined) continue;
    scanned += 1;

    // Full ordered account list: static keys, then loaded writable, readonly.
    const keys: string[] = (Array.isArray(r?.transaction?.message?.accountKeys) ? r.transaction.message.accountKeys : [])
      .map((k: any) => k?.pubkey)
      .filter((s: unknown): s is string => typeof s === 'string');
    for (const grp of ['writable', 'readonly']) {
      const arr = r?.meta?.loadedAddresses?.[grp];
      if (Array.isArray(arr)) {
        for (const k of arr) {
          if (typeof k === 'string') keys.push(k);
        }
      }
    }
    const keySet = new Set(keys);
    const touchOrca = keySet.has(cfg.orcaPool);
    const touchRay = keySet.has(cfg.rayPool);
    if (!touchOrca && !touchRay) continue;

    // Top-level program IDs.
    const top = new Set<string>(
      (Array.isArray(r?.transaction?.message?.instructions) ? r.transaction.message.instructions : [])
        .map((i: any) => i?.programId)
        .filter((s: unknown): s is string => typeof s === 'string'),
    );
    const dexTopLevel = top.has(ORCA_PROGRAM) || top.has(RAY_CLMM_PROGRAM);
    if (dexTopLevel) direct += 1;
    else routed += 1;

    // Tip: balance delta of any Jito tip account in this tx.
    const pre: number[] | undefined = r?.meta?.preBalances;
    const post: number[] | undefined = r?.meta?.postBalances;
    let tip = 0.0;
    if (Array.isArray(pre) && Array.isArray(post)) {
      keys.forEach((k, i) => {
        if (tipAccounts.has(k)) {
          const d = (post[i] ?? 0) - (pre[i] ?? 0);
          if (d > 0.0) tip += d;
        }
      });
    }
    if (tip > 0.0) allTips.push(tip);

    // Cross-venue arb = touches BOTH pools.
    if (touchOrca && touchRay) {
      arbs += 1;
      if (tip > 0.0) arbTips.push(tip);
      // fee-payer USDC delta = arb profit
      const payer: string = r?.transaction?.message?.accountKeys?.[0]?.pubkey ?? '';
      const sum = (key: string): number => {
        const arr: any[] = Array.isArray(r?.meta?.[key]) ? r.meta[key] : [];
        return arr
          .filter((b) => b?.mint === USDC && b?.owner === payer)
          .map((b) => b?.uiTokenAmount?.uiAmount)
          .filter((x): x is number => typeof x === 'number')
          .reduce((a, b) => a + b, 0);
      };
      arbProfits.push(sum('postTokenBalances') - sum('preTokenBalances'));
    }
  }

  const tot = Math.max(direct + routed, 1);
  console.log(`\n═══ FLOW (${scanned} pool txns scanned, ${fetchFail} fetch fails) ═══`);
  console.log(`DIRECT swaps (DEX top-level → decodable/co-bundlable): ${direct} (${((100.0 * direct) / tot).toFixed(1)}%)`);
  console.log(`ROUTED swaps (DEX via CPI → opaque in shred):          ${routed} (${((100.0 * routed) / tot).toFixed(1)}%)`);

  console.log('\n═══ WINNING TIPS (SOL) ═══');
  const report = (label: string, v: number[]): void => {
    v.sort((a, b) => a - b);
    if (v.length === 0) {
      console.log(`${label}: (none)`);
      return;
    }
    console.log(
      `${label}: n=${v.length} | p25=${(pct(v, 0.25) / 1e9).toFixed(5)} med=${(pct(v, 0.5) / 1e9).toFixed(5)} p75=${(pct(v, 0.75) / 1e9).toFixed(5)} max=${(pct(v, 1.0) / 1e9).toFixed(5)}`,
    );
  };
  report('all pool-tx tips', allTips);
  report('cross-venue ARB tips', arbTips);

  console.log(`\n═══ ARBS (${arbs} cross-venue) ═══`);
  arbProfits.sort((a, b) => a - b);
  if (arbProfits.length > 0) {
    console.log(
      `payer USDC profit: med=${pct(arbProfits, 0.5).toFixed(4)} max=${pct(arbProfits, 1.0).toFixed(4)} (note: excl. tip/fees; many are $0 routed user swaps)`,
    );
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
