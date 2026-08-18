// Port of src/bin/liq_crank_probe.rs
//
// Step-5 gate: simulate the FULL crank+liquidate bundle against REAL marginfi
// accounts. Scans for the nearest-to-threshold borrowers whose asset bank has
// a crankable (shard-0 sponsored) oracle, fetches a fresh Hermes update for
// that feed, and simulateBundles:
//
//   [crank_setup, crank_fire, (start_fl · liquidate · end_fl)]
//
// Expected on a healthy market: crank txs SUCCEED (feed advances) and the
// liquidate hits marginfi's HealthyAccount guard (custom 6068) — proving the
// whole chain composes and the chain judged AT the cranked price. If an
// account is genuinely underwater at the true price, the gate passes outright
// (that's a live opportunity).
//
// Usage: HELIUS_RPC=<url> [TOP=3] [SEIZE_FRAC=0.1] tsx src/bin/liqCrankProbe.ts

import 'dotenv/config';
import { type AccountMeta, PublicKey, TransactionMessage, VersionedTransaction } from '@solana/web3.js';
import {
  type BankMap,
  decodeBank,
  decodeMarginfiAccount,
  decodeOraclePrice,
  decodePriceUpdateV2,
  MA_SIZE,
  maintenanceHealth,
  type MarginfiAccount,
  type PriceMap,
} from '../lib/liquidation.js';
import * as marginfi from '../lib/marginfi.js';
import * as acc from '../lib/pythAccumulator.js';
import * as pythCrank from '../lib/pythCrank.js';

const MARGINFI_PROGRAM = 'MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA';
const MARGINFI_GROUP = '4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8';
const DEFAULT_LIQUIDATOR_MA = 'B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD';
const DEFAULT_AUTHORITY = 'DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak';
const HEALTHY_ACCOUNT_ERR = 6068;
// solana_hash::Hash::default() (32 zero bytes) base58-encodes to this string.
const DEFAULT_HASH = '11111111111111111111111111111111';

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
  }
  return out;
}

function hexs(b: Buffer): string {
  return b.toString('hex');
}

interface Candidate {
  ratio: number;
  pk: PublicKey;
  a: MarginfiAccount;
  assetBank: PublicKey;
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (!endpoint) throw new Error('HELIUS_RPC');
  const top = Number.parseInt(process.env.TOP ?? '', 10) || 3;
  const seizeFrac = Number.parseFloat(process.env.SEIZE_FRAC ?? '') || 0.1;
  const hermes = process.env.HERMES ?? 'https://hermes.pyth.network';
  const authority = new PublicKey(process.env.AUTHORITY ?? DEFAULT_AUTHORITY);
  const liquidatorMa = new PublicKey(process.env.LIQUIDATOR_MA ?? DEFAULT_LIQUIDATOR_MA);
  const usdcBank = new PublicKey(marginfi.USDC_BANK);
  const tp = new PublicKey('TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA');

