import 'dotenv/config';
import { Detector, report, type ArbEvent } from './detector.js';
import { startGrpcFeed } from './grpc-feed.js';

// Shadow harness: wires a feed → the fee-adjusted arb Detector → reaction-budget
// report. Feed-agnostic — swap startGrpcFeed for a ShredStream feed on the NY
// box (same Tick contract) to get the real "would-we-be-first" latency.

const RUN_MS = Number(process.env.RUN_MS || 120_000);
// Verified on-chain: Orca whirlpool 4 bps, Raydium AMM v4 25 bps.
const detector = new Detector({ venues: ['Orca', 'Raydium'], feeBps: { Orca: 4, Raydium: 25 } });

async function main() {
  console.log(`\nShadow engine — feed: gRPC (swap to ShredStream on box). Threshold ${detector.thresholdBps} bps. Running ${RUN_MS / 1000}s…\n`);
  const events: ArbEvent[] = [];
  const stop = await startGrpcFeed((tick) => {
    const r = detector.onTick(tick);
    if (r.type === 'open') {
      console.log(`⚡ arb OPEN  slot ${tick.slot}  net ${r.netBps.toFixed(1)} bps`);
    } else if (r.type === 'close') {
      events.push(r.event);
      console.log(`   closed after ${r.event.lifetimeSlots} slots / ${r.event.lifetimeMs} ms  (peak ${r.event.peakNetBps.toFixed(1)} bps)`);
    }
  });

  setTimeout(() => {
    stop();
    report(events, detector.thresholdBps, RUN_MS / 1000);
    process.exit(0);
  }, RUN_MS);
}

main().catch((e) => {
  console.error('\nError:', e instanceof Error ? e.message : e);
  process.exit(1);
});
