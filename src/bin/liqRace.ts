// Port of src/bin/liq_race.rs
//
// Race monitor — the decisive "are we losing on SPEED or on TIP?" diagnostic.
//
// Scans actual on-chain liquidations that COMPETITORS won on Solend, then
// cross-references our own detection log ({RUN_DIR}/decisions.jsonl, which
// records the obligation + timestamp each time we processed it). For every
// real liquidation it classifies us as:
//   AHEAD   — we had flagged that obligation BEFORE it was liquidated → we
//             lost the fill on ACTION/TIP (auction), not detection.
//   BEHIND  — we flagged it only AFTER it was already liquidated → detection
//             was too slow.
//   MISSED  — we never saw it at all → detection missed it entirely (poll too
//             slow / not in the watch-set).
// The AHEAD vs BEHIND/MISSED split tells us exactly which bottleneck to fix.
//
// Usage: HELIUS_RPC=<url> [RUN_DIR=runs/save] [PAGES=6] tsx src/bin/liqRace.ts

import 'dotenv/config';
import * as fs from 'node:fs';
import * as readline from 'node:readline';
import bs58 from 'bs58';

const SOLEND_PROGRAM = 'So1endDq2YkqhipRh3WViPa8hdiSpxWy6z3Z6tMCpAo';

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
    await sleep(400 << attempt);
  }
  return undefined;
}

/** Earliest unix-second we logged each obligation, from our decisions ledger. */
async function ourFirstSeen(runDir: string): Promise<Map<string, number>> {
  const m = new Map<string, number>();
  const path = `${runDir}/decisions.jsonl`;
  if (!fs.existsSync(path)) return m;
  const rl = readline.createInterface({ input: fs.createReadStream(path), crlfDelay: Infinity });
  for await (const line of rl) {
    let v: any;
    try {
      v = JSON.parse(line);
    } catch {
      continue;
    }
    // Save/Kamino key the account as "obligation"; marginfi as "liquidatee".
    const acct: unknown = v?.obligation ?? v?.liquidatee;
    const t: unknown = v?.t;
    if (typeof acct === 'string' && typeof t === 'number') {
      const existing = m.get(acct);
      m.set(acct, existing === undefined ? t : Math.min(existing, t));
    }
  }
  return m;
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (!endpoint) throw new Error('HELIUS_RPC');
  const runDir = process.env.RUN_DIR ?? 'runs/save';
  const pages = Number.parseInt(process.env.PAGES ?? '', 10) || 6;

  const seen = await ourFirstSeen(runDir);
  console.error(`[race] loaded ${seen.size} distinct obligations we logged (from ${runDir}/decisions.jsonl)`);

  // Page recent Solend signatures.
  const sigs: Array<[string, number | undefined]> = [];
  let before: string | undefined;
  for (let i = 0; i < pages; i++) {
    const params: any = { limit: 1000 };
    if (before !== undefined) params.before = before;
    const resp = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'getSignaturesForAddress',
      params: [SOLEND_PROGRAM, params],
    });
    const page: any[] = Array.isArray(resp?.result) ? resp.result : [];
    if (page.length === 0) break;
    before = page[page.length - 1]?.signature;
    for (const e of page) {
      if (e?.err === null || e?.err === undefined) {
        sigs.push([typeof e?.signature === 'string' ? e.signature : '', typeof e?.blockTime === 'number' ? e.blockTime : undefined]);
      }
    }
  }
  console.error(`[race] scanning ${sigs.length} Solend txs for competitor liquidations …`);

  let ahead = 0;
  let behind = 0;
  let missed = 0;
  let total = 0;
  const aheadSecs: number[] = [];

  for (const [sig, bt] of sigs) {
    const tx = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'getTransaction',
      params: [sig, { encoding: 'jsonParsed', maxSupportedTransactionVersion: 0, commitment: 'confirmed' }],
    });
    const result = tx?.result;
    if (result === null || result === undefined) continue;
    const ixs: any[] = Array.isArray(result?.transaction?.message?.instructions)
      ? [...result.transaction.message.instructions]
      : [];
    const innerGroups: any[] = Array.isArray(result?.meta?.innerInstructions) ? result.meta.innerInstructions : [];
    for (const inner of innerGroups) {
      if (Array.isArray(inner?.instructions)) ixs.push(...inner.instructions);
    }
    for (const ix of ixs) {
      if (ix?.programId !== SOLEND_PROGRAM) continue;
      let data: Uint8Array;
      try {
        data = bs58.decode(typeof ix?.data === 'string' ? ix.data : '');
      } catch {
        continue;
      }
      if (data.length === 0) continue;
      const tag = data[0];
      // obligation account index: tag 17 (LiquidateAndRedeem) → [10]; tag 12 → [6].
      let idx: number | undefined;
      if (tag === 17) idx = 10;
      else if (tag === 12) idx = 6;
      else continue;
      const accts: any[] = Array.isArray(ix?.accounts) ? ix.accounts : [];
      const obl = accts[idx];
      if (typeof obl !== 'string') continue;
      total += 1;
      const landed = bt ?? 0;
      const ourT = seen.get(obl);
      if (ourT !== undefined && ourT <= landed) {
        ahead += 1;
        aheadSecs.push(landed - ourT);
      } else if (ourT !== undefined) {
        behind += 1;
      } else {
        missed += 1;
      }
    }
    await sleep(15);
  }

  console.log(`\n═══ race analysis (Solend, vs our ${runDir} detections) ═══`);
  console.log(`competitor liquidations seen: ${total}`);
  console.log(`  AHEAD  (we flagged it BEFORE it was liquidated → lost on TIP/ACTION): ${ahead}`);
  console.log(`  BEHIND (flagged only after it was already gone → detection slow):     ${behind}`);
  console.log(`  MISSED (never saw it at all → detection missed entirely):             ${missed}`);
  if (aheadSecs.length > 0) {
    aheadSecs.sort((a, b) => a - b);
    const med = aheadSecs[Math.floor(aheadSecs.length / 2)];
    console.log(`  of the AHEAD ones, median lead time: ${med}s (we had this long to win the auction)`);
  }
  console.log('\nVERDICT:');
  if (total === 0) {
    console.log('  no competitor liquidations in window — raise PAGES or run during activity.');
  } else if (ahead >= behind + missed) {
    console.log('  Mostly AHEAD → detection is NOT the bottleneck; we\'re losing the AUCTION.');
    console.log('  Optimize: tip sizing (TIP_FRACTION_BPS), arm coverage, submit colocation — not detection.');
  } else {
    console.log('  Mostly BEHIND/MISSED → DETECTION SPEED is the bottleneck.');
    console.log('  Optimize: event-driven Lazer trigger (kill polls), watch-set coverage, crank front-run.');
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
