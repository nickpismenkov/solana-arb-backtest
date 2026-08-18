// Port of src/bin/mfi_pyth_decode.rs
//
// Capture a REAL marginfi liquidation that embeds a Pyth price update, and
// dump the Pyth-receiver instruction(s) — program, discriminator, accounts,
// data length — so the embedded-update builder is derived from observed
// mainnet truth (the marginfi/Kamino lesson). Top liquidators post the fresh
// Pyth price IN THEIR OWN TX (post_update / post_update_atomic) right before
// the liquidate, so they don't wait for anyone's crank. This finds an example
// to copy.
//
// Usage: HELIUS_RPC=<url> [SAMPLES=3] [LIMIT=8000] npx tsx src/bin/mfiPythDecode.ts

import 'dotenv/config';
import bs58 from 'bs58';

const MARGINFI = 'MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA';
const PYTH_RECEIVER = 'rec5EKMGg6MxZYaMdyBfgwp4d5rB9T1VQH5pJv5LtFJ';
// LendingAccountLiquidate disc.
const DISC_LIQ = Buffer.from([214, 169, 151, 213, 251, 167, 86, 219]);

async function rpc(endpoint: string, body: unknown): Promise<any | null> {
  for (let attempt = 0; attempt < 4; attempt++) {
    try {
      const resp = await fetch(endpoint, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(body),
      });
      return await resp.json();
    } catch {
      /* retry */
    }
    await new Promise((r) => setTimeout(r, 400 << attempt));
  }
  return null;
}

function decodeBs58(s: string | undefined): Buffer {
  if (!s) return Buffer.alloc(0);
  try {
    return Buffer.from(bs58.decode(s));
  } catch {
    return Buffer.alloc(0);
  }
}

function dumpIx(label: string, ix: any): void {
  const data = decodeBs58(ix?.data);
  const discSlice = data.subarray(0, Math.min(8, data.length));
  console.log(
    `  ${label}: prog=${ix?.programId ?? '?'} disc=[${[...discSlice].map((b) => `0x${b.toString(16).padStart(2, '0')}`).join(', ')}] data_len=${data.length}`,
  );
  const accounts: any[] = Array.isArray(ix?.accounts) ? ix.accounts : [];
  accounts.forEach((a, i) => {
    console.log(`    [${String(i).padStart(2, ' ')}] ${typeof a === 'string' ? a : '?'}`);
  });
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (!endpoint) throw new Error('HELIUS_RPC');
  const samples = Number.parseInt(process.env.SAMPLES ?? '', 10) || 3;
  const limit = Number.parseInt(process.env.LIMIT ?? '', 10) || 8000;

  // Page marginfi signatures back until we have `limit`.
  const sigs: string[] = [];
  let before: string | undefined;
  while (sigs.length < limit) {
    const params: Record<string, unknown> = { limit: 1000 };
    if (before !== undefined) params.before = before;
    const resp = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'getSignaturesForAddress',
      params: [MARGINFI, params],
    });
    const page: any[] = Array.isArray(resp?.result) ? resp.result : [];
    if (page.length === 0) break;
    const last = page[page.length - 1];
    before = typeof last?.signature === 'string' ? last.signature : undefined;
    for (const e of page) {
      if (e?.err === null && typeof e?.signature === 'string') sigs.push(e.signature);
    }
    console.error(`[pyth] paged: ${sigs.length} sigs`);
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
    if (tx === null) continue;
    const result = tx?.result;
    if (result === null || result === undefined) continue;

    const all: Array<[string, any]> = [];
    const topIxs: any[] = Array.isArray(result?.transaction?.message?.instructions) ? result.transaction.message.instructions : [];
    topIxs.forEach((ix, ti) => all.push([`top[${ti}]`, ix]));
    const innerIxs: any[] = Array.isArray(result?.meta?.innerInstructions) ? result.meta.innerInstructions : [];
    for (const inner of innerIxs) {
      const p = typeof inner?.index === 'number' ? inner.index : 0;
      const ixs: any[] = Array.isArray(inner?.instructions) ? inner.instructions : [];
      ixs.forEach((ix, ii) => all.push([`inner[${p}.${ii}]`, ix]));
    }
    const hasLiq = all.some(([, ix]) => {
      if (ix?.programId !== MARGINFI) return false;
      const d = decodeBs58(ix?.data);
      return d.length >= 8 && d.subarray(0, 8).equals(DISC_LIQ);
    });
    const hasPyth = all.some(([, ix]) => ix?.programId === PYTH_RECEIVER);
    if (!(hasLiq && hasPyth)) {
      await new Promise((r) => setTimeout(r, 50));
      continue;
    }

    found += 1;
    console.log(`\n════ marginfi liq + Pyth update #${found}: ${sig}`);
    console.log(`  fee payer: ${result?.transaction?.message?.accountKeys?.[0]?.pubkey}`);
    for (const [label, ix] of all) {
      if (ix?.programId === PYTH_RECEIVER) {
        dumpIx(`${label} PYTH`, ix);
      } else if (ix?.programId === MARGINFI) {
        const data = decodeBs58(ix?.data);
        if (data.length >= 8 && data.subarray(0, 8).equals(DISC_LIQ)) {
          console.log(`  ${label} MARGINFI_LIQUIDATE`);
        }
      }
    }
    await new Promise((r) => setTimeout(r, 50));
  }
  if (found === 0) console.log(`no marginfi-liq-with-embedded-Pyth-update found in ${sigs.length} sigs — raise LIMIT`);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
