// Port of src/bin/pyth_crank_probe.rs
//
// Simulate the FULL self-crank against the sponsored feed marginfi reads
// (step-4 gate): fetch a fresh Hermes update, build the two-tx crank
// (pyth_crank), simulateBundle [setup, fire] with skipSigVerify, and confirm
// the sponsored feed's publish_time ADVANCES past the live on-chain value.
// Read-only — nothing is submitted; the payer is a funded mainnet cranker
// (sim only needs its lamports), the buffer an ephemeral keypair.
//
// Usage: HELIUS_RPC=<url> [FEED=<hex feed id>] [PAYER=<pubkey>]
//        tsx src/bin/pythCrankProbe.ts

import 'dotenv/config';
import {
  ComputeBudgetProgram,
  Keypair,
  PublicKey,
  TransactionMessage,
  VersionedTransaction,
} from '@solana/web3.js';
import { fetchHermes } from '../lib/pythAccumulator.js';
import { buildCrankIxs, sponsoredFeed } from '../lib/pythCrank.js';
import { decodePriceUpdateV2 } from '../lib/liquidation.js';

// Canonical Pyth feed ids (hex).
const SOL = 'ef0d8b6fda2ceba41da15d4095d1da392a0d2f8ed0c6c7bc0f4cfac8c280b56d';
const USDC = 'eaa020c61cc479712813461ce153894a96a6c00b21ed0cfc2798d1f9a9e9c94a';
// A real, funded sponsored-feed cranker (payer for simulation only).
const DEFAULT_PAYER = '4p16wya1Vw2u9w22oah4yXQgySb6eWKRRLMsEXCreish';

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

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
    await sleep(400 << attempt);
  }
  return undefined;
}

function hex32(s: string): Buffer {
  const out = Buffer.alloc(32);
  for (let i = 0; i < 32; i++) {
    out[i] = Number.parseInt(s.slice(2 * i, 2 * i + 2), 16);
  }
  return out;
}

function cuLimitIx(units: number) {
  return ComputeBudgetProgram.setComputeUnitLimit({ units });
}

/** Unsigned v0 tx (placeholder sigs — simulateBundle runs skipSigVerify). */
function unsignedTx(payer: PublicKey, ixs: any[], numSigners: number): string {
  // Default (all-zero) Hash — matches Rust's `Hash::default()`; same 32
  // zero bytes as PublicKey.default, base58-encoded identically.
  const msg = new TransactionMessage({
    payerKey: payer,
    recentBlockhash: PublicKey.default.toBase58(),
    instructions: ixs,
  }).compileToV0Message([]);
  const tx = new VersionedTransaction(msg);
  tx.signatures = Array.from({ length: numSigners }, () => new Uint8Array(64));
  const raw = tx.serialize();
  console.error(`[crank] tx: ${raw.length}B (${ixs.length} ixs)`);
  return Buffer.from(raw).toString('base64');
}

