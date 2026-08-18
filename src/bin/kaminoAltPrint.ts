// Port of src/bin/kamino_alt_print.rs
//
// Emit the address set for the Kamino liquidation ALT: fixed accounts
// (programs, sysvars, main market + authority + scope, USDC repay-reserve set,
// JupLend flash-loan constants, wallet + USDC ATA) plus the TOP-K collateral
// reserves by deposit frequency, each with its 5 liquidate sub-accounts. This
// compresses the fire tx under 1232 bytes for the common collateral; rare
// collateral falls back to inline (executor logs + skips if it overflows).
//
// Setup (one-time):
//   solana address-lookup-table create --keypair ~/arb-keypair.json -u <rpc>
//   solana address-lookup-table extend <TABLE> --addresses "$(tsx src/bin/kaminoAltPrint.ts | paste -sd, -)" …
//
// Usage: HELIUS_RPC=<url> [AUTHORITY=<pk>] [TOP_K=20] tsx src/bin/kaminoAltPrint.ts

import 'dotenv/config';
import { PublicKey } from '@solana/web3.js';
import { decodeObligation, OBLIGATION_SIZE, RESERVE_SIZE } from '../lib/kamino.js';
import { lendingMarketAuthority, ReserveAccounts } from '../lib/kaminoIx.js';
import { ataFor } from '../lib/flashloan.js';

const KLEND = 'KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD';
const MAIN_MARKET = '7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF';
const USDC_MINT = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v';
const DEFAULT_AUTHORITY = 'DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak';

// JupLend flash-loan constants (from flashloan.rs).
const JUP_LEND_PROGRAM = 'jupgfSgfuAXv4B6R2Uxu85Z1qdzgju79s6MfZekN6XS';
const JUP_M = [
  'ALXWtv2P4GqH1B7Lq731joag52yRBRqmHV4naiXPTYWL',
  '94vK29npVbyRHXH63rRcTiSr26SFhrQTzbpNJuhQEDu',
  'J9dyC4pBTBPvzzPh7J9rhFhg8RvgerDNKkUH9kEwGMsj',
  '5pjzT5dFTsXcwixoab1QDLvZQvpYJxJeBphkyfHGn688',
  'BmkUoKMFYBxNSzWXyUjyMJjMAaVz4d8ZnxwwmhDCUXFB',
  '7s1da8DduuBFqGra5bJBjpnvL5E9mGzCuMk1Qkh4or2Z',
  'jupeiUmn818Jg1ekPURTpr4mFo29p46vygyykFJ3wZC',
];

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

// eslint-disable-next-line @typescript-eslint/no-explicit-any
function b64(data: any): Buffer | undefined {
  const s = data?.[0];
  if (typeof s !== 'string') return undefined;
  try {
    return Buffer.from(s, 'base64');
  } catch {
    return undefined;
  }
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (!endpoint) throw new Error('HELIUS_RPC');
  const authority = process.env.AUTHORITY ?? DEFAULT_AUTHORITY;
  const topK = Number.parseInt(process.env.TOP_K ?? '', 10) || 20;
  const market = new PublicKey(MAIN_MARKET);
  const authPk = new PublicKey(authority);
  const usdc = new PublicKey(USDC_MINT);

  // Rank collateral reserves by deposit frequency across main-market obligations.
  const resp = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getProgramAccounts',
    params: [
      KLEND,
      {
        encoding: 'base64',
        dataSlice: { offset: 0, length: 2288 },
        filters: [{ dataSize: OBLIGATION_SIZE }, { memcmp: { offset: 32, bytes: MAIN_MARKET } }],
      },
    ],
  });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const entries: any[] = Array.isArray(resp?.result) ? resp.result : [];
  const freq = new Map<string, number>();
  for (const e of entries) {
    const bytes = e?.account?.data !== undefined ? b64(e.account.data) : undefined;
    if (bytes === undefined) continue;
    const ob = decodeObligation(bytes);
    if (ob === null) continue;
    for (const [r] of ob.deposits) {
      const key = r.toBase58();
      freq.set(key, (freq.get(key) ?? 0) + 1);
    }
  }
  const ranked = Array.from(freq.entries()).sort((a, b) => b[1] - a[1]);
  const top = ranked.slice(0, topK).map(([r]) => new PublicKey(r));
  console.error(`[alt] ${entries.length} obligations, top ${top.length} collateral reserves by deposit count`);
  for (const [r, n] of ranked.slice(0, topK)) console.error(`  ${r} : ${n}`);

  // Fixed accounts.
  const addrs: string[] = [
    KLEND,
    'FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr',
    'JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4',
    JUP_LEND_PROGRAM,
    'TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA',
    'TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb',
    'ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL',
    '11111111111111111111111111111111',
    'ComputeBudget111111111111111111111111111111',
    'Sysvar1nstructions1111111111111111111111111',
    MAIN_MARKET,
    lendingMarketAuthority(market).toBase58(),
    USDC_MINT,
    authority,
    // USDC ATA (repay source + swap out).
    ataFor(authPk, usdc, new PublicKey('TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA')).toBase58(),
  ];
  addrs.push(...JUP_M);

  // Find the main-market USDC reserve (always the v1 repay side).
  const usdcResp = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getProgramAccounts',
    params: [
      KLEND,
      {
        encoding: 'base64',
        filters: [
          { dataSize: RESERVE_SIZE },
          { memcmp: { offset: 32, bytes: MAIN_MARKET } },
          { memcmp: { offset: 128, bytes: USDC_MINT } },
        ],
      },
    ],
  });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const usdcEntries: any[] = Array.isArray(usdcResp?.result) ? usdcResp.result : [];
  const usdcReservePkStr: string | undefined = usdcEntries[0]?.pubkey;
  if (!usdcReservePkStr) throw new Error('USDC reserve not found');
  const usdcReserve = new PublicKey(usdcReservePkStr);
  console.error(`[alt] USDC repay reserve: ${usdcReserve.toBase58()}`);

  // USDC repay-reserve + top collateral reserves, each with its 5 sub-accounts.
  const reservePks = [usdcReserve, ...top];
  const strs = reservePks.map((k) => k.toBase58());
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getMultipleAccounts',
    params: [strs, { encoding: 'base64' }],
  });
  const values: unknown[] = Array.isArray(v?.result?.value) ? v.result.value : [];
  for (let i = 0; i < values.length; i++) {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const acc = values[i] as any;
    const data = acc?.data !== undefined ? b64(acc.data) : undefined;
    if (data === undefined) continue;
    const r = ReserveAccounts.decode(reservePks[i], data);
    if (r === null) continue;
    for (const a of [r.reserve, r.liquidityMint, r.liquiditySupply, r.feeReceiver, r.collateralMint, r.collateralSupply, r.scopePrices]) {
      addrs.push(a.toBase58());
    }
  }

  // Dedup, preserve order.
  const seen = new Set<string>();
  for (const a of addrs) {
    if (seen.has(a)) continue;
    seen.add(a);
    console.log(a);
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
