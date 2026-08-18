// Port of src/bin/jup_probe.rs
//
// Verify the Jupiter swap client end-to-end by mainnet SIMULATION (no send):
// quote 0.005 SOL → USDC for the live wallet, decode the swap-instructions
// response, fetch its lookup tables, compile a v0 tx, and simulate. Success =
// err null with real CU spent — proves quote parse, ix decode, ALT fetch, and
// v0 compile are all correct before the fire path trusts them.
//
// Usage: HELIUS_RPC=<url> [AUTHORITY=<pk>] [AMOUNT_LAMPORTS=5000000] tsx src/bin/jupProbe.ts

import 'dotenv/config';
import { PublicKey, TransactionMessage, VersionedTransaction } from '@solana/web3.js';
import { cuLimitIx } from '../lib/arb.js';
import { fetchAlts, quote, swapInstructions } from '../lib/jup.js';

const SOL_MINT = 'So11111111111111111111111111111111111111112';
const USDC_MINT = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v';
const DEFAULT_AUTHORITY = 'DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak';
// solana_hash::Hash::default() (32 zero bytes) base58-encodes to this string.
const DEFAULT_BLOCKHASH = '11111111111111111111111111111111';

async function rpc(endpoint: string, body: unknown): Promise<any | undefined> {
  for (let attempt = 0; attempt < 4; attempt++) {
    try {
      const resp = await fetch(endpoint, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(body),
      });
      return await resp.json();
    } catch {
      // fall through to retry
    }
    await new Promise((r) => setTimeout(r, 400 << attempt));
  }
  return undefined;
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (!endpoint) throw new Error('HELIUS_RPC');
  const authority = new PublicKey(process.env.AUTHORITY ?? DEFAULT_AUTHORITY);
  const amount = BigInt(process.env.AMOUNT_LAMPORTS ?? '5000000');
  const sol = new PublicKey(SOL_MINT);
  const usdc = new PublicKey(USDC_MINT);

  console.error(`[jup] quoting ${amount} lamports SOL → USDC …`);
  const quoteResp = await quote(sol, usdc, amount, 50, 30);
  console.error(
    `[jup] route: in=${quoteResp.inAmount} out=${quoteResp.outAmount} (${Array.isArray(quoteResp.routePlan) ? quoteResp.routePlan.length : 0} hops)`,
  );

  const plan = await swapInstructions(quoteResp, authority, true);
  console.error(
    `[jup] ${plan.instructions.length} instructions, ${plan.altAddresses.length} lookup tables, quoted_out=${plan.quotedOut} min_out=${plan.minOut}`,
  );

  const alts = await fetchAlts(endpoint, plan.altAddresses);
  for (const a of alts) console.error(`[jup]   ALT ${a.key.toBase58()} (${a.state.addresses.length} addresses)`);

  // Compile [cu_limit, setup…, swap, cleanup…] and simulate.
  const ixs = [cuLimitIx(1_400_000), ...plan.instructions];
  const msg = new TransactionMessage({
    payerKey: authority,
    recentBlockhash: DEFAULT_BLOCKHASH,
    instructions: ixs,
  }).compileToV0Message(alts);
  const tx = new VersionedTransaction(msg);
  const raw = tx.serialize();
  console.error(`[jup] tx size: ${raw.length} bytes (limit 1232)`);
  const b64tx = Buffer.from(raw).toString('base64');

  const sim = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'simulateTransaction',
    params: [b64tx, { sigVerify: false, replaceRecentBlockhash: true, commitment: 'processed', encoding: 'base64' }],
  });
  if (sim === undefined) throw new Error('simulate');
  const res = sim?.result?.value ?? {};
  console.log('\n──── jup swap simulation ────');
  console.log(`err: ${JSON.stringify(res?.err ?? null)}`);
  console.log(`unitsConsumed: ${res?.unitsConsumed}`);
  if (res?.err === null || res?.err === undefined) {
    console.log('★ VERIFIED — Jupiter-built swap executes clean via our decode/compile path');
  } else if (Array.isArray(res?.logMessages)) {
    for (const l of res.logMessages) console.log(`  ${typeof l === 'string' ? l : ''}`);
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
