// Port of src/bin/jup_alt_print.rs
//
// Print the FIXED accounts a Jupiter Lend (Fluid) liquidation fire tx needs in
// its dedicated address-lookup-table (JUP_ALT), so the marginfi-flash-loan-
// wrapped `liquidate`+swap+repay tx fits under the 1232-byte single-packet limit.
// Without an ALT the wrapped tx is ~1723B (see jupiter_fire_probe STAGE 5);
// moving these ~24 fixed accounts off the static keys (~31B saved each) brings it
// under 1232, exactly as SAVE_ALT / the Kamino ALT do for their paths.
//
// What's FIXED (goes in the ALT) vs per-fire (stays inline / from Jupiter's ALTs):
//   FIXED  — programs + sysvars, the marginfi USDC flash-loan set, the Fluid
//            liquidity global PDA, the USDC *borrow-side* per-mint liquidity
//            accounts (reserve / rate_model / liquidity token vault — identical
//            for every USDC-debt vault), the Fluid oracle program, and the
//            wallet + its USDC ATA.
//   PER-FIRE — vault_config/state, oracle (+ its price sources), the collateral
//            (supply) mint and its reserve/position/rate_model/token vault, the
//            vault's borrow position, new_branch, and the tick/branch/tick_has_debt
//            remaining accounts. These vary per vault; the collateral swap route
//            rides Jupiter's own ALTs. (A future per-collateral ALT could fold the
//            common collateral side in too, like the Kamino top-K approach.)
//
// Setup (one-time; ALT creation needs wallet signing — do this on the box):
//   solana address-lookup-table create --keypair ~/arb-keypair.json -u <rpc>
//   solana address-lookup-table extend <TABLE> --addresses "$(jup_alt_print | paste -sd, -)" …
// Then export JUP_ALT=<TABLE> for liq_jupiter_executor / jupiter_fire_probe.
//
// Usage: [HELIUS_RPC=<url>] [AUTHORITY=<pk>] [LIQUIDATOR_MA=<pk>]
//        tsx src/bin/jupAltPrint.ts

import 'dotenv/config';
import bs58 from 'bs58';
import { PublicKey } from '@solana/web3.js';
import { ataFor } from '../lib/flashloan.js';
import * as jupiter from '../lib/jupiter.js';
import { liquidityPda, rateModelPda, reservePda } from '../lib/jupiterMath.js';
import * as marginfi from '../lib/marginfi.js';

const DEFAULT_AUTHORITY = 'DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak';
const DEFAULT_LIQUIDATOR_MA = 'B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD';
const TOKEN = 'TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA';
const TOKEN22 = 'TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb';
const ATA_PROGRAM = 'ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL';
const SYSTEM = '11111111111111111111111111111111';
const COMPUTE_BUDGET = 'ComputeBudget111111111111111111111111111111';
const JUPITER_PROGRAM = 'JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4';
// Fluid oracle program (owner of every vault `oracle` account; constant on-chain,
// verified: it is both the `oracle_program` config field and the oracle owner).
const ORACLE_PROGRAM = 'jupnw4B6Eqs7ft6rxpzYLJZYSnrpRgPcr589n5Kv4oc';

async function rpc(endpoint: string, body: unknown): Promise<any | undefined> {
  try {
    const res = await fetch(endpoint, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body),
    });
    return await res.json();
  } catch {
    return undefined;
  }
}

/**
 * If RPC is available, read the oracle program id as the OWNER of any vault's
 * oracle account (ground truth); returns undefined otherwise.
 */
