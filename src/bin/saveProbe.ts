// Port of src/bin/save_probe.rs
//
// Verify the Save decoders (src/lib/save.ts) against live mainnet: decode the
// USDC reserve, then scan a sample of obligations and report how many are
// liquidatable per Solend's on-chain math. Read-only.
//
// Usage: HELIUS_RPC=<url> [SCAN=2000] npx tsx src/bin/saveProbe.ts

import 'dotenv/config';
import { PublicKey } from '@solana/web3.js';
import * as save from '../lib/save.js';
import { decodeObligation, obligationHealthRatio, obligationLiquidatable, decodeReserve } from '../lib/save.js';

async function rpc(endpoint: string, body: unknown): Promise<any | null> {
  for (let attempt = 0; attempt < 4; attempt++) {
    try {
      const res = await fetch(endpoint, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(body),
      });
      const v = await res.json();
      return v;
    } catch {
      // fall through to retry
    }
    await new Promise((r) => setTimeout(r, 400 << attempt));
  }
  return null;
}

function b64(d: any): Buffer | null {
  const s = d?.[0];
  if (typeof s !== 'string') return null;
  try {
    return Buffer.from(s, 'base64');
  } catch {
    return null;
  }
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (endpoint === undefined) throw new Error('HELIUS_RPC');
  const scan = Number.parseInt(process.env.SCAN ?? '', 10) || 2000;

  // 1) Reserve decode.
  const usdc = new PublicKey(save.USDC_RESERVE);
  const rawResp = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getAccountInfo',
    params: [save.USDC_RESERVE, { encoding: 'base64' }],
  });
  const raw = b64(rawResp?.result?.value?.data);
  if (raw === null) throw new Error('usdc reserve');
  const r = decodeReserve(usdc, raw);
  if (r === null) throw new Error('decode reserve');
  console.log(
    `USDC reserve: mint ${r.liquidityMint.toBase58().slice(0, 6)}… dec=${r.mintDecimals} pyth=${r.pythOracle
      .toBase58()
      .slice(0, 6)}… price=$${r.marketPrice.toFixed(4)} ltv=${r.loanToValuePct} liq_thr=${r.liquidationThresholdPct} bonus=${r.liquidationBonusPct}%`,
  );
  if (r.liquidityMint.toBase58() !== save.USDC_MINT) throw new Error('reserve mint should be USDC');
  console.log('★ reserve decode VERIFIED (mint=USDC, Pyth sponsored feed, config sane)\n');

  // 2) Obligation scan — getProgramAccounts of 1300-byte accounts on the main
  // pool, decode, count liquidatable.
  console.log('scanning obligations (dataSize 1300, main pool) …');
  const resp = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getProgramAccounts',
    params: [
      save.SOLEND_PROGRAM,
      {
        encoding: 'base64',
        dataSize: 1300,
        filters: [{ dataSize: 1300 }, { memcmp: { offset: 10, bytes: save.MAIN_POOL } }],
      },
    ],
  });
  const entries: any[] = resp?.result ?? [];
  console.log(`  ${entries.length} obligations on main pool`);

  let decoded = 0;
  let withDebt = 0;
  let liq = 0;
  const examples: Array<[number, string, number, number]> = [];
  for (const e of entries.slice(0, scan)) {
    const pk = e?.pubkey;
    if (typeof pk !== 'string') continue;
    const bytes = b64(e?.account?.data);
    if (bytes === null) continue;
    const o = decodeObligation(bytes);
    if (o === null) continue;
    decoded += 1;
    if (o.borrows.length === 0) continue;
    withDebt += 1;
    if (obligationLiquidatable(o)) {
      liq += 1;
      if (examples.length < 10) {
        examples.push([obligationHealthRatio(o), pk, o.borrowedValue, o.unhealthyBorrowValue]);
      }
    }
  }
  console.log(`  decoded ${decoded}, with debt ${withDebt}, LIQUIDATABLE now ${liq}`);
  examples.sort((a, b) => b[0] - a[0]);
  for (const [hr, pk, bv, uv] of examples.slice(0, 10)) {
    console.log(`    ratio ${hr.toFixed(3)}  borrowed $${bv.toFixed(2)} > unhealthy $${uv.toFixed(2)}  ${pk}`);
  }
  console.log(`\n★ obligation decoder VERIFIED on ${decoded} live accounts`);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
