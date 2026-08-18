// Port of src/bin/liq_monitor.rs
//
// marginfi continuous liquidation monitor (read-only, real-data test).
//
// The go/no-go experiment: does a *profitable* liquidation opportunity ever
// sit around long enough for us to take it, or is it gone in milliseconds?
//
// Architecture (getProgramAccounts over 500k accounts is ~850MB, far too heavy
// to loop): scan ONCE to build a watch-set of accounts near liquidation, then
// on a fast loop poll only (a) oracle prices and (b) the watch-set's fresh
// account data — a few cheap getMultipleAccounts calls. We detect the instant
// a watched account crosses underwater (APPEARED) and when it recovers / gets
// liquidated (RESOLVED), logging how long it stayed takeable. Full re-scan
// every FULL_REFRESH_SECS to catch new entrants.
//
// Usage: HELIUS_RPC=<url> [POLL_SECS=5] [FULL_REFRESH_SECS=600]
//        [WATCH_RATIO=0.85] [MIN_COLLATERAL_USD=50] tsx src/bin/liqMonitor.ts

import 'dotenv/config';
import * as fs from 'node:fs';
import { PublicKey } from '@solana/web3.js';
import {
  type BankMap,
  decodeBank,
  decodeMarginfiAccount,
  decodePriceUpdateV2,
  MA_SIZE,
  maintenanceHealth,
  type MarginfiAccount,
  type PriceMap,
} from '../lib/liquidation.js';

const MARGINFI_PROGRAM = 'MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA';
const MARGINFI_GROUP = '4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8';

function nowTs(): number {
  return Math.floor(Date.now() / 1000);
}

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

/** Batch getMultipleAccounts (100/call) → pubkey (base58) → raw bytes. */
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
    await sleep(40);
  }
  return out;
}

/** A watched account: its address, authority, and last-known balances. */
interface Watched {
  authority: PublicKey;
  account: MarginfiAccount;
}

/**
 * Full scan → banks, bank→oracle map, and the near-liquidation watch-set
 * (keyed by account pubkey base58). Heavy; run rarely.
 */
async function fullScan(
  endpoint: string,
  watchRatio: number,
  minCollateral: number,
): Promise<{ banks: BankMap; bankOracle: Map<string, PublicKey>; watch: Map<string, Watched> }> {
  console.error(`[monitor] full scan: getProgramAccounts (group ${MARGINFI_GROUP.slice(0, 8)}) …`);
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
  const entries: any[] = Array.isArray(resp?.result) ? resp.result : [];
  console.error(`[monitor]   ${entries.length} accounts`);

  // Decode borrowers, remembering the account pubkey.
  const borrowers: Array<[string, MarginfiAccount]> = [];
  for (const e of entries) {
    const pkStr = typeof e?.pubkey === 'string' ? e.pubkey : undefined;
    if (pkStr === undefined) continue;
    let pk: PublicKey;
    try {
      pk = new PublicKey(pkStr);
    } catch {
      continue;
    }
    const bytes = e?.account?.data !== undefined ? b64(e.account.data) : undefined;
    if (bytes === undefined) continue;
    const acct = decodeMarginfiAccount(bytes);
    if (acct === null) continue;
    void pk;
    if (acct.balances.some((b) => b.liabilityShares > 0.0)) {
      borrowers.push([pkStr, acct]);
    }
  }

  // Banks + oracle prices (once) to score the watch-set.
  const bankPkSet = new Set<string>();
  const bankPks: PublicKey[] = [];
  for (const [, a] of borrowers) {
    for (const b of a.balances) {
      const key = b.bankPk.toBase58();
      if (!bankPkSet.has(key)) {
        bankPkSet.add(key);
        bankPks.push(b.bankPk);
      }
    }
  }
  const bankRaw = await getMultiple(endpoint, bankPks);
  const banks: BankMap = new Map();
  const bankOracle = new Map<string, PublicKey>();
  for (const [pkStr, raw] of bankRaw) {
    const bank = decodeBank(raw);
    if (bank !== null) {
      bankOracle.set(pkStr, bank.oracleKey);
      banks.set(pkStr, bank);
    }
  }
  const prices = await refreshPrices(endpoint, banks, bankOracle);

  // Watch-set: fully-priced borrowers within striking distance.
  const watch = new Map<string, Watched>();
  for (const [pkStr, acct] of borrowers) {
    const r = maintenanceHealth(acct, banks, prices);
    if (r.missing > 0) continue;
    if (r.health.weightedAssets < minCollateral) continue;
    const ratio = r.health.weightedAssets === 0.0 ? Infinity : r.health.weightedLiabilities / r.health.weightedAssets;
    if (ratio >= watchRatio) {
      watch.set(pkStr, { authority: acct.authority, account: acct });
    }
  }
  console.error(
    `[monitor]   watch-set: ${watch.size} accounts (liab/asset ≥ ${watchRatio.toFixed(2)}, collateral ≥ $${minCollateral.toFixed(0)})`,
  );
  return { banks, bankOracle, watch };
}

