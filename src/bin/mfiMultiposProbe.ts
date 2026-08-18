// Port of src/bin/mfi_multipos_probe.rs
//
// Wiring proof for MULTI-POSITION liquidation.
//
// The live fire path skips any account with >1 collateral or >1 debt
// (liq_executor is_v1_fireable / try_arm's `assets.len()!=1 || liabs.len()!=1`).
// The census showed that's where ~99% of at-risk collateral sits ($2.6M / $941k /
// $791k positions). marginfi's `lending_account_liquidate` is single-leg (one
// asset_bank, one liab_bank) but carries the FULL balance list in the
// observation accounts — so liquidating ONE leg of a multi-position account is
// supported by the program. This probe proves the single-leg tx COMPOSES against
// a real multi-position account: build [start_fl, liquidate(one leg), end_fl] and
// simulate. Outcome classification:
//   err=null            → the leg is fireable right now (real opportunity)
//   HealthyAccount 6068 → wiring OK; account healthy at this leg/size (expected calm)
//   other Custom code   → an account-specific gate (stale oracle, etc.), still wiring-OK
//   error at a DIFFERENT ix index → a WIRING BUG (what this probe exists to catch)
//
// Usage: HELIUS_RPC=<url> [LIQUIDATOR_MA=…] [AUTHORITY=…] [TOPN=5] npx tsx src/bin/mfiMultiposProbe.ts

import 'dotenv/config';
import { PublicKey, TransactionMessage, VersionedTransaction, type AccountMeta } from '@solana/web3.js';
import {
  decodeBank,
  decodeMarginfiAccount,
  decodeOraclePriceFresh,
  maintenanceHealth,
  healthRatio,
  DEFAULT_MAX_SB_STALE_SLOTS,
  MA_SIZE,
  type Bank,
  type BankMap,
  type Balance,
  type MarginfiAccount,
  type PriceMap,
} from '../lib/liquidation.js';
import { startFlashloan, endFlashloan, lendingAccountLiquidate } from '../lib/marginfi.js';

const MARGINFI_PROGRAM = 'MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA';
const MARGINFI_GROUP = '4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8';
const DEFAULT_LIQUIDATOR_MA = 'B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD';
const DEFAULT_AUTHORITY = 'DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak';
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

async function mintOwner(endpoint: string, mint: PublicKey): Promise<PublicKey | null> {
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getAccountInfo',
    params: [mint.toBase58(), { encoding: 'jsonParsed' }],
  });
  const owner = v?.result?.value?.owner;
  if (typeof owner !== 'string') return null;
  try {
    return new PublicKey(owner);
  } catch {
    return null;
  }
}

function isDebtMint(m: PublicKey): boolean {
  const s = m.toBase58();
  return s === USDC_MINT || s === USDT_MINT || s === SOL_MINT;
}

