// Port of src/bin/arb_probe.rs
//
// Verifies the shared arb builder (arb::build_arb_tx — the exact code the
// executor runs) by simulating BOTH directions against mainnet via the ALT.
// With no spread each direction reverts at leg2 (insufficient funds) — the
// profit-or-revert guard. A structural error would look different (bad meta,
// sqrt-limit, layout).
//
// Usage: RPC_ENDPOINT=<url> ALT_ADDRESS=<alt> [BORROW_USDC=500] [SIGNER=<pubkey>] \
//   [SHOW_LOGS=1] tsx src/bin/arbProbe.ts

import 'dotenv/config';
import { PublicKey } from '@solana/web3.js';
import { buildArbTx, loadAlt, type PoolData } from '../lib/arb.js';
import { pair } from '../lib/pools.js';

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

async function accountData(endpoint: string, addr: string): Promise<Uint8Array> {
  const v = await rpc(endpoint, { jsonrpc: '2.0', id: 1, method: 'getAccountInfo', params: [addr, { encoding: 'base64' }] });
  const b64 = v?.result?.value?.data?.[0];
  if (typeof b64 !== 'string') throw new Error('data');
  return Uint8Array.from(Buffer.from(b64, 'base64'));
}

// Instruction order in buildArbTx: [0 cu_limit, 1 cu_price, 2 ata, 3 ata,
// 4 borrow, 5 leg1, 6 leg2(guard), 7 payback, 8 tip]. Only a leg2 revert is
// the guard doing its job; a revert anywhere else is a structural bug.
const LEG2_IX = 6;

// base58 of Hash::default() (32 zero bytes) — placeholder blockhash for
// replace-blockhash simulation.
const DEFAULT_HASH_B58 = '11111111111111111111111111111111';

function classify(err: any): string {
  if (err === null || err === undefined) {
    return '✅ SIMULATED CLEAN — a profitable arb exists right now; tx would land';
  }
  const ix = err?.InstructionError?.[0];
  if (ix === LEG2_IX) {
    return '✅ GUARD WORKING — borrow+leg1 executed, leg2 reverted (no spread → profit-or-revert)';
  } else if (typeof ix === 'number') {
    return `❌ STRUCTURAL ERROR — reverted at instruction ${ix} (before the guard): ${JSON.stringify(err)}`;
  } else {
    return `⚠️  inconclusive — ${JSON.stringify(err)}`;
  }
}

async function main(): Promise<void> {
  const endpoint = process.env.RPC_ENDPOINT ?? 'https://api.mainnet-beta.solana.com';
  const altAddr = process.env.ALT_ADDRESS;
  if (altAddr === undefined) throw new Error('set ALT_ADDRESS');
  const borrowUi = Number.parseFloat(process.env.BORROW_USDC ?? '') || 500.0;
  const borrowAmount = BigInt(Math.round(borrowUi * 1e6));
  const cfg = pair();
  const signerStr = process.env.SIGNER ?? 'Anu6Awu4kxaEDrg1nkpcikx6tJ2xhfVci5TvDrZBsZEB';
  const signer = new PublicKey(signerStr);
  const showLogs = process.env.SHOW_LOGS === '1';

  const alt = loadAlt(altAddr, await accountData(endpoint, altAddr));
  const pools: PoolData = {
    orca: await accountData(endpoint, cfg.orcaPool),
    ray: await accountData(endpoint, cfg.rayPool),
  };

  console.log(`arb-probe ${cfg.label} borrow ${borrowUi} USDC — verifying both directions via arb::build_arb_tx\n`);

  for (const orcaFirst of [true, false]) {
    const dir = orcaFirst ? 'orca→ray (buy Orca, sell Ray)' : 'ray→orca (buy Ray, sell Orca)';
    const tx = buildArbTx(pools, signer, alt, borrowAmount, orcaFirst, undefined, 0n, 10_000n, DEFAULT_HASH_B58, 0n);
    const raw = tx.serialize();
    const b64 = Buffer.from(raw).toString('base64');
    const v = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'simulateTransaction',
      params: [b64, { encoding: 'base64', sigVerify: false, replaceRecentBlockhash: true }],
    });
    const err = v?.error;
    if (err !== undefined && err !== null) {
      console.log(`=== ${dir} ===\n  ⛔ not simulated: ${err?.message ?? ''}\n`);
      continue;
    }
    const val = v?.result?.value ?? {};
    const logs: string[] = Array.isArray(val?.logs) ? val.logs.filter((l: unknown): l is string => typeof l === 'string') : [];
    console.log(`=== ${dir} ===`);
    console.log(`  signer ${signer.toBase58()} | tx ${raw.length} bytes | err ${JSON.stringify(val?.err ?? null)}`);
    console.log(`  ${classify(val?.err)}`);
    if (showLogs) {
      for (const l of logs) {
        console.log(`    ${l}`);
      }
    }
    console.log();
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