/** Fetch + decode the oracle accounts, returning bank_pk (base58) → USD price. */
async function refreshPrices(endpoint: string, banks: BankMap, bankOracle: Map<string, PublicKey>): Promise<PriceMap> {
  const oraclePkSet = new Set<string>();
  const oraclePks: PublicKey[] = [];
  for (const oc of bankOracle.values()) {
    const key = oc.toBase58();
    if (!oraclePkSet.has(key)) {
      oraclePkSet.add(key);
      oraclePks.push(oc);
    }
  }
  const oracleRaw = await getMultiple(endpoint, oraclePks);
  const oraclePrice = new Map<string, number>();
  for (const [pkStr, raw] of oracleRaw) {
    const pyth = decodePriceUpdateV2(raw);
    if (pyth !== null) oraclePrice.set(pkStr, pyth.usdPrice);
  }
  const prices: PriceMap = new Map();
  for (const [bankPk, oraclePk] of bankOracle) {
    if (banks.has(bankPk)) {
      const p = oraclePrice.get(oraclePk.toBase58());
      if (p !== undefined) prices.set(bankPk, p);
    }
  }
  return prices;
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (!endpoint) throw new Error('HELIUS_RPC in .env');
  const pollSecs = Number.parseInt(process.env.POLL_SECS ?? '', 10) || 5;
  const fullRefresh = Number.parseInt(process.env.FULL_REFRESH_SECS ?? '', 10) || 600;
  const watchRatio = Number.parseFloat(process.env.WATCH_RATIO ?? '') || 0.85;
  const minCollateral = Number.parseFloat(process.env.MIN_COLLATERAL_USD ?? '') || 50.0;

  const logPath = 'runs/liq_events.jsonl';
  fs.mkdirSync('runs', { recursive: true });
  const logFd = fs.openSync(logPath, 'a');
  const emit = (v: unknown): void => {
    fs.writeSync(logFd, `${JSON.stringify(v)}\n`);
  };

  console.error(
    `[monitor] poll=${pollSecs}s full_refresh=${fullRefresh}s watch_ratio=${watchRatio} min_collateral=$${minCollateral}`,
  );

  let { banks, bankOracle, watch } = await fullScan(endpoint, watchRatio, minCollateral);
  let lastFull = nowTs();
  // account (base58) → [ts first seen liquidatable, peak collateral, peak deficit]
  const open = new Map<string, [number, number, number]>();
  let appeared = 0;
  let resolved = 0;
  const resolveDurations: number[] = [];

  for (;;) {
    // Periodic full re-scan for new entrants + fresh positions.
    if (nowTs() - lastFull >= fullRefresh) {
      const r = await fullScan(endpoint, watchRatio, minCollateral);
      banks = r.banks;
      bankOracle = r.bankOracle;
      // Keep open events even if an account drops out of the watch-set.
      watch = r.watch;
      lastFull = nowTs();
    }

    const prices = await refreshPrices(endpoint, banks, bankOracle);

    // Fresh account data for the watch-set (positions change too, not just price).
    const watchPks = Array.from(watch.keys()).map((k) => new PublicKey(k));
    const fresh = await getMultiple(endpoint, watchPks);
    for (const [pkStr, raw] of fresh) {
      const a = decodeMarginfiAccount(raw);
      if (a !== null) {
        const w = watch.get(pkStr);
        if (w !== undefined) w.account = a;
      }
    }

    const ts = nowTs();
    let curLiq = 0;
    let tightest: [number, PublicKey, number] = [Number.NEGATIVE_INFINITY, PublicKey.default, 0.0];
    for (const [pkStr, w] of watch) {
      const r = maintenanceHealth(w.account, banks, prices);
      if (r.missing > 0) continue;
      const ratio = r.health.weightedAssets === 0.0 ? Infinity : r.health.weightedLiabilities / r.health.weightedAssets;
      const deficit = r.health.weightedAssets - r.health.weightedLiabilities;
      const collateral = r.health.weightedAssets;
      if (ratio > tightest[0]) tightest = [ratio, w.authority, collateral];

      const isLiq = deficit < 0.0 && collateral >= minCollateral;
      if (isLiq) {
        curLiq += 1;
        let e = open.get(pkStr);
        if (e === undefined) {
          appeared += 1;
          emit({
            ts,
            event: 'appeared',
            account: pkStr,
            authority: w.authority.toBase58(),
            collateral_usd: Math.round(collateral * 100) / 100,
            deficit_usd: Math.round(deficit * 100) / 100,
            ratio: Math.round(ratio * 10000) / 10000,
          });
          console.error(
            `[APPEARED ${ts}] ${w.authority.toBase58().slice(0, 8)}… collateral=$${collateral.toFixed(0)} deficit=${deficit >= 0 ? '+' : ''}${deficit.toFixed(2)}`,
          );
          e = [ts, collateral, deficit];
          open.set(pkStr, e);
        }
        e[1] = Math.max(e[1], collateral);
        e[2] = Math.min(e[2], deficit);
      } else {
        const e = open.get(pkStr);
        if (e !== undefined) {
          open.delete(pkStr);
          resolved += 1;
          const [t0, peakCol, peakDef] = e;
          const dur = Math.max(0, ts - t0);
          resolveDurations.push(dur);
          emit({
            ts,
            event: 'resolved',
            account: pkStr,
            authority: w.authority.toBase58(),
            seen_secs: dur,
            peak_collateral_usd: Math.round(peakCol * 100) / 100,
            peak_deficit_usd: Math.round(peakDef * 100) / 100,
          });
          console.error(
            `[RESOLVED ${ts}] ${w.authority.toBase58().slice(0, 8)}… after ${dur}s (peak collateral $${peakCol.toFixed(0)}, peak deficit ${peakDef >= 0 ? '+' : ''}${peakDef.toFixed(2)})`,
          );
        }
      }
    }

    resolveDurations.sort((a, b) => a - b);
    const med = resolveDurations.length > 0 ? resolveDurations[Math.floor(resolveDurations.length / 2)] : 0;
    const tightestPkStr = tightest[1].toBase58();
    console.error(
      `[monitor ${ts}] watch=${watch.size} liq_now=${curLiq} open=${open.size} | total appeared=${appeared} resolved=${resolved} med_lifetime=${med}s | tightest ${tightest[0].toFixed(4)} (${tightestPkStr.slice(0, Math.min(8, tightestPkStr.length))}…)`,
    );

    await sleep(pollSecs * 1000);
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
