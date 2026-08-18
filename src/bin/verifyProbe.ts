// Port of src/bin/verify_probe.rs
//
// Temporary local verification probe (not for prod):
// 1. send_bundle with a garbage tx → expect a 400 whose Jito response body is captured
// 2. getLatestBlockhash twice, 3s apart → expect different hashes

import 'dotenv/config';
import { defaultBlockEngine, getTipAccounts, sendBundle } from '../lib/jito.js';

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function latestBlockhash(endpoint: string): Promise<string | undefined> {
  const body = { jsonrpc: '2.0', id: 1, method: 'getLatestBlockhash', params: [{ commitment: 'confirmed' }] };
  try {
    const res = await fetch(endpoint, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body),
    });
    const v: any = await res.json();
    const bh = v?.result?.value?.blockhash;
    return typeof bh === 'string' ? bh : undefined;
  } catch {
    return undefined;
  }
}

async function main(): Promise<void> {
  const be = defaultBlockEngine();
  const rpc = process.env.RPC_ENDPOINT ?? 'https://api.mainnet-beta.solana.com';

  console.log('── 1. Jito connectivity (getTipAccounts) ──');
  try {
    const t = await getTipAccounts(be);
    console.log(`OK: ${t.length} tip accounts`);
  } catch (e) {
    console.log(`FAIL: ${e}`);
  }

  console.log('── 2. send_bundle with garbage tx → expect error WITH response body ──');
  try {
    const id = await sendBundle(be, ['aGVsbG8gd29ybGQ=']);
    console.log(`UNEXPECTED OK: ${id}`);
  } catch (e) {
    console.log(`error captured: ${e}`);
  }

  console.log('── 3. blockhash freshness (2 samples, 3s apart) ──');
  const a = await latestBlockhash(rpc);
  await sleep(3000);
  const b = await latestBlockhash(rpc);
  if (a !== undefined && b !== undefined && a !== b) {
    console.log(`OK: hashes differ (${a.slice(0, 8)}… vs ${b.slice(0, 8)}…)`);
  } else if (a !== undefined && b !== undefined) {
    console.log(`FAIL: identical hash after 3s (${a} == ${b})`);
  } else {
    console.log('FAIL: RPC call failed');
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
