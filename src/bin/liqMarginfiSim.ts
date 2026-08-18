// Port of src/bin/liq_marginfi_sim.rs
//
// marginfi liquidation SIMULATION probe — assembles the flashloan-wrapped
// liquidate against a REAL liquidatable account and simulates it on mainnet
// (sigVerify=false, replaceRecentBlockhash). Proves the instruction wiring
// executes: we want to see the LendingAccountLiquidate handler run (state
// change or a meaningful marginfi error), NOT a deserialize/account error.
//
// Picks the top liquidatable borrower with exactly one collateral + one debt
// bank (simplest case). Tx = [start_flashloan, liquidate(2% of collateral),
// end_flashloan]; end_flashloan re-checks health over both liquidator balances.
//
// Usage: HELIUS_RPC=<url> [LIQUIDATOR_MA=<acct>] [AUTHORITY=<pk>] npx tsx src/bin/liqMarginfiSim.ts

import 'dotenv/config';
import { PublicKey, TransactionMessage, VersionedTransaction, type AccountMeta } from '@solana/web3.js';
import {
  decodeBank,
  decodeMarginfiAccount,
  decodeOraclePrice,
  maintenanceHealth,
  healthLiquidatable,
  MA_SIZE,
  type Bank,
  type BankMap,
  type MarginfiAccount,
  type PriceMap,
} from '../lib/liquidation.js';
import { startFlashloan, endFlashloan, lendingAccountLiquidate } from '../lib/marginfi.js';

