// Port of src/main.rs
//
// shadow (TS) — reproduces the TS `shadow` harness: gRPC prices → the
// fee-adjusted Detector → reaction-budget report, with a live price heartbeat.
// Testable locally against Tatum; the ShredStream feed + shred-time pricing
// land in later PRs.

import 'dotenv/config';
import { medianF64, medianU128, type ArbEvent, Detector, type Tick, type TickResult } from '../lib/detector.js';
import { runGrpcFeed } from '../lib/grpc.js';
import { pair } from '../lib/pools.js';

async function main(): Promise<void> {
  const endpoint = process.env.GRPC_ENDPOINT ?? 'https://solana-mainnet-grpc.gateway.tatum.io';
  const xToken = process.env.GRPC_X_TOKEN;
  if (xToken === undefined) throw new Error('set GRPC_X_TOKEN in .env');
  const runMs = Number.parseInt(process.env.RUN_MS ?? '', 10) || 120_000;

  const cfg = pair();
  const detector = new Detector('Orca', 'Raydium', cfg.orcaFeeBps, cfg.rayFeeBps);
  console.log(
    `\nshadow (TS) — gRPC prices, pair ${cfg.label}. Threshold ${detector.thresholdBps} bps. Running ${runMs / 1000}s…\n`,
  );

  const ticksQueue: Tick[] = [];
  let wake: (() => void) | undefined;
  const pushTick = (t: Tick): void => {
    ticksQueue.push(t);
    if (wake) {
      const w = wake;
      wake = undefined;
      w();
    }
  };

  runGrpcFeed(endpoint, xToken, pushTick).catch((e) => {
    console.error(`gRPC feed error: ${e}`);
  });

  const events: ArbEvent[] = [];
  let lastOrca = Number.NaN;
  let lastRay = Number.NaN;
  let ticks = 0;

  const deadline = Date.now() + runMs;

  // Races: deadline / heartbeat tick (every 10s) / next queued Tick — mirrors
  // the Rust `tokio::select!` which handles exactly one event per iteration.
  type Event = { kind: 'deadline' } | { kind: 'heartbeat' } | { kind: 'tick'; t: Tick };
  const nextEvent = (heartbeatDueAt: number): Promise<Event> =>
    new Promise((resolve) => {
      if (ticksQueue.length > 0) {
        resolve({ kind: 'tick', t: ticksQueue.shift()! });
        return;
      }
      const now = Date.now();
      const untilDeadline = deadline - now;
      const untilHeartbeat = heartbeatDueAt - now;
      const waitMs = Math.max(0, Math.min(untilDeadline, untilHeartbeat));
      const timer = setTimeout(() => {
        wake = undefined;
        resolve(now + waitMs >= deadline ? { kind: 'deadline' } : { kind: 'heartbeat' });
      }, waitMs);
      wake = () => {
        clearTimeout(timer);
        resolve({ kind: 'tick', t: ticksQueue.shift()! });
      };
    });

  let heartbeatDueAt = Date.now() + 10_000;
  while (true) {
    const ev = await nextEvent(heartbeatDueAt);
    if (ev.kind === 'deadline') {
      break;
    } else if (ev.kind === 'heartbeat') {
      heartbeatDueAt = Date.now() + 10_000;
      const spread =
        Number.isFinite(lastOrca) && Number.isFinite(lastRay)
          ? `${(((lastRay - lastOrca) / Math.min(lastOrca, lastRay)) * 10_000).toFixed(1)} bps`
          : 'n/a';
      console.log(
        `[gRPC] ticks=${ticks} Orca=$${lastOrca.toFixed(4)} Raydium=$${lastRay.toFixed(4)} spread=${spread} (arb>${detector.thresholdBps}bps)`,
      );
    } else {
      const t = ev.t;
      ticks += 1;
      if (t.venue === 'Orca') {
        lastOrca = t.price;
      } else {
        lastRay = t.price;
      }
      const result: TickResult = detector.onTick(t);
      if (result.kind === 'open') {
        console.log(`⚡ arb OPEN slot ${t.slot} net ${result.netBps.toFixed(1)}bps`);
      } else if (result.kind === 'close') {
        const closed = result.event;
        console.log(
          `   closed ${closed.lifetimeSlots} slots / ${closed.lifetimeMs}ms · peak ${closed.peakNetBps.toFixed(1)}bps`,
        );
        events.push(closed);
      }
    }
  }

  console.log(`\n──────── shadow report (${runMs / 1000}s) ────────`);
  console.log(`gRPC ticks: ${ticks}`);
  console.log(`Real fee-adjusted arbs: ${events.length}`);
  if (events.length === 0) {
    console.log('  none: pair arbed to within fees at this feed resolution.');
  } else {
    const slots = events.map((e) => e.lifetimeSlots);
    const ms = events.map((e) => e.lifetimeMs);
    const nets = events.map((e) => e.peakNetBps);
    const maxNet = nets.reduce((a, b) => Math.max(a, b), Number.NEGATIVE_INFINITY);
    console.log(`  peak net edge: median ${medianF64(nets).toFixed(1)} bps, max ${maxNet.toFixed(1)} bps`);
    console.log(`  lifetime (reaction budget): median ${medianU128(slots)} slots / ${medianU128(ms)} ms`);
  }
}

main();
