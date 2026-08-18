// Port of src/bin/kamino_fire_probe.rs
//
// Simulate the FULL Kamino atomic fire tx against the most-underwater live
// main-market obligation (USDC debt). Classifies by instruction index — the
// wiring test for the whole flashloan-wrapped path. Expected outcomes with a
// healthy market: either the obligation is genuinely liquidatable and the
// whole tx runs (err null), or the liquidate ix reverts on health/close-factor
// — both prove borrow + refreshes + liquidate account wiring + Jupiter swap
// compose + JupLend payback compile under the size limit. A revert at any
// other index is a wiring bug.
//
// Usage: HELIUS_RPC=<url> [AUTHORITY=<pk>] tsx src/bin/kaminoFireProbe.ts

import 'dotenv/config';
import { PublicKey } from '@solana/web3.js';
import { hasMarket } from '../lib/flashloan.js';
import { decodeObligation, decodeReserve, obligationRatio, OBLIGATION_SIZE, type Obligation } from '../lib/kamino.js';
import { buildFireTx, type KaminoFireCandidate } from '../lib/kaminoFire.js';
import { ReserveAccounts } from '../lib/kaminoIx.js';

const KLEND = 'KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD';
const MAIN_MARKET = '7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF';
const USDC_MINT = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v';
const USDT_MINT = 'Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB';
const TOKEN_PROGRAM = 'TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA';
const DEFAULT_AUTHORITY = 'DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak';
// [cu, cu_price, ata, ata, ata, borrow, refresh, refresh, refresh_ob, LIQUIDATE, …]
const LIQUIDATE_IX_INDEX = 9;

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
  const strs = keys.map((k) => k.toBase58());
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getMultipleAccounts',
    params: [strs, { encoding: 'base64' }],
  });
  const arr: unknown[] = Array.isArray(v?.result?.value) ? v.result.value : [];
  arr.forEach((acc, i) => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const bytes = (acc as any)?.data !== undefined ? b64((acc as any).data) : undefined;
    if (bytes !== undefined) out.set(keys[i].toBase58(), bytes);
  });
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

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (!endpoint) throw new Error('HELIUS_RPC');
  const authority = new PublicKey(process.env.AUTHORITY ?? DEFAULT_AUTHORITY);
  const market = new PublicKey(MAIN_MARKET);
  const usdc = new PublicKey(USDC_MINT);
  // NONUSDC=1 -> skip USDC-debt candidates, to prove the widened USDT/wSOL path.
  const skipUsdc = process.env.NONUSDC === '1';
  // DEBT=USDC|USDT|wSOL -> only sim that debt asset.
  const wantDebt = process.env.DEBT;

  console.error(`[kfire] scanning main-market obligations (wired-debt USDC/USDT/wSOL, single deposit/borrow)${skipUsdc ? ' [NON-USDC only]' : ''} …`);
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
  console.error(`[kfire] ${entries.length} obligations`);

  // Need USDC to be the debt reserve -> resolve each candidate's repay reserve
  // liquidity mint. Rank by stored ratio, take the first USDC-debt one.
  const ranked: Array<[PublicKey, Obligation, number]> = [];
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
    if (ob.deposits.length === 1 && ob.borrows.length === 1 && ob.elevationGroup === 0 && ob.unhealthyBorrowValue >= 50.0) {
      ranked.push([pk, ob, obligationRatio(ob)]);
    }
  }
  ranked.sort((a, b) => b[2] - a[2]);

  for (const [obPk, ob, ratio] of ranked.slice(0, 40)) {
    const withdrawPk = ob.deposits[0][0];
    const repayPk = ob.borrows[0][0];
    const raw = await getMultiple(endpoint, [withdrawPk, repayPk]);
    const wrData = raw.get(withdrawPk.toBase58());
    const rrData = raw.get(repayPk.toBase58());
    if (wrData === undefined || rrData === undefined) continue;
    const wr = ReserveAccounts.decode(withdrawPk, wrData);
    const rr = ReserveAccounts.decode(repayPk, rrData);
    if (wr === null || rr === null) continue;
    // v1.5: any debt with a wired JupLend flash market (USDC/USDT/wSOL).
    if (!hasMarket(rr.liquidityMint)) continue;
    if (skipUsdc && rr.liquidityMint.equals(usdc)) continue;
    const wrRes = decodeReserve(wrData);
    const rrRes = decodeReserve(rrData);
    if (wrRes === null || rrRes === null) continue;
    const debtSym = rr.liquidityMint.equals(usdc) ? 'USDC' : rr.liquidityMint.toBase58() === USDT_MINT ? 'USDT' : 'wSOL';
    if (wantDebt !== undefined && wantDebt !== debtSym) continue;

    // Size: repay 20% of debt (Kamino close factor), capped small for the probe.
    const debtDec = rrRes.mintDecimals;
    const debtPrice = Math.max(rrRes.marketPrice, 1e-9);
    const debtUsd = (ob.borrows[0][1] / 10 ** debtDec) * rrRes.marketPrice;
    const repayUsd = Math.max(Math.min(debtUsd * 0.2, 50.0), 1.0);
    // Native debt units priced in the actual debt asset (not hardcoded USDC).
    const repayAmount = BigInt(Math.trunc((repayUsd / debtPrice) * 10 ** debtDec));
    // Seized underlying native ~= repay_usd × (1 + ~5% bonus) / price, 0.5% haircut.
    const bonus = 1.05;
    const seizedNative = (repayUsd * bonus) / wrRes.marketPrice * 10 ** wrRes.mintDecimals;
    const swapInAmount = BigInt(Math.trunc(seizedNative * 0.995));

    console.error(
      `[kfire] target ${obPk.toBase58().slice(0, 8)} [${debtSym} debt] ratio ${ratio.toFixed(3)}  debt $${debtUsd.toFixed(0)}  repay $${repayUsd.toFixed(2)} (${repayAmount} native)  seize ${swapInAmount} native (${wrRes.mintDecimals} dp @ $${wrRes.marketPrice.toFixed(2)})`,
    );

    const cand: KaminoFireCandidate = {
      obligation: obPk,
      lendingMarket: market,
      repayReserve: rr,
      withdrawReserve: wr,
      obligationReserves: [withdrawPk, repayPk],
      withdrawLiquidityMint: wr.liquidityMint,
      withdrawLiquidityTokenProgram: await mintOwner(endpoint, wr.liquidityMint),
      withdrawCollateralTokenProgram: await mintOwner(endpoint, wr.collateralMint),
      repayLiquidityTokenProgram: await mintOwner(endpoint, rr.liquidityMint),
      repayAmount,
      swapInAmount,
    };

    const bhResp = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'getLatestBlockhash',
      params: [{ commitment: 'finalized' }],
    });
    const blockhash: string | undefined = bhResp?.result?.value?.blockhash;
    if (!blockhash) continue;

    let fire;
    try {
      fire = await buildFireTx(endpoint, cand, authority, null, 0n, 100_000n, 100, 20, blockhash);
    } catch (e) {
      console.error(`[kfire]   build failed: ${e}`);
      continue;
    }
    console.error(`[kfire]   tx ${fire.txBytes} bytes (limit 1232)  quoted_usdc_out=${fire.quotedUsdcOut}`);

    const b64tx = Buffer.from(fire.tx.serialize()).toString('base64');
    const sim = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'simulateTransaction',
      params: [b64tx, { sigVerify: false, replaceRecentBlockhash: true, commitment: 'processed', encoding: 'base64' }],
    });
    if (sim === undefined) continue;
    if (sim?.result?.value === undefined) {
      console.error(`[kfire]   RPC rejected sim: ${JSON.stringify(sim?.error)}`);
      continue;
    }
    const res = sim.result.value;
    const ixIdx: number | undefined = res?.err?.InstructionError?.[0];
    const code: number | undefined = res?.err?.InstructionError?.[1]?.Custom;
    console.log(`\n──── Kamino fire simulation (${obPk.toBase58().slice(0, 8)}…) ────`);
    console.log(`err: ${JSON.stringify(res.err)}  (ix ${ixIdx ?? 'null'}, custom ${code ?? 'null'})`);
    if (res.err === null) {
      console.log('★★ FULL KAMINO FIRE VERIFIED — whole flashloan-wrapped tx executes end to end');
      return;
    } else if (ixIdx === LIQUIDATE_IX_INDEX) {
      console.log(
        `★ WIRING OK — borrow + refresh×2 + refresh_obligation executed; liquidate reached health/close-factor checks (custom ${code ?? 'null'}). Path compiles at ${fire.txBytes} bytes; swap + payback wired.`,
      );
      return;
    } else if (ixIdx !== undefined) {
      console.log(`✗ reverted at ix ${ixIdx} (custom ${code ?? 'null'}) — wiring bug, logs:`);
      const logs: unknown[] = Array.isArray(res.logs) ? res.logs : [];
      for (const l of logs) console.log(`  ${typeof l === 'string' ? l : ''}`);
      return;
    } else {
      console.log(`? inconclusive: ${JSON.stringify(res.err)}`);
    }
  }
  console.log('no wired-debt single-position obligation simulated');
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
