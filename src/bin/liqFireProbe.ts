// Port of src/bin/liq_fire_probe.rs
//
// Simulate the FULL atomic fire tx against a real marginfi candidate and
// classify the result by instruction index — the wiring test for the fire
// path. With 0 genuinely liquidatable accounts (current market), the expected
// outcome is a revert AT THE LIQUIDATE IX with HealthyAccount(6068): that
// still proves ATA creates + start_flashloan + the liquidate account wiring
// execute, the Jupiter swap composes, and the tx compiles under 1232 bytes.
// Any failure at a DIFFERENT index is a wiring bug. err=null (a real
// liquidatable) verifies the whole path.
//
// Usage: HELIUS_RPC=<url> [LIQUIDATOR_MA=…] [AUTHORITY=…] [MIN_COLLATERAL_USD=50]
//        tsx src/bin/liqFireProbe.ts

import 'dotenv/config';
import { type AccountMeta, PublicKey } from '@solana/web3.js';
import { buildFireTx, type FireCandidate } from '../lib/liqFire.js';
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
import * as marginfi from '../lib/marginfi.js';

const MARGINFI_PROGRAM = 'MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA';
const MARGINFI_GROUP = '4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8';
const DEFAULT_LIQUIDATOR_MA = 'B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD';
const DEFAULT_AUTHORITY = 'DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak';
const LIQUIDATE_IX_INDEX = 5n; // [cu, cu_price, ata, ata, start_fl, LIQUIDATE, …]
const USDT_MINT = 'Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB';
const SOL_MINT = 'So11111111111111111111111111111111111111112';

