// Port of src/bin/mfi_reject_audit.rs
//
// Why do accounts our engine flags get rejected? Audit the REAL revert codes.
//
// liq_executor logs "chain says healthy at the actionable price" whenever the
// size-ladder sim returns Some(false) — but simulate_gate maps EVERY custom
// error to Some(false), not just HealthyAccount(6068). So that one log line
// actually hides: 6068 (genuinely healthy — if OUR maintenance_health says
// liquidatable at the SAME on-chain price, that's a health-MATH bug), 6049
// (Switchboard stale), 6210 (Kamino reserve), size guards, etc.
//
// This probe finds every account our maintenanceHealth flags liquidatable at
// FRESH on-chain prices (staleness-gated), sims the single-leg liquidate, and
// tallies the true codes so we can see the real cause distribution.
//
// Usage: HELIUS_RPC=<url> [LIQUIDATOR_MA=…] [AUTHORITY=…] npx tsx src/bin/mfiRejectAudit.ts

import 'dotenv/config';
import { PublicKey, TransactionMessage, VersionedTransaction, type AccountMeta } from '@solana/web3.js';
import {
  decodeBank,
  decodeMarginfiAccount,
  decodeOraclePriceFresh,
  maintenanceHealth,
  healthLiquidatable,
  maxStaleSlotsFor,
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

async function mintOwner(endpoint: string, mint: PublicKey): Promise<PublicKey> {
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getAccountInfo',
    params: [mint.toBase58(), { encoding: 'jsonParsed' }],
  });
  const owner = v?.result?.value?.owner;
  if (typeof owner === 'string') {
    try {
      return new PublicKey(owner);
    } catch {
      /* fall through */
    }
  }
  return new PublicKey('TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA');
}

function isDebtMint(m: PublicKey): boolean {
  const s = m.toBase58();
  return s === USDC_MINT || s === USDT_MINT || s === SOL_MINT;
}

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
  const cap = Number.parseInt(process.env.CAP ?? '', 10) || 60;

  console.error('[audit] scanning marginfi group …');
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
  // PER-BANK staleness gate (the fix): each bank's on-chain oracle_max_age.
  const prices: PriceMap = new Map();
  for (const [bankPk, oraclePk] of oracleOf) {
    const raw = oracleRaw.get(oraclePk.toBase58());
    if (!raw) continue;
    const maxAge = banks.get(bankPk)?.oracleMaxAge ?? 0;
    const maxStale = maxStaleSlotsFor(maxAge, DEFAULT_MAX_SB_STALE_SLOTS);
    const usd = decodeOraclePriceFresh(raw, slot, maxStale);
    if (usd !== null) prices.set(bankPk, usd);
  }

  // Accounts OUR maintenance_health flags liquidatable at FRESH on-chain price,
  // with a wired-debt leg (the ones try_arm would evaluate + reject).
  const flagged: Array<{ pk: PublicKey; a: MarginfiAccount; assetBank: PublicKey; liabBank: PublicKey }> = [];
  for (const [pk, a] of accts) {
    const h = maintenanceHealth(a, banks, prices);
    if (h.missing > 0 || !healthLiquidatable(h.health)) continue;
    const assetValue = (b: Balance): number => {
      const bk = banks.get(b.bankPk.toBase58());
      const px = prices.get(b.bankPk.toBase58());
      if (!bk || px === undefined) return 0.0;
      return (b.assetShares * bk.assetShareValue) / 10 ** bk.mintDecimals * px;
    };
    let asset: Balance | undefined;
    for (const b of a.balances.filter((x) => x.assetShares > 0.0)) {
      if (asset === undefined || assetValue(b) > assetValue(asset)) asset = b;
    }
    const debt = a.balances.filter((b) => b.liabilityShares > 0.0).find((b) => {
      const bk = banks.get(b.bankPk.toBase58());
      return bk ? isDebtMint(bk.mint) : false;
    });
    if (asset !== undefined && debt !== undefined) {
      flagged.push({ pk, a, assetBank: asset.bankPk, liabBank: debt.bankPk });
    }
  }
  console.error(
    `[audit] ${flagged.length} accounts our maintenance_health flags LIQUIDATABLE at fresh on-chain price (with a wired-debt leg)\n`,
  );

  const tally = new Map<string, number>();
  const examples = new Map<string, string>();
  for (const { pk, a, assetBank, liabBank } of flagged.slice(0, cap)) {
    const abk = banks.get(assetBank.toBase58())!;
    const bal = a.balances.find((b) => b.bankPk.equals(assetBank))!;
    const seize = BigInt(Math.trunc(bal.assetShares * abk.assetShareValue * 0.02));
    const tp = await mintOwner(endpoint, abk.mint);
    const gate = gateTxB64(authority, liquidatorMa, tp, pk, a, assetBank, liabBank, seize, oracleOf);
    if (gate === null) continue;
    const sim = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'simulateTransaction',
      params: [gate, { sigVerify: false, replaceRecentBlockhash: true, commitment: 'processed', encoding: 'base64' }],
    });
    const res = sim?.result?.value;
    const err = res?.err;
    let key: string;
    if (err === null) {
      key = 'null → FIREABLE (real!)';
    } else if (err !== undefined) {
      const ie = err?.InstructionError;
      const idx: number | undefined = Array.isArray(ie) ? ie[0] : undefined;
      const code: number | undefined = Array.isArray(ie) ? ie[1]?.Custom : undefined;
      if (idx === 1 && code === 6068) {
        key = '6068 HealthyAccount  (our math DISAGREES w/ chain at same price → BUG)';
      } else if (idx === 1 && code === 6049) {
        key = '6049 SwitchboardStalePrice (oracle stale — detection issue)';
      } else if (idx === 1 && code === 6210) {
        key = '6210 KaminoReserveValidation';
      } else if (idx === 1 && code !== undefined) {
        key = `in-liquidate Custom(${code})`;
      } else if (idx !== undefined) {
        key = `ix ${idx} Custom(${code ?? 'null'}) — WIRING?`;
      } else {
        key = `other: ${JSON.stringify(err)}`;
      }
    } else {
      key = 'rpc-error/no-result';
    }
    tally.set(key, (tally.get(key) ?? 0) + 1);
    if (!examples.has(key)) examples.set(key, pk.toBase58());
  }

  console.log('\n═══ REJECT-CODE DISTRIBUTION (why flagged accounts don\'t fire) ═══');
  const rows = [...tally.entries()].sort((a, b) => b[1] - a[1]);
  for (const [k, n] of rows) {
    console.log(`  ${String(n).padStart(3)}  ${k}`);
    console.log(`        e.g. ${examples.get(k) ?? ''}`);
  }
  console.log('\nKEY: 6068 = our health math over-flags vs the chain (a real logic bug to fix).');
  console.log('     6049 = stale oracle (detection; the generous 5000-slot gate lets some through).');
  console.log('     null = a genuinely fireable account we should have taken.');
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
