// Port of src/bin/liq_finder.rs
//
// marginfi liquidatable-account finder (read-only, Stage 1 live test).
//
// Scans every MarginfiAccount in the main group, prices each position from the
// on-chain Pyth oracle the protocol itself reads (PriceUpdateV2), computes
// maintenance health, and lists who is liquidatable (+ the closest near-misses,
// so we can see how tight the market is). No money moves.
//
// Usage: HELIUS_RPC=<https json-rpc url> tsx src/bin/liqFinder.ts
//   [NEAR=20]  how many near-liquidation accounts to show

import 'dotenv/config';
import { PublicKey } from '@solana/web3.js';
import {
  type BankMap,
  decodeBank,
  decodeMarginfiAccount,
  decodeOraclePrice,
  MA_SIZE,
  maintenanceHealth,
  type MarginfiAccount,
  type PriceMap,
} from '../lib/liquidation.js';

const MARGINFI_PROGRAM = 'MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA';
const MARGINFI_GROUP = '4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8';

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
      const v = await res.json();
      return v;
    } catch {
      // fall through to retry
    }
    await sleep(400 << attempt);
  }
  return undefined;
}

function b64(data: any): Buffer | undefined {
  const s = data?.[0];
  if (typeof s !== 'string') return undefined;
  try {
    return Buffer.from(s, 'base64');
  } catch {
    return undefined;
  }
}

/** Batch getMultipleAccounts (100 keys/call) → map pubkey (base58) → raw bytes. */
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
    if (v === undefined) continue;
    const arr = v?.result?.value;
    if (!Array.isArray(arr)) continue;
    for (let j = 0; j < arr.length; j++) {
      const bytes = arr[j]?.data !== undefined ? b64(arr[j].data) : undefined;
      if (bytes !== undefined) out.set(chunk[j].toBase58(), bytes);
    }
    await sleep(50);
  }
  return out;
}

interface Scored {
  assets: number;
  deficit: number;
  ratio: number;
  a: MarginfiAccount;
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (!endpoint) throw new Error('HELIUS_RPC (a getProgramAccounts-capable JSON-RPC url) in .env');
  const nearN = Number.parseInt(process.env.NEAR ?? '', 10) || 20;

  // 1) All MarginfiAccounts in the main group. dataSlice trims to the balances
  //    region (1736 B) so the payload is ~half. Server-side dataSize filter
  //    still guarantees these are full 2312-byte accounts.
  console.error(`[finder] getProgramAccounts (group ${MARGINFI_GROUP.slice(0, 8)}) …`);
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
  const accountsJson: any[] = Array.isArray(resp?.result) ? resp.result : [];
  console.error(`[finder] ${accountsJson.length} accounts in group`);
  if (accountsJson.length === 0) {
    console.error('[finder] nothing returned — check RPC supports getProgramAccounts + the group filter');
    return;
  }

  const accounts: MarginfiAccount[] = [];
  for (const e of accountsJson) {
    const bytes = e?.account?.data !== undefined ? b64(e.account.data) : undefined;
    if (bytes === undefined) continue;
    const acct = decodeMarginfiAccount(bytes);
    if (acct === null) continue;
    if (acct.balances.length === 0) continue;
    accounts.push(acct);
  }
  const borrowers = accounts.filter((a) => a.balances.some((b) => b.liabilityShares > 0.0));
  console.error(`[finder] ${accounts.length} accounts with balances, ${borrowers.length} with an open borrow`);

  // 2) Fetch every referenced Bank.
  const bankPkSet = new Set<string>();
  const bankPks: PublicKey[] = [];
  for (const a of borrowers) {
    for (const b of a.balances) {
      const key = b.bankPk.toBase58();
      if (!bankPkSet.has(key)) {
        bankPkSet.add(key);
        bankPks.push(b.bankPk);
      }
    }
  }
  console.error(`[finder] fetching ${bankPks.length} banks …`);
  const bankRaw = await getMultiple(endpoint, bankPks);
  const banks: BankMap = new Map();
  const oracleOf = new Map<string, PublicKey>();
  for (const [pkStr, raw] of bankRaw) {
    const bank = decodeBank(raw);
    if (bank !== null) {
      oracleOf.set(pkStr, bank.oracleKey);
      banks.set(pkStr, bank);
    }
  }

  // 3) Price each bank from its on-chain Pyth oracle (PriceUpdateV2).
  const oraclePkSet = new Set<string>();
  const oraclePks: PublicKey[] = [];
  for (const oc of oracleOf.values()) {
    const key = oc.toBase58();
    if (!oraclePkSet.has(key)) {
      oraclePkSet.add(key);
      oraclePks.push(oc);
    }
  }
  console.error(`[finder] fetching ${oraclePks.length} oracle accounts …`);
  const oracleRaw = await getMultiple(endpoint, oraclePks);
  const oraclePrice = new Map<string, number>();
  for (const [pkStr, raw] of oracleRaw) {
    const usd = decodeOraclePrice(raw);
    if (usd !== null) oraclePrice.set(pkStr, usd);
  }
  const prices: PriceMap = new Map();
  for (const [bankPk, oraclePk] of oracleOf) {
    const p = oraclePrice.get(oraclePk.toBase58());
    if (p !== undefined) prices.set(bankPk, p);
  }
  console.error(`[finder] priced ${prices.size}/${banks.size} banks`);

