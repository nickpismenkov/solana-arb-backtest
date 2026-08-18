// Port of src/bin/mfi_liq_census.rs
//
// Census the accounts our emode-aware maintenanceHealth flags as liquidatable
// (ratio ≥ 1.0, on-chain prices), categorized by shape — to explain why the
// live engine still reports a large "liquidatable now" after the emode fix:
// are they FIREABLE v1 accounts (1 collateral / 1 USDC debt / crankable), or
// non-v1 accounts the fire path skips anyway (multi-position, non-USDC debt)?
//
// Usage: HELIUS_RPC=<url> [SAMPLE=5] npx tsx src/bin/mfiLiqCensus.ts

import 'dotenv/config';
import { PublicKey } from '@solana/web3.js';
import {
  decodeBank,
  decodeMarginfiAccount,
  decodeOraclePrice,
  decodePriceUpdateV2,
  maintenanceHealth,
  healthRatio,
  healthLiquidatable,
  MA_SIZE,
  type Bank,
  type BankMap,
  type MarginfiAccount,
  type PriceMap,
} from '../lib/liquidation.js';
import { USDC_BANK } from '../lib/marginfi.js';
import { sponsoredFeed } from '../lib/pythCrank.js';

const MARGINFI_PROGRAM = 'MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA';
const MARGINFI_GROUP = '4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8';

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

function b64(d: any): Buffer | null {
  const s = d?.[0];
  if (typeof s !== 'string') return null;
  try {
    return Buffer.from(s, 'base64');
  } catch {
    return null;
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
    if (v === null) continue;
    const arr: any[] = Array.isArray(v?.result?.value) ? v.result.value : [];
    arr.forEach((acc, idx) => {
      const b = b64(acc?.data);
      if (b) out.set(chunk[idx].toBase58(), b);
    });
  }
  return out;
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (!endpoint) throw new Error('HELIUS_RPC');
  const sample = Number.parseInt(process.env.SAMPLE ?? '', 10) || 5;
  const usdcBank = new PublicKey(USDC_BANK);

  const resp = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getProgramAccounts',
    params: [
      MARGINFI_PROGRAM,
      {
        encoding: 'base64',
        dataSlice: { offset: 0, length: 1736 },
        filters: [{ dataSize: MA_SIZE }, { memcmp: { offset: 8, bytes: MARGINFI_GROUP } }],
      },
    ],
  });
  if (resp === null) throw new Error('scan');
  const entries: any[] = Array.isArray(resp?.result) ? resp.result : [];
  const accts: Array<[PublicKey, MarginfiAccount]> = [];
  for (const e of entries) {
    const pkStr = e?.pubkey;
    if (typeof pkStr !== 'string') continue;
    const raw = b64(e?.account?.data);
    if (!raw) continue;
    let pk: PublicKey;
    try {
      pk = new PublicKey(pkStr);
    } catch {
      continue;
    }
    const a = decodeMarginfiAccount(raw);
    if (!a) continue;
    if (!a.balances.some((b) => b.liabilityShares > 0.0)) continue;
    accts.push([pk, a]);
  }

  const bankPkSet = new Map<string, PublicKey>();
  for (const [, a] of accts) for (const b of a.balances) bankPkSet.set(b.bankPk.toBase58(), b.bankPk);
  const bankPks = [...bankPkSet.values()];
  const bankRaw = await getMultiple(endpoint, bankPks);
  const banks: BankMap = new Map();
  const oracleOf = new Map<string, PublicKey>();
  for (const [pkStr, r] of bankRaw) {
    const bk = decodeBank(r);
    if (bk) {
      oracleOf.set(pkStr, bk.oracleKey);
      banks.set(pkStr, bk);
    }
  }
  const oraclePkSet = new Map<string, PublicKey>();
  for (const oc of oracleOf.values()) oraclePkSet.set(oc.toBase58(), oc);
  const oraclePks = [...oraclePkSet.values()];
  const oracleRaw = await getMultiple(endpoint, oraclePks);
  const priceByOracle = new Map<string, number>();
  for (const [pkStr, r] of oracleRaw) {
    const p = decodeOraclePrice(r);
    if (p !== null) priceByOracle.set(pkStr, p);
  }
  const crankable = new Set<string>();
  for (const [bankStr, oracle] of oracleOf) {
    const oracleStr = oracle.toBase58();
    const r = oracleRaw.get(oracleStr);
    if (!r) continue;
    const pu = decodePriceUpdateV2(r);
    if (!pu) continue;
    if (sponsoredFeed(0, pu.feedId).toBase58() === oracleStr) crankable.add(bankStr);
  }
  const prices: PriceMap = new Map();
  for (const [bk, oc] of oracleOf) {
    const p = priceByOracle.get(oc.toBase58());
    if (p !== undefined) prices.set(bk, p);
  }
  console.log(`${accts.length} borrowers, ${prices.size} banks priced, ${crankable.size} crankable`);

  // MIN_COLLATERAL mirrors the executor's filter (default 100) — excludes the
  // tiny/mis-priced (weighted-assets≈0, absurd-ratio) accounts.
  const minCollateral = Number.parseFloat(process.env.MIN_COLLATERAL_USD ?? '') || 100.0;

  // Categorize the liquidatable (emode-aware, on-chain price) set by shape,
  // AFTER the min-collateral filter — i.e. what the engine would actually see.
  let v1Usdc = 0;
  let v1Crank = 0;
  let v1Nonusdc = 0;
  let multi = 0;
  let missing = 0;
  let tiny = 0;
  const examples: Array<[number, string, string]> = [];
  for (const [pk, a] of accts) {
    const r = maintenanceHealth(a, banks, prices);
    if (r.missing > 0) {
      if (healthLiquidatable(r.health)) missing += 1;
      continue;
    }
    if (!healthLiquidatable(r.health)) continue;
    if (r.health.weightedAssets < minCollateral) {
      tiny += 1;
      continue;
    }
    const assets = a.balances.filter((b) => b.assetShares > 0.0);
    const liabs = a.balances.filter((b) => b.liabilityShares > 0.0);
    let cat: string;
    if (assets.length === 1 && liabs.length === 1) {
      if (liabs[0].bankPk.equals(usdcBank)) {
        v1Usdc += 1;
        if (crankable.has(assets[0].bankPk.toBase58())) v1Crank += 1;
        cat = 'v1_USDC_debt(FIREABLE)';
      } else {
        v1Nonusdc += 1;
        cat = 'v1_nonUSDC_debt(skip)';
      }
    } else {
      multi += 1;
      cat = 'multi(skip)';
    }
    if (examples.length < sample * 4) {
      examples.push([healthRatio(r.health), cat, pk.toBase58()]);
    }
  }
  console.log(`\nLIQUIDATABLE (emode-aware, on-chain price, collateral ≥ $${minCollateral}):`);
  console.log(`  v1 USDC-debt  (FIREABLE): ${v1Usdc}   (of which crankable: ${v1Crank})`);
  console.log(`  v1 non-USDC-debt (skip):  ${v1Nonusdc}`);
  console.log(`  multi-position   (skip):  ${multi}`);
  console.log(`  below min-collateral (excluded): ${tiny}`);
  console.log(`  incomplete/missing price: ${missing}`);
  console.log('\nexamples (ratio, category, account):');
  examples.sort((a, b) => b[0] - a[0]);
  for (const [ratio, cat, pk] of examples.slice(0, sample * 2)) {
    console.log(`  ${ratio.toFixed(3)}  ${cat.padEnd(14)}  ${pk}`);
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
