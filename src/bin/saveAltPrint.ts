// Port of src/bin/save_alt_print.rs
//
// Print the FIXED accounts a Save (Solend) liquidation fire tx needs in its
// dedicated address-lookup-table (SAVE_ALT), so the JupLend-flash-loan-wrapped
// `liquidate_and_redeem` + swap + payback tx fits under the 1232-byte
// single-packet limit. Without an ALT the wrapped cross-mint tx is ~1716–1936B
// (see the Save widen PR / saveFireProbe); moving these fixed accounts off the
// static keys (~31B saved each) brings it under 1232 — exactly as jupAltPrint
// / the Kamino ALT do for their paths.
//
// What's FIXED (goes in the ALT) vs per-fire (stays inline / rides Jupiter's ALTs):
//   FIXED  — programs + sysvars; the Solend main pool + its lending-market
//            authority; and, for EACH supported debt asset (USDC/USDT/wSOL): the
//            Solend debt (repay) reserve + its sub-accounts (liquidity supply,
//            pyth/switchboard oracles, collateral mint/supply, fee receiver), the
//            JupLend flash-market account set (reserve/token/rate_model/vault +
//            globals), and the wallet's debt ATA. A given fire uses only ONE debt
//            asset, but the ALT holds all three so any is covered.
//   PER-FIRE — the obligation, the COLLATERAL (withdraw) reserve + its
//            sub-accounts, and the collateral→debt swap route (rides Jupiter's
//            own ALTs). These vary per liquidation.
//
// The account lists are pulled from the REAL ix builders (flashloan.borrow +
// the decoded Reserve fields), so they are guaranteed to match what
// buildSaveFireTx actually references — no hand-maintained duplicate list.
//
// Setup (one-time; ALT creation needs wallet signing — do this on the box):
//   solana address-lookup-table create --keypair ~/arb-keypair.json -u <rpc>
//   solana address-lookup-table extend <TABLE> --addresses "$(saveAltPrint | paste -sd, -)" …
// Then export SAVE_ALT=<TABLE> for liqSaveExecutor / saveFireProbe.
//
// Usage: HELIUS_RPC=<url> [AUTHORITY=<pk>] npx tsx src/bin/saveAltPrint.ts

import 'dotenv/config';
import { PublicKey } from '@solana/web3.js';
import * as flashloan from '../lib/flashloan.js';
import * as save from '../lib/save.js';
import { decodeReserve, type Reserve } from '../lib/save.js';

const DEFAULT_AUTHORITY = 'DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak';
const TOKEN = 'TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA';
const TOKEN22 = 'TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb';
const ATA_PROGRAM = 'ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL';
const SYSTEM = '11111111111111111111111111111111';
const COMPUTE_BUDGET = 'ComputeBudget111111111111111111111111111111';
const JUPITER_PROGRAM = 'JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4';

async function rpc(endpoint: string, body: unknown): Promise<any | null> {
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
    await new Promise((r) => setTimeout(r, 400 << attempt));
  }
  return null;
}

async function getReserve(endpoint: string, pk: PublicKey): Promise<Reserve | null> {
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getAccountInfo',
    params: [pk.toBase58(), { encoding: 'base64' }],
  });
  const b64 = v?.result?.value?.data?.[0];
  if (typeof b64 !== 'string') return null;
  let raw: Buffer;
  try {
    raw = Buffer.from(b64, 'base64');
  } catch {
    return null;
  }
  return decodeReserve(pk, raw);
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (endpoint === undefined) {
    throw new Error('set HELIUS_RPC (needed to read the debt reserves’ oracle/supply sub-accounts)');
  }
  const authority = new PublicKey(process.env.AUTHORITY ?? DEFAULT_AUTHORITY);

  const mainPool = new PublicKey(save.MAIN_POOL);

  // Ordered, deduped accumulator (preserve first-seen order for readable output).
  const seen = new Set<string>();
  const out: string[] = [];
  const push = (pk: string): void => {
    if (!seen.has(pk)) {
      seen.add(pk);
      out.push(pk);
    }
  };

  // ── programs + sysvars + Solend globals ──
  for (const s of [
    save.SOLEND_PROGRAM,
    flashloan.JUP_LEND_PROGRAM,
    JUPITER_PROGRAM,
    TOKEN,
    TOKEN22,
    ATA_PROGRAM,
    SYSTEM,
    COMPUTE_BUDGET,
    save.MAIN_POOL,
  ]) {
    push(s);
  }
  push(save.lendingMarketAuthority(mainPool).toBase58());

  // ── per debt asset (USDC/USDT/wSOL): Solend reserve sub-accounts + JupLend flash set + ATA ──
  const debts: Array<[string, string, string]> = [
    ['USDC', save.USDC_RESERVE, save.USDC_MINT],
    ['USDT', save.USDT_RESERVE, save.USDT_MINT],
    ['wSOL', save.WSOL_RESERVE, save.WSOL_MINT],
  ];
  const token = new PublicKey(TOKEN);
  for (const [label, reserveStr, mintStr] of debts) {
    const reservePk = new PublicKey(reserveStr);
    const mint = new PublicKey(mintStr);

    // Solend debt-reserve fixed sub-accounts, straight from the decoded reserve
    // (these are exactly what refreshReserve + liquidateAndRedeem reference
    // for the repay side).
    const r = await getReserve(endpoint, reservePk);
    if (r !== null) {
      for (const pk of [
        r.reserve,
        r.liquidityMint,
        r.liquiditySupply,
        r.pythOracle,
        r.switchboardOracle,
        r.collateralMint,
        r.collateralSupply,
        r.feeReceiver,
      ]) {
        push(pk.toBase58());
      }
    } else {
      console.error(
        `[save-alt] WARN could not fetch ${label} reserve ${reservePk.toBase58()} — its sub-accounts are missing from this list; re-run with a working RPC`,
      );
    }

    // JupLend flash-market fixed set — pulled from the REAL borrow ix so it
    // matches buildSaveFireTx exactly (signer/ATA/mint/reserve/token/
    // rate_model/vault + JupLend globals).
    const ix = flashloan.borrow(authority, mint, 0n);
    if (ix !== undefined) {
      for (const m of ix.keys) {
        push(m.pubkey.toBase58());
      }
    }

    // Wallet's debt ATA (classic SPL — USDC/USDT/wSOL are all classic).
    push(flashloan.ataFor(authority, mint, token).toBase58());
  }

  // ── wallet ──
  push(authority.toBase58());

  for (const a of out) {
    console.log(a);
  }
  console.error(`[save-alt] ${out.length} fixed accounts. Create the ALT + extend with these, then export SAVE_ALT=<table>.`);
  console.error(`[save-alt] lending_market_authority = ${save.lendingMarketAuthority(mainPool).toBase58()}`);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
