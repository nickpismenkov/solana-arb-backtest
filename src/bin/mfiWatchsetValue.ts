// Port of src/bin/mfi_watchset_value.rs
//
// ADDRESSABLE-MARKET census: how much money is actually within reach on marginfi?
//
// The liquidations that LAND on a calm day are dust (2026-07-14: 119 liquidations
// across all 4 protocols moved $171 total). That says nothing about the size of
// the opportunity when volatility hits — for that you have to look at the standing
// borrower population, not the fills. This bins every marginfi borrower by distance
// to liquidation and sums the collateral in each bin, so we can answer: "if the
// market drops X%, how much collateral comes into liquidation range, and is any of
// it big enough to be worth firing at?"
//
// Also reports how much of that collateral our fire path could actually TAKE (v1
// shape: 1 collateral / 1 USDC|USDT|wSOL debt) vs. what it would skip.
//
// Uses the same decoders as the live executor (liquidation.maintenanceHealth,
// on-chain oracle prices) so the numbers match what the bot sees.
//
// Usage: HELIUS_RPC=<url> [DROP_PCT=10] npx tsx src/bin/mfiWatchsetValue.ts

import 'dotenv/config';
import { PublicKey } from '@solana/web3.js';
import {
  decodeBank,
  decodeMarginfiAccount,
  decodeOraclePrice,
  decodeOraclePriceFresh,
  decodeSwitchboardPullSlot,
  maintenanceHealth,
  healthRatio,
  DEFAULT_MAX_SB_STALE_SLOTS,
  MA_SIZE,
  type Bank,
  type BankMap,
  type MarginfiAccount,
  type PriceMap,
} from '../lib/liquidation.js';

const MARGINFI_PROGRAM = 'MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA';
const MARGINFI_GROUP = '4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8';
const USDC_MINT = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v';
const USDT_MINT = 'Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB';
const SOL_MINT = 'So11111111111111111111111111111111111111112';

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

/** Is this the shape the fire path can act on? 1 collateral / 1 stable-or-SOL debt. */
function isFireableShape(a: MarginfiAccount, banks: BankMap): boolean {
  const assets = a.balances.filter((b) => b.assetShares > 0.0);
  const liabs = a.balances.filter((b) => b.liabilityShares > 0.0);
  if (assets.length !== 1 || liabs.length !== 1) return false;
  const lb = banks.get(liabs[0].bankPk.toBase58());
  if (!lb) return false;
  const m = lb.mint.toBase58();
  return m === USDC_MINT || m === USDT_MINT || m === SOL_MINT;
}

interface Row {
  pk: PublicKey;
  coll: number;
  ratio: number;
  ratioShocked: number;
  fireable: boolean;
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (!endpoint) throw new Error('HELIUS_RPC');
  const dropPct = Number.parseFloat(process.env.DROP_PCT ?? '') || 10.0;

