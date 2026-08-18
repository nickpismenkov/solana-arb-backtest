// Port of src/bin/land_probe.rs
//
// End-to-end LANDING certification: fire a Jito bundle that does NOT depend
// on any market spread — [flash-borrow 1 USDC, payback 1 USDC, tip] — so it
// always succeeds and should land on-chain. Proves the whole live path:
// signing, blockhash, flash loan, bundle submission, tip payment, readback.
// Cost when it lands: tip + base fee + priority (~10k lamports, <$0.01).
//
// Default is simulate-only. LIVE=1 submits for real.
// MODE=jito (default) submits as a Jito bundle; MODE=rpc submits the SAME tx
// via plain sendTransaction — bisects "tx invalid" from "Jito bundle path
// broken": if rpc lands and jito doesn't, the tx is fine and Jito is the issue.
//
// Usage: RPC_ENDPOINT=<url> KEYPAIR_PATH=<path> [LIVE=1] [MODE=jito|rpc] \
//   [TIP_LAMPORTS=5000] tsx src/bin/landProbe.ts

import 'dotenv/config';
import { readFileSync } from 'node:fs';
import bs58 from 'bs58';
import { Keypair, PublicKey, TransactionMessage, VersionedTransaction } from '@solana/web3.js';
import { cuLimitIx, cuPriceIx, transferIx } from '../lib/arb.js';
import { borrowUsdc, createAtaIdempotent, paybackUsdc, USDC_MINT } from '../lib/flashloan.js';
import { bundleStatus, defaultBlockEngine, getTipAccounts, sendBundle } from '../lib/jito.js';

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function rpc(endpoint: string, body: unknown): Promise<any | undefined> {
  for (let attempt = 0; attempt < 3; attempt++) {
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
    await sleep(300 << attempt);
  }
  return undefined;
}