  // Scan borrowers + banks (same shape as the executor's full_scan).
  console.error('[probe] scanning marginfi accounts…');
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
  const accts: Array<[PublicKey, MarginfiAccount]> = [];
  for (const e of entries) {
    const pkStr = typeof e?.pubkey === 'string' ? e.pubkey : undefined;
    if (pkStr === undefined) continue;
    const bytes = e?.account?.data !== undefined ? b64(e.account.data) : undefined;
    if (bytes === undefined) continue;
    const a = decodeMarginfiAccount(bytes);
    if (a === null) continue;
    if (a.balances.some((b) => b.liabilityShares > 0.0)) {
      accts.push([new PublicKey(pkStr), a]);
    }
  }
  const bankPkSet = new Set<string>();
  const bankPks: PublicKey[] = [];
  for (const [, a] of accts) {
    for (const b of a.balances) {
      const key = b.bankPk.toBase58();
      if (!bankPkSet.has(key)) {
        bankPkSet.add(key);
        bankPks.push(b.bankPk);
      }
    }
  }
  const banks: BankMap = new Map();
  const oracleOf = new Map<string, PublicKey>();
  for (const [pkStr, raw] of await getMultiple(endpoint, bankPks)) {
    const bk = decodeBank(raw);
    if (bk !== null) {
      oracleOf.set(pkStr, bk.oracleKey);
      banks.set(pkStr, bk);
    }
  }
  const oraclePkSet = new Set<string>();
  const oraclePks: PublicKey[] = [];
  for (const oc of oracleOf.values()) {
    const key = oc.toBase58();
    if (!oraclePkSet.has(key)) {
      oraclePkSet.add(key);
      oraclePks.push(oc);
    }
  }
  const oracleRaw = await getMultiple(endpoint, oraclePks);
  const feedOf = new Map<string, Buffer>(); // bank pk -> feed id (32 bytes)
  const crankable = new Set<string>(); // bank pk
  const prices: PriceMap = new Map();
  for (const [bank, oracle] of oracleOf) {
    const raw = oracleRaw.get(oracle.toBase58());
    if (raw === undefined) continue;
    const usd = decodeOraclePrice(raw);
    if (usd !== null) prices.set(bank, usd);
    const pyth = decodePriceUpdateV2(raw);
    if (pyth !== null) {
      feedOf.set(bank, pyth.feedId);
      if (pythCrank.sponsoredFeed(0, pyth.feedId).equals(oracle)) crankable.add(bank);
    }
  }
  console.error(`[probe] ${accts.length} borrowers, ${banks.size} banks, ${crankable.size} crankable`);

  // Candidates: 1-asset/1-liab-USDC, crankable asset bank, ranked by ratio.
  const cands: Candidate[] = [];
  for (const [pk, a] of accts) {
    const assets = a.balances.filter((b) => b.assetShares > 0.0);
    const liabs = a.balances.filter((b) => b.liabilityShares > 0.0);
    if (assets.length !== 1 || liabs.length !== 1 || !liabs[0].bankPk.equals(usdcBank)) continue;
    if (!crankable.has(assets[0].bankPk.toBase58())) continue;
    const r = maintenanceHealth(a, banks, prices);
    if (r.missing > 0 || r.health.weightedAssets < 50.0) continue;
    const ratio =
      r.health.weightedAssets === 0.0 ? Infinity : r.health.weightedLiabilities / r.health.weightedAssets;
    cands.push({ ratio, pk, a, assetBank: assets[0].bankPk });
  }
  cands.sort((x, y) => y.ratio - x.ratio);
  cands.length = Math.min(cands.length, top);

