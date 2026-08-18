// Port of src/bin/pyth_crank_decode.rs
//
// Dump a REAL sponsored-feed crank tx from mainnet so the crank builder is
// derived from observed truth (the marginfi/Kamino lesson): scan recent Pyth
// receiver txs for one that goes through the PUSH WRAPPER (program id starts
// "pythWSns" — the only writer of the shared sponsored feeds marginfi reads),
// then print the FULL instruction sequence — Wormhole encoded-VAA
// init/write/verify, the wrapper update — with full program ids, account
// lists (signer/writable flags), discriminators, and data hex. Also decodes
// the target PriceUpdateV2 feed (feed id, write_authority, publish_time).
//
// Usage: HELIUS_RPC=<url> [SAMPLES=2] [LIMIT=300] tsx src/bin/pythCrankDecode.ts

import 'dotenv/config';
import bs58 from 'bs58';
import { decodePriceUpdateV2 } from '../lib/liquidation.js';

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

function hexs(b: Buffer): string {
  return b.toString('hex');
}

function b64acc(v: any): Buffer | undefined {
  const s = v?.result?.value?.data?.[0];
  if (typeof s !== 'string') return undefined;
  try {
    return Buffer.from(s, 'base64');
  } catch {
    return undefined;
  }
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (endpoint === undefined) throw new Error('HELIUS_RPC');
  const samples = Number.parseInt(process.env.SAMPLES ?? '', 10) || 2;
  const limit = Number.parseInt(process.env.LIMIT ?? '', 10) || 300;

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
  console.error(`[crank] ${sigs.length} receiver signatures`);

  let found = 0;
  const feedsSeen = new Map<string, number>();
  for (const sig of sigs) {
    if (found >= samples) break;
    const tx = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'getTransaction',
      params: [sig, { encoding: 'jsonParsed', maxSupportedTransactionVersion: 0, commitment: 'confirmed' }],
    });
    const result = tx?.result;
    if (result === null || result === undefined) continue;

    const all: [string, any][] = [];
    const topIxs: any[] = Array.isArray(result?.transaction?.message?.instructions)
      ? result.transaction.message.instructions
      : [];
    topIxs.forEach((ix, ti) => all.push([`top[${ti}]`, ix]));
    const innerGroups: any[] = Array.isArray(result?.meta?.innerInstructions) ? result.meta.innerInstructions : [];
    for (const inner of innerGroups) {
      const p = inner?.index ?? 0;
      const innerIxs: any[] = Array.isArray(inner?.instructions) ? inner.instructions : [];
      innerIxs.forEach((ix, ii) => all.push([`inner[${p}.${ii}]`, ix]));
    }
    // Sponsored-feed cranks go through the push wrapper.
    const wrapper: string | undefined = all
      .map(([, ix]) => ix?.programId)
      .find((p): p is string => typeof p === 'string' && p.startsWith('pythWSns'));
    if (wrapper === undefined) {
      await sleep(30);
      continue;
    }

    found += 1;
    console.log(`\n════ sponsored crank #${found}: ${sig}`);
    console.log(`  push wrapper program: ${wrapper}`);
    console.log('  ── accountKeys (s=signer w=writable) ──');
    const accountKeys: any[] = Array.isArray(result?.transaction?.message?.accountKeys)
      ? result.transaction.message.accountKeys
      : [];
    accountKeys.forEach((k, i) => {
      console.log(
        `    [${String(i).padStart(2)}] ${k?.pubkey ?? '?'} ${k?.signer === true ? 's' : '-'}${k?.writable === true ? 'w' : '-'}`,
      );
    });
    console.log('  ── instruction sequence ──');
    for (const [label, ix] of all) {
      const prog = ix?.programId ?? '?';
      let data: Buffer;
      try {
        data = Buffer.from(bs58.decode(ix?.data ?? ''));
      } catch {
        data = Buffer.alloc(0);
      }
      const disc = hexs(data.subarray(0, Math.min(8, data.length)));
      console.log(`  ${label}: prog=${prog}`);
      console.log(`      disc=${disc} data_len=${data.length}`);
      // Full data hex for everything except huge VAA-write chunks (cap 96B shown).
      if (data.length <= 96) {
        console.log(`      data=${hexs(data)}`);
      } else {
        console.log(`      data[..96]=${hexs(data.subarray(0, 96))}…`);
      }
      const accs: any[] = Array.isArray(ix?.accounts) ? ix.accounts : [];
      accs.forEach((a, i) => {
        console.log(`      [${String(i).padStart(2)}] ${typeof a === 'string' ? a : '?'}`);
      });
    }
    // Target feed = writable non-signer account of the wrapper's ix that
    // decodes as PriceUpdateV2. Just decode every wrapper-ix account.
    for (const [, ix] of all.filter(([, ix]) => ix?.programId === wrapper)) {
      const accs: any[] = Array.isArray(ix?.accounts) ? ix.accounts : [];
      for (const a of accs) {
        const pk = typeof a === 'string' ? a : undefined;
        if (pk === undefined) continue;
        const info = await rpc(endpoint, {
          jsonrpc: '2.0',
          id: 1,
          method: 'getAccountInfo',
          params: [pk, { encoding: 'base64' }],
        });
        const bytes = b64acc(info);
        if (bytes === undefined) continue;
        const decoded = decodePriceUpdateV2(bytes);
        if (decoded !== null) {
          const wa = hexs(bytes.subarray(8, 40));
          const selfHex = hexs(Buffer.from(bs58.decode(pk)));
          console.log(`  ── target feed ${pk}`);
          console.log(`      feed_id=${hexs(decoded.feedId)} price=$${decoded.usdPrice.toFixed(4)} publish_time=${decoded.publishTime}`);
          console.log(`      write_authority==self: ${wa === selfHex}`);
          feedsSeen.set(pk, (feedsSeen.get(pk) ?? 0) + 1);
        }
      }
    }
    await sleep(40);
  }
  console.log('\n──── sponsored feeds seen ────');
  for (const [f, n] of feedsSeen) {
    console.log(`  ${f}  ×${n}`);
  }
  if (found === 0) console.log(`no push-wrapper crank in ${sigs.length} sigs — raise LIMIT`);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
