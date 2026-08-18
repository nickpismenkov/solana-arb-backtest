// Port of src/bin/clmm_probe.rs
//
// Verify the in-memory CLMM math against reality. For a range of USDC sizes,
// compare our `apply_swap` (USDC→base, per venue) against Jupiter's quote for
// that exact single-venue swap. Close match = our within-tick math is right
// and we can trust the profit optimiser. Divergence at larger sizes = where
// tick-crossing starts to matter (Stage 1b).
//
// Then print the current optimal cross-venue arb (size + exact profit) so we
// can see, live, whether SOL/USDC ever shows a positive number.
//
// Usage: RPC_ENDPOINT=<url> tsx src/bin/clmmProbe.ts

import 'dotenv/config';
import { applySwap, clmmFromOrca, clmmFromRay, optimalArb, uiPrice, wsol } from '../lib/clmm.js';
import { pair } from '../lib/pools.js';

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

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
    await sleep(300 << attempt);
  }
  return undefined;
}

async function accountData(endpoint: string, addr: string): Promise<Uint8Array> {
  const v = await rpc(endpoint, { jsonrpc: '2.0', id: 1, method: 'getAccountInfo', params: [addr, { encoding: 'base64' }] });
  const b64 = v?.result?.value?.data?.[0];
  if (typeof b64 !== 'string') throw new Error('data');
  return Uint8Array.from(Buffer.from(b64, 'base64'));
}

const USDC = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v';

/** Jupiter quote: exact-in USDC→SOL restricted to one DEX. Returns SOL out (raw lamports). */
async function jupQuote(baseMint: string, amountUsdcRaw: bigint, dex: string): Promise<number | undefined> {
  const url = `https://lite-api.jup.ag/swap/v1/quote?inputMint=${USDC}&outputMint=${baseMint}&amount=${amountUsdcRaw}&onlyDirectRoutes=true&swapMode=ExactIn&dexes=${dex}`;
  let v: any;
  try {
    const res = await fetch(url);
    v = await res.json();
  } catch (e) {
    console.error(`  (jup err: ${e})`);
    return undefined;
  }
  const out = v?.outAmount;
  if (typeof out !== 'string') return undefined;
  const n = Number.parseFloat(out);
  return Number.isFinite(n) ? n : undefined;
}

async function main(): Promise<void> {
  const endpoint = process.env.RPC_ENDPOINT;
  if (endpoint === undefined) throw new Error('RPC_ENDPOINT');
  const cfg = pair();
  const base = wsol();

  const od = await accountData(endpoint, cfg.orcaPool);
  const rd = await accountData(endpoint, cfg.rayPool);
  // Orca decimals by mint order: mintA@101.
  const mintAState = clmmFromOrca(od, 0, 0, cfg.orcaFeeBps);
  if (mintAState === undefined) throw new Error('orca state');
  const mintA = mintAState.mint0;
  const baseIsA = mintA.equals(base);
  const [oaDec0, oaDec1] = baseIsA ? [cfg.baseDec, cfg.quoteDec] : [cfg.quoteDec, cfg.baseDec];
  const orca = clmmFromOrca(od, oaDec0, oaDec1, cfg.orcaFeeBps);
  if (orca === undefined) throw new Error('orca state');
  const ray = clmmFromRay(rd, cfg.rayFeeBps);
  if (ray === undefined) throw new Error('ray state');

  console.log(`Orca  ui_price=${uiPrice(orca).toFixed(4)}  L=${orca.liquidity.toExponential(3)}  fee=${cfg.orcaFeeBps}bp`);
  console.log(`Ray   ui_price=${uiPrice(ray).toFixed(4)}  L=${ray.liquidity.toExponential(3)}  fee=${cfg.rayFeeBps}bp`);

  console.log('\n═══ apply_swap vs Jupiter (USDC→SOL, single venue) ═══');
  for (const usdc of [100.0, 500.0, 2000.0, 10000.0]) {
    const raw = BigInt(Math.round(usdc * 1e6));
    const usdcIs0Orca = !orca.mint0.equals(base);
    const oursOrca = applySwap(orca, usdcIs0Orca, usdc * 1e6) / 1e9;
    const jupOrca = await jupQuote(cfg.baseMint, raw, 'Whirlpool');
    const jupOrcaSol = jupOrca !== undefined ? jupOrca / 1e9 : undefined;
    const usdcIs0Ray = !ray.mint0.equals(base);
    const oursRay = applySwap(ray, usdcIs0Ray, usdc * 1e6) / 1e9;
    const jupRay = await jupQuote(cfg.baseMint, raw, 'Raydium%20CLMM');
    const jupRaySol = jupRay !== undefined ? jupRay / 1e9 : undefined;
    const err = (ours: number, jup: number | undefined): string =>
      jup !== undefined ? `jup=${jup.toFixed(6)} Δ=${(100.0 * ((ours - jup) / jup)).toFixed(3)}%` : 'jup=n/a';
    console.log(`  ${usdc.toFixed(0).padStart(7)} USDC | Orca ours=${oursOrca.toFixed(6)} ${err(oursOrca, jupOrcaSol)}`);
    console.log(`          | Ray  ours=${oursRay.toFixed(6)} ${err(oursRay, jupRaySol)}`);
    await sleep(400);
  }

  console.log('\n═══ optimal cross-venue arb RIGHT NOW ═══');
  const [size, profit, buyOrca] = optimalArb(orca, ray, base, 50_000.0 * 1e6);
  console.log(
    `  optimal borrow=${(size / 1e6).toFixed(1)} USDC → net profit=${(profit / 1e6).toFixed(4)} USDC, dir=${buyOrca ? 'buy-Orca/sell-Ray' : 'buy-Ray/sell-Orca'} (after ${cfg.roundTripFeeBps()}bp fees, before tip)`,
  );
  if (profit <= 0.0) {
    console.log('  → no profitable arb at this instant (expected on SOL/USDC)');
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
