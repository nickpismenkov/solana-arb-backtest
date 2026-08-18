// Port of src/bin/jito_probe.rs
//
// Connectivity check for the Jito block engine (safe — read-only, no bundle
// sent, no money). Confirms we can reach the region-matched block engine and
// fetch tip accounts before wiring live submission.
//
// Usage: JITO_BLOCK_ENGINE=https://amsterdam.mainnet.block-engine.jito.wtf \
//          tsx src/bin/jitoProbe.ts

import 'dotenv/config';
import { defaultBlockEngine, getTipAccounts } from '../lib/jito.js';

async function main(): Promise<void> {
  const be = defaultBlockEngine();
  console.log(`block engine: ${be}`);
  try {
    const accts = await getTipAccounts(be);
    console.log(`reachable ✓  ${accts.length} tip accounts:`);
    for (const a of accts) {
      console.log(`  ${a.toBase58()}`);
    }
  } catch (e) {
    console.log(`FAILED: ${e}`);
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
