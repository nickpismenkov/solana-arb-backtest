// Port of src/bin/alt_deploy.rs
//
// Deploy an address-lookup-table (ALT) on-chain from a list of accounts on
// stdin — so the box doesn't need the `solana` CLI to create SAVE_ALT / JUP_ALT.
//
// Reads pubkeys (one per line) from stdin, creates a fresh lookup table owned by
// the KEYPAIR_PATH wallet, extends it with those addresses (batched to fit the
// 1232B tx limit), and prints the resulting table address.
//
// Pipe it from the *_alt_print bins:
//   LIVE=1 tsx src/bin/saveAltPrint.ts | tsx src/bin/altDeploy.ts
//   LIVE=1 tsx src/bin/jupAltPrint.ts  | tsx src/bin/altDeploy.ts
// then `export SAVE_ALT=<printed table>` (or JUP_ALT=…).
//
// SAFETY: DRY-RUN by default — it only prints the plan. Set LIVE=1 to submit
// real txs (creates on-chain state + spends a little SOL for rent + fees, signed
// by KEYPAIR_PATH). Uses the official solana-address-lookup-table-interface
// instruction builders, not a hand-rolled format.
//
// Env: HELIUS_RPC|RPC_HTTP|RPC_ENDPOINT, KEYPAIR_PATH, [LIVE=1], [CU_PRICE=<micro-lamports>]

import 'dotenv/config';
import { readFileSync } from 'node:fs';
import readline from 'node:readline';
import {
  AddressLookupTableProgram,
  ComputeBudgetProgram,
  Keypair,
  PublicKey,
  TransactionMessage,
  VersionedTransaction,
} from '@solana/web3.js';

/** Addresses per extend tx. Each pubkey is 32B of ix data; 20 keeps the tx well
 * under the 1232B single-packet limit with room for the header + signature. */
const EXTEND_BATCH = 20;

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

async function latestBlockhash(endpoint: string): Promise<string> {
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getLatestBlockhash',
    params: [{ commitment: 'finalized' }],
  });
  const bh = v?.result?.value?.blockhash;
  if (typeof bh !== 'string') throw new Error('getLatestBlockhash');
  return bh;
}

function cuLimitIx(units: number) {
  return ComputeBudgetProgram.setComputeUnitLimit({ units });
}
function cuPriceIx(microLamports: number) {
  return ComputeBudgetProgram.setComputeUnitPrice({ microLamports });
}

/**
 * Build a v0 tx from `ixs`, sign with `kp`, submit, and poll until confirmed.
 * Returns the signature. Exits the process on rejection or confirm timeout.
 */
async function sendAndConfirm(endpoint: string, kp: Keypair, ixs: any[], label: string): Promise<string> {
  const bh = await latestBlockhash(endpoint);
  const msg = new TransactionMessage({
    payerKey: kp.publicKey,
    recentBlockhash: bh,
    instructions: ixs,
  }).compileToV0Message();
  const tx = new VersionedTransaction(msg);
  tx.sign([kp]);
  const b64 = Buffer.from(tx.serialize()).toString('base64');

  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'sendTransaction',
    params: [b64, { encoding: 'base64', skipPreflight: false, preflightCommitment: 'confirmed', maxRetries: 5 }],
  });
  if (v === undefined) throw new Error('sendTransaction');
  if (v?.error != null) {
    console.error(`⛔ ${label}: sendTransaction rejected: ${JSON.stringify(v.error)}`);
    process.exit(1);
  }
  const txSig: string = v.result;
  console.log(`  ${label}: submitted ${txSig} — confirming…`);

  const start = Date.now();
  while (Date.now() - start < 45_000) {
    await sleep(1500);
    const s = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'getSignatureStatuses',
      params: [[txSig], { searchTransactionHistory: false }],
    });
    if (s === undefined) continue;
    const st = s?.result?.value?.[0];
    if (st === null || st === undefined) continue;
    if (st?.err !== null && st?.err !== undefined) {
      console.error(`⛔ ${label}: tx failed on-chain: ${JSON.stringify(st.err)}`);
      process.exit(1);
    }
    const cs = st?.confirmationStatus ?? '';
    if (cs === 'confirmed' || cs === 'finalized') {
      console.log(`  ${label}: ✅ ${cs}`);
      return txSig;
    }
  }
  console.error(`⛔ ${label}: not confirmed within 45s (sig ${txSig}); check explorer before retrying to avoid a duplicate table`);
  process.exit(1);
}