async function main(): Promise<void> {
  const endpoint = process.env.RPC_ENDPOINT;
  if (endpoint === undefined) throw new Error('RPC_ENDPOINT');
  const keypairPath = process.env.KEYPAIR_PATH;
  if (keypairPath === undefined) throw new Error('KEYPAIR_PATH');
  const live = process.env.LIVE === '1';
  const mode = process.env.MODE ?? 'jito';
  const tipLamports = BigInt(process.env.TIP_LAMPORTS ?? '5000');
  const blockEngine = defaultBlockEngine();

  const bytes: number[] = JSON.parse(readFileSync(keypairPath, 'utf8'));
  const kp = Keypair.fromSecretKey(Uint8Array.from(bytes));
  const signer = kp.publicKey;
  const usdc = new PublicKey(USDC_MINT);

  const tipAccounts = await getTipAccounts(blockEngine);
  const tipTo = tipAccounts[0];
  if (tipTo === undefined) throw new Error('no tip accounts');
  // FINALIZED blockhash: visible to every bank (confirmed-fresh hashes can be
  // rejected as BlockhashNotFound by validators/preflight still on finalized).
  // Still ~60s of validity left — plenty for a probe.
  const bhResp = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getLatestBlockhash',
    params: [{ commitment: 'finalized' }],
  });
  if (bhResp === undefined) throw new Error('blockhash');
  const bhStr: string = bhResp?.result?.value?.blockhash;
  if (typeof bhStr !== 'string') throw new Error('blockhash str');
  console.log(`blockhash ${bhStr} (slot ${bhResp?.result?.context?.slot}, lastValidBlockHeight ${bhResp?.result?.value?.lastValidBlockHeight})`);

  // No-spread-required bundle: borrow 1 USDC, pay it straight back, tip.
  // BARE=1 drops the flash-loan legs (isolates "Jito filters the flash-loan
  // program" — a bare self-transfer + tip has nothing left to object to).
  const bare = process.env.BARE === '1';
  const withAta = process.env.ATA === '1';
  const ixs = bare
    ? [
        cuLimitIx(50_000),
        cuPriceIx(10_000n),
        ...(withAta ? [createAtaIdempotent(signer, usdc)] : []),
        transferIx(signer, signer, 1_000n),
        transferIx(signer, tipTo, tipLamports),
      ]
    : [
        cuLimitIx(200_000),
        cuPriceIx(10_000n),
        createAtaIdempotent(signer, usdc),
        borrowUsdc(signer, 1_000_000n),
        paybackUsdc(signer, 1_000_000n),
        transferIx(signer, tipTo, tipLamports),
      ];
  const msg = new TransactionMessage({ payerKey: signer, recentBlockhash: bhStr, instructions: ixs }).compileToV0Message([]);
  const tx = new VersionedTransaction(msg);
  tx.sign([kp]);
  const sig = bs58.encode(tx.signatures[0]!);
  const raw = tx.serialize();
  const b64 = Buffer.from(raw).toString('base64');

  console.log(`land-probe: signer=${signer.toBase58()} tx=${raw.length}B tip=${tipLamports} lamports sig=${sig}`);

  // Always simulate first — refuse to submit a tx that wouldn't succeed.
  const sim = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'simulateTransaction',
    params: [b64, { encoding: 'base64', sigVerify: false, replaceRecentBlockhash: true }],
  });
  if (sim === undefined) throw new Error('simulate');
  const err = sim?.result?.value?.err;
  if (err !== null && err !== undefined) {
    console.log(`⛔ simulation failed, NOT submitting: ${JSON.stringify(err)}`);
    const logs: any[] = Array.isArray(sim?.result?.value?.logs) ? sim.result.value.logs : [];
    for (const l of logs) {
      console.log(`  ${typeof l === 'string' ? l : ''}`);
    }
    process.exit(1);
  }
  console.log(`✅ simulates clean (${sim?.result?.value?.unitsConsumed} CU)`);

  // MODE=simbundle: Jito simulateBundle — executes the bundle as the block
  // engine would (needs Helius/QuickNode Lil-JIT). Read-only, no cost.
  if (mode === 'simbundle') {
    const v = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'simulateBundle',
      params: [{ encodedTransactions: [b64] }],
    });
    if (v !== undefined && v?.error != null && v.error !== null) {
      console.log(`simulateBundle error: ${JSON.stringify(v.error)}`);
    } else if (v !== undefined) {
      console.log(`simulateBundle summary: ${JSON.stringify(v?.result?.value?.summary)}`);
      const results: any[] = Array.isArray(v?.result?.value?.transactionResults) ? v.result.value.transactionResults : [];
      results.forEach((r, i) => {
        console.log(`  tx[${i}] err=${JSON.stringify(r?.err)} cu=${r?.unitsConsumed}`);
      });
    } else {
      console.log('simulateBundle: no response (RPC must support it — use Helius)');
    }
    return;
  }

  if (!live) {
    console.log(`dry run (set LIVE=1 to submit for real — costs ~${tipLamports + 10_000n} lamports)`);
    return;
  }

  let bundleId = '';
  if (mode === 'jitotx') {
    // Jito transactions endpoint, bundleOnly=true → single-tx bundle
    // WITH revert protection; documented low-latency send path.
    const url = `${blockEngine}/api/v1/transactions?bundleOnly=true`;
    let v: any;
    try {
      const res = await fetch(url, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'sendTransaction', params: [b64, { encoding: 'base64' }] }),
      });
      if (!res.ok) {
        const body = await res.text().catch(() => '');
        v = { error: `HTTP ${res.status}: ${body}` };
      } else {
        v = await res.json();
      }
    } catch (e) {
      v = { error: String(e) };
    }
    if (v?.error != null && v.error !== null) {
      console.log(`⛔ jito sendTransaction rejected: ${JSON.stringify(v.error)}`);
      process.exit(1);
    }
    console.log(`⚡ sent via jito transactions endpoint (bundleOnly): ${v?.result}`);
  } else if (mode === 'rpc') {
    // Plain sendTransaction — no Jito. If THIS lands, the tx is valid
    // and any Jito non-landing is a bundle-path problem, not ours.
    const v = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'sendTransaction',
      params: [b64, { encoding: 'base64', skipPreflight: false, preflightCommitment: 'confirmed', maxRetries: 5 }],
    });
    if (v === undefined) throw new Error('sendTransaction');
    if (v?.error != null && v.error !== null) {
      console.log(`⛔ sendTransaction rejected: ${JSON.stringify(v.error)}`);
      process.exit(1);
    }
    console.log(`⚡ sent via plain RPC: ${v?.result}`);
  } else {
    // The unauth lane 429s often — retry with backoff for up to ~60s.
    let attempt = 0;
    for (;;) {
      attempt += 1;
      try {
        bundleId = await sendBundle(blockEngine, [b64]);
        break;
      } catch (e) {
        const msg = e instanceof Error ? e.message : String(e);
        if (msg.includes('429') && attempt < 12) {
          console.log(`  [attempt ${attempt}] rate limited, retrying in 5s…`);
          await sleep(5000);
          continue;
        }
        throw new Error(`send bundle: ${msg}`);
      }
    }
    console.log(`⚡ submitted bundle ${bundleId} (attempt ${attempt})`);
  }

  // Poll until landed (or give up after ~90s).
  for (let i = 1; i <= 18; i++) {
    await sleep(5000);
    const status = mode === 'jito' ? ((await bundleStatus(blockEngine, bundleId)) ?? 'unknown') : 'n/a';
    const txMeta = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'getTransaction',
      params: [sig, { encoding: 'json', maxSupportedTransactionVersion: 0, commitment: 'confirmed' }],
    });
    const landed = txMeta?.result !== null && txMeta?.result !== undefined;
    console.log(`[${i * 5}s] jito_status=${status} on_chain=${landed}`);
    if (landed) {
      const meta = txMeta.result;
      console.log(`\n🎉 LANDED — slot ${meta?.slot} fee ${meta?.meta?.fee} lamports err ${JSON.stringify(meta?.meta?.err ?? null)}`);
      console.log(`https://solscan.io/tx/${sig}`);
      return;
    }
  }
  console.log(
    '\n⚠️ not seen on-chain after 90s — if MODE=rpc also fails, the tx itself is the problem; if only jito fails, raise TIP_LAMPORTS or the bundle path is at fault',
  );
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