const MARGINFI_PROGRAM = 'MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA';
const MARGINFI_GROUP = '4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8';
const TOKEN_PROGRAM = 'TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA';
// Liquidator marginfi account created earlier (authority = arb wallet).
const DEFAULT_LIQUIDATOR_MA = 'B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD';
const DEFAULT_AUTHORITY = 'DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak';

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
  const liquidatorMa = new PublicKey(process.env.LIQUIDATOR_MA ?? DEFAULT_LIQUIDATOR_MA);
  const authority = new PublicKey(process.env.AUTHORITY ?? DEFAULT_AUTHORITY);
  const tp = new PublicKey(TOKEN_PROGRAM);

  // 1) Scan group → borrowers.
  console.error('[sim] scanning marginfi group …');
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
  // keep account pubkey alongside the decoded account.
  const accts: Array<[PublicKey, MarginfiAccount]> = [];
  for (const e of entries) {
    const pkStr = e?.pubkey;
    if (typeof pkStr !== 'string') continue;
    let pk: PublicKey;
    try {
      pk = new PublicKey(pkStr);
    } catch {
      continue;
    }
    const raw = b64(e?.account?.data);
    if (!raw) continue;
    const a = decodeMarginfiAccount(raw);
    if (!a) continue;
    if (!a.balances.some((b) => b.liabilityShares > 0.0)) continue;
    accts.push([pk, a]);
  }
  console.error(`[sim] ${accts.length} borrowers`);

  // 2) Banks + oracle prices.
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
  const oracleRaw = await getMultiple(endpoint, oraclePks);
  const oprice = new Map<string, number>();
  for (const [pkStr, raw] of oracleRaw) {
    const usd = decodeOraclePrice(raw);
    if (usd !== null) oprice.set(pkStr, usd);
  }
  const prices: PriceMap = new Map();
  for (const [bk, oc] of oracleOf) {
    const p = oprice.get(oc.toBase58());
    if (p !== undefined) prices.set(bk, p);
  }

  // 3) Pick top liquidatable with exactly 1 collateral + 1 debt bank, both priced.
  let best: { pk: PublicKey; a: MarginfiAccount; assetBank: PublicKey; liabBank: PublicKey; collateralUsd: number } | undefined;
  for (const [pk, a] of accts) {
    const r = maintenanceHealth(a, banks, prices);
    if (r.missing > 0 || !healthLiquidatable(r.health)) continue;
    const assets = a.balances.filter((b) => b.assetShares > 0.0);
    const liabs = a.balances.filter((b) => b.liabilityShares > 0.0);
    if (assets.length !== 1 || liabs.length !== 1) continue;
    if (r.health.weightedAssets < 50.0) continue;
    if (best === undefined || r.health.weightedAssets > best.collateralUsd) {
      best = { pk, a, assetBank: assets[0].bankPk, liabBank: liabs[0].bankPk, collateralUsd: r.health.weightedAssets };
    }
  }
  if (best === undefined) {
    console.error('[sim] no single-collateral/single-debt liquidatable account found');
    return;
  }
  const { pk: liquidatee, a: acct, assetBank, liabBank, collateralUsd: collat } = best;
  const assetBk = banks.get(assetBank.toBase58())!;
  const assetOracle = oracleOf.get(assetBank.toBase58())!;
  const liabOracle = oracleOf.get(liabBank.toBase58())!;
  // asset_amount = 2% of the liquidatee's collateral native units.
  const assetBal = acct.balances.find((b) => b.bankPk.equals(assetBank))!;
  const native = assetBal.assetShares * assetBk.assetShareValue;
  const assetAmount = BigInt(Math.trunc(native * 0.02));
  // Diagnostic: reconcile my weights vs marginfi's on-chain calc.
  const px = prices.get(assetBank.toBase58()) ?? 0.0;
  const dec = 10 ** assetBk.mintDecimals;
  const rawVal = (native / dec) * px;
  console.error(`[sim] asset_bank ${assetBank.toBase58()} decimals=${assetBk.mintDecimals} price=$${px.toFixed(4)}`);
  console.error(`[sim] asset_weight_init=${assetBk.assetWeightInit.toFixed(4)} asset_weight_maint=${assetBk.assetWeightMaint.toFixed(4)}`);
  console.error(
    `[sim] raw collateral value=$${rawVal.toFixed(0)}  × init=${(rawVal * assetBk.assetWeightInit).toFixed(0)}  × maint=${(rawVal * assetBk.assetWeightMaint).toFixed(0)}  (marginfi said assets=$39558)`,
  );
  console.error(`[sim] liquidatee ${liquidatee.toBase58().slice(0, 8)} collateral=$${collat.toFixed(0)}`);
  console.error(
    `[sim] asset_bank ${assetBank.toBase58().slice(0, 8)}… liab_bank ${liabBank.toBase58().slice(0, 8)}… asset_amount=${assetAmount} (2% of ${native.toFixed(0)} native)`,
  );

  // 4) Build flashloan-wrapped [start_fl, liquidate, end_fl].
  // liquidatee obs: for each active balance [bank, oracle] in slot order.
  const liquidateeObs: AccountMeta[] = [];
  for (const b of acct.balances) {
    liquidateeObs.push({ pubkey: b.bankPk, isSigner: false, isWritable: false });
    liquidateeObs.push({ pubkey: oracleOf.get(b.bankPk.toBase58())!, isSigner: false, isWritable: false });
  }
  const endIndex = 2n; // ixs: 0 start_fl, 1 liquidate, 2 end_fl
  const start = startFlashloan(liquidatorMa, authority, endIndex);
  const liqIx = lendingAccountLiquidate(assetBank, liabBank, liquidatorMa, authority, liquidatee, tp, assetAmount, assetOracle, liabOracle, liquidateeObs);
  // end_flashloan obs = liquidator's post-liquidation balances: seized asset + new liab.
  const endObs: AccountMeta[] = [
    { pubkey: assetBank, isSigner: false, isWritable: false },
    { pubkey: assetOracle, isSigner: false, isWritable: false },
    { pubkey: liabBank, isSigner: false, isWritable: false },
    { pubkey: liabOracle, isSigner: false, isWritable: false },
  ];
  const end = endFlashloan(liquidatorMa, authority, endObs);

  // 5) Assemble a v0 tx + simulate (sigVerify=false, replaceRecentBlockhash).
  const bhResp = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getLatestBlockhash',
    params: [{ commitment: 'finalized' }],
  });
  const bh = bhResp?.result?.value?.blockhash;
  if (typeof bh !== 'string') throw new Error('blockhash');
  const msg = new TransactionMessage({
    payerKey: authority,
    recentBlockhash: bh,
    instructions: [start, liqIx, end],
  }).compileToV0Message([]);
  const tx = new VersionedTransaction(msg);
  tx.signatures = [new Uint8Array(64)];
  const b64tx = Buffer.from(tx.serialize()).toString('base64');

  console.error('[sim] simulating …');
  const sim = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'simulateTransaction',
    params: [b64tx, { sigVerify: false, replaceRecentBlockhash: true, commitment: 'processed', encoding: 'base64' }],
  });
  if (sim === null) {
    console.error('[sim] no response');
    return;
  }
  const res = sim?.result?.value;
  console.log('\n──── simulation result ────');
  console.log(`err: ${JSON.stringify(res?.err ?? null)}`);
  const logs = res?.logMessages;
  if (Array.isArray(logs)) {
    for (const l of logs) console.log(`  ${typeof l === 'string' ? l : ''}`);
  } else {
    console.log(`  (no logs — ${JSON.stringify(sim)})`);
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
