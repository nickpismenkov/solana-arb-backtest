// Port of src/bin/pyth_recv_decode.rs
//
// Decode a real Pyth receiver `post_update` / `post_update_atomic` instruction
// from mainnet to pin the exact discriminator, account layout, and data shape
// before building our own crank ix. Receiver traffic is constant, so a small
// scan finds examples fast. Also reports which target price-feed account each
// writes (to confirm we can crank marginfi's sponsored feed, e.g. Dpw1EAVr…).
//
// Usage: HELIUS_RPC=<url> [SAMPLES=4] [LIMIT=400] tsx src/bin/pythRecvDecode.ts

import 'dotenv/config';
import bs58 from 'bs58';

const PYTH_RECEIVER = 'rec5EKMGg6MxZYaMdyBfgwp4d5rB9T1VQH5pJv5LtFJ';

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

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (endpoint === undefined) throw new Error('HELIUS_RPC');
  const samples = Number.parseInt(process.env.SAMPLES ?? '', 10) || 4;
  const limit = Number.parseInt(process.env.LIMIT ?? '', 10) || 400;

  const sigsResp = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getSignaturesForAddress',
    params: [PYTH_RECEIVER, { limit }],
  });
  const sigsArr: any[] = Array.isArray(sigsResp?.result) ? sigsResp.result : [];
  const sigs: string[] = sigsArr
    .filter((e) => e?.err === null || e?.err === undefined)
    .map((e) => e?.signature)
    .filter((s): s is string => typeof s === 'string');
  console.error(`[recv] ${sigs.length} receiver signatures`);

  // disc → count
  const seen = new Map<string, number>();
  let dumped = 0;
  for (const sig of sigs) {
    if (dumped >= samples) break;
    const tx = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'getTransaction',
      params: [sig, { encoding: 'jsonParsed', maxSupportedTransactionVersion: 0, commitment: 'confirmed' }],
    });
    const result = tx?.result;
    if (result === null || result === undefined) continue;
    const ixs: any[] = Array.isArray(result?.transaction?.message?.instructions)
      ? [...result.transaction.message.instructions]
      : [];
    const innerGroups: any[] = Array.isArray(result?.meta?.innerInstructions) ? result.meta.innerInstructions : [];
    for (const inner of innerGroups) {
      const innerIxs: any[] = Array.isArray(inner?.instructions) ? inner.instructions : [];
      ixs.push(...innerIxs);
    }
    for (const ix of ixs) {
      if (ix?.programId !== PYTH_RECEIVER) continue;
      let data: Buffer;
      try {
        data = Buffer.from(bs58.decode(ix?.data ?? ''));
      } catch {
        data = Buffer.alloc(0);
      }
      if (data.length < 8) continue;
      const disc = data.subarray(0, 8).toString('hex');
      const n = (seen.get(disc) ?? 0) + 1;
      seen.set(disc, n);
      if (n === 1 && dumped < samples) {
        dumped += 1;
        console.log(`\n════ receiver ix disc=${disc}  data_len=${data.length}  sig=${sig}`);
        const accs: any[] = Array.isArray(ix?.accounts) ? ix.accounts : [];
        accs.forEach((a, i) => {
          console.log(`    [${String(i).padStart(2)}] ${typeof a === 'string' ? a : '?'}`);
        });
      }
    }
    await sleep(40);
  }
  console.log('\n──── disc histogram ────');
  const entries = Array.from(seen.entries());
  entries.sort((a, b) => b[1] - a[1]);
  for (const [d, n] of entries) {
    console.log(`  ${d}  ×${n}`);
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
