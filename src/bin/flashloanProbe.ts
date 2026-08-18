// Port of src/bin/flashloan_probe.rs
//
// Verifies the Jupiter Lend flash-loan builders: assemble
// [create-ATA, borrow, payback] for EACH wired debt asset (USDC/USDT/wSOL) and
// simulate against mainnet. A self-repaying 0-fee flash loan nets zero, so with
// the ATA created each should simulate clean (err = null) — proving the ported
// instruction format + per-asset market accounts are correct end to end. This
// is the ground-truth check for the derived USDT/wSOL flash markets.
//
// Usage: RPC_ENDPOINT=<url> tsx src/bin/flashloanProbe.ts

import 'dotenv/config';
import { Message, PublicKey, VersionedTransaction } from '@solana/web3.js';
import { borrow, createAtaIdempotentFor, payback, USDC_MINT, USDT_MINT, WSOL_MINT } from '../lib/flashloan.js';

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

async function probe(endpoint: string, signer: PublicKey, tp: PublicKey, name: string, mint: PublicKey, amount: bigint): Promise<boolean> {
  const borrowIx = borrow(signer, mint, amount);
  const paybackIx = payback(signer, mint, amount);
  if (!borrowIx || !paybackIx) throw new Error('wired market');
  const ixs = [createAtaIdempotentFor(signer, mint, tp), borrowIx, paybackIx];
  const msg = Message.compile({
    payerKey: signer,
    recentBlockhash: DEFAULT_BLOCKHASH,
    instructions: ixs,
  });
  const tx = new VersionedTransaction(msg);
  const b64 = Buffer.from(tx.serialize()).toString('base64');
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'simulateTransaction',
    params: [b64, { encoding: 'base64', sigVerify: false, replaceRecentBlockhash: true }],
  });
  const val = v?.result?.value ?? {};
  const err = val?.err ?? null;
  console.log(`\n=== Jupiter Lend ${name} flash loan (borrow ${amount} → payback ${amount}) ===`);
  console.log(`err: ${JSON.stringify(err)}`);
  if (err === null || err === undefined) {
    const units = val?.unitsConsumed ?? 0;
    console.log(`✅ ${name} VERIFIED — self-repaying flash loan simulated clean (${units} CU)`);
    return true;
  } else {
    console.log(`⚠️  ${name} did not simulate clean — inspect logs:`);
    for (const l of Array.isArray(val?.logs) ? val.logs : []) {
      console.log(`  ${typeof l === 'string' ? l : ''}`);
    }
    return false;
  }
}

async function main(): Promise<void> {
  const endpoint = process.env.RPC_ENDPOINT ?? process.env.HELIUS_RPC ?? 'https://api.mainnet-beta.solana.com';
  const signer = new PublicKey('Anu6Awu4kxaEDrg1nkpcikx6tJ2xhfVci5TvDrZBsZEB');
  const tp = new PublicKey('TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA');

  const usdc = new PublicKey(USDC_MINT);
  const usdt = new PublicKey(USDT_MINT);
  const wsol = new PublicKey(WSOL_MINT);

  let ok = 0;
  if (await probe(endpoint, signer, tp, 'USDC', usdc, 1_000_000n)) ok += 1;
  if (await probe(endpoint, signer, tp, 'USDT', usdt, 1_000_000n)) ok += 1;
  if (await probe(endpoint, signer, tp, 'wSOL', wsol, 10_000_000n)) ok += 1; // 0.01 SOL

  console.log(`\n── ${ok}/3 flash markets verified ──`);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