async function readStdin(): Promise<string> {
  const rl = readline.createInterface({ input: process.stdin, terminal: false });
  const lines: string[] = [];
  for await (const line of rl) {
    lines.push(line);
  }
  return lines.join('\n');
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP ?? process.env.RPC_ENDPOINT;
  if (endpoint === undefined) throw new Error('set HELIUS_RPC / RPC_HTTP / RPC_ENDPOINT');
  const keypairPath = process.env.KEYPAIR_PATH;
  if (keypairPath === undefined) throw new Error('set KEYPAIR_PATH');
  const live = process.env.LIVE === '1';
  const cuPrice = Number.parseInt(process.env.CU_PRICE ?? '', 10) || 100_000;

  const bytes: number[] = JSON.parse(readFileSync(keypairPath, 'utf8'));
  const kp = Keypair.fromSecretKey(new Uint8Array(bytes));
  const authority = kp.publicKey;

  // Read addresses from stdin (one per line). Non-parseable lines are skipped
  // so piping a print bin's stdout (addresses only; notes go to stderr) is clean.
  const input = await readStdin();
  const seen = new Set<string>();
  const addrs: PublicKey[] = [];
  for (const l of input.split('\n')) {
    const trimmed = l.trim();
    if (trimmed.length === 0) continue;
    let pk: PublicKey;
    try {
      pk = new PublicKey(trimmed);
    } catch {
      continue;
    }
    const s = pk.toBase58();
    if (seen.has(s)) continue;
    seen.add(s);
    addrs.push(pk);
  }
  if (addrs.length === 0) {
    console.error('⛔ no valid pubkeys on stdin — pipe from save_alt_print / jup_alt_print');
    process.exit(1);
  }

  // Recent (finalized) slot for the CreateLookupTable derivation.
  const slotV = await rpc(endpoint, { jsonrpc: '2.0', id: 1, method: 'getSlot', params: [{ commitment: 'finalized' }] });
  const slot: number | undefined = slotV?.result;
  if (slot === undefined) throw new Error('getSlot');
  const [createIx, table] = AddressLookupTableProgram.createLookupTable({
    authority,
    payer: authority,
    recentSlot: slot,
  });
  const batches = Math.ceil(addrs.length / EXTEND_BATCH);

  console.log('ALT deploy plan:');
  console.log(`  authority/payer : ${authority.toBase58()}`);
  console.log(`  addresses       : ${addrs.length}`);
  console.log(`  recent slot     : ${slot}`);
  console.log(`  table address   : ${table.toBase58()}`);
  console.log(`  txs             : 1 create + ${batches} extend (batches of ${EXTEND_BATCH})`);

  if (!live) {
    console.log('\nDRY RUN — nothing submitted. Set LIVE=1 to deploy for real.');
    console.log('NOTE: the table address above is derived from the current slot and will DIFFER on the live run;');
    console.log('      the real address is printed at the end of the LIVE run.');
    return;
  }

  console.log('\nLIVE — submitting…');
  await sendAndConfirm(endpoint, kp, [cuLimitIx(60_000), cuPriceIx(cuPrice), createIx], 'create');
  for (let i = 0; i * EXTEND_BATCH < addrs.length; i++) {
    const chunk = addrs.slice(i * EXTEND_BATCH, (i + 1) * EXTEND_BATCH);
    const ix = AddressLookupTableProgram.extendLookupTable({
      payer: authority,
      authority,
      lookupTable: table,
      addresses: chunk,
    });
    await sendAndConfirm(endpoint, kp, [cuLimitIx(60_000), cuPriceIx(cuPrice), ix], `extend ${i + 1}/${batches}`);
  }

  console.log(`\n✅ ALT deployed with ${addrs.length} addresses.`);
  console.log(`   table = ${table.toBase58()}`);
  console.log(`   → export the matching var, e.g.  export SAVE_ALT=${table.toBase58()}   (or JUP_ALT=${table.toBase58()})`);
  console.log('   (an ALT needs ~1 slot to warm up before it can be used in a tx.)');
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
