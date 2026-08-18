// Port of src/bin/kamino_liq_decode.rs
//
// Capture REAL Kamino (KLend) liquidation transactions and dump the exact
// instruction sequence — account lists (resolved through ALTs via jsonParsed)
// and data bytes — for refresh_reserve / refresh_obligation /
// liquidate_obligation_and_redeem_reserve_collateral. The builders in
// kamino.ts are derived from THESE captured bytes, not from docs (the
// marginfi lesson: build from observed mainnet truth, verify by simulation).
//
// Usage: HELIUS_RPC=<url> [SAMPLES=3] [LIMIT=1000] tsx src/bin/kaminoLiqDecode.ts

import 'dotenv/config';
import bs58 from 'bs58';

const KLEND = 'KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD';
const DISC_LIQ_V1 = Buffer.from([177, 71, 154, 188, 226, 133, 74, 55]);
const DISC_LIQ_V2 = Buffer.from([162, 161, 35, 143, 30, 187, 185, 103]);

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
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

function decodeData(s: string | undefined): Buffer {
  if (!s) return Buffer.alloc(0);
  try {
    return Buffer.from(bs58.decode(s));
  } catch {
    return Buffer.alloc(0);
  }
}

function hex(b: Buffer): string {
  return `[${Array.from(b)
    .map((x) => `0x${x.toString(16).padStart(2, '0')}`)
    .join(', ')}]`;
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
function dumpIx(label: string, ix: any): void {
  const data = decodeData(ix?.data);
  const disc = data.subarray(0, Math.min(8, data.length));
  const rest = data.subarray(Math.min(8, data.length));
  console.log(`  ${label}: disc=${hex(disc)} data_len=${data.length} rest=${hex(rest)}`);
  const accounts: unknown[] = Array.isArray(ix?.accounts) ? ix.accounts : [];
  accounts.forEach((a, i) => {
    console.log(`    [${String(i).padStart(2, ' ')}] ${typeof a === 'string' ? a : '?'}`);
  });
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (!endpoint) throw new Error('HELIUS_RPC');
  const samples = Number.parseInt(process.env.SAMPLES ?? '', 10) || 3;
  const limit = Number.parseInt(process.env.LIMIT ?? '', 10) || 1000;

  // Page back until we have enough signatures (one page ~= a minute of KLend
  // activity; liquidations are ~1 per 5 min).
  const sigs: string[] = [];
  let before: string | undefined;
  while (sigs.length < limit) {
    const params: Record<string, unknown> = { limit: 1000 };
    if (before !== undefined) params.before = before;
    const resp = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'getSignaturesForAddress',
      params: [KLEND, params],
    });
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const page: any[] = Array.isArray(resp?.result) ? resp.result : [];
    if (page.length === 0) break;
    before = page[page.length - 1]?.signature;
    for (const e of page) {
      if (e?.err === null && typeof e?.signature === 'string') sigs.push(e.signature);
    }
    console.error(`[decode] paged: ${sigs.length} signatures`);
  }

  let found = 0;
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

    // Gather ALL KLend instructions in execution order: top-level + inner.
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const klendIxs: Array<[string, any]> = [];
    const topIxs: unknown[] = Array.isArray(result?.transaction?.message?.instructions)
      ? result.transaction.message.instructions
      : [];
    topIxs.forEach((ix, ti) => {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const ixAny = ix as any;
      if (ixAny?.programId === KLEND) klendIxs.push([`top[${ti}]`, ixAny]);
    });
    const inners: unknown[] = Array.isArray(result?.meta?.innerInstructions) ? result.meta.innerInstructions : [];
    for (const inner of inners) {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const innerAny = inner as any;
      const parent = innerAny?.index ?? 0;
      const innerIxs: unknown[] = Array.isArray(innerAny?.instructions) ? innerAny.instructions : [];
      innerIxs.forEach((ix, ii) => {
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        const ixAny = ix as any;
        if (ixAny?.programId === KLEND) klendIxs.push([`inner[${parent}.${ii}]`, ixAny]);
      });
    }
    const hasLiq = klendIxs.some(([, ix]) => {
      const data = decodeData(ix?.data);
      return data.length >= 8 && (data.subarray(0, 8).equals(DISC_LIQ_V1) || data.subarray(0, 8).equals(DISC_LIQ_V2));
    });
    if (!hasLiq) {
      await sleep(60);
      continue;
    }

    found += 1;
    console.log(`\n════ liquidation tx #${found}: ${sig}`);
    console.log(`  fee payer: ${result?.transaction?.message?.accountKeys?.[0]?.pubkey}`);
    for (const [label, ix] of klendIxs) {
      const data = decodeData(ix?.data);
      let name = '?';
      if (data.length >= 8) {
        const disc = data.subarray(0, 8);
        if (disc.equals(DISC_LIQ_V1)) name = 'LIQUIDATE_v1';
        else if (disc.equals(DISC_LIQ_V2)) name = 'LIQUIDATE_v2';
        else name = 'other';
      }
      dumpIx(`${label} ${name}`, ix);
    }
    await sleep(60);
  }
  if (found === 0) console.log(`no liquidation found in the last ${sigs.length} txs — raise LIMIT`);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