/** Build [start_fl, liquidate(asset_bank, liab_bank, amount), end_fl] as base64. */
function gateTxB64(
  authority: PublicKey,
  liquidatorMa: PublicKey,
  tp: PublicKey,
  liquidatee: PublicKey,
  acct: MarginfiAccount,
  assetBank: PublicKey,
  liabBank: PublicKey,
  assetAmount: bigint,
  oracleOf: Map<string, PublicKey>,
): string | null {
  const obs: AccountMeta[] = [];
  for (const b of acct.balances) {
    const oc = oracleOf.get(b.bankPk.toBase58());
    if (!oc) return null;
    obs.push({ pubkey: b.bankPk, isSigner: false, isWritable: false });
    obs.push({ pubkey: oc, isSigner: false, isWritable: false });
  }
  const assetOracle = oracleOf.get(assetBank.toBase58());
  const liabOracle = oracleOf.get(liabBank.toBase58());
  if (!assetOracle || !liabOracle) return null;
  const start = startFlashloan(liquidatorMa, authority, 2n);
  const liqIx = lendingAccountLiquidate(assetBank, liabBank, liquidatorMa, authority, liquidatee, tp, assetAmount, assetOracle, liabOracle, obs);
  const endObs: AccountMeta[] = [
    { pubkey: assetBank, isSigner: false, isWritable: false },
    { pubkey: assetOracle, isSigner: false, isWritable: false },
    { pubkey: liabBank, isSigner: false, isWritable: false },
    { pubkey: liabOracle, isSigner: false, isWritable: false },
  ];
  const end = endFlashloan(liquidatorMa, authority, endObs);

  // Rust builds this with solana_hash::Hash::default() (all-zero) and a
  // single all-zero dummy signature (sigVerify=false in the sim call below,
  // so the signature content doesn't matter) — mirror that exactly rather
  // than fetching a real recent blockhash.
  const zeroBlockhash = new PublicKey(Buffer.alloc(32)).toBase58();
  const msg = new TransactionMessage({
    payerKey: authority,
    recentBlockhash: zeroBlockhash,
    instructions: [start, liqIx, end],
  }).compileToV0Message([]);
  const tx = new VersionedTransaction(msg);
  tx.signatures = [new Uint8Array(64)];
  return Buffer.from(tx.serialize()).toString('base64');
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (!endpoint) throw new Error('HELIUS_RPC');
  const liquidatorMa = new PublicKey(process.env.LIQUIDATOR_MA ?? DEFAULT_LIQUIDATOR_MA);
  const authority = new PublicKey(process.env.AUTHORITY ?? DEFAULT_AUTHORITY);
  const topn = Number.parseInt(process.env.TOPN ?? '', 10) || 5;

  console.error('[mp] scanning marginfi group …');
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
    const na = a.balances.filter((b) => b.assetShares > 0.0).length;
    const nl = a.balances.filter((b) => b.liabilityShares > 0.0).length;
    if (na + nl <= 2) continue; // multi-position: more than one collateral OR more than one debt
    accts.push([pk, a]);
  }

  const bankPkSet = new Map<string, PublicKey>();
  for (const [, a] of accts) for (const b of a.balances) bankPkSet.set(b.bankPk.toBase58(), b.bankPk);
  const bankPks = [...bankPkSet.values()];
  const bankRaw = await getMultiple(endpoint, bankPks);
  const banks: BankMap = new Map();
  const oracleOf = new Map<string, PublicKey>();
  for (const [pkStr, raw] of bankRaw) {
    const bk = decodeBank(raw);
    if (bk) {
      oracleOf.set(pkStr, bk.oracleKey);
      banks.set(pkStr, bk);
    }
  }
  const oraclePkSet = new Map<string, PublicKey>();
  for (const oc of oracleOf.values()) oraclePkSet.set(oc.toBase58(), oc);
  const oraclePks = [...oraclePkSet.values()];
  const slotResp = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getSlot',
    params: [{ commitment: 'confirmed' }],
  });
  const slot = BigInt(typeof slotResp?.result === 'number' ? slotResp.result : 0);
  const oracleRaw = await getMultiple(endpoint, oraclePks);
  const prices: PriceMap = new Map();
  for (const [pkStr, raw] of oracleRaw) {
    const usd = decodeOraclePriceFresh(raw, slot, DEFAULT_MAX_SB_STALE_SLOTS);
    if (usd === null) continue;
    for (const [bk, oc] of oracleOf) {
      if (oc.toBase58() === pkStr) prices.set(bk, usd);
    }
  }

  // Rank multi-position accounts by collateral USD (fresh-priced, complete health).
  const ranked: Array<{ pk: PublicKey; a: MarginfiAccount; coll: number; ratio: number }> = [];
  for (const [pk, a] of accts) {
    const h = maintenanceHealth(a, banks, prices);
    if (h.missing > 0 || h.health.weightedAssets <= 0.0) continue;
    let coll = 0.0;
    for (const b of a.balances) {
      if (!(b.assetShares > 0.0)) continue;
      const bk = banks.get(b.bankPk.toBase58());
      const px = prices.get(b.bankPk.toBase58());
      if (!bk || px === undefined) continue;
      coll += (b.assetShares * bk.assetShareValue) / 10 ** bk.mintDecimals * px;
    }
    ranked.push({ pk, a, coll, ratio: healthRatio(h.health) });
  }
  ranked.sort((a, b) => b.coll - a.coll);
  console.error(`[mp] ${ranked.length} multi-position accounts (complete health); probing top ${topn}\n`);

  for (const { pk, a, coll, ratio } of ranked.slice(0, topn)) {
    // Leg picker: choose the (collateral, debt) pair maximizing seized-collateral
    // USD, restricted to a wired debt mint (USDC/USDT/wSOL).
    const assets = a.balances.filter((b) => b.assetShares > 0.0);
    const liabs = a.balances.filter((b) => b.liabilityShares > 0.0);
    const debtCandidates = liabs.filter((b) => {
      const bk = banks.get(b.bankPk.toBase58());
      return bk ? isDebtMint(bk.mint) : false;
    });
    const debtValue = (b: Balance): number => {
      const bk = banks.get(b.bankPk.toBase58());
      const px = prices.get(b.bankPk.toBase58());
      if (!bk || px === undefined) return 0.0;
      return (b.liabilityShares * bk.liabilityShareValue) / 10 ** bk.mintDecimals * px;
    };
    const collValue = (b: Balance): number => {
      const bk = banks.get(b.bankPk.toBase58());
      const px = prices.get(b.bankPk.toBase58());
      if (!bk || px === undefined) return 0.0;
      return (b.assetShares * bk.assetShareValue) / 10 ** bk.mintDecimals * px;
    };
    let debtLeg: Balance | undefined;
    for (const b of debtCandidates) {
      if (debtLeg === undefined || debtValue(b) > debtValue(debtLeg)) debtLeg = b;
    }
    let collLeg: Balance | undefined;
    for (const b of assets) {
      if (collLeg === undefined || collValue(b) > collValue(collLeg)) collLeg = b;
    }
    if (debtLeg === undefined || collLeg === undefined) {
      console.log(`  ${pk.toBase58().slice(0, 8)} coll≈$${coll.toFixed(0)} ratio ${ratio.toFixed(3)}  [SKIP: no wired-debt leg]`);
      continue;
    }
    const assetBank = collLeg.bankPk;
    const liabBank = debtLeg.bankPk;
    const abk = banks.get(assetBank.toBase58())!;
    const native = collLeg.assetShares * abk.assetShareValue;
    const seize = BigInt(Math.trunc(native * 0.02)); // 2% rung — just proving composition
    const tp = (await mintOwner(endpoint, abk.mint)) ?? new PublicKey('TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA');
    const na = assets.length;
    const nl = liabs.length;
    const gate = gateTxB64(authority, liquidatorMa, tp, pk, a, assetBank, liabBank, seize, oracleOf);
    if (gate === null) {
      console.log(`  ${pk.toBase58().slice(0, 8)} [${na}c/${nl}d] coll≈$${coll.toFixed(0)}  [tx build failed]`);
      continue;
    }
    const sim = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'simulateTransaction',
      params: [gate, { sigVerify: false, replaceRecentBlockhash: true, commitment: 'processed', encoding: 'base64' }],
    });
    const res = sim?.result?.value;
    const err = res?.err;
    if (process.env.LOGS === '1') {
      console.error(`  --- ${pk.toBase58().slice(0, 8)} RAW sim response ---`);
      console.error(JSON.stringify(sim ?? null, null, 2));
    }
    const ie = err?.InstructionError;
    const idx: number | undefined = Array.isArray(ie) ? ie[0] : undefined;
    const code: number | undefined = Array.isArray(ie) ? ie[1]?.Custom : undefined;
    // An RPC-level error means the sim never ran (bad params/tx) — must never
    // be read as "no instruction error → fireable".
    if (sim?.error != null) {
      console.log(
        `  ${pk.toBase58().slice(0, 8)} [${assets.length}c/${liabs.length}d] coll≈$${coll.toFixed(0)}  →  ⚠ RPC error, sim did not run: ${sim.error?.message ?? ''}`,
      );
      continue;
    }
    let verdict: string;
    if (err === null) {
      verdict = '✅ err=null — FIREABLE NOW (real multi-position opportunity)';
    } else if (idx === 1 && code === 6068) {
      verdict = '✅ WIRING OK — liquidate ix ran, HealthyAccount(6068) at this leg/size';
    } else if (idx === 1 && code !== undefined) {
      verdict = `✅ WIRING OK — liquidate ix ran, reverted in-ix Custom(${code})`;
    } else if (idx !== undefined) {
      verdict = `⚠ error at ix ${idx} (not the liquidate ix) code=${code ?? 'null'} — INVESTIGATE`;
    } else {
      verdict = `? unclassified: ${JSON.stringify(err)}`;
    }
    console.log(`  ${pk.toBase58().slice(0, 8)} [${na}c/${nl}d] coll≈$${coll.toFixed(0)} ratio ${ratio.toFixed(3)}  seize2%=${seize}  →  ${verdict}`);
  }
  console.error(
    "\n[mp] If every top account shows 'WIRING OK' (ix 1), single-leg liquidation composes on\n     multi-position accounts and the fix is purely a leg-PICKER in try_arm, not an N-leg tx rewrite.",
  );
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
