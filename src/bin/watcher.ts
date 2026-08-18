// Port of src/bin/watcher.rs
//
// Ground-truth competitor watcher — SEPARATE process, fully off the executor's
// hot path. Rolling scan of both pools' recent transactions; a tx whose
// resolved account set touches BOTH pools is a cross-venue arb. If the signer
// isn't us, a competitor captured it. We estimate their profit (fee-payer USDC
// delta) and cross-reference our own decisions.jsonl to classify what happened
// on our side: never-triggered / skipped / fired-and-lost. Appends missed.jsonl.
//
// This is the only way to see the opportunities our own logs can't — the ones
// we didn't act on or lost. Runs on RPC, seconds-lagged; never touches the
// executor.
//
// Usage: RPC_ENDPOINT=<url> [RUN_DIR=runs] [POLL_SECS=10] [OUR_WALLET=<pk>] \
//   tsx src/bin/watcher.ts

import 'dotenv/config';
import { mkdirSync, appendFileSync, existsSync, readFileSync } from 'node:fs';
import { pair } from '../lib/pools.js';

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function rpc(endpoint: string, body: unknown): Promise<any | undefined> {
  for (let attempt = 0; attempt < 4; attempt++) {
    try {
      const resp = await fetch(endpoint, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(body),
      });
      return await resp.json();
    } catch {
      // fall through to retry
    }
    await sleep(400 << attempt);
  }
  return undefined;
}

async function recentSigs(endpoint: string, pool: string, limit: number): Promise<[string, number][]> {
  const v = await rpc(endpoint, { jsonrpc: '2.0', id: 1, method: 'getSignaturesForAddress', params: [pool, { limit }] });
  const arr: any[] = Array.isArray(v?.result) ? v.result : [];
  return arr
    .filter((e) => e?.err === null || e?.err === undefined)
    .map((e): [string, number] | undefined => {
      const sig = e?.signature;
      if (typeof sig !== 'string') return undefined;
      return [sig, typeof e?.slot === 'number' ? e.slot : 0];
    })
    .filter((x): x is [string, number] => x !== undefined);
}

const USDC = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v';

/** Full resolved account key set (static + ALT-loaded) + fee payer + USDC delta. */
async function txTouchAndProfit(endpoint: string, sig: string): Promise<[Set<string>, string, number] | undefined> {
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getTransaction',
    params: [sig, { encoding: 'jsonParsed', maxSupportedTransactionVersion: 0, commitment: 'confirmed' }],
  });
  const r = v?.result;
  if (r === null || r === undefined) return undefined;
  if (r?.meta?.err !== null && r?.meta?.err !== undefined) return undefined;
  const keys = new Set<string>();
  const accountKeys: any[] = Array.isArray(r?.transaction?.message?.accountKeys) ? r.transaction.message.accountKeys : [];
  for (const k of accountKeys) {
    if (typeof k?.pubkey === 'string') keys.add(k.pubkey);
  }
  for (const grp of ['writable', 'readonly']) {
    const arr: any[] = Array.isArray(r?.meta?.loadedAddresses?.[grp]) ? r.meta.loadedAddresses[grp] : [];
    for (const k of arr) {
      if (typeof k === 'string') keys.add(k);
    }
  }
  const payer = r?.transaction?.message?.accountKeys?.[0]?.pubkey;
  if (typeof payer !== 'string') return undefined;
  const sum = (key: string): number => {
    const arr: any[] = Array.isArray(r?.meta?.[key]) ? r.meta[key] : [];
    return arr
      .filter((b) => b?.mint === USDC && b?.owner === payer)
      .map((b) => b?.uiTokenAmount?.uiAmount)
      .filter((x): x is number => typeof x === 'number')
      .reduce((a, b) => a + b, 0);
  };
  const profit = sum('postTokenBalances') - sum('preTokenBalances');
  return [keys, payer, profit];
}

/** Slots we triggered / fired on, from our decisions ledger. */
function ourSlots(dir: string): [Set<number>, Set<number>] {
  const triggered = new Set<number>();
  const fired = new Set<number>();
  const path = `${dir}/decisions.jsonl`;
  if (!existsSync(path)) return [triggered, fired];
  let text: string;
  try {
    text = readFileSync(path, 'utf8');
  } catch {
    return [triggered, fired];
  }
  for (const line of text.split('\n')) {
    if (line.length === 0) continue;
    try {
      const v = JSON.parse(line);
      const slot = v?.slot;
      if (typeof slot === 'number') {
        triggered.add(slot);
        if (v?.fired === true) fired.add(slot);
      }
    } catch {
      // ignore
    }
  }
  return [triggered, fired];
}

async function main(): Promise<void> {
  const endpoint = process.env.RPC_ENDPOINT;
  if (endpoint === undefined) throw new Error('RPC_ENDPOINT');
  const dir = process.env.RUN_DIR ?? 'runs';
  const poll = Number.parseInt(process.env.POLL_SECS ?? '', 10) || 10;
  const ourWallet = process.env.OUR_WALLET ?? '';
  const cfg = pair();
  try {
    mkdirSync(dir, { recursive: true });
  } catch {
    // ignore
  }

  console.error(`watcher ${cfg.label} — scanning for cross-venue arbs every ${poll}s → ${dir}/missed.jsonl`);
  const seen = new Set<string>();
  let competitorWins = 0n;
  let ourWins = 0n;

  while (true) {
    const sigs: [string, number][] = [];
    for (const pool of [cfg.orcaPool, cfg.rayPool]) {
      sigs.push(...(await recentSigs(endpoint, pool, 40)));
    }
    const [triggered, fired] = ourSlots(dir);

    for (const [sig, slot] of sigs) {
      if (seen.has(sig)) continue;
      seen.add(sig);
      const result = await txTouchAndProfit(endpoint, sig);
      if (result === undefined) continue;
      const [keys, payer, profit] = result;
      // Cross-venue arb = touches BOTH pools in one tx.
      if (!(keys.has(cfg.orcaPool) && keys.has(cfg.rayPool))) continue;
      const ours = ourWallet !== '' && payer === ourWallet;
      // The arb lands a few slots after the victim we'd have triggered on;
      // match our trigger/fire within a small window ending at the arb slot.
      const inWindow = (set: Set<number>): boolean => {
        for (let d = 0; d <= 5; d++) {
          if (set.has(Math.max(0, slot - d))) return true;
        }
        return false;
      };
      let status: string;
      if (ours) {
        ourWins += 1n;
        status = 'we_won';
      } else if (inWindow(fired)) {
        status = 'fired_lost';
      } else if (inWindow(triggered)) {
        status = 'triggered_skipped';
      } else {
        status = 'not_triggered';
      }
      if (!ours) competitorWins += 1n;
      const row = { sig, payer, competitor: !ours, est_profit_usd: profit, our_status: status };
      try {
        appendFileSync(`${dir}/missed.jsonl`, `${JSON.stringify(row)}\n`);
      } catch {
        // ignore
      }
      console.error(
        `arb ${sig.slice(0, Math.min(12, sig.length))} by ${payer.slice(0, Math.min(8, payer.length))}… profit $${profit.toFixed(4)} [${status}]`,
      );
    }
    // Cap the seen-set so it doesn't grow unbounded.
    if (seen.size > 20_000) {
      seen.clear();
    }
    console.error(`[watcher] competitor_wins=${competitorWins} our_wins=${ourWins}`);
    await sleep(poll * 1000);
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