  console.error('scanning marginfi borrowers …');
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
  const slotResp = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getSlot',
    params: [{ commitment: 'confirmed' }],
  });
  const slot = BigInt(typeof slotResp?.result === 'number' ? slotResp.result : 0);
  const maxStale = process.env.MAX_SB_STALE_SLOTS ? BigInt(process.env.MAX_SB_STALE_SLOTS) : DEFAULT_MAX_SB_STALE_SLOTS;
  const gate = process.env.STALE_GATE !== '0'; // STALE_GATE=0 → old behavior
  const oraclePkSet = new Map<string, PublicKey>();
  for (const oc of oracleOf.values()) oraclePkSet.set(oc.toBase58(), oc);
  const oraclePks = [...oraclePkSet.values()];
  const oracleRaw = await getMultiple(endpoint, oraclePks);
  const priceByOracle = new Map<string, number>();
  for (const [pkStr, r] of oracleRaw) {
    const p = gate ? decodeOraclePriceFresh(r, slot, maxStale) : decodeOraclePrice(r);
    if (p !== null) priceByOracle.set(pkStr, p);
  }
  console.error(`slot ${slot}, max_stale ${maxStale} slots, gate ${gate ? 'ON' : 'OFF'}`);
  const prices: PriceMap = new Map();
  for (const [bk, oc] of oracleOf) {
    const p = priceByOracle.get(oc.toBase58());
    if (p !== undefined) prices.set(bk, p);
  }
  console.error(`${accts.length} borrowers, ${prices.size} banks priced\n`);

  // Health today, and health after an adverse move of DROP_PCT on every
  // non-stable collateral (the "what does a real selloff put in range" question).
  const stable = (m: string) => m === USDC_MINT || m === USDT_MINT;
  const shocked: PriceMap = new Map(prices);
  for (const [bankPk, bank] of banks) {
    if (stable(bank.mint.toBase58())) continue;
    const p = shocked.get(bankPk);
    if (p !== undefined) shocked.set(bankPk, p * (1.0 - dropPct / 100.0));
  }

  const rows: Row[] = [];
  for (const [pk, a] of accts) {
    const now = maintenanceHealth(a, banks, prices);
    if (now.missing > 0 || now.health.weightedAssets <= 0.0 || now.health.weightedLiabilities <= 0.0) continue;
    const then = maintenanceHealth(a, banks, shocked);
    // Unweighted collateral USD = what a liquidator can actually seize against.
    let coll = 0.0;
    for (const b of a.balances) {
      if (!(b.assetShares > 0.0)) continue;
      const bank = banks.get(b.bankPk.toBase58());
      const px = prices.get(b.bankPk.toBase58());
      if (!bank || px === undefined) continue;
      const scale = 10 ** bank.mintDecimals;
      coll += (b.assetShares * bank.assetShareValue) / scale * px;
    }
    rows.push({ pk, coll, ratio: healthRatio(now.health), ratioShocked: healthRatio(then.health), fireable: isFireableShape(a, banks) });
  }

  const bins: Array<[number, number, string]> = [
    [0.0, 0.85, '< 0.85  (safe)'],
    [0.85, 0.95, '0.85 – 0.95'],
    [0.95, 0.97, '0.95 – 0.97'],
    [0.97, 1.0, '0.97 – 1.00  (ARM)'],
    [1.0, Number.POSITIVE_INFINITY, '≥ 1.00  (LIQUIDATABLE)'],
  ];
  console.log(`MARGINFI BORROWER POPULATION — ${rows.length} priced accounts with debt\n`);
  console.log(
    `${'health ratio'.padEnd(24)} ${'accts'.padStart(7)} ${'collateral $'.padStart(16)} ${'≥ $1k'.padStart(10)} ${'fireable coll $'.padStart(16)}`,
  );
  console.log('-'.repeat(78));
  for (const [lo, hi, label] of bins) {
    const sel = rows.filter((r) => r.ratio >= lo && r.ratio < hi);
    const tot = sel.reduce((s, r) => s + r.coll, 0);
    const big = sel.filter((r) => r.coll >= 1000.0).length;
    const fire = sel.filter((r) => r.fireable).reduce((s, r) => s + r.coll, 0);
    console.log(
      `${label.padEnd(24)} ${String(sel.length).padStart(7)} ${tot.toFixed(0).padStart(16)} ${String(big).padStart(10)} ${fire.toFixed(0).padStart(16)}`,
    );
  }

  // The money question: a DROP_PCT selloff — what comes into range?
  const newly = rows.filter((r) => r.ratio < 1.0 && r.ratioShocked >= 1.0);
  const newlyColl = newly.reduce((s, r) => s + r.coll, 0);
  const newlyFire = newly.filter((r) => r.fireable).reduce((s, r) => s + r.coll, 0);
  const newlyBig = newly.filter((r) => r.coll >= 1000.0).length;
  console.log(`\n▶ IF EVERY VOLATILE COLLATERAL DROPS ${dropPct}%:`);
  console.log(`   ${newly.length} accounts newly cross into liquidation range`);
  console.log(`   $${newlyColl.toFixed(0)} collateral comes into range  ($${newlyFire.toFixed(0)} of it in our fireable shape)`);
  console.log(`   of those, ${newlyBig} are ≥ $1k positions (worth firing at)`);

  const top = rows.filter((r) => r.ratio >= 0.9);
  top.sort((a, b) => b.coll - a.coll);
  console.log('\n▶ LARGEST POSITIONS ALREADY WITHIN 10% OF THE THRESHOLD (ratio ≥ 0.90):');
  for (const r of top.slice(0, 12)) {
    console.log(
      `   $${r.coll.toFixed(0).padStart(12)}  ratio ${r.ratio.toFixed(3)}  ${(r.fireable ? 'fireable' : 'SKIP (shape)').padEnd(12)}  ${r.pk.toBase58()}`,
    );
  }
  // The phantom question: big accounts our math says are ALREADY liquidatable
  // and that our fire path could shape-wise take. If these were real, the
  // competitor bots would have eaten them in seconds — they persist for days,
  // so either our health math over-flags or the chain refuses for a reason we
  // do not model. These are the accounts to simulate against.
  const phantoms = rows.filter((r) => r.ratio >= 1.0 && r.fireable && r.coll >= 1000.0);
  phantoms.sort((a, b) => b.coll - a.coll);
  console.log("\n▶ BIG 'LIQUIDATABLE' + FIREABLE-SHAPE ACCOUNTS (the phantom suspects):");
  // Report each survivor's collateral-oracle staleness so we can see whether a
  // tighter (still-safe) MAX_SB_STALE_SLOTS would catch it. Healthy feeds run
  // ~350 slots behind head; a survivor far above that is a stale-oracle phantom.
  const acctByPk = new Map<string, MarginfiAccount>();
  for (const [pk, a] of accts) acctByPk.set(pk.toBase58(), a);
  for (const r of phantoms.slice(0, 10)) {
    let stale = '(pyth or fresh)';
    const a = acctByPk.get(r.pk.toBase58());
    if (a) {
      const cb = a.balances.find((b) => b.assetShares > 0.0);
      if (cb) {
        const oc = oracleOf.get(cb.bankPk.toBase58());
        if (oc) {
          const raw = oracleRaw.get(oc.toBase58());
          if (raw) {
            const s = decodeSwitchboardPullSlot(raw);
            if (s !== null) {
              const behind = slot > s ? slot - s : 0n;
              stale = `SB oracle ${behind} slots behind head`;
            }
          }
        }
      }
    }
    console.log(`   $${r.coll.toFixed(0).padStart(12)}  ratio ${r.ratio.toFixed(3)}  ${r.pk.toBase58()}  [${stale}]`);
  }
  const nearTotal = top.reduce((s, r) => s + r.coll, 0);
  console.log(`   → ${top.length} accounts, $${nearTotal.toFixed(0)} total collateral`);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