async function feedState(endpoint: string, feed: PublicKey): Promise<[number, bigint] | undefined> {
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getAccountInfo',
    params: [feed.toBase58(), { encoding: 'base64' }],
  });
  const b64 = v?.result?.value?.data?.[0];
  if (typeof b64 !== 'string') return undefined;
  const bytes = Buffer.from(b64, 'base64');
  const decoded = decodePriceUpdateV2(bytes);
  if (decoded === null) return undefined;
  return [decoded.usdPrice, decoded.publishTime];
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (endpoint === undefined) throw new Error('HELIUS_RPC');
  const feedHex = process.env.FEED ?? SOL;
  const payer = new PublicKey(process.env.PAYER ?? DEFAULT_PAYER);
  const feedId = hex32(feedHex);

  // Where marginfi looks: shard-0 sponsored feed PDAs.
  const feedAcct = sponsoredFeed(0, feedId);
  console.log(`sponsored feed (shard 0): ${feedAcct.toBase58()}`);
  console.log(`  (USDC shard-0 ref: ${sponsoredFeed(0, hex32(USDC)).toBase58()})`);

  const pre = await feedState(endpoint, feedAcct);
  if (pre !== undefined) {
    console.log(`live feed:  price=$${pre[0].toFixed(4)} publish_time=${pre[1]}`);
  } else {
    console.log('live feed: <not found / undecodable>');
  }

  // Fresh Hermes update for this feed.
  const hermes = process.env.HERMES ?? 'https://hermes.pyth.network';
  const update = await fetchHermes(hermes, [feedHex]);
  console.log(`hermes: VAA ${update.vaa.length}B, ${update.updates.length} update(s)`);
  const mu = update.updates.find((u) => {
    const fid = u.feedId();
    return fid !== undefined && fid.equals(feedId);
  });
  if (mu === undefined) throw new Error('feed in blob');

  // Two-tx crank with an ephemeral buffer.
  const buffer = Keypair.generate();
  const ixs = buildCrankIxs(payer, buffer.publicKey, update.vaa, [mu], 0, 0);
  if (ixs === undefined) throw new Error('build crank ixs');
  const setup = [cuLimitIx(30_000), ...ixs.setup];
  const fire = [cuLimitIx(500_000), ...ixs.fire];
  const setupB64 = unsignedTx(payer, setup, 2); // payer + buffer keypair
  const fireB64 = unsignedTx(payer, fire, 1);

  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'simulateBundle',
    params: [
      { encodedTransactions: [setupB64, fireB64] },
      {
        skipSigVerify: true,
        replaceRecentBlockhash: true,
        preExecutionAccountsConfigs: [null, null],
        postExecutionAccountsConfigs: [null, { encoding: 'base64', addresses: [feedAcct.toBase58()] }],
      },
    ],
  });
  if (v === undefined) throw new Error('simulateBundle');
  if (v?.error != null) {
    console.log(`⛔ simulateBundle error: ${JSON.stringify(v.error)}`);
    process.exit(1);
  }
  const val = v?.result?.value;
  console.log(`simulateBundle summary: ${JSON.stringify(val?.summary)}`);
  let post: [number, bigint] | undefined;
  const txResults: any[] = Array.isArray(val?.transactionResults) ? val.transactionResults : [];
  txResults.forEach((r, i) => {
    console.log(`  tx[${i}] err=${JSON.stringify(r?.err)} cu=${r?.unitsConsumed}`);
    if (r?.err != null) {
      const logs: any[] = Array.isArray(r?.logs) ? r.logs : [];
      for (const l of logs) {
        console.log(`    ${typeof l === 'string' ? l : ''}`);
      }
    }
    const postAccts: any[] = Array.isArray(r?.postExecutionAccounts) ? r.postExecutionAccounts : [];
    for (const a of postAccts) {
      const s = a?.data?.[0];
      if (typeof s !== 'string') continue;
      let b: Buffer;
      try {
        b = Buffer.from(s, 'base64');
      } catch {
        continue;
      }
      const decoded = decodePriceUpdateV2(b);
      post = decoded !== null ? [decoded.usdPrice, decoded.publishTime] : undefined;
    }
  });

  if (pre !== undefined && post !== undefined) {
    const [, preTs] = pre;
    const [usd, ts] = post;
    console.log(`post-crank: price=$${usd.toFixed(4)} publish_time=${ts}`);
    const adv = ts - preTs;
    if (adv > 0n) {
      console.log(
        `★ CRANK VERIFIED — publish_time advanced ${adv}s past the live feed ($${pre[0].toFixed(4)}@${preTs} → $${usd.toFixed(4)}@${ts})`,
      );
    } else {
      console.log(`✗ publish_time did NOT advance (${preTs} → ${ts}) — feed already fresher than Hermes blob?`);
    }
  } else if (pre === undefined && post !== undefined) {
    const [usd, ts] = post;
    console.log(`post-crank: price=$${usd.toFixed(4)} publish_time=${ts} (no live baseline)`);
  } else {
    console.log('✗ no post-execution feed state returned — check simulateBundle output above');
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