  // Sanity: dump a few priced banks (eyeball USDC≈$1, SOL≈$82, …).
  console.error('[finder] price sanity (mint → USD):');
  let dumped = 0;
  for (const [pkStr, bank] of banks) {
    if (dumped >= 200) break;
    dumped += 1;
    const p = prices.get(pkStr);
    if (p !== undefined) {
      console.error(`    ${bank.mint.toBase58().slice(0, 8)}… (dec ${bank.mintDecimals}) = $${p.toFixed(4)}`);
    }
  }

  // Dust threshold: below this seizable collateral (USD) a liquidation can't
  // cover gas+priority, so it isn't a real opportunity.
  const minCollateral = Number.parseFloat(process.env.MIN_COLLATERAL_USD ?? '') || 10.0;

  // 4) Health — only for borrowers whose EVERY bank we could price. An
  //    account with any unpriced bank is "incomplete", not a signal.
  const scored: Scored[] = [];
  let incomplete = 0;
  for (const a of borrowers) {
    const r = maintenanceHealth(a, banks, prices);
    if (r.missing > 0) {
      incomplete += 1;
      continue;
    }
    scored.push({
      assets: r.health.weightedAssets,
      deficit: r.health.weightedAssets - r.health.weightedLiabilities,
      ratio: r.health.weightedAssets === 0.0 ? Infinity : r.health.weightedLiabilities / r.health.weightedAssets,
      a,
    });
  }

  console.log('\n════ marginfi liquidatable finder ════');
  console.log(`borrowers scanned:        ${borrowers.length}`);
  console.log(`fully priced (judgable):  ${scored.length}`);
  console.log(`incomplete (unpriced bank, skipped): ${incomplete}`);

  // Liquidatable with REAL collateral to seize, ranked by seizable value.
  const real = scored.filter((s) => s.deficit < 0.0 && s.assets >= minCollateral);
  real.sort((x, y) => y.assets - x.assets);
  const dust = scored.filter((s) => s.deficit < 0.0 && s.assets < minCollateral).length;

  console.log(
    `LIQUIDATABLE (collateral ≥ $${minCollateral.toFixed(0)}): ${real.length}   [+${dust} dust ignored]`,
  );
  for (const s of real.slice(0, 50)) {
    console.log(
      `  authority ${s.a.authority.toBase58().slice(0, 8)}…  collateral=$${s.assets.toFixed(2)}  deficit=${s.deficit >= 0 ? '+' : ''}${s.deficit.toFixed(2)} USD  liab/asset=${s.ratio.toFixed(4)}`,
    );
  }

  // Per-bank breakdown of the largest liquidatable account — tells us whether
  // the collateral is liquid (real opportunity) or a stuck/illiquid token.
  if (real.length > 0) {
    const top = real[0];
    console.log(`\n── breakdown: ${top.a.authority.toBase58().slice(0, 8)}… (largest liquidatable) ──`);
    for (const b of top.a.balances) {
      const bank = banks.get(b.bankPk.toBase58());
      if (bank === undefined) {
        console.log(`  bank ${b.bankPk.toBase58().slice(0, 8)}… UNPRICED`);
        continue;
      }
      const price = prices.get(b.bankPk.toBase58()) ?? NaN;
      const scale = 10 ** bank.mintDecimals;
      if (b.assetShares > 0.0) {
        const ui = (b.assetShares * bank.assetShareValue) / scale;
        console.log(
          `  COLLATERAL mint ${bank.mint.toBase58().slice(0, 8)}… ${ui.toFixed(4)} tok × $${price.toFixed(4)} = $${(ui * price).toFixed(2)}  (maint w ${bank.assetWeightMaint.toFixed(2)}, tier via weights)`,
        );
      }
      if (b.liabilityShares > 0.0) {
        const ui = (b.liabilityShares * bank.liabilityShareValue) / scale;
        console.log(
          `  BORROW     mint ${bank.mint.toBase58().slice(0, 8)}… ${ui.toFixed(4)} tok × $${price.toFixed(4)} = $${(ui * price).toFixed(2)}  (maint w ${bank.liabilityWeightMaint.toFixed(2)})`,
        );
      }
    }
  }

  // Closest healthy accounts WITH real collateral — the ones worth monitoring.
  const near = scored.filter((s) => s.deficit >= 0.0 && s.assets >= minCollateral);
  near.sort((x, y) => y.ratio - x.ratio);
  console.log(`\nclosest to liquidation (collateral ≥ $${minCollateral.toFixed(0)}, liab/asset→1.0):`);
  for (const s of near.slice(0, nearN)) {
    console.log(
      `  ${s.a.authority.toBase58().slice(0, 8)}…  liab/asset=${s.ratio.toFixed(4)}  collateral=$${s.assets.toFixed(2)}  buffer=${s.deficit >= 0 ? '+' : ''}${s.deficit.toFixed(2)} USD`,
    );
  }
  console.log();
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
