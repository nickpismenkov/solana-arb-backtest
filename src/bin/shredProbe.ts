// Port of src/bin/shred_probe.rs
//
// Standalone ShredStream feed probe — run on the co-located box to confirm
// the fast feed is live and hitting our pools before wiring it into the
// shadow harness. `RUN_MS=60000 tsx src/bin/shredProbe.ts`.

import 'dotenv/config';
import { runShredstreamFeed, type Trigger } from '../lib/shredstream.js';

async function main(): Promise<void> {
  const port = Number.parseInt(process.env.SHREDSTREAM_PORT ?? '', 10) || 20000;
  const runMs = Number.parseInt(process.env.RUN_MS ?? '', 10) || 60_000;
  // ALT resolution needs an RPC; set RPC_ENDPOINT to catch routed swaps.
  const rpc = process.env.RPC_ENDPOINT;
  console.log(`shred-probe — listening udp/${port} for ${runMs / 1000}s (ALT resolution: ${rpc !== undefined ? 'on' : 'off'})…\n`);

  let count = 0n;
  const triggers: Trigger[] = [];
  let wake: (() => void) | undefined;
  runShredstreamFeed(port, rpc, (t) => {
    triggers.push(t);
    if (wake) {
      const w = wake;
      wake = undefined;
      w();
    }
  });

  const deadline = Date.now() + runMs;
  await new Promise<void>((resolve) => {
    const tick = (): void => {
      const now = Date.now();
      if (now >= deadline) {
        resolve();
        return;
      }
      if (triggers.length > 0) {
        const t = triggers.shift()!;
        count += 1n;
        if (count <= 20n || count % 100n === 0n) {
          const sig = t.sig.slice(0, Math.min(8, t.sig.length));
          console.log(`trigger #${count} ${t.venue} slot ${t.slot} sig ${sig}…`);
        }
        tick();
        return;
      }
      const timer = setTimeout(tick, Math.min(50, deadline - now));
      wake = () => {
        clearTimeout(timer);
        tick();
      };
    };
    tick();
  });

  console.log(`\nshred-probe: ${count} pool triggers in ${runMs / 1000}s`);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
