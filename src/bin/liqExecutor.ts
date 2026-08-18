// Port of src/bin/liq_executor.rs
//
// Production marginfi liquidation executor — continuous loop, DRY_RUN default.
//
// Detection is simulation-gated (the emode lesson: don't replicate marginfi's
// risk math off-chain — let the chain judge). Pipeline per candidate:
//
//   full scan (RESCAN_SECS) -> watch-set of near-liquidation borrowers
//   fast poll (POLL_MS): fresh watch-set accounts + bank/oracle prices
//   base-weight liquidatable? -> sim-gate [start_fl, liquidate, end_fl]
//   -> SIZE the seize by simulation ladder (largest passing fraction)
//   -> build the atomic fire tx (liquidate->withdraw->Jupiter swap->repay_all)
//   -> profit gate (quoted USDC out vs ~97.5% liability taken + tip)
//   -> FULL fire-tx simulation (ground truth for every leg incl. swap+repay)
//   -> DRY_RUN: log, LIVE: sign + submit via Helius Sender, readback P&L
//
// Self-crank mode (the stale-oracle edge): marginfi's Pyth feeds lag the true
// price by 8-44s. When an account is underwater at the TRUE (Lazer-blended)
// price but still healthy at on-chain prices, the Sender path can't fire — the
// chain would judge it healthy. If the asset bank's oracle is a shard-0
// sponsored feed (permissionless crank), we instead fire an atomic Jito bundle
// [crank_setup, crank_fire (posts the fresh Hermes price), liquidate]. Sizing +
// ground truth for these run through simulateBundle so the chain judges AT the
// cranked price. The bundle is all-or-nothing: a losing fire never lands.
//
// Shared boilerplate (rpc/getMultiple/mintOwner/latestBlockhash/solBalance/
// simulate helpers/Cfg loader/daily-tip-budget) lives in ../lib/liqExecutor.ts
// — see PLAN.md's note on the 5 near-duplicate liq_*_executor binaries.
//
// Usage: HELIUS_RPC=<url> [DRY_RUN=1] [KEYPAIR_PATH=~/arb-keypair.json]
//        [PYTH_LAZER_TOKEN=... (required for the crank edge)] [CRANK=1]
//        [MIN_COLLATERAL_USD=100] [MIN_PROFIT_USD=0.5] [TIP_FRACTION_BPS=3000]
//        [POLL_MS=5000] [RESCAN_SECS=300] [WATCH_RATIO=0.85] [RUN_DIR=runs]
//        [MAX_BLOB_AGE_MS=3000] [JITO_BLOCK_ENGINE=...]
//        npx tsx src/bin/liqExecutor.ts

import 'dotenv/config';
import { type AccountMeta, Keypair, PublicKey } from '@solana/web3.js';
import { bundleStatus } from '../lib/jito.js';
import { buildFireTx, type FireCandidate } from '../lib/liqFire.js';
import {
  type Bank,
  type BankMap,
  type Balance,
  decodeBank,
  decodeMarginfiAccount,
  decodeOraclePriceFresh,
  decodePriceUpdateV2,
  activeBankPks,
  maintenanceHealth,
  maxStaleSlotsFor,
  DEFAULT_MAX_SB_STALE_SLOTS,
  MA_SIZE,
  type MarginfiAccount,
  type PriceMap,
  type HealthResult,
} from '../lib/liquidation.js';
import * as marginfi from '../lib/marginfi.js';
import { alert, logDecision, logTrade } from '../lib/observe.js';
import * as lazerLib from '../lib/lazer.js';
import * as pyth from '../lib/pyth.js';
import { buildCrankTxs, sponsoredFeed } from '../lib/pythCrank.js';
import { Engine as LiqEngine } from '../lib/liqEngine.js';
import {
  b64,
  belowWalletFloor,
  currentSlot,
  DailyTipBudget,
  DEFAULT_AUTHORITY,
  DEFAULT_LIQUIDATOR_MA,
  getMultiple,
  latestBlockhash,
  loadBaseCfg,
  loadCrankCtx,
  loadKeypair,
  logLatency,
  mintOwner,
  now,
  nowUs,
  pickTip,
  rpc,
  signAndSubmit,
  simulateBundle,
  simulateTxB64,
  sleep,
  solBalance,
  spawnPnlReadback,
  txToB64,
  type BaseCfg,
  type CrankCtx,
} from '../lib/liqExecutor.js';

const MARGINFI_PROGRAM = 'MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA';
const MARGINFI_GROUP = '4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8';
const SOL_MINT = 'So11111111111111111111111111111111111111112';
const USDT_MINT = 'Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB';

/** Debt (liability) assets the fire path can repay: USDC, USDT, wSOL. */
function isDebtMint(mint: PublicKey): boolean {
  const m = mint.toBase58();
  return m === marginfi.USDC_MINT || m === USDT_MINT || m === SOL_MINT;
}

/** Largest -> smallest: bigger seize = more profit; marginfi rejects over-liquidation, so walk down. */
const SIZE_LADDER = [1.0, 0.5, 0.25, 0.1, 0.02];

// ── DecisionLog / TradeLog row shapes (logged via observe.ts's logDecision/logTrade) ──
interface DecisionLog {
  t: number;
  liquidatee: string;
  mode: string;
  collateral_usd: number;
  ratio: number;
  seize_native: string;
  quoted_usdc_out: number;
  est_liab_usdc: number;
  est_profit_usdc: number;
  fire_sim_ok: boolean;
  fired: boolean;
  reason: string;
}
interface TradeLog {
  t: number;
  liquidatee: string;
  seize_native: string;
  est_profit_usdc: number;
  tip_lamports: string;
  signature: string | undefined;
  bundle: string | undefined;
  realized_usdc: number | undefined;
  error: string | undefined;
}