  let chainVerified = 0;
  for (const { ratio, pk, a, assetBank } of cands) {
    const feedId = feedOf.get(assetBank.toBase58());
    const bank = banks.get(assetBank.toBase58());
    if (feedId === undefined || bank === undefined) continue;
    console.log(
      `\n════ candidate ${pk.toBase58()}  ratio ${ratio.toFixed(4)}  asset bank ${assetBank.toBase58().slice(0, 8)}…  feed ${hexs(feedId).slice(0, 16)}…`,
    );

    // Fresh Hermes update → crank txs.
    const fidHex = hexs(feedId);
    let update: acc.AccumulatorUpdate;
    try {
      update = await acc.fetchHermes(hermes, [fidHex]);
    } catch (e) {
      console.log(`  ✗ hermes: ${e}`);
      continue;
    }
    const mu = update.updates.find((u) => {
      const id = u.feedId();
      return id !== undefined && id.equals(feedId);
    });
    if (mu === undefined) {
      console.log('  ✗ feed missing from blob');
      continue;
    }
    let txs: pythCrank.CrankTxs;
    try {
      txs = pythCrank.buildCrankTxs(authority, update.vaa, [mu], 0, 0, DEFAULT_HASH);
    } catch (e) {
      console.log(`  ✗ crank build: ${e}`);
      continue;
    }
    const [setupB64, crankB64] = txs.toB64();

    // Gate tx at SEIZE_FRAC of the collateral.
    const assetBal = a.balances.find((b) => b.assetShares > 0.0);
    const nativeTotal = assetBal !== undefined ? assetBal.assetShares * bank.assetShareValue : 0.0;
    const amount = BigInt(Math.trunc(nativeTotal * seizeFrac));
    const obs: AccountMeta[] = [];
    for (const b of a.balances) {
      obs.push({ pubkey: b.bankPk, isSigner: false, isWritable: false });
      const oc = oracleOf.get(b.bankPk.toBase58());
      if (oc === undefined) continue;
      obs.push({ pubkey: oc, isSigner: false, isWritable: false });
    }
    const assetOracle = oracleOf.get(assetBank.toBase58());
    const usdcOracle = oracleOf.get(usdcBank.toBase58());
    if (assetOracle === undefined || usdcOracle === undefined) {
      console.log('  ✗ missing oracle for asset/usdc bank');
      continue;
    }
    const start = marginfi.startFlashloan(liquidatorMa, authority, 2n);
    const liqIx = marginfi.lendingAccountLiquidate(
      assetBank,
      usdcBank,
      liquidatorMa,
      authority,
      pk,
      tp,
      amount,
      assetOracle,
      usdcOracle,
      obs,
    );
    const endObs: AccountMeta[] = [
      { pubkey: assetBank, isSigner: false, isWritable: false },
      { pubkey: assetOracle, isSigner: false, isWritable: false },
      { pubkey: usdcBank, isSigner: false, isWritable: false },
      { pubkey: usdcOracle, isSigner: false, isWritable: false },
    ];
    const end = marginfi.endFlashloan(liquidatorMa, authority, endObs);

    const msg = new TransactionMessage({
      payerKey: authority,
      recentBlockhash: DEFAULT_HASH,
      instructions: [start, liqIx, end],
    }).compileToV0Message([]);
    const gate = new VersionedTransaction(msg);
    const gateB64 = Buffer.from(gate.serialize()).toString('base64');

    const v = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'simulateBundle',
      params: [
        { encodedTransactions: [setupB64, crankB64, gateB64] },
        {
          skipSigVerify: true,
          replaceRecentBlockhash: true,
          preExecutionAccountsConfigs: [null, null, null],
          postExecutionAccountsConfigs: [null, null, null],
        },
      ],
    });
    if (v === undefined) throw new Error('simulateBundle');
    if (v?.error !== undefined && v?.error !== null) {
      console.log(`  ✗ simulateBundle error: ${JSON.stringify(v.error)}`);
      continue;
    }
    const val = v?.result?.value ?? {};
    const results: any[] = Array.isArray(val?.transactionResults) ? val.transactionResults : [];
    const ok = results.filter((r) => r?.err === null || r?.err === undefined).length;
    console.log(
      `  bundle: ${ok} of 3 txs succeeded  summary=${val?.summary === 'succeeded' ? 'succeeded' : JSON.stringify(val?.summary)}`,
    );
    results.forEach((r, i) => {
      console.log(`    tx[${i}] err=${JSON.stringify(r?.err ?? null)} cu=${r?.unitsConsumed}`);
    });
    // Crank landed iff the first two txs succeeded; the gate's verdict is
    // then the chain's judgment AT the cranked price.
    const crankOk = results.length >= 2 && results.slice(0, 2).every((r) => r?.err === null || r?.err === undefined);
    const gateCode = results[2]?.err?.InstructionError?.[1]?.Custom;
    if (crankOk) {
      if (ok === 3) {
        console.log(`  ★★ LIVE OPPORTUNITY — liquidate ACCEPTED at the cranked price (would seize ${amount})`);
        chainVerified += 1;
      } else if (gateCode === HEALTHY_ACCOUNT_ERR) {
        console.log(
          '  ★ CHAIN-VERIFIED — crank landed, marginfi judged at the fresh price: HealthyAccount (6068), account not (yet) underwater',
        );
        chainVerified += 1;
      } else {
        console.log(`  ⚠ crank landed but liquidate failed with custom ${gateCode ?? 'null'} (emode/size guard?)`);
      }
    } else {
      console.log('  ✗ crank txs failed in bundle — inspect logs above');
    }
  }
  console.log(`\n${chainVerified} of ${cands.length} candidates chain-verified through the crank bundle`);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
