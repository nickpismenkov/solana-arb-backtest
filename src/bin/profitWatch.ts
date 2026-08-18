// Port of src/bin/profit_watch.rs
//
// Profitability watcher — proves the WINNING path with zero cost. Loops over
// live mainnet: refresh both pools, build the guarded arb (both directions)
// from fresh state, simulateTransaction. Most iterations show the guard
// reverting at leg 2 (no edge). The instant a real edge appears — standing or
// transient — the sim comes back clean (err=null), meaning a profitable arb
// exists right now and our tx would land. Logs every clean hit + the spot edge
// to profit_watch.jsonl. No money, no submission — pure measurement.
//
// Usage: RPC_ENDPOINT=<url> ALT_ADDRESS=<alt> [BORROW_USDC=500] [POLL_MS=800] \
//   [RUN_DIR=runs] tsx src/bin/profitWatch.ts

import 'dotenv/config';
import { mkdirSync } from 'node:fs';
import { appendFile } from 'node:fs/promises';
import { PublicKey } from '@solana/web3.js';
import { buildArbTx, loadAlt, type PoolData } from '../lib/arb.js';
import { orcaPrice, pair, rayClmmPrice } from '../lib/pools.js';

// solana_hash::Hash::default() (32 zero bytes) base58-encodes to this string.
const DEFAULT_BLOCKHASH = '11111111111111111111111111111111';

async function rpc(endpoint: string, body: unknown): Promise<any | undefined> {
  for (let attempt = 0; attempt < 3; attempt++) {
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
    await new Promise((r) => setTimeout(r, 200 << attempt));
  }
  return undefined;
}

async function accountData(endpoint: string, addr: string): Promise<Buffer | undefined> {
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getAccountInfo',
    params: [addr, { encoding: 'base64' }],
  });
  const b64: string | undefined = v?.result?.value?.data?.[0];
  if (typeof b64 !== 'string') return undefined;
  return Buffer.from(b64, 'base64');
}

function now(): number {
  return Math.floor(Date.now() / 1000);
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function main(): Promise<void> {
  const endpoint = process.env.RPC_ENDPOINT;
  if (!endpoint) throw new Error('RPC_ENDPOINT');
  const altAddr = process.env.ALT_ADDRESS;
  if (!altAddr) throw new Error('ALT_ADDRESS');
  const borrowUi = Number.parseFloat(process.env.BORROW_USDC ?? '') || 500.0;
  const pollMs = Number.parseInt(process.env.POLL_MS ?? '', 10) || 800;
  const runDir = process.env.RUN_DIR ?? 'runs';
  const borrowAmount = BigInt(Math.trunc(borrowUi * 1e6));
  const cfg = pair();
  // Placeholder signer — simulate with sigVerify=false, so no keypair needed.
  const signer = new PublicKey('Anu6Awu4kxaEDrg1nkpcikx6tJ2xhfVci5TvDrZBsZEB');

  const altData = await accountData(endpoint, altAddr);
  if (altData === undefined) throw new Error('ALT');
  const alt = loadAlt(altAddr, altData);
  mkdirSync(runDir, { recursive: true });
  const out = `${runDir}/profit_watch.jsonl`;

  console.error(`profit-watch ${cfg.label} borrow ${borrowUi} USDC poll ${pollMs}ms — simulating both dirs; logs clean hits → ${out}`);
  let iters = 0n;
  let clean = 0n;
  let bestEdgeBps = Number.NEGATIVE_INFINITY;

  for (;;) {
    iters += 1n;
    const [o, r] = await Promise.all([accountData(endpoint, cfg.orcaPool), accountData(endpoint, cfg.rayPool)]);
    if (o === undefined || r === undefined) {
      await sleep(pollMs);
      continue;
    }
    // Spot edge for context (stale-free here: just-fetched pools).
    const po = orcaPrice(o);
    const pr = rayClmmPrice(r);
    let edgeBps = Number.NaN;
    if (po !== undefined && pr !== undefined && po > 0.0 && pr > 0.0) {
      edgeBps = (Math.abs(pr - po) / Math.min(po, pr)) * 1e4 - cfg.roundTripFeeBps();
    }
    if (Number.isFinite(edgeBps) && edgeBps > bestEdgeBps) {
      bestEdgeBps = edgeBps;
    }
    const pools: PoolData = { orca: o, ray: r };
    const bh = DEFAULT_BLOCKHASH;

    for (const orcaFirst of [false, true]) {
      let tx;
      try {
        tx = buildArbTx(pools, signer, alt, borrowAmount, orcaFirst, undefined, 0n, 10_000n, bh, 0n);
      } catch {
        continue;
      }
      const b64 = Buffer.from(tx.serialize()).toString('base64');
      const v = await rpc(endpoint, {
        jsonrpc: '2.0',
        id: 1,
        method: 'simulateTransaction',
        params: [b64, { encoding: 'base64', sigVerify: false, replaceRecentBlockhash: true }],
      });
      const val = v?.result?.value ?? null;
      if (val !== null && (val?.err === null || val?.err === undefined)) {
        clean += 1n;
        const dir = orcaFirst ? 'orca→ray' : 'ray→orca';
        const cu = val?.unitsConsumed;
        console.error(`🎉 CLEAN SIM [${dir}] — profitable arb exists NOW, tx would land (edge≈${edgeBps.toFixed(2)}bp, cu=${cu})`);
        const row = { t: now(), dir, edge_bps: edgeBps, cu, borrow_usdc: borrowUi };
        try {
          await appendFile(out, `${JSON.stringify(row)}\n`);
        } catch {
          // best-effort, mirrors the Rust `let _ =` swallow
        }
      }
    }
    if (iters % 50n === 0n) {
      console.error(`[profit-watch] iters=${iters} clean_sims=${clean} best_edge=${bestEdgeBps.toFixed(2)}bp (need >0 to profit)`);
    }
    await sleep(pollMs);
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
