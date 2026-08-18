// Port of src/bin/save_liq_census.rs
//
// Ground truth: how many liquidations ACTUALLY happened on Save/Solend in the
// recent window, and how many were in OUR scope (v1: single-collateral,
// single-USDC-debt)? If real in-scope liquidations happened and our bot fired
// zero, that's a bug/miss — not "no opportunity." Scans the program's recent
// liquidate txs (tag 12/17), extracts repay reserve (debt) + withdraw reserve
// (collateral) from the ix accounts, and tallies USDC-debt vs other.
//
// Usage: HELIUS_RPC=<url> [PAGES=6] npx tsx src/bin/saveLiqCensus.ts

import 'dotenv/config';
import bs58 from 'bs58';

const SOLEND_PROGRAM = 'So1endDq2YkqhipRh3WViPa8hdiSpxWy6z3Z6tMCpAo';
const USDC_RESERVE = 'BgxfHJDzm44T7XG68MYKx7YisTjZu73tVovyZSjJMpmw';

async function rpc(endpoint: string, body: unknown): Promise<any | null> {
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
    await new Promise((r) => setTimeout(r, 400 << attempt));
  }
  return null;
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (endpoint === undefined) throw new Error('HELIUS_RPC');
  const pages = Number.parseInt(process.env.PAGES ?? '', 10) || 6;

  // Page recent program signatures.
  const sigs: Array<[string, number | null]> = [];
  let before: string | undefined;
  for (let i = 0; i < pages; i++) {
    const params: any = { limit: 1000 };
    if (before !== undefined) params.before = before;
    const resp = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'getSignaturesForAddress',
      params: [SOLEND_PROGRAM, params],
    });
    const page: any[] = resp?.result ?? [];
    if (page.length === 0) break;
    before = page[page.length - 1]?.signature;
    for (const e of page) {
      if (e?.err === null) {
        sigs.push([typeof e?.signature === 'string' ? e.signature : '', typeof e?.blockTime === 'number' ? e.blockTime : null]);
      }
    }
    console.error(`[census] paged ${sigs.length} sigs`);
  }
  const newest = sigs.length > 0 ? sigs[0][1] : null;
  const oldest = sigs.length > 0 ? sigs[sigs.length - 1][1] : null;
  const spanH = newest !== null && oldest !== null ? (newest - oldest) / 3600.0 : 0.0;

  let liqs = 0;
  let usdcDebt = 0;
  const collateralReserves = new Map<string, number>();
  const examples: string[] = [];
  for (const [sig] of sigs) {
    const tx = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'getTransaction',
      params: [sig, { encoding: 'jsonParsed', maxSupportedTransactionVersion: 0, commitment: 'confirmed' }],
    });
    const result = tx?.result;
    if (result === null || result === undefined) continue;
    const ixs: any[] = [...(result?.transaction?.message?.instructions ?? [])];
    for (const inner of result?.meta?.innerInstructions ?? []) {
      ixs.push(...(inner?.instructions ?? []));
    }
    for (const ix of ixs) {
      if (ix?.programId !== SOLEND_PROGRAM) continue;
      let data: Uint8Array;
      try {
        data = bs58.decode(typeof ix?.data === 'string' ? ix.data : '');
      } catch {
        continue;
      }
      const tag = data[0];
      if (tag === undefined) continue;
      if (tag !== 12 && tag !== 17) continue;
      liqs += 1;
      // tag 17 accounts: [3]=repay_reserve, [5]=withdraw_reserve.
      const accts: any[] = ix?.accounts ?? [];
      const repay = typeof accts[3] === 'string' ? accts[3] : '';
      const withdraw = typeof accts[5] === 'string' ? accts[5] : '';
      if (repay === USDC_RESERVE) usdcDebt += 1;
      collateralReserves.set(withdraw, (collateralReserves.get(withdraw) ?? 0) + 1);
      if (examples.length < 8) {
        const repayLabel = repay === USDC_RESERVE ? 'USDC' : repay.slice(0, Math.min(8, repay.length));
        const withdrawLabel = withdraw.slice(0, 8);
        examples.push(`${sig}  repay=${repayLabel} withdraw=${withdrawLabel}`);
      }
    }
    await new Promise((r) => setTimeout(r, 15));
  }

  console.log('\n═══ Save/Solend liquidation census ═══');
  console.log(`window: ${sigs.length} txs over ~${spanH.toFixed(1)} h`);
  console.log(`LIQUIDATIONS that actually happened: ${liqs}`);
  console.log(`  of which USDC-debt (our v1 scope for debt): ${usdcDebt}`);
  console.log(
    `  → est rate: ${(spanH > 0.0 ? liqs / spanH : 0.0).toFixed(1)} liquidations/hour, ${(spanH > 0.0 ? usdcDebt / spanH : 0.0).toFixed(1)} USDC-debt/hour`,
  );
  console.log('\ncollateral reserves seized (top):');
  const cr = Array.from(collateralReserves.entries()).sort((a, b) => b[1] - a[1]);
  for (const [r, n] of cr.slice(0, 8)) {
    console.log(`  ${n.toString().padStart(3, ' ')}  ${r.slice(0, 16)}`);
  }
  console.log('\nexamples:');
  for (const e of examples) console.log(`  ${e}`);
  if (usdcDebt === 0 && liqs > 0) {
    console.log('\n→ liquidations happened but NONE were USDC-debt — our v1 debt scope is the gap.');
  } else if (usdcDebt > 0) {
    console.log(
      `\n→ ${usdcDebt} USDC-debt liquidations happened that we should be able to fire — if we fired 0, investigate why (shape filter, sizing, or timing).`,
    );
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
