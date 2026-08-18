// Port of src/bin/kamino_liq_probe.rs
//
// Kamino liquidation WIRING probe — assembles the real 3-ix sequence
// (refresh_reserve x2 + refresh_obligation + liquidate_and_redeem_v2) against
// the most-underwater live main-market obligation and simulates it (no send,
// no money). Classifies by instruction INDEX:
//   err null                              -> fully liquidatable, whole seq runs
//   revert at the LIQUIDATE ix            -> wiring OK, guard/health rejected
//                                            (expected while 0 real liquidatable)
//   revert at an earlier ix               -> refresh/account wiring bug
//
// Uses the liquidator's existing USDC ATA as the repay source and the wSOL /
// collateral-mint ATA as the destination (created idempotently in the real
// fire path; here we just need the accounts to exist for the account list).
//
// Usage: HELIUS_RPC=<url> [AUTHORITY=<pk>] tsx src/bin/kaminoLiqProbe.ts

import 'dotenv/config';
import { PublicKey, TransactionMessage, VersionedTransaction } from '@solana/web3.js';
import { ataFor } from '../lib/flashloan.js';
import { decodeObligation, decodeReserve, obligationRatio, OBLIGATION_SIZE, type Obligation } from '../lib/kamino.js';
import { liquidateAndRedeemV2, refreshObligation, refreshReserve, ReserveAccounts } from '../lib/kaminoIx.js';

const KLEND = 'KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD';
const MAIN_MARKET = '7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF';
const TOKEN_PROGRAM = 'TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA';
const DEFAULT_AUTHORITY = 'DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak';

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

async function getMultiple(endpoint: string, keys: PublicKey[]): Promise<Map<string, Buffer>> {
  const out = new Map<string, Buffer>();
  for (let i = 0; i < keys.length; i += 100) {
    const chunk = keys.slice(i, i + 100);
    const strs = chunk.map((k) => k.toBase58());
    const v = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'getMultipleAccounts',
      params: [strs, { encoding: 'base64' }],
    });
    const arr: unknown[] = Array.isArray(v?.result?.value) ? v.result.value : [];
    arr.forEach((acc, j) => {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const bytes = (acc as any)?.data !== undefined ? b64((acc as any).data) : undefined;
      if (bytes !== undefined) out.set(chunk[j].toBase58(), bytes);
    });
  }
  return out;
}

async function mintOwner(endpoint: string, mint: PublicKey): Promise<PublicKey> {
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getAccountInfo',
    params: [mint.toBase58(), { encoding: 'base64' }],
  });
  const owner: string | undefined = v?.result?.value?.owner;
  if (typeof owner === 'string') {
    try {
      return new PublicKey(owner);
    } catch {
      /* fall through */
    }
  }
  return new PublicKey(TOKEN_PROGRAM);
}

