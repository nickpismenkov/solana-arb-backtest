import 'dotenv/config';
import { Detector, type ArbEvent } from './detector.js';
import { startGrpcFeed } from './grpc-feed.js';
import { startShredstreamFeed } from './shredstream-feed.js';

// The real shadow test (run on the co-located Amsterdam box):
//  • gRPC account feed → Detector → real fee-adjusted arb OPEN/CLOSE (reaction
//    budget in slots — market ground truth).
//  • ShredStream feed → the fast trigger: we timestamp when WE'd have seen the
//    dislocating swap. Both feeds share the box clock, so the delta is honest.
//  • At each arb open we record how long AGO ShredStream last delivered a swap
//    on our pools = ShredStream's head start over the (slower) gRPC feed.
//
// Verdict inputs: budget (slots) = how long you have; lead (ms) = how much
// sooner ShredStream tells you. Contendable ≈ budget ≥ 1 slot AND we had a
// fresh ShredStream trigger.

const RUN_MS = Number(process.env.RUN_MS || 600_000); // 10 min default
// Orca whirlpool 4 bps, Raydium CLMM 4 bps → 8 bp round-trip threshold.
const detector = new Detector({ venues: ['Orca', 'Raydium'], feeBps: { Orca: 4, Raydium: 4 } });
const median = (a: number[]) => (a.length ? [...a].sort((x, y) => x - y)[Math.floor(a.length / 2)] : 0);

async function main() {
  console.log(`\nshadow-live — gRPC prices + ShredStream triggers. Threshold ${detector.thresholdBps} bps. Running ${RUN_MS / 1000}s…\n`);

  let triggerCount = 0;
  let lastTriggerMs = 0;
  const stopShreds = await startShredstreamFeed((t) => {
    triggerCount++;
    lastTriggerMs = t.tsMs;
  });

  const events: (ArbEvent & { leadMs: number })[] = [];
  let openLeadMs = 0;
  let gTicks = 0;
  let lastOrca = NaN;
  let lastRay = NaN;
  // Heartbeat so we can SEE the price feed is alive + the live spread (proves a
  // 0-arb result is genuine parity, not a dead feed).
  const ghb = setInterval(() => {
    const spread =
      Number.isFinite(lastOrca) && Number.isFinite(lastRay)
        ? (((lastRay - lastOrca) / Math.min(lastOrca, lastRay)) * 10_000).toFixed(1) + ' bps'
        : 'n/a';
    console.log(`[gRPC] ticks=${gTicks} Orca=$${lastOrca.toFixed(4)} Raydium=$${lastRay.toFixed(4)} spread=${spread} (arb>${detector.thresholdBps}bps)`);
  }, 10_000);

  const stopGrpc = await startGrpcFeed((tick) => {
    gTicks++;
    if (tick.venue === 'Orca') lastOrca = tick.price;
    else if (tick.venue === 'Raydium') lastRay = tick.price;
    const r = detector.onTick(tick);
    if (r.type === 'open') {
      // ms since ShredStream last delivered a swap on our pools = its head start
      // over what the gRPC feed is only now reflecting.
      openLeadMs = lastTriggerMs ? tick.tsMs - lastTriggerMs : -1;
      console.log(`⚡ arb OPEN slot ${tick.slot} net ${r.netBps.toFixed(1)}bps · shredstream saw a swap ${openLeadMs < 0 ? '(none yet)' : openLeadMs + 'ms ago'}`);
    } else if (r.type === 'close') {
      events.push({ ...r.event, leadMs: openLeadMs });
      console.log(`   closed ${r.event.lifetimeSlots} slots / ${r.event.lifetimeMs}ms · peak ${r.event.peakNetBps.toFixed(1)}bps`);
    }
  });

  setTimeout(() => {
    stopShreds();
    stopGrpc();
    clearInterval(ghb);
    console.log(`\n──────── shadow-live report (${RUN_MS / 1000}s) ────────`);
    console.log(`ShredStream pool triggers seen: ${triggerCount}`);
    console.log(`Real fee-adjusted arbs: ${events.length}`);
    if (events.length) {
      const slots = events.map((e) => e.lifetimeSlots);
      const leads = events.map((e) => e.leadMs).filter((x) => x >= 0);
      const contend = events.filter((e) => e.lifetimeSlots >= 1 && e.leadMs >= 0).length;
      console.log(`  reaction budget: median ${median(slots)} slots / ${median(events.map((e) => e.lifetimeMs))} ms`);
      console.log(`  ShredStream head start at open: median ${median(leads).toFixed(0)} ms (how much sooner we saw the swap vs gRPC)`);
      console.log(`  contendable (budget ≥ 1 slot & had a ShredStream signal): ${contend}/${events.length} (${((contend / events.length) * 100).toFixed(0)}%)`);
    } else {
      console.log('  none: pair arbed to within fees even at ShredStream resolution over this window.');
    }
    console.log();
    process.exit(0);
  }, RUN_MS);
}

main().catch((e) => {
  console.error('\nError:', e instanceof Error ? e.message : e);
  process.exit(1);
});
