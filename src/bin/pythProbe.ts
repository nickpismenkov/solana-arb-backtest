// Port of src/bin/pyth_probe.rs
//
// Pyth Lazer feed probe — connects with PYTH_LAZER_TOKEN, subscribes to a few
// feeds, and prints live prices from the shared table for ~10s. Confirms the
// Rust feed module works end-to-end (auth, subscribe, parse, scale).
//
// Usage: PYTH_LAZER_TOKEN=<key> [FEED_IDS=6,7] tsx src/bin/pythProbe.ts

import 'dotenv/config';
import { get, newTable, spawnLazer } from '../lib/pyth.js';

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function main(): Promise<void> {
  const token = process.env.PYTH_LAZER_TOKEN;
  if (token === undefined) throw new Error('PYTH_LAZER_TOKEN (.env)');
  const feedIds: number[] = (process.env.FEED_IDS ?? '6,7')
    .split(',')
    .map((s) => Number.parseInt(s.trim(), 10))
    .filter((n) => Number.isFinite(n));

  const names = new Map<number, string>([
    [1, 'BTC'],
    [2, 'ETH'],
    [6, 'SOL'],
    [7, 'USDC'],
  ]);

  console.error(`[pyth_probe] subscribing to feeds ${JSON.stringify(feedIds)} …`);
  const table = newTable();
  spawnLazer(token, feedIds, table);

  for (let tick = 0; tick < 10; tick++) {
    await sleep(1000);
    let line = `t+${tick}s  `;
    for (const id of feedIds) {
      const p = get(table, id);
      const name = names.get(id) ?? '?';
      if (p !== undefined) {
        line += `${name}(${id})=$${p.price.toFixed(4)} [${p.tsUs}µs]  `;
      } else {
        line += `${name}(${id})=…  `;
      }
    }
    console.log(line);
  }
  console.error('[pyth_probe] done');
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