function returnReason(code: number): string {
  if (code === 6017) return 'obligation healthy (not liquidatable)';
  return 'custom error past refresh — see logs';
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (!endpoint) throw new Error('HELIUS_RPC');
  const authority = new PublicKey(process.env.AUTHORITY ?? DEFAULT_AUTHORITY);
  const market = new PublicKey(MAIN_MARKET);

  // Scan main-market obligations (borrows present), pick the most underwater
  // by STORED health (fresh enough to be a real wiring target).
  console.error('[kliq] scanning main-market obligations …');
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
  console.error(`[kliq] ${entries.length} obligations`);
  let best: [PublicKey, Obligation, number] | undefined;
  for (const e of entries) {
    let pk: PublicKey;
    try {
      pk = new PublicKey(e?.pubkey);
    } catch {
      continue;
    }
    const bytes = e?.account?.data !== undefined ? b64(e.account.data) : undefined;
    if (bytes === undefined) continue;
    const ob = decodeObligation(bytes);
    if (ob === null) continue;
    if (ob.deposits.length !== 1 || ob.borrows.length !== 1 || ob.elevationGroup !== 0) continue;
    if (ob.unhealthyBorrowValue < 50.0) continue;
    const ratio = obligationRatio(ob);
    if (best === undefined || ratio > best[2]) best = [pk, ob, ratio];
  }
  if (best === undefined) {
    console.error('[kliq] no single-deposit/single-borrow obligation found');
    return;
  }
  const [obPk, ob, ratio] = best;
  console.error(
    `[kliq] target ${obPk.toBase58().slice(0, 8)} ratio ${ratio.toFixed(3)} deposit_reserve ${ob.deposits[0][0].toBase58().slice(0, 8)} borrow_reserve ${ob.borrows[0][0].toBase58().slice(0, 8)}`,
  );

  const withdrawReservePk = ob.deposits[0][0]; // collateral we seize
  const repayReservePk = ob.borrows[0][0]; // debt we repay
  const raw = await getMultiple(endpoint, [withdrawReservePk, repayReservePk]);
  const wrData = raw.get(withdrawReservePk.toBase58());
  const rrData = raw.get(repayReservePk.toBase58());
  const wr = wrData !== undefined ? ReserveAccounts.decode(withdrawReservePk, wrData) : null;
  const rr = rrData !== undefined ? ReserveAccounts.decode(repayReservePk, rrData) : null;
  if (wr === null || rr === null) {
    console.error('[kliq] reserve decode failed');
    return;
  }
  // Reserve for token-program + decimals of each side (decoded but unused
  // beyond the Rust `_wr_dec` binding, kept for parity / potential logging).
  void (wrData !== undefined ? decodeReserve(wrData)?.mintDecimals : undefined);

  const repayTp = await mintOwner(endpoint, rr.liquidityMint);
  const withdrawLiqTp = await mintOwner(endpoint, wr.liquidityMint);
  const collTp = await mintOwner(endpoint, wr.collateralMint);

  // ATAs (the fire path creates these idempotently; probe just references them).
  const userSourceLiquidity = ataFor(authority, rr.liquidityMint, repayTp); // repay from USDC ATA
  const userDestLiquidity = ataFor(authority, wr.liquidityMint, withdrawLiqTp); // seized underlying
  const userDestCollateral = ataFor(authority, wr.collateralMint, collTp);

  // 3-ix sequence.
  const ixs = [
    refreshReserve(rr),
    refreshReserve(wr),
    refreshObligation(market, obPk, [withdrawReservePk, repayReservePk]),
    liquidateAndRedeemV2(
      authority,
      obPk,
      market,
      rr,
      wr,
      userDestCollateral,
      userDestLiquidity,
      userSourceLiquidity,
      collTp,
      repayTp,
      withdrawLiqTp,
      1_000_000n,
      0n,
      0n,
    ),
  ];
  const LIQUIDATE_IX_INDEX = 3;

  const bhResp = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getLatestBlockhash',
    params: [{ commitment: 'finalized' }],
  });
  const blockhash: string | undefined = bhResp?.result?.value?.blockhash;
  if (!blockhash) throw new Error('no blockhash');
  const msg = new TransactionMessage({ payerKey: authority, recentBlockhash: blockhash, instructions: ixs }).compileToV0Message([]);
  const tx = new VersionedTransaction(msg);
  const b64tx = Buffer.from(tx.serialize()).toString('base64');

  console.error('[kliq] simulating refresh×2 + refresh_obligation + liquidate …');
  const sim = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'simulateTransaction',
    params: [b64tx, { sigVerify: false, replaceRecentBlockhash: true, commitment: 'processed', encoding: 'base64' }],
  });
  if (sim === undefined || sim?.result?.value === undefined) {
    console.log(`✗ RPC rejected simulation: ${JSON.stringify(sim)}`);
    return;
  }
  const res = sim.result.value;
  console.log('\n──── Kamino liquidation-wiring simulation ────');
  console.log(`err: ${JSON.stringify(res.err)}`);
  const ixIdx: number | undefined = res?.err?.InstructionError?.[0];
  const code: number | undefined = res?.err?.InstructionError?.[1]?.Custom;
  if (res.err === null) {
    console.log('★★ FULLY LIQUIDATABLE — whole sequence executes end to end');
  } else if (ixIdx === LIQUIDATE_IX_INDEX) {
    let why: string;
    if (code === 3012) {
      why = 'missing destination ATA (3012 AccountNotInitialized) — the fire path creates these; health gate PASSED';
    } else if (code !== undefined) {
      why = returnReason(code);
    } else {
      why = 'non-custom revert';
    }
    console.log(`★ WIRING OK — refresh×2 + refresh_obligation executed; liquidate reached account/health checks: ${why}. Account layout verified.`);
  } else if (ixIdx !== undefined) {
    console.log(`✗ reverted at ix ${ixIdx} (custom ${code ?? 'null'}) — refresh/account wiring bug:`);
    const logs: unknown[] = Array.isArray(res.logMessages) ? res.logMessages : [];
    for (const l of logs) console.log(`  ${typeof l === 'string' ? l : ''}`);
  } else {
    console.log(`? inconclusive: ${JSON.stringify(res.err)}`);
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
