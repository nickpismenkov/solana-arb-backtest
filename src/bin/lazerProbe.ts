// Port of src/bin/lazer_probe.rs
//
// Live verification of Pyth Lazer pre-positioning (run on the box, where
// PYTH_LAZER_TOKEN lives). Streams the volatile majors, scans the marginfi
// watch-set once, then each interval recomputes health with Lazer prices
// blended over the on-chain baseline and prints the nearest-to-liquidation
// accounts + the Lazer-vs-on-chain price delta per major. Confirms (a) the
// feed is live, (b) the mint→feed mapping resolves banks, and (c) Lazer leads
// the on-chain oracle (nonzero delta = the pre-positioning edge).
//
// Usage: HELIUS_RPC=<url> PYTH_LAZER_TOKEN=<token> [INTERVAL_MS=2000]
//        tsx src/bin/lazerProbe.ts

import 'dotenv/config';
import { PublicKey } from '@solana/web3.js';
import { armFeedIds, blend, LAZER_SOL, mintFeedMap, spawnLazerThread, status } from '../lib/lazer.js';
import {
  decodeBank,
  decodeMarginfiAccount,
  decodeOraclePrice,
  MA_SIZE,
  maintenanceHealth,
  healthRatio,
  type Bank,
  type BankMap,
  type MarginfiAccount,
  type PriceMap,
} from '../lib/liquidation.js';
import { get, newTable } from '../lib/pyth.js';

const MARGINFI_PROGRAM = 'MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA';
const MARGINFI_GROUP = '4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8';

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function rpc(endpoint: string, body: unknown): Promise<any | undefined> {
  for (let attempt = 0; attempt < 4; attempt++) {
    try {
      const resp = await fetch(endpoint, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(body),
      });
      return await resp.json();
    } catch {
      // fall through to retry
    }
    await sleep(400 << attempt);
  }
  return undefined;
}

function b64(d: any): Buffer | undefined {
  const s = d?.[0];
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
    const arr: any[] = Array.isArray(v?.result?.value) ? v.result.value : [];
    arr.forEach((acc, idx) => {
      const data = b64(acc?.data);
      if (data !== undefined) out.set(chunk[idx].toBase58(), data);
    });
  }
  return out;
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (endpoint === undefined) throw new Error('HELIUS_RPC');
  const token = process.env.PYTH_LAZER_TOKEN;
  if (token === undefined) throw new Error('PYTH_LAZER_TOKEN (lives on the box)');
  const interval = Number.parseInt(process.env.INTERVAL_MS ?? '', 10) || 2000;

  const table = newTable();
  spawnLazerThread(token, armFeedIds(), table);
  console.error('[lazer] subscribed to majors; scanning marginfi group …');

  // Scan borrowers + banks + on-chain oracle prices once (baseline).
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
  const accts: [PublicKey, MarginfiAccount][] = [];
  for (const e of entries) {
    const pkStr = e?.pubkey;
    const data = b64(e?.account?.data);
    if (typeof pkStr !== 'string' || data === undefined) continue;
    const acct = decodeMarginfiAccount(data);
    if (acct === null) continue;
    if (!acct.balances.some((b) => b.liabilityShares > 0.0)) continue;
    accts.push([new PublicKey(pkStr), acct]);
  }
  const bankPksSet = new Set<string>();
  const bankPks: PublicKey[] = [];
  for (const [, a] of accts) {
    for (const b of a.balances) {
      const key = b.bankPk.toBase58();
      if (!bankPksSet.has(key)) {
        bankPksSet.add(key);
        bankPks.push(b.bankPk);
      }
    }
  }
  const banks: BankMap = new Map();
  const oracleOf = new Map<string, PublicKey>();
  const bankRaw = await getMultiple(endpoint, bankPks);
  for (const [pkStr, raw] of bankRaw) {
    const bk = decodeBank(raw);
    if (bk !== null) {
      oracleOf.set(pkStr, bk.oracleKey);
      banks.set(pkStr, bk);
    }
  }
  const oraclePksSet = new Set<string>();
  const oraclePks: PublicKey[] = [];
  for (const oc of oracleOf.values()) {
    const key = oc.toBase58();
    if (!oraclePksSet.has(key)) {
      oraclePksSet.add(key);
      oraclePks.push(oc);
    }
  }
  const onChain: PriceMap = new Map();
  const oracleRaw = await getMultiple(endpoint, oraclePks);
  for (const [pkStr, raw] of oracleRaw) {
    const usd = decodeOraclePrice(raw);
    if (usd !== null) {
      for (const [bk, oc] of oracleOf) {
        if (oc.toBase58() === pkStr) onChain.set(bk, usd);
      }
    }
  }
  const map = mintFeedMap();
  console.error(`[lazer] ${accts.length} borrowers, ${banks.size} banks, ${onChain.size} on-chain-priced`);

  for (;;) {
    await sleep(interval);
    if (get(table, LAZER_SOL) === undefined) {
      console.error('[lazer] waiting for first tick …');
      continue;
    }
    const [blended, led] = blend(banks, onChain, table, map);
    // Lazer-vs-on-chain delta on SOL (the leading-edge proof).
    let solDelta = 'no SOL bank';
    for (const [pk, b] of banks) {
      if (map.get(b.mint.toBase58()) === LAZER_SOL) {
        const oc = onChain.get(pk);
        const lz = blended.get(pk);
        if (oc !== undefined && lz !== undefined) {
          solDelta = `SOL on-chain $${oc.toFixed(2)} → Lazer $${lz.toFixed(2)} (Δ${lz - oc >= 0 ? '+' : ''}${(lz - oc).toFixed(2)})`;
        }
        break;
      }
    }

    // Nearest-to-liquidation by Lazer-blended health.
    const ranked: [PublicKey, number, number][] = [];
    for (const [pk, a] of accts) {
      const r = maintenanceHealth(a, banks, blended);
      if (r.missing === 0 && r.health.weightedAssets >= 100.0) {
        ranked.push([pk, healthRatio(r.health), r.health.weightedAssets]);
      }
    }
    ranked.sort((a, b) => b[1] - a[1]);
    console.log(`\n[${status(table)}] ${solDelta}  (${led} banks Lazer-led)`);
    for (const [pk, ratio, assets] of ranked.slice(0, 5)) {
      const pkStr = pk.toBase58();
      console.log(
        `  ${pkStr.slice(0, 8)}  ratio ${ratio.toFixed(4)}  collateral $${assets.toFixed(0)}${ratio >= 1.0 ? '  ← LIQUIDATABLE (Lazer)' : ''}`,
      );
    }
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
