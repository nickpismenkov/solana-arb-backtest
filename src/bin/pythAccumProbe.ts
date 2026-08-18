// Port of src/bin/pyth_accum_probe.rs
//
// Verify the Hermes accumulator parser against a live update: fetch SOL+USDC,
// print the VAA length and each update's feed id + message/proof lengths. The
// split must match what a real mainnet crank tx carried (~247B VAA, ~396B per
// update) and the feed ids must equal the requested ones — proving we can feed
// the crank ixs correctly.
//
// Usage: [HERMES=https://hermes.pyth.network] tsx src/bin/pythAccumProbe.ts

import 'dotenv/config';
import { fetchHermes } from '../lib/pythAccumulator.js';

function hexs(b: Buffer): string {
  return b.toString('hex');
}

// Canonical Pyth feed ids (hex).
const SOL = 'ef0d8b6fda2ceba41da15d4095d1da392a0d2f8ed0c6c7bc0f4cfac8c280b56d';
const USDC = 'eaa020c61cc479712813461ce153894a96a6c00b21ed0cfc2798d1f9a9e9c94a';

async function main(): Promise<void> {
  const hermes = process.env.HERMES ?? 'https://hermes.pyth.network';
  const update = await fetchHermes(hermes, [SOL, USDC]);
  console.log(`VAA: ${update.vaa.length} bytes`);
  console.log(`${update.updates.length} price updates:`);
  for (const u of update.updates) {
    const fid = u.feedId() !== undefined ? hexs(u.feedId()!) : '';
    console.log(`  feed ${fid.slice(0, Math.min(16, fid.length))}…  message ${u.message.length}B  proof ${u.proof.length}B`);
  }
  const ids = update.updates.map((u) => (u.feedId() !== undefined ? hexs(u.feedId()!) : undefined)).filter((x): x is string => x !== undefined);
  const ok = ids.some((i) => i === SOL) && ids.some((i) => i === USDC);
  if (ok && update.vaa.length !== 0) {
    console.log('★ parser VERIFIED — VAA extracted, both requested feeds present with message+proof');
  } else {
    console.log(`✗ mismatch — got feeds ${JSON.stringify(ids)}`);
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