async function liveOracleProgram(): Promise<string | undefined> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (!endpoint) return undefined;
  const disc58 = bs58.encode(jupiter.VAULT_CONFIG_DISC);
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getProgramAccounts',
    params: [
      jupiter.VAULTS_PROGRAM,
      { encoding: 'base64', dataSlice: { offset: 0, length: 0 }, filters: [{ memcmp: { offset: 0, bytes: disc58 } }] },
    ],
  });
  if (v === undefined) return undefined;
  const first = Array.isArray(v?.result) ? v.result[0] : undefined;
  const cfgPk: string | undefined = first?.pubkey;
  if (typeof cfgPk !== 'string') return undefined;
  const cv = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getAccountInfo',
    params: [cfgPk, { encoding: 'base64' }],
  });
  if (cv === undefined) return undefined;
  const b64: string | undefined = cv?.result?.value?.data?.[0];
  if (typeof b64 !== 'string') return undefined;
  const raw = Buffer.from(b64, 'base64');
  const cfg = jupiter.VaultConfig.decode(raw);
  if (cfg === undefined) return undefined;
  const ov = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getAccountInfo',
    params: [cfg.oracle.toBase58(), { encoding: 'base64' }],
  });
  if (ov === undefined) return undefined;
  const owner: string | undefined = ov?.result?.value?.owner;
  return owner;
}

async function main(): Promise<void> {
  const authority = new PublicKey(process.env.AUTHORITY ?? DEFAULT_AUTHORITY);
  const liquidatorMa = new PublicKey(process.env.LIQUIDATOR_MA ?? DEFAULT_LIQUIDATOR_MA);
  const usdc = new PublicKey(marginfi.USDC_MINT);
  const usdcBank = new PublicKey(marginfi.USDC_BANK);
  const token = new PublicKey(TOKEN);

  // Fluid liquidity USDC borrow-side (identical for every USDC-debt vault).
  const liquidity = liquidityPda();
  const usdcReserve = reservePda(usdc);
  const usdcRateModel = rateModelPda(usdc);
  const usdcVaultTokAcct = ataFor(liquidity, usdc, token); // vault_borrow_token_account

  // Fluid oracle program: resolve live from any vault's oracle owner if we have
  // RPC (authoritative); else fall back to the known constant.
  const oracleProgram = (await liveOracleProgram()) ?? ORACLE_PROGRAM;

  const addrs: string[] = [
    // ── programs + sysvars ──
    jupiter.VAULTS_PROGRAM,
    jupiter.LIQUIDITY_PROGRAM,
    oracleProgram,
    JUPITER_PROGRAM,
    marginfi.MARGINFI_PROGRAM,
    TOKEN,
    TOKEN22,
    ATA_PROGRAM,
    SYSTEM,
    COMPUTE_BUDGET,
    // ── marginfi USDC flash-loan fixed set ──
    marginfi.MARGINFI_GROUP,
    marginfi.USDC_BANK,
    marginfi.bankLiquidityVault(usdcBank).toBase58(),
    marginfi.bankLiquidityVaultAuth(usdcBank).toBase58(),
    marginfi.bankInsuranceVault(usdcBank).toBase58(),
    // ── Fluid liquidity USDC borrow-side (per-mint; same for all USDC vaults) ──
    liquidity.toBase58(),
    usdcReserve.toBase58(),
    usdcRateModel.toBase58(),
    usdcVaultTokAcct.toBase58(),
    // ── wallet + USDC ──
    marginfi.USDC_MINT,
    authority.toBase58(),
    liquidatorMa.toBase58(),
    ataFor(authority, usdc, token).toBase58(),
  ];

  // Dedup, preserve order.
  const seen = new Set<string>();
  let n = 0;
  for (const a of addrs) {
    if (seen.has(a)) continue;
    seen.add(a);
    console.log(a);
    n += 1;
  }
  console.error(`[jup-alt] ${n} fixed accounts. Extend the JUP_ALT with these, then export JUP_ALT=<table>.`);
  console.error(`[jup-alt] liquidity global PDA = ${liquidity.toBase58()}`);
  console.error(`[jup-alt] USDC reserve = ${usdcReserve.toBase58()}  rate_model = ${usdcRateModel.toBase58()}  vault_tok_acct = ${usdcVaultTokAcct.toBase58()}`);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
