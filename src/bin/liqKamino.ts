// Port of src/bin/liq_kamino.rs
//
// Kamino liquidatable-obligation finder (read-only, Stage 1 live test).
//
// Scans every klend Obligation, reads its STORED health values (no oracle
// needed — Kamino pre-computes them), and lists who is liquidatable
// (borrow_factor_adjusted_debt >= unhealthy_borrow_value), ranked by seizable
// collateral. Reports staleness: a "fresh" liquidatable obligation is a
// high-confidence opportunity; a "stale" one needs an on-chain refresh to
// confirm (its stored values predate the latest price).
//
// Usage: HELIUS_RPC=<url> [MARKET=<pubkey|all>] [MIN_COLLATERAL_USD=50]
//        [NEAR=25] [STALE_SLOTS=150] tsx src/bin/liqKamino.ts

import 'dotenv/config';
import { decodeObligation, KLEND_PROGRAM, obligationLiquidatable, obligationRatio, OBLIGATION_SIZE, type Obligation } from '../lib/kamino.js';

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
  if (!endpoint) throw new Error('HELIUS_RPC in .env');
  const market = process.env.MARKET ?? 'all';
  const minCollateral = Number.parseFloat(process.env.MIN_COLLATERAL_USD ?? '') || 50.0;
  const nearN = Number.parseInt(process.env.NEAR ?? '', 10) || 25;
  const staleSlots = BigInt(Number.parseInt(process.env.STALE_SLOTS ?? '', 10) || 150);

  // Current slot -> staleness age of each obligation.
  const slotResp = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getSlot',
    params: [{ commitment: 'confirmed' }],
  });
  const curSlot: bigint = typeof slotResp?.result === 'number' ? BigInt(slotResp.result) : 0n;

  // Obligations: dataSize filter, dataSlice trims to the fields we read.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const filters: any[] = [{ dataSize: OBLIGATION_SIZE }];
  if (market !== 'all') filters.push({ memcmp: { offset: 32, bytes: market } });
  console.error(`[kamino] getProgramAccounts (market=${market === 'all' ? 'all' : market.slice(0, 8)}) …`);
  const resp = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getProgramAccounts',
    params: [KLEND_PROGRAM, { encoding: 'base64', dataSlice: { offset: 0, length: 2272 }, filters }],
  });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const entries: any[] = Array.isArray(resp?.result) ? resp.result : [];
  console.error(`[kamino] ${entries.length} obligations, current slot ${curSlot}`);
  if (entries.length === 0) {
    console.error('[kamino] nothing returned — RPC must support getProgramAccounts');
    return;
  }

  const obs: Obligation[] = [];
  for (const e of entries) {
    const bytes = e?.account?.data !== undefined ? b64(e.account.data) : undefined;
    if (bytes === undefined) continue;
    const o = decodeObligation(bytes);
    if (o !== null && o.borrowedValue > 0.0) obs.push(o);
  }

  // Liquidatable, split by freshness, ranked by seizable collateral.
  const liq = obs.filter((o) => obligationLiquidatable(o) && o.depositedValue >= minCollateral);
  liq.sort((a, b) => b.depositedValue - a.depositedValue);
  const isFresh = (o: Obligation): boolean => !o.stale && curSlot - o.lastUpdateSlot <= staleSlots;
  const dust = obs.filter((o) => obligationLiquidatable(o) && o.depositedValue < minCollateral).length;

  console.log('\n════ Kamino liquidatable finder ════');
  console.log(`borrowers scanned:       ${obs.length}`);
  const freshLiq = liq.filter((o) => isFresh(o)).length;
  console.log(
    `LIQUIDATABLE (collateral ≥ $${minCollateral.toFixed(0)}): ${liq.length}   [${freshLiq} fresh, ${liq.length - freshLiq} stale, +${dust} dust]`,
  );
  for (const o of liq.slice(0, 50)) {
    const age = curSlot - o.lastUpdateSlot;
    const tag = isFresh(o) ? 'FRESH' : 'stale';
    console.log(
      `  ${tag} ${o.owner.toBase58().slice(0, 8)}…  collateral=$${o.depositedValue.toFixed(2)}  debt=$${o.bfAdjustedDebt.toFixed(2)}  thresh=$${o.unhealthyBorrowValue.toFixed(2)}  ratio=${obligationRatio(o).toFixed(4)}  (age ${age}sl)`,
    );
  }

  // Closest healthy obligations with real collateral — monitor candidates.
  const near = obs.filter((o) => !obligationLiquidatable(o) && o.depositedValue >= minCollateral && o.unhealthyBorrowValue > 0.0);
  near.sort((a, b) => obligationRatio(b) - obligationRatio(a));
  console.log('\nclosest to liquidation (debt/threshold → 1.0):');
  for (const o of near.slice(0, nearN)) {
    console.log(
      `  ${o.owner.toBase58().slice(0, 8)}…  ratio=${obligationRatio(o).toFixed(4)}  debt=$${o.bfAdjustedDebt.toFixed(2)}  thresh=$${o.unhealthyBorrowValue.toFixed(2)}  collateral=$${o.depositedValue.toFixed(2)}`,
    );
  }
  console.log();
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
