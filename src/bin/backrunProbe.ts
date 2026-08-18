// Port of src/bin/backrun_probe.rs
//
// The go/no-go measurement. On each ShredStream trigger (a swap hitting one of
// our pools), simulate that victim tx against current state and read the
// POST-victim pool prices — the residual cross-venue gap a backrun placed
// right after it could capture. Real chain math (CPI and all), no tx
// construction, no money at risk.
//
// Reverts (the victim's own slippage guard) are skipped — those wouldn't have
// moved the pool anyway. Sampling: while a simulate is in flight, queued
// triggers are drained and counted as skipped (we measure at the RPC's pace).
//
// Usage (on the box):
//   RPC_ENDPOINT=<helius-url> SHREDSTREAM_PORT=20000 RUN_MS=600000 \
//     tsx src/bin/backrunProbe.ts

import 'dotenv/config';
import { orcaPrice, pair, rayClmmPrice } from '../lib/pools.js';
import { runShredstreamFeed, type Trigger } from '../lib/shredstream.js';

const TIP_CUSHION_BPS = 2.0; // rough gas+tip headroom

async function simulateVictim(rpc: string, txB64: string): Promise<[number, number] | undefined> {
  const body = {
    jsonrpc: '2.0',
    id: 1,
    method: 'simulateTransaction',
    params: [
      txB64,
      {
        encoding: 'base64',
        sigVerify: false,
        replaceRecentBlockhash: true,
        accounts: { encoding: 'base64', addresses: [pair().orcaPool, pair().rayPool] },
      },
    ],
  };
  let resp: any;
  try {
    const res = await fetch(rpc, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body),
    });
    resp = await res.json();
  } catch {
    return undefined;
  }
  const v = resp?.result?.value;
  if (v?.err !== null && v?.err !== undefined) {
    return undefined; // victim reverted (slippage) — wouldn't move the pool
  }
  const accs: any[] = v?.accounts;
  if (!Array.isArray(accs)) return undefined;
  const dec = (i: number): Uint8Array | undefined => {
    const b64 = accs[i]?.data?.[0];
    if (typeof b64 !== 'string') return undefined;
    return Uint8Array.from(Buffer.from(b64, 'base64'));
  };
  const orcaData = dec(0);
  const rayData = dec(1);
  if (orcaData === undefined || rayData === undefined) return undefined;
  const orca = orcaPrice(orcaData);
  const ray = rayClmmPrice(rayData);
  if (orca === undefined || ray === undefined) return undefined;
  return [orca, ray];
}

function median(v: number[]): number {
  if (v.length === 0) return 0.0;
  const sorted = [...v].sort((a, b) => a - b);
  return sorted[Math.floor(sorted.length / 2)]!;
}

async function main(): Promise<void> {
  const rpc = process.env.RPC_ENDPOINT;
  if (rpc === undefined) throw new Error('set RPC_ENDPOINT (Helius) for simulate + ALT');
  const port = Number.parseInt(process.env.SHREDSTREAM_PORT ?? '', 10) || 20000;
  const runMs = Number.parseInt(process.env.RUN_MS ?? '', 10) || 600_000;

  const feeBps = pair().roundTripFeeBps();
  console.log(
    `backrun-probe — simulate victims → residual gap. pair ${pair().label}, threshold: fee ${feeBps}bp (+${TIP_CUSHION_BPS}bp cushion). Running ${runMs / 1000}s…\n`,
  );

  const triggerQueue: Trigger[] = [];
  let wake: (() => void) | undefined;
  const pushTrigger = (t: Trigger): void => {
    triggerQueue.push(t);
    if (wake) {
      const w = wake;
      wake = undefined;
      w();
    }
  };
  runShredstreamFeed(port, rpc, pushTrigger);

  let triggers = 0;
  let skipped = 0;
  let simmed = 0;
  let reverted = 0;
  let opps = 0;
  let oppsNet = 0;
  const gaps: number[] = [];
  const nets: number[] = [];

  const deadline = Date.now() + runMs;

  type Event = { kind: 'deadline' } | { kind: 'trigger'; t: Trigger };
  const nextEvent = (): Promise<Event> =>
    new Promise((resolve) => {
      if (triggerQueue.length > 0) {
        resolve({ kind: 'trigger', t: triggerQueue.shift()! });
        return;
      }
      const waitMs = Math.max(0, deadline - Date.now());
      const timer = setTimeout(() => {
        wake = undefined;
        resolve({ kind: 'deadline' });
      }, waitMs);
      wake = () => {
        clearTimeout(timer);
        resolve({ kind: 'trigger', t: triggerQueue.shift()! });
      };
    });

  while (true) {
    if (Date.now() >= deadline) break;
    const ev = await nextEvent();
    if (ev.kind === 'deadline') break;
    const t = ev.t;
    triggers += 1;
    // Drain backlog accumulated while the last sim ran → sample at RPC pace.
    while (triggerQueue.length > 0) {
      triggerQueue.shift();
      triggers += 1;
      skipped += 1;
    }
    if (t.raw.length === 0) continue;
    const txB64 = Buffer.from(t.raw).toString('base64');
    const res = await simulateVictim(rpc, txB64);
    if (res === undefined) {
      reverted += 1;
    } else {
      const [orca, ray] = res;
      simmed += 1;
      const gap = Math.abs(((ray - orca) / Math.min(orca, ray)) * 10_000.0);
      gaps.push(gap);
      if (gap > feeBps) {
        opps += 1;
        const net = gap - feeBps;
        nets.push(net);
        if (gap > feeBps + TIP_CUSHION_BPS) oppsNet += 1;
        console.log(
          `⚡ backrunnable via ${t.venue} slot ${t.slot} — gap ${gap.toFixed(1)}bp, net ${net.toFixed(1)}bp (post-victim Orca $${orca.toFixed(4)} / Ray $${ray.toFixed(4)})`,
        );
      }
    }
  }

  console.log(`\n──────── backrun-probe report (${runMs / 1000}s) ────────`);
  console.log(`pool triggers seen:        ${triggers}`);
  console.log(`  simulated (sampled):     ${simmed + reverted}`);
  console.log(`  skipped (RPC-paced):     ${skipped}`);
  console.log(`victim sims applied ok:    ${simmed}`);
  console.log(`victim sims reverted:      ${reverted}  (own slippage — no pool move)`);
  console.log('── residual cross-venue gap after a real swap ──');
  if (simmed > 0) {
    console.log(`  median gap: ${median(gaps).toFixed(1)} bp   max gap: ${gaps.reduce((a, b) => Math.max(a, b), 0.0).toFixed(1)} bp`);
    console.log(`  fee-clearing (>${feeBps}bp):        ${opps}/${simmed} (${((opps / simmed) * 100).toFixed(0)}%)`);
    console.log(`  after tip cushion (>${(feeBps + TIP_CUSHION_BPS).toFixed(0)}bp): ${oppsNet}/${simmed} (${((oppsNet / simmed) * 100).toFixed(0)}%)`);
    if (nets.length > 0) {
      console.log(`  net edge when present: median ${median(nets).toFixed(1)} bp, max ${nets.reduce((a, b) => Math.max(a, b), 0.0).toFixed(1)} bp`);
    }
  } else {
    console.log('  no successful victim sims — check RPC / freshness.');
  }
  console.log();
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