/** How a cached fire gets submitted. */
type FireMode = { kind: 'sender' } | { kind: 'crank'; feedId: Buffer };
function modeName(m: FireMode): string {
  return m.kind;
}

/** True if the fire path can act on at least one LEG of this account. */
function isV1Fireable(a: MarginfiAccount, banks: BankMap): boolean {
  const hasCollateral = a.balances.some((b) => b.assetShares > 0.0);
  const hasWiredDebt = a.balances
    .filter((b) => b.liabilityShares > 0.0)
    .some((b) => {
      const bk = banks.get(b.bankPk.toBase58());
      return bk !== undefined && isDebtMint(bk.mint);
    });
  return hasCollateral && hasWiredDebt;
}

interface Scan {
  accts: Array<[PublicKey, MarginfiAccount]>;
  banks: BankMap;
  oracleOf: Map<string, PublicKey>;
  /** bank -> 32-byte Pyth feed id, decoded from the oracle account itself. */
  feedOf: Map<string, Buffer>;
  /** Banks whose oracle IS the shard-0 sponsored feed PDA (permissionlessly crankable). */
  crankable: Set<string>;
}

async function fullScan(endpoint: string): Promise<Scan | undefined> {
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
  if (entries.length === 0) return undefined;
  const accts: Array<[PublicKey, MarginfiAccount]> = [];
  for (const e of entries) {
    const pkStr = e?.pubkey;
    const raw = b64(e?.account?.data);
    if (typeof pkStr !== 'string' || raw === undefined) continue;
    const a = decodeMarginfiAccount(raw);
    if (a === null) continue;
    if (!a.balances.some((b) => b.liabilityShares > 0.0)) continue;
    accts.push([new PublicKey(pkStr), a]);
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
  const feedOf = new Map<string, Buffer>();
  const crankable = new Set<string>();
  for (const [bank, oracle] of oracleOf) {
    const raw = oracleRaw.get(oracle.toBase58());
    if (raw === undefined) continue;
    const pyu = decodePriceUpdateV2(raw);
    if (pyu === null) continue;
    feedOf.set(bank, pyu.feedId);
    if (sponsoredFeed(0, pyu.feedId).equals(oracle)) crankable.add(bank);
  }
  return { accts, banks, oracleOf, feedOf, crankable };
}

async function freshPrices(endpoint: string, banks: BankMap, oracleOf: Map<string, PublicKey>): Promise<PriceMap> {
  const slot = await currentSlot(endpoint);
  const defaultStale = process.env.MAX_SB_STALE_SLOTS ? BigInt(process.env.MAX_SB_STALE_SLOTS) : DEFAULT_MAX_SB_STALE_SLOTS;
  const oraclePkSet = new Set<string>();
  const oraclePks: PublicKey[] = [];
  for (const oc of oracleOf.values()) {
    const key = oc.toBase58();
    if (!oraclePkSet.has(key)) {
      oraclePkSet.add(key);
      oraclePks.push(oc);
    }
  }
  const raw = await getMultiple(endpoint, oraclePks);
  const out: PriceMap = new Map();
  for (const [bankPk, oraclePk] of oracleOf) {
    const maxAge = banks.get(bankPk)?.oracleMaxAge ?? 0;
    const maxStale = maxStaleSlotsFor(maxAge, defaultStale);
    const r = raw.get(oraclePk.toBase58());
    if (r === undefined) continue;
    const usd = decodeOraclePriceFresh(r, slot, maxStale);
    if (usd !== null) out.set(bankPk, usd);
  }
  return out;
}

/** The sizing-gate tx [start_fl, liquidate(asset_amount), end_fl] as base64. */
async function gateTxB64(
  authority: PublicKey,
  liquidatorMa: PublicKey,
  tp: PublicKey,
  liquidatee: PublicKey,
  acct: MarginfiAccount,
  assetBank: PublicKey,
  liabBank: PublicKey,
  assetAmount: bigint,
  oracleOf: Map<string, PublicKey>,
): Promise<string | undefined> {
  const { TransactionMessage, VersionedTransaction } = await import('@solana/web3.js');
  const liquidateeObs: AccountMeta[] = [];
  for (const b of acct.balances) {
    const oc = oracleOf.get(b.bankPk.toBase58());
    if (oc === undefined) return undefined;
    liquidateeObs.push({ pubkey: b.bankPk, isSigner: false, isWritable: false });
    liquidateeObs.push({ pubkey: oc, isSigner: false, isWritable: false });
  }
  const assetOracle = oracleOf.get(assetBank.toBase58());
  const liabOracle = oracleOf.get(liabBank.toBase58());
  if (assetOracle === undefined || liabOracle === undefined) return undefined;
  const start = marginfi.startFlashloan(liquidatorMa, authority, 2n);
  const liqIx = marginfi.lendingAccountLiquidate(
    assetBank,
    liabBank,
    liquidatorMa,
    authority,
    liquidatee,
    tp,
    assetAmount,
    assetOracle,
    liabOracle,
    liquidateeObs,
  );
  const endObs: AccountMeta[] = [
    { pubkey: assetBank, isSigner: false, isWritable: false },
    { pubkey: assetOracle, isSigner: false, isWritable: false },
    { pubkey: liabBank, isSigner: false, isWritable: false },
    { pubkey: liabOracle, isSigner: false, isWritable: false },
  ];
  const end = marginfi.endFlashloan(liquidatorMa, authority, endObs);
  const msg = new TransactionMessage({
    payerKey: authority,
    recentBlockhash: '11111111111111111111111111111111111111111111',
    instructions: [start, liqIx, end],
  }).compileToV0Message([]);
  const tx = new VersionedTransaction(msg);
  return txToB64(tx);
}

/** Some(true) = marginfi accepts the liquidation at this size. */
type GateSim = { kind: 'fireable' } | { kind: 'reverted'; code: number | undefined } | { kind: 'unusable' };

function revertReason(code: number | undefined): string {
  switch (code) {
    case 6068:
      return 'chain says healthy at the actionable price (not truly liquidatable)';
    case 6049:
      return 'collateral oracle stale on-chain (SwitchboardStalePrice) — not actionable';
    case 6009:
      return 'risk engine rejected: bad health or stale oracle';
    case 6012:
      return 'liquidation amount rounded to zero (position too small)';
    case 6210:
      return 'Kamino-integrated collateral: reserve validation failed';
    default:
      return code !== undefined ? `liquidate reverted with marginfi error ${code}` : 'liquidate reverted (no custom code)';
  }
}

async function simulateGate(
  endpoint: string,
  authority: PublicKey,
  liquidatorMa: PublicKey,
  tp: PublicKey,
  liquidatee: PublicKey,
  acct: MarginfiAccount,
  assetBank: PublicKey,
  liabBank: PublicKey,
  assetAmount: bigint,
  oracleOf: Map<string, PublicKey>,
  crankB64: [string, string] | undefined,
): Promise<GateSim> {
  const gate = await gateTxB64(authority, liquidatorMa, tp, liquidatee, acct, assetBank, liabBank, assetAmount, oracleOf);
  if (gate === undefined) return { kind: 'unusable' };
  if (crankB64 !== undefined) {
    const [setup, fire] = crankB64;
    const sim = await simulateBundle(endpoint, [setup, fire, gate]);
    if (sim === undefined) return { kind: 'unusable' };
    if (sim.ranOk === 3) return { kind: 'fireable' };
    if (sim.ranOk < 2) return { kind: 'unusable' }; // crank itself broke, not a healthy account
    return { kind: 'reverted', code: sim.failCode };
  }
  const res = await simulateTxB64(endpoint, gate);
  if (res === undefined) return { kind: 'unusable' };
  if (res.err == null) return { kind: 'fireable' };
  const code = res.err?.InstructionError?.[1]?.Custom;
  return { kind: 'reverted', code: typeof code === 'number' ? code : undefined };
}

/** Copy-able config bundle for the arm/fire helpers. */
interface Cfg {
  liquidatorMa: PublicKey;
  authority: PublicKey;
  tp: PublicKey;
  tipAccount: PublicKey;
  tipFractionBps: bigint;
  minTipSol: number;
  minProfit: number;
  slippageBps: number;
}

/** A fully-built, sim-verified fire tx kept hot for an armed account. */
interface CachedFire {
  tx: any; // VersionedTransaction
  mode: FireMode;
  tipLamports: bigint;
  tipSol: number;
  estProfit: number;
  seize: bigint;
  built: number; // performance.now() timestamp
}

/** Ranked (collateral, debt) leg pairs a multi-position account can act on. */
function fireableLegs(a: MarginfiAccount, banks: BankMap, prices: PriceMap): Array<[PublicKey, PublicKey]> {
  const sideUsd = (b: Balance, isAsset: boolean): number => {
    const bk = banks.get(b.bankPk.toBase58());
    const p = prices.get(b.bankPk.toBase58());
    if (bk === undefined || p === undefined) return 0.0;
    const native = isAsset ? b.assetShares * bk.assetShareValue : b.liabilityShares * bk.liabilityShareValue;
    return (native / 10 ** bk.mintDecimals) * p;
  };
  const assets = a.balances.filter((b) => b.assetShares > 0.0);
  const debts = a.balances.filter((b) => {
    if (b.liabilityShares <= 0.0) return false;
    const bk = banks.get(b.bankPk.toBase58());
    return bk !== undefined && isDebtMint(bk.mint);
  });
  const legs: Array<[PublicKey, PublicKey, number]> = [];
  for (const c of assets) {
    for (const d of debts) {
      legs.push([c.bankPk, d.bankPk, Math.min(sideUsd(c, true), sideUsd(d, false))]);
    }
  }
  legs.sort((x, y) => y[2] - x[2]);
  return legs.map(([c, d]) => [c, d]);
}

/** Arm an account: try its ranked fireable legs (capped) and return the first that clears every gate. */
async function tryArm(
  endpoint: string,
  runDir: string,
  cfg: Cfg,
  crank: CrankCtx,
  scan: Scan,
  a: MarginfiAccount,
  pk: PublicKey,
  prices: PriceMap,
  base: PriceMap,
  mintTp: Map<string, PublicKey>,
): Promise<CachedFire | undefined> {
  const r = maintenanceHealth(a, scan.banks, prices);
  const legs = fireableLegs(a, scan.banks, prices);
  if (legs.length === 0) return undefined;
  const maxLegs = Number.parseInt(process.env.MAX_LEGS_PER_ARM ?? '', 10) || 3;
  for (const [assetBank, liabBank] of legs.slice(0, maxLegs)) {
    const c = await tryArmLeg(endpoint, runDir, cfg, crank, scan, a, pk, prices, base, mintTp, r, assetBank, liabBank);
    if (c !== undefined) return c;
  }
  return undefined;
}

async function tryArmLeg(
  endpoint: string,
  runDir: string,
  cfg: Cfg,
  crank: CrankCtx,
  scan: Scan,
  a: MarginfiAccount,
  pk: PublicKey,
  prices: PriceMap,
  base: PriceMap,
  mintTp: Map<string, PublicKey>,
  r: HealthResult,
  assetBank: PublicKey,
  liabBank: PublicKey,
): Promise<CachedFire | undefined> {
  const liabBankInfo = scan.banks.get(liabBank.toBase58());
  if (liabBankInfo === undefined || !isDebtMint(liabBankInfo.mint)) return undefined;
  const bank = scan.banks.get(assetBank.toBase58());
  if (bank === undefined) return undefined;
  const assetBal = a.balances.find((b) => b.bankPk.equals(assetBank) && b.assetShares > 0.0);
  if (assetBal === undefined) return undefined;
  const nativeTotal = assetBal.assetShares * bank.assetShareValue;

  const logSkip = (mode: string, reason: string): void => {
    logDecision<DecisionLog>(runDir, {
      t: now(),
      liquidatee: pk.toBase58(),
      mode,
      collateral_usd: r.health.weightedAssets,
      ratio: r.health.weightedAssets === 0.0 ? Number.POSITIVE_INFINITY : r.health.weightedLiabilities / r.health.weightedAssets,
      seize_native: '0',
      quoted_usdc_out: 0.0,
      est_liab_usdc: 0.0,
      est_profit_usdc: 0.0,
      fire_sim_ok: false,
      fired: false,
      reason,
    });
  };

  const onchain = maintenanceHealth(a, scan.banks, base);
  const onchainRatio = onchain.health.weightedAssets === 0.0 ? Number.POSITIVE_INFINITY : onchain.health.weightedLiabilities / onchain.health.weightedAssets;
  const rRatio = r.health.weightedAssets === 0.0 ? Number.POSITIVE_INFINITY : r.health.weightedLiabilities / r.health.weightedAssets;
  let mode: FireMode;
  if (onchain.missing === 0 && onchainRatio >= 1.0) {
    mode = { kind: 'sender' };
  } else {
    if (!crank.on) return undefined;
    if (r.missing > 0 || rRatio < 1.0) return undefined;
    if (!scan.crankable.has(assetBank.toBase58())) {
      logSkip('crank', 'flagged at Lazer price but healthy on-chain and oracle not crankable (non-Pyth/non-sponsored) — cannot act');
      return undefined;
    }
    const feedId = scan.feedOf.get(assetBank.toBase58());
    if (feedId === undefined) {
      logSkip('crank', 'crankable but feed id missing — cannot build crank');
      return undefined;
    }
    if (crank.hermes.updateFor(feedId) === undefined) {
      logSkip('crank', 'crankable but no fresh Hermes blob for feed yet');
      return undefined;
    }
    mode = { kind: 'crank', feedId };
  }

  let crankB64: [string, string] | undefined;
  if (mode.kind === 'crank') {
    const u = crank.hermes.updateFor(mode.feedId);
    if (u === undefined) return undefined;
    const txs = buildCrankTxs(cfg.authority, u.vaa, [u.update], 0, 0, '11111111111111111111111111111111111111111111');
    crankB64 = txs.toB64();
  }

  let seize = 0n;
  let lastRevert: number | undefined;
  for (const frac of SIZE_LADDER) {
    const amount = BigInt(Math.trunc(nativeTotal * frac));
    if (amount === 0n) continue;
    const g = await simulateGate(endpoint, cfg.authority, cfg.liquidatorMa, cfg.tp, pk, a, assetBank, liabBank, amount, scan.oracleOf, crankB64);
    if (g.kind === 'fireable') {
      seize = amount;
      break;
    } else if (g.kind === 'reverted') {
      lastRevert = g.code;
    }
  }
  if (seize === 0n) {
    logSkip(modeName(mode), revertReason(lastRevert));
    return undefined;
  }

  let assetTp = mintTp.get(bank.mint.toBase58());
  if (assetTp === undefined) {
    assetTp = await mintOwner(endpoint, bank.mint, mintTp);
    if (assetTp === undefined) return undefined;
  }
  const debtMint = liabBankInfo.mint;
  let debtTp = mintTp.get(debtMint.toBase58());
  if (debtTp === undefined) {
    debtTp = await mintOwner(endpoint, debtMint, mintTp);
    if (debtTp === undefined) return undefined;
  }
  const obs: AccountMeta[] = [];
  for (const b of a.balances) {
    const oc = scan.oracleOf.get(b.bankPk.toBase58());
    if (oc === undefined) return undefined;
    obs.push({ pubkey: b.bankPk, isSigner: false, isWritable: false });
    obs.push({ pubkey: oc, isSigner: false, isWritable: false });
  }
  const assetOracle = scan.oracleOf.get(assetBank.toBase58())!;
  const liabOracle = scan.oracleOf.get(liabBank.toBase58())!;
  const cand: FireCandidate = {
    liquidatee: pk,
    assetBank,
    assetMint: bank.mint,
    assetTokenProgram: assetTp,
    assetAmount: seize,
    liabBank,
    debtMint,
    debtTokenProgram: debtTp,
    assetOracle,
    liabOracle,
    liquidateeObs: obs,
  };
  const price = prices.get(assetBank.toBase58()) ?? 0.0;
  const seizedUsd = (Number(seize) / 10 ** bank.mintDecimals) * price;
  const estLiab = seizedUsd * 0.975;
  const debtDec = liabBankInfo.mintDecimals;
  const debtPrice = prices.get(liabBank.toBase58()) ?? (debtMint.toBase58() === SOL_MINT ? 150.0 : 1.0);
  const debtOutUsd = (native: bigint): number => (Number(native) / 10 ** debtDec) * debtPrice;
  let solUsd = 150.0;
  for (const [bk, bank2] of scan.banks) {
    if (bank2.mint.toBase58() === SOL_MINT) {
      const p = prices.get(bk);
      if (p !== undefined) solUsd = p;
      break;
    }
  }

  const log: DecisionLog = {
    t: now(),
    liquidatee: pk.toBase58(),
    mode: modeName(mode),
    collateral_usd: r.health.weightedAssets,
    ratio: rRatio,
    seize_native: seize.toString(),
    quoted_usdc_out: 0.0,
    est_liab_usdc: estLiab,
    est_profit_usdc: 0.0,
    fire_sim_ok: false,
    fired: false,
    reason: '',
  };
  let tipTo: PublicKey;
  if (mode.kind === 'sender') {
    tipTo = cfg.tipAccount;
  } else {
    const t = pickTip(crank);
    if (t === undefined) {
      log.reason = 'no Jito tip accounts';
      logDecision(runDir, log);
      return undefined;
    }
    tipTo = t;
  }
  const ph = '11111111111111111111111111111111111111111111';
  let fire;
  try {
    fire = await buildFireTx(endpoint, cand, cfg.liquidatorMa, cfg.authority, tipTo, 0n, 100_000n, cfg.slippageBps, 20, ph);
  } catch (e) {
    log.reason = `build: ${e}`;
    logDecision(runDir, log);
    return undefined;
  }
  log.quoted_usdc_out = debtOutUsd(fire.quotedUsdcOut);
  const estProfit = debtOutUsd(fire.quotedUsdcOut) - estLiab;
  log.est_profit_usdc = estProfit;
  const tipSol = Math.max((estProfit * Number(cfg.tipFractionBps)) / 10_000.0 / solUsd, cfg.minTipSol);
  const tipLamports = BigInt(Math.trunc(tipSol * 1e9));
  if (estProfit < cfg.minProfit + tipSol * solUsd) {
    log.reason = `below min profit (est $${estProfit.toFixed(2)}, tip $${(tipSol * solUsd).toFixed(2)})`;
    logDecision(runDir, log);
    return undefined;
  }
  try {
    fire = await buildFireTx(endpoint, cand, cfg.liquidatorMa, cfg.authority, tipTo, tipLamports, 100_000n, cfg.slippageBps, 20, ph);
  } catch (e) {
    log.reason = `rebuild: ${e}`;
    logDecision(runDir, log);
    return undefined;
  }
  const txB64 = txToB64(fire.tx);
  let simOk: boolean;
  if (crankB64 === undefined) {
    const res = await simulateTxB64(endpoint, txB64);
    simOk = res !== undefined && res.err == null;
  } else {
    const [s, c] = crankB64;
    const bs = await simulateBundle(endpoint, [s, c, txB64]);
    simOk = bs !== undefined && bs.ranOk === 3;
  }
  log.fire_sim_ok = simOk;
  if (!simOk) {
    log.reason = 'fire sim revert (swap/repay would not cover liability)';
    logDecision(runDir, log);
    return undefined;
  }
  return { tx: fire.tx, mode, tipLamports, tipSol, estProfit, seize, built: performance.now() };
}

async function fireCached(
  endpoint: string,
  runDir: string,
  senderUrl: string,
  cfg: Cfg,
  crank: CrankCtx,
  dryRun: boolean,
  pk: PublicKey,
  cached: CachedFire,
  freshBh: string,
  kp: Keypair | undefined,
  budget: DailyTipBudget,
  walletMinSol: number,
  webhook: string | undefined,
): Promise<void> {
  const mode = modeName(cached.mode);
  const log: DecisionLog = {
    t: now(),
    liquidatee: pk.toBase58(),
    mode,
    collateral_usd: 0.0,
    ratio: 0.0,
    seize_native: cached.seize.toString(),
    quoted_usdc_out: 0.0,
    est_liab_usdc: 0.0,
    est_profit_usdc: cached.estProfit,
    fire_sim_ok: true,
    fired: false,
    reason: '',
  };
  const armedAgoMs = performance.now() - cached.built;
  console.log(
    `* LIQUIDATABLE [${mode}]  ${pk.toBase58().slice(0, 8)}  seize ${cached.seize}  est profit $${cached.estProfit.toFixed(2)}  tip ${cached.tipSol.toFixed(5)} SOL  (armed ${armedAgoMs.toFixed(0)}ms ago)`,
  );
  if (dryRun) {
    log.reason = `dry-run: would fire (${mode}, armed)`;
    logDecision(runDir, log);
    alert(webhook, 'liq-dry', `DRY-RUN ${mode} liquidation: ${pk.toBase58()} est profit $${cached.estProfit.toFixed(2)}`);
    return;
  }
  if (budget.wouldExceed(cached.tipSol)) {
    log.reason = 'daily tip cap';
    logDecision(runDir, log);
    alert(webhook, 'liq-cap', 'daily tip cap reached');
    return;
  }
  if (await belowWalletFloor(endpoint, cfg.authority, walletMinSol)) {
    log.reason = 'wallet below floor';
    logDecision(runDir, log);
    alert(webhook, 'liq-floor', 'wallet below floor — not firing');
    return;
  }
  const kpReq = kp!;
  const { seize, estProfit, tipLamports, tipSol } = cached;
  const submit = await signAndSubmit(
    cached.tx,
    kpReq,
    freshBh,
    senderUrl,
    cached.mode.kind === 'crank' ? { ...crank, authority: cfg.authority, feedId: cached.mode.feedId } : undefined,
  );

  log.fired = submit.ok;
  log.reason = `fired (${mode}, armed cache)`;
  logDecision(runDir, log);
  if (submit.ok) {
    const sig = submit.signature;
    console.error(`[exec] FIRED [${mode}] ${sig}${submit.bundleId ? ` bundle ${submit.bundleId}` : ''}`);
    logTrade<TradeLog>(runDir, {
      t: now(),
      liquidatee: pk.toBase58(),
      seize_native: seize.toString(),
      est_profit_usdc: estProfit,
      tip_lamports: tipLamports.toString(),
      signature: sig,
      bundle: submit.bundleId,
      realized_usdc: undefined,
      error: undefined,
    });
    const bundleId = submit.bundleId;
    spawnPnlReadback(
      endpoint,
      sig,
      cfg.authority.toBase58(),
      (pnl) => {
        budget.add(tipSol);
        logTrade<TradeLog>(runDir, {
          t: now(),
          liquidatee: '',
          seize_native: '0',
          est_profit_usdc: 0.0,
          tip_lamports: '0',
          signature: sig,
          bundle: undefined,
          realized_usdc: pnl,
          error: undefined,
        });
        alert(webhook, 'liq-landed', `liquidation landed ${sig}: realized $${pnl.toFixed(2)}`);
      },
      () => {
        void (async () => {
          const status = bundleId !== undefined ? await bundleStatus(crank.blockEngine, bundleId) : undefined;
          alert(webhook, 'liq-miss', `liquidation ${sig} never confirmed (bundle status: ${status ?? ''})`);
        })();
      },
    );
  } else {
    console.error(`[exec] send failed: ${submit.error}`);
    logTrade<TradeLog>(runDir, {
      t: now(),
      liquidatee: pk.toBase58(),
      seize_native: seize.toString(),
      est_profit_usdc: estProfit,
      tip_lamports: tipLamports.toString(),
      signature: undefined,
      bundle: undefined,
      realized_usdc: undefined,
      error: submit.error,
    });
  }
}

async function main(): Promise<void> {
  const base: BaseCfg = loadBaseCfg({ runDir: 'runs', pollMs: 5000, rescanSecs: 300 });
  const { endpoint, dryRun, runDir } = base;
  const minCollateral = Number.parseFloat(process.env.MIN_COLLATERAL_USD ?? '') || 100.0;
  const watchRatio = Number.parseFloat(process.env.WATCH_RATIO ?? '') || 0.85;
  const liquidatorMa = new PublicKey(process.env.LIQUIDATOR_MA ?? DEFAULT_LIQUIDATOR_MA);
  const usdcBank = new PublicKey(marginfi.USDC_BANK);
  void usdcBank; // kept for boot-time logging parity; fire path resolves the real debt bank per account
  const tp = new PublicKey('TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA');

  const { kp, authority } = loadKeypair(dryRun);

  const lazerTable = pyth.newTable();
  const lazerMap = lazerLib.mintFeedMap();
  let lazerOn = false;
  const lazerToken = process.env.PYTH_LAZER_TOKEN;
  if (lazerToken) {
    lazerLib.spawnLazerThread(lazerToken, lazerLib.armFeedIds(), lazerTable);
    lazerOn = true;
    console.error('[exec] Pyth Lazer pre-positioning ENABLED');
  }

  const crank = await loadCrankCtx('[exec]');

  console.error(
    `[exec] marginfi liquidation executor ${dryRun ? '[DRY RUN]' : '[LIVE]'}  authority=${authority.toBase58()}  min_profit=$${base.minProfitUsd}  poll=${base.pollMs}ms rescan=${base.rescanSecs}s  lazer=${lazerOn}`,
  );
  if (!dryRun) {
    const bal = await solBalance(endpoint, authority.toBase58());
    console.error(`[exec] wallet balance: ${bal} SOL`);
    if (bal < base.walletMinSol) throw new Error(`wallet below floor ${base.walletMinSol}`);
  }

  const mintFeed = lazerLib.mintFeedMap();
  const lazerDirect = lazerLib.oneToOneMints();
  let scan = await fullScan(endpoint);
  if (scan === undefined) throw new Error('initial scan');
  let lastScan = performance.now();
  let watch: PublicKey[] = [];
  const engine = new LiqEngine(minCollateral);
  const budget = new DailyTipBudget(base.maxDailyTipSol);
  const mintTpCache = new Map<string, PublicKey>();
  const simCooldownMs = base.simCooldownSecs * 1000;
  const simBackoffMs = (strikes: number): number => Math.min(simCooldownMs * 2 ** Math.min(Math.max(strikes - 1, 0), 6), 3_600_000);
  const handleCooldownMs = base.handleCooldownSecs * 1000;
  const handled = new Map<string, number>();
  const simRejected = new Map<string, { t: number; strikes: number }>();
  let lastTickUs = 0;
  const tickPollMs = base.tickPollMs;
  let first = true;

  const cfg: Cfg = {
    liquidatorMa,
    authority,
    tp,
    tipAccount: base.tipAccount,
    tipFractionBps: base.tipFractionBps,
    minTipSol: base.minTipSol,
    minProfit: base.minProfitUsd,
    slippageBps: base.slippageBps,
  };
  const armRatio = Number.parseFloat(process.env.ARM_RATIO ?? '') || 0.97;
  const armTtlMs = (Number.parseInt(process.env.ARM_TTL_SECS ?? '', 10) || 20) * 1000;
  const maxArm = Number.parseInt(process.env.MAX_ARM_PER_CYCLE ?? '', 10) || 8;
  const maxFire = Number.parseInt(process.env.MAX_FIRE_PER_CYCLE ?? '', 10) || 4;
  const cache = new Map<string, CachedFire>();
  let freshBh = '11111111111111111111111111111111111111111111';
  let lastBh = performance.now() - 9999_000;
  const hbEvery = Number.parseInt(process.env.HEARTBEAT_SECS ?? '', 10) || 30;
  let lastHb = performance.now() - 9999_000;
  let fireDeferred = 0;
  let armDeferred = 0;

  for (;;) {
    if (first || performance.now() - lastScan >= base.rescanSecs * 1000) {
      if (!first) {
        const s = await fullScan(endpoint);
        if (s !== undefined) scan = s;
      }
      lastScan = performance.now();
      const baseline = await freshPrices(endpoint, scan.banks, scan.oracleOf);
      const [prices] = lazerLib.blend(scan.banks, baseline, lazerTable, lazerMap);
      const fireable: Array<[PublicKey, MarginfiAccount]> = scan.accts.filter(([, a]) => isV1Fireable(a, scan!.banks));
      watch = [];
      for (const [pk, a] of fireable) {
        const r = maintenanceHealth(a, scan.banks, prices);
        const ratio = r.health.weightedAssets === 0.0 ? Number.POSITIVE_INFINITY : r.health.weightedLiabilities / r.health.weightedAssets;
        if (r.missing === 0 && ratio >= watchRatio && r.health.weightedAssets >= minCollateral) watch.push(pk);
      }
      const lazerSnapshot = new Map<number, number>();
      for (const f of lazerLib.armFeedIds()) {
        const p = pyth.get(lazerTable, f);
        if (p !== undefined) lazerSnapshot.set(f, p.price);
      }
      const armed = engine.rebuild(fireable, scan.banks, baseline, mintFeed, lazerDirect, lazerSnapshot, watchRatio);
      console.error(
        `[exec] scan: ${scan.accts.length} borrowers -> ${fireable.length} fireable-shaped -> watch-set ${watch.length} (ratio >= ${watchRatio}), engine armed ${armed}`,
      );
      if (crank.on) {
        const watchSet = new Set(watch.map((p) => p.toBase58()));
        const feeds = new Set<string>();
        for (const [pk, a] of scan.accts) {
          if (!watchSet.has(pk.toBase58())) continue;
          for (const b of a.balances) {
            if (b.assetShares > 0.0 && scan.crankable.has(b.bankPk.toBase58())) {
              const fid = scan.feedOf.get(b.bankPk.toBase58());
              if (fid !== undefined) feeds.add(fid.toString('hex'));
            }
          }
        }
        console.error(`[exec] crank: ${scan.crankable.size} crankable banks, ${feeds.size} feeds in Hermes cache`);
        const wantBlob = feeds.size > 0;
        crank.hermes.setFeeds([...feeds]);
        if (first && wantBlob) {
          const warmStart = performance.now();
          while (crank.hermes.latest() === undefined && performance.now() - warmStart < 5000) {
            await sleep(50);
          }
          console.error(
            `[exec] hermes warm-up: blob ${crank.hermes.latest() !== undefined ? 'READY' : 'still pending (continuing)'} after ${(performance.now() - warmStart).toFixed(0)}ms`,
          );
        }
      }
      first = false;
    }

    budget.rollDay();
    if (performance.now() - lastBh >= 2000) {
      const bh = await latestBlockhash(endpoint);
      if (bh !== undefined) {
        freshBh = bh;
        lastBh = performance.now();
      }
    }

    let toEval: PublicKey[];
    let snap = new Map<number, number>();
    if (lazerOn) {
      const deadline = performance.now() + base.pollMs;
      for (;;) {
        let cur = 0;
        for (const f of lazerLib.armFeedIds()) {
          const p = pyth.get(lazerTable, f);
          if (p !== undefined) cur = Math.max(cur, p.tsUs);
        }
        if (cur > lastTickUs) {
          lastTickUs = cur;
          break;
        }
        if (performance.now() >= deadline) break;
        await sleep(tickPollMs);
      }
      snap = new Map();
      for (const f of lazerLib.armFeedIds()) {
        const p = pyth.get(lazerTable, f);
        if (p !== undefined) snap.set(f, p.price);
      }
      const ranked = engine
        .crossedRanked(snap, 1.0)
        .map(([pk]) => pk)
        .filter((pk) => {
          const h = handled.get(pk.toBase58());
          return h === undefined || performance.now() - h >= handleCooldownMs;
        })
        .filter((pk) => {
          const s = simRejected.get(pk.toBase58());
          return s === undefined || performance.now() - s.t >= simBackoffMs(s.strikes);
        });
      fireDeferred = Math.max(0, ranked.length - maxFire);
      toEval = ranked.slice(0, maxFire);
    } else {
      await sleep(base.pollMs);
      toEval = [...watch];
    }

    if (lazerOn && hbEvery > 0 && performance.now() - lastHb >= hbEvery * 1000) {
      const totalFeeds = lazerLib.armFeedIds().length;
      const near = engine.crossed(snap, armRatio).length;
      const crossing = engine.crossed(snap, 1.0).length;
      const defer = fireDeferred + armDeferred > 0 ? ` | DEFERRED fire ${fireDeferred}/arm ${armDeferred} (raise MAX_*_PER_CYCLE)` : '';
      let freshest = 0;
      for (const f of lazerLib.armFeedIds()) {
        const p = pyth.get(lazerTable, f);
        if (p !== undefined) freshest = Math.max(freshest, p.tsUs);
      }
      const lagMs = Math.max(0, Math.trunc((nowUs() - freshest) / 1000));
      console.error(
        `[hb] lazer feeds ${snap.size}/${totalFeeds} live | detect_lag ${lagMs}ms | ${near} within arm(${armRatio}) | ${crossing} liquidatable now | cache ${cache.size}${defer} | ${lazerLib.status(lazerTable)}`,
      );
      lastHb = performance.now();
    }

    if (lazerOn) {
      const armRanked = engine.crossedRanked(snap, armRatio);
      const armKeys = new Set(armRanked.map(([pk]) => pk.toBase58()));
      for (const [key, c] of cache) {
        if (!armKeys.has(key) || performance.now() - c.built >= armTtlMs) cache.delete(key);
      }
      const candidates = armRanked
        .map(([pk]) => pk)
        .filter((pk) => !cache.has(pk.toBase58()))
        .filter((pk) => {
          const s = simRejected.get(pk.toBase58());
          return s === undefined || performance.now() - s.t >= simBackoffMs(s.strikes);
        });
      armDeferred = Math.max(0, candidates.length - maxArm);
      const need = candidates.slice(0, maxArm);
      if (need.length > 0) {
        const raw = await getMultiple(endpoint, need);
        const baseline2 = await freshPrices(endpoint, scan.banks, scan.oracleOf);
        const [prices2] = lazerLib.blend(scan.banks, baseline2, lazerTable, lazerMap);
        for (const pk of need) {
          const r = raw.get(pk.toBase58());
          const a = r !== undefined ? decodeMarginfiAccount(r) : null;
          if (a === null || a === undefined) continue;
          const c = await tryArm(endpoint, runDir, cfg, crank, scan, a, pk, prices2, baseline2, mintTpCache);
          const key = pk.toBase58();
          if (c !== undefined) {
            simRejected.delete(key);
            cache.set(key, c);
          } else {
            const e = simRejected.get(key) ?? { t: 0, strikes: 0 };
            simRejected.set(key, { t: performance.now(), strikes: e.strikes + 1 });
          }
        }
      }
    }

    toEval = toEval.filter((pk) => {
      const h = handled.get(pk.toBase58());
      return h === undefined || performance.now() - h >= handleCooldownMs;
    });
    if (toEval.length === 0) continue;

    const freshRaw = await getMultiple(endpoint, toEval);
    const base3 = await freshPrices(endpoint, scan.banks, scan.oracleOf);
    const [prices3] = lazerLib.blend(scan.banks, base3, lazerTable, lazerMap);
    for (const pk of toEval) {
      const key = pk.toBase58();
      handled.set(key, performance.now());
      let cached = cache.get(key);
      if (cached !== undefined && performance.now() - cached.built < armTtlMs) {
        cache.delete(key);
      } else {
        cached = undefined;
        const raw = freshRaw.get(key);
        const a = raw !== undefined ? decodeMarginfiAccount(raw) : null;
        if (a === null || a === undefined) continue;
        const r = maintenanceHealth(a, scan.banks, prices3);
        const ratio = r.health.weightedAssets === 0.0 ? Number.POSITIVE_INFINITY : r.health.weightedLiabilities / r.health.weightedAssets;
        if (r.missing > 0 || ratio < 1.0 || r.health.weightedAssets < minCollateral) continue;
        const c = await tryArm(endpoint, runDir, cfg, crank, scan, a, pk, prices3, base3, mintTpCache);
        if (c !== undefined) {
          simRejected.delete(key);
          cached = c;
        } else {
          const e = simRejected.get(key) ?? { t: 0, strikes: 0 };
          simRejected.set(key, { t: performance.now(), strikes: e.strikes + 1 });
        }
      }
      if (cached !== undefined) {
        const armedFromCache = performance.now() - cached.built < armTtlMs && performance.now() - cached.built > 0;
        const fireStart = nowUs();
        await fireCached(endpoint, runDir, base.senderUrl, cfg, crank, dryRun, pk, cached, freshBh, kp, budget, base.walletMinSol, base.webhook);
        const done = nowUs();
        logLatency(runDir, {
          t: now(),
          account: key,
          mode: modeName(cached.mode),
          appeared_us: lastTickUs,
          detected_lag_ms: Math.max(0, Math.trunc((fireStart - lastTickUs) / 1000)),
          submit_lag_ms: Math.max(0, Math.trunc((done - lastTickUs) / 1000)),
          fire_submit_ms: Math.trunc((done - fireStart) / 1000),
          armed: armedFromCache,
          dry_run: dryRun,
        });
      }
    }
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