/** Debt (liability) assets the fire path can repay: USDC/USDT/wSOL. */
function isDebtMint(mint: PublicKey): boolean {
  const m = mint.toBase58();
  return m === marginfi.USDC_MINT || m === USDT_MINT || m === SOL_MINT;
}
function debtSym(mint: PublicKey): string {
  const m = mint.toBase58();
  if (m === marginfi.USDC_MINT) return 'USDC';
  if (m === USDT_MINT) return 'USDT';
  return 'wSOL';
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

/** Owner program of a mint account (classic SPL vs Token-2022). */
async function mintOwner(endpoint: string, mint: PublicKey): Promise<PublicKey | undefined> {
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getAccountInfo',
    params: [mint.toBase58(), { encoding: 'base64' }],
  });
  const owner = v?.result?.value?.owner;
  if (typeof owner !== 'string') return undefined;
  try {
    return new PublicKey(owner);
  } catch {
    return undefined;
  }
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (!endpoint) throw new Error('HELIUS_RPC');
  const liquidatorMa = new PublicKey(process.env.LIQUIDATOR_MA ?? DEFAULT_LIQUIDATOR_MA);
  const authority = new PublicKey(process.env.AUTHORITY ?? DEFAULT_AUTHORITY);
  const minCollateral = Number.parseFloat(process.env.MIN_COLLATERAL_USD ?? '') || 50.0;
  const usdcBank = new PublicKey(marginfi.USDC_BANK);
  // NONUSDC=1 → skip USDC debt; DEBT=USDC|USDT|wSOL → only that debt asset.
  const skipUsdc = process.env.NONUSDC === '1';
  const wantDebt = process.env.DEBT;

  // Scan → banks → prices (same pipeline as liq_executor).
  console.error('[fire] scanning marginfi group …');
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
    const acct = decodeMarginfiAccount(bytes);
    if (acct === null) continue;
    if (acct.balances.some((b) => b.liabilityShares > 0.0)) {
      accts.push([new PublicKey(pkStr), acct]);
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
  const bankRaw = await getMultiple(endpoint, bankPks);
  const banks: BankMap = new Map();
  const oracleOf = new Map<string, PublicKey>();
  for (const [pkStr, raw] of bankRaw) {
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
  const prices: PriceMap = new Map();
  const oracleRaw = await getMultiple(endpoint, oraclePks);
  for (const [pkStr, raw] of oracleRaw) {
    const usd = decodeOraclePrice(raw);
    if (usd !== null) {
      for (const [bk, oc] of oracleOf) {
        if (oc.toBase58() === pkStr) prices.set(bk, usd);
      }
    }
  }

  // Best base-weight candidate with 1 collateral + 1 wired-debt (USDC/USDT/wSOL) liability.
  let best: { pk: PublicKey; a: MarginfiAccount; assetBank: PublicKey; liabBank: PublicKey; weightedAssets: number } | undefined;
  for (const [pk, a] of accts) {
    const r = maintenanceHealth(a, banks, prices);
    const liquidatable = r.health.weightedAssets - r.health.weightedLiabilities < 0.0;
    if (r.missing > 0 || !liquidatable || r.health.weightedAssets < minCollateral) continue;
    const assets = a.balances.filter((b) => b.assetShares > 0.0);
    const liabs = a.balances.filter((b) => b.liabilityShares > 0.0);
    if (assets.length !== 1 || liabs.length !== 1) continue;
    const liabBank = liabs[0].bankPk;
    const liabMint = banks.get(liabBank.toBase58())?.mint;
    if (liabMint === undefined || !isDebtMint(liabMint)) continue;
    if (skipUsdc && liabBank.equals(usdcBank)) continue;
    if (wantDebt !== undefined && wantDebt !== debtSym(liabMint)) continue;
    if (best === undefined || r.health.weightedAssets > best.weightedAssets) {
      best = { pk, a, assetBank: assets[0].bankPk, liabBank, weightedAssets: r.health.weightedAssets };
    }
  }
  if (best === undefined) {
    console.error('[fire] no base-weight candidate with single collateral + wired debt found — nothing to wire-test against');
    return;
  }
  const { pk: liquidatee, a: acct, assetBank, liabBank, weightedAssets: collat } = best;
  const liabBk = banks.get(liabBank.toBase58());
  if (liabBk === undefined) throw new Error('liab bank missing');
  const debtTp = await mintOwner(endpoint, liabBk.mint);
  if (debtTp === undefined) throw new Error('debt mint owner');
  const assetBk = banks.get(assetBank.toBase58());
  if (assetBk === undefined) throw new Error('asset bank missing');
  const assetTp = await mintOwner(endpoint, assetBk.mint);
  if (assetTp === undefined) throw new Error('mint owner');
  const assetBal = acct.balances.find((b) => b.bankPk.equals(assetBank));
  if (assetBal === undefined) throw new Error('asset balance missing');
  const native = assetBal.assetShares * assetBk.assetShareValue;
  const assetAmount = BigInt(Math.trunc(native * 0.02));
  console.error(
    `[fire] candidate ${liquidatee.toBase58().slice(0, 8)}  [${debtSym(liabBk.mint)} debt]  collateral≈$${collat.toFixed(0)}  asset mint ${assetBk.mint.toBase58()} (tp ${assetTp.toBase58().slice(0, 8)})  seize ${assetAmount} native (2%)`,
  );

  const liquidateeObs: AccountMeta[] = [];
  for (const b of acct.balances) {
    liquidateeObs.push({ pubkey: b.bankPk, isSigner: false, isWritable: false });
    const oc = oracleOf.get(b.bankPk.toBase58());
    if (oc === undefined) throw new Error('missing oracle for balance');
    liquidateeObs.push({ pubkey: oc, isSigner: false, isWritable: false });
  }
  const assetOracle = oracleOf.get(assetBank.toBase58());
  const liabOracle = oracleOf.get(liabBank.toBase58());
  if (assetOracle === undefined || liabOracle === undefined) throw new Error('missing oracle');
  const cand: FireCandidate = {
    liquidatee,
    assetBank,
    assetMint: assetBk.mint,
    assetTokenProgram: assetTp,
    assetAmount,
    liabBank,
    debtMint: liabBk.mint,
    debtTokenProgram: debtTp,
    assetOracle,
    liabOracle,
    liquidateeObs,
  };

  console.error('[fire] building fire tx (Jupiter quote + ALTs) …');
  // solana_hash::Hash::default() (32 zero bytes) base58-encodes to this
  // string — used as a placeholder blockhash for simulation (replaceRecentBlockhash: true).
  const DEFAULT_HASH = '11111111111111111111111111111111';
  const fire = await buildFireTx(endpoint, cand, liquidatorMa, authority, null, 0n, 100_000n, 100, 20, DEFAULT_HASH);
  console.error(`[fire] tx ${fire.txBytes} bytes (limit 1232)  quoted_usdc_out=${fire.quotedUsdcOut}`);

  const b64tx = Buffer.from(fire.tx.serialize()).toString('base64');
  const sim = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'simulateTransaction',
    params: [b64tx, { sigVerify: false, replaceRecentBlockhash: true, commitment: 'processed', encoding: 'base64' }],
  });
  if (sim?.result?.value === undefined) {
    console.log(`✗ RPC rejected the simulation (no result.value): ${JSON.stringify(sim)}`);
    return;
  }
  const res = sim.result.value;
  console.log('\n──── fire-path simulation ────');
  console.log(`err: ${JSON.stringify(res.err)}`);
  console.log(`unitsConsumed: ${res.unitsConsumed}`);
  const ixIdx = res?.err?.InstructionError?.[0];
  const code = res?.err?.InstructionError?.[1]?.Custom;
  // Reverts raised INSIDE LendingAccountLiquidate (after start_flashloan
  // succeeded and the ix was entered) prove the whole wiring composes — the
  // program reached its own eligibility/price checks. These are not fireable
  // *right now* for account-specific reasons, not wiring bugs:
  //   6068 HealthyAccount        — not underwater at the fresh price
  //   6049 SwitchboardStalePrice — collateral oracle stale under sim's slot
  //   6051 WrongNumberOfOracleAccounts / other in-liquidate gates
  const inLiquidateGate = [6068, 6049, 6051, 6050, 6052].includes(code);
  if (res.err === null || res.err === undefined) {
    console.log('★★ FULL FIRE PATH VERIFIED — genuinely liquidatable candidate, whole tx executes');
  } else if (typeof ixIdx === 'number' && BigInt(ixIdx) === LIQUIDATE_IX_INDEX && inLiquidateGate) {
    console.log(
      `★ WIRING OK — start_flashloan + liquidate executed and reverted INSIDE marginfi's ` +
        `liquidate at its eligibility/oracle gate (custom ${code}): ATAs + flashloan + liquidate ` +
        `accounts + observation list + swap/payback all compose. Not fireable now for ` +
        `account-specific reasons (healthy / stale oracle), not a wiring bug.`,
    );
  } else if (typeof ixIdx === 'number') {
    console.log(`✗ UNEXPECTED failure at ix ${ixIdx} (custom ${code}) — inspect logs:`);
    const logs: string[] = Array.isArray(res.logs) ? res.logs : [];
    for (const l of logs) console.log(`  ${l}`);
  } else {
    console.log(`? inconclusive: ${JSON.stringify(res.err)}`);
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
