// Port of src/bin/liq_save_executor.rs
//
// Production Save (Solend) liquidation executor — EVENT-DRIVEN, DRY_RUN default.
//
// Mirrors the marginfi executor's architecture — a Lazer WebSocket feeds an
// in-memory health engine (saveEngine.ts) that recomputes every obligation's
// borrowed/unhealthy on each ~ms price tick with ZERO RPC.
//
//   full scan (RESCAN_SECS): v1 (1 collateral / 1 debt, debt in {USDC,USDT,wSOL})
//     obligations -> saveEngine watch-set (stored on-chain health + per-side
//     Lazer anchors)
//   Lazer tick (TICK_POLL_MS in-memory poll): the trigger to RE-CHECK, not the
//     liquidatable verdict — Lazer leads/diverges from the on-chain Pyth price
//   FIRE tier (TWO-TIER GATING): Lazer NARROWS the watch-set; the ON-CHAIN
//     oracle price GATES the expensive sim. Only obligations liquidatable at
//     the on-chain price Solend settles against earn a sim, ranked by USD
//     deficit, capped top-K (MAX_FIRE_PER_CYCLE).
//   ARM those FIRE-tier candidates: pre-build+size+sim the fire tx -> hot cache
//   FIRE on tick: stamp fresh blockhash, sign, submit (no build/quote/sim on
//     the critical path)
//
// Two fire modes, exactly like marginfi:
//   Sender — obligation already liquidatable at ON-CHAIN prices -> single tx
//     via Helius Sender.
//   Crank  — underwater at the true (Lazer) price but Solend hasn't cranked
//     its Pyth feed yet -> atomic Jito bundle [crank_setup, crank_fire, fire]
//     that posts the fresh price then liquidates.
//
// Profit-or-revert (payback_all fails unless the swap covered the borrow), so
// a losing fire that lands costs only the base fee; a failing bundle never lands.
//
// Usage: HELIUS_RPC=<url> [DRY_RUN=1] [KEYPAIR_PATH=~/arb-keypair.json]
//        [PYTH_LAZER_TOKEN=... (required for event-driven + crank)] [CRANK=1]
//        [MIN_DEBT_USD=100] [MIN_PROFIT_USD=0.5] [REPAY_FRACS=0.2,0.1,0.05]
//        [WATCH_RATIO=0.85] [ARM_RATIO=0.97] [RESCAN_SECS=30] [TICK_POLL_MS=1]
//        [MAX_ARM_PER_CYCLE=8] [MAX_FIRE_PER_CYCLE=4] [SLIPPAGE_BPS=100]
//        [MAX_SWAP_ACCOUNTS=18] [MAX_BLOB_AGE_MS=3000] npx tsx src/bin/liqSaveExecutor.ts

import 'dotenv/config';
import { Keypair, PublicKey, type VersionedTransaction } from '@solana/web3.js';
import { bundleStatus } from '../lib/jito.js';
import { decodePriceUpdateV2 } from '../lib/liquidation.js';
import { alert, logDecision, logTrade } from '../lib/observe.js';
import * as lazer from '../lib/lazer.js';
import * as pyth from '../lib/pyth.js';
import { buildCrankTxs, sponsoredFeed } from '../lib/pythCrank.js';
import * as save from '../lib/save.js';
import { Engine as SaveEngine } from '../lib/saveEngine.js';
import { buildSaveFireTx, type SaveFireCandidate, type SaveFireTx } from '../lib/saveFire.js';
import {
  b64,
  belowWalletFloor,
  DailyTipBudget,
  DEFAULT_AUTHORITY,
  getAccount,
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
  simulateOk,
  sleep,
  solBalance,
  spawnPnlReadback,
  txToB64,
  type BaseCfg,
  type CrankCtx,
} from '../lib/liqExecutor.js';

const CLASSIC_TOKEN_PROGRAM = new PublicKey('TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA');
/** Pyth Lazer USDT/USD numeric feed id (SOL=6/USDC=7 already shared; USDT added locally). */
const LAZER_USDT = 8;

function armFeeds(): number[] {
  const v = lazer.armFeedIds();
  if (!v.includes(LAZER_USDT)) v.push(LAZER_USDT);
  return v;
}
function mintFeedExt(): Map<string, number> {
  const m = lazer.mintFeedMap();
  m.set(save.USDT_MINT, LAZER_USDT);
  return m;
}

interface DecisionLog {
  t: number;
  obligation: string;
  protocol: 'save';
  mode: string;
  debt_usd: number;
  ratio: number;
  repay_native: string;
  quoted_usdc_out: number;
  est_profit_usdc: number;
  fired: boolean;
  reason: string;
}
interface TradeLog {
  t: number;
  obligation: string;
  protocol: 'save';
  repay_native: string;
  est_profit_usdc: number;
  tip_lamports: string;
  signature: string | undefined;
  bundle: string | undefined;
  realized_usdc: number | undefined;
  error: string | undefined;
}

type FireMode = { kind: 'sender' } | { kind: 'crank'; feedId: Buffer };
function modeName(m: FireMode): string {
  return m.kind;
}

interface SaveScan {
  obls: Array<[PublicKey, save.Obligation]>;
  reserves: Map<string, save.Reserve>;
  ctpOf: Map<string, PublicKey>;
  feedOf: Map<string, Buffer>;
  crankable: Set<string>;
}

async function fullScanSave(
  endpoint: string,
  debtReserves: Map<string, save.Reserve>,
  minDebt: number,
  ctpCache: Map<string, PublicKey>,
): Promise<SaveScan | undefined> {
  const entries: any[] = [];
  for (const pool of save.SCAN_POOLS) {
    const resp = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'getProgramAccounts',
      params: [save.SOLEND_PROGRAM, { encoding: 'base64', dataSize: 1300, filters: [{ dataSize: 1300 }, { memcmp: { offset: 10, bytes: pool } }] }],
    });
    if (Array.isArray(resp?.result)) entries.push(...resp.result);
  }
  if (entries.length === 0) return undefined;
  const obls: Array<[PublicKey, save.Obligation]> = [];
  for (const e of entries) {
    const pkStr = e?.pubkey;
    const d = b64(e?.account?.data);
    if (typeof pkStr !== 'string' || d === undefined) continue;
    const o = save.decodeObligation(d);
    if (o === null) continue;
    if (o.deposits.length !== 1 || o.borrows.length !== 1) continue;
    if (!debtReserves.has(o.borrows[0]!.reserve.toBase58())) continue;
    if (o.borrowedValue < minDebt) continue;
    obls.push([new PublicKey(pkStr), o]);
  }
  const collPkSet = new Set<string>();
  const collPks: PublicKey[] = [];
  for (const [, o] of obls) {
    const key = o.deposits[0]!.reserve.toBase58();
    if (!collPkSet.has(key)) {
      collPkSet.add(key);
      collPks.push(o.deposits[0]!.reserve);
    }
  }
  const reserves = new Map<string, save.Reserve>(debtReserves);
  const rraw = await getMultiple(endpoint, collPks);
  for (const pk of collPks) {
    const raw = rraw.get(pk.toBase58());
    if (raw === undefined) continue;
    const r = save.decodeReserve(pk, raw);
    if (r !== null) reserves.set(pk.toBase58(), r);
  }
  const ctpOf = new Map<string, PublicKey>();
  for (const pk of collPks) {
    const r = reserves.get(pk.toBase58());
    if (r === undefined) continue;
    const key = r.liquidityMint.toBase58();
    let tp = ctpCache.get(key);
    if (tp === undefined) {
      const owner = await mintOwner(endpoint, r.liquidityMint);
      if (owner === undefined) continue;
      ctpCache.set(key, owner);
      tp = owner;
    }
    ctpOf.set(key, tp);
  }
  const oraclePkSet = new Set<string>();
  const oraclePks: PublicKey[] = [];
  for (const pk of collPks) {
    const r = reserves.get(pk.toBase58());
    if (r === undefined) continue;
    const key = r.pythOracle.toBase58();
    if (!oraclePkSet.has(key)) {
      oraclePkSet.add(key);
      oraclePks.push(r.pythOracle);
    }
  }
  const oracleRaw = await getMultiple(endpoint, oraclePks);
  const feedOf = new Map<string, Buffer>();
  const crankable = new Set<string>();
  for (const pk of collPks) {
    const r = reserves.get(pk.toBase58());
    if (r === undefined) continue;
    const raw = oracleRaw.get(r.pythOracle.toBase58());
    if (raw === undefined) continue;
    const pyu = decodePriceUpdateV2(raw);
    if (pyu === null) continue;
    feedOf.set(pk.toBase58(), pyu.feedId);
    if (sponsoredFeed(0, pyu.feedId).equals(r.pythOracle)) crankable.add(pk.toBase58());
  }
  return { obls, reserves, ctpOf, feedOf, crankable };
}

interface Cfg {
  authority: PublicKey;
  tipAccount: PublicKey;
  tipFractionBps: bigint;
  minTipSol: number;
  minProfit: number;
  slippageBps: number;
  maxSwapAccounts: number;
}

interface CachedFire {
  tx: VersionedTransaction;
  mode: FireMode;
  tipLamports: bigint;
  tipSol: number;
  estProfit: number;
  repay: bigint;
  debtUsd: number;
  ratio: number;
  built: number;
}

async function tryArm(
  endpoint: string,
  runDir: string,
  cfg: Cfg,
  crank: CrankCtx,
  scan: SaveScan,
  pk: PublicKey,
  repayFracs: number[],
  engineRatio: number,
): Promise<CachedFire | undefined> {
  const logSkip = (mode: string, debt: number, ratio: number, reason: string): void => {
    logDecision<DecisionLog>(runDir, {
      t: now(),
      obligation: pk.toBase58(),
      protocol: 'save',
      mode,
      debt_usd: debt,
      ratio,
      repay_native: '0',
      quoted_usdc_out: 0.0,
      est_profit_usdc: 0.0,
      fired: false,
      reason,
    });
  };

  const oRaw = await getAccount(endpoint, pk);
  const o = oRaw !== undefined ? save.decodeObligation(oRaw) : null;
  if (o === null || o === undefined) return undefined;
  if (o.deposits.length !== 1 || o.borrows.length !== 1) return undefined;
  const collPk = o.deposits[0]!.reserve;
  const coll = scan.reserves.get(collPk.toBase58());
  if (coll === undefined) return undefined;
  const ctp = scan.ctpOf.get(coll.liquidityMint.toBase58());
  if (ctp === undefined) return undefined;
  const debtReserve = scan.reserves.get(o.borrows[0]!.reserve.toBase58());
  if (debtReserve === undefined) return undefined;
  const debtDec = 10 ** debtReserve.mintDecimals;
  const debtTp = CLASSIC_TOKEN_PROGRAM;
  const debtUsd = o.borrowedValue;

  let mode: FireMode;
  if (save.obligationFreshLiquidatable(o, scan.reserves)) {
    mode = { kind: 'sender' };
  } else {
    if (!crank.on) return undefined;
    if (!scan.crankable.has(collPk.toBase58())) {
      logSkip('crank', debtUsd, engineRatio, 'flagged at Lazer price but healthy on-chain and collateral oracle not crankable — cannot act');
      return undefined;
    }
    const feedId = scan.feedOf.get(collPk.toBase58());
    if (feedId === undefined) {
      logSkip('crank', debtUsd, engineRatio, 'crankable but feed id missing');
      return undefined;
    }
    if (crank.hermes.updateFor(feedId) === undefined) {
      logSkip('crank', debtUsd, engineRatio, 'crankable but no fresh Hermes blob for feed yet');
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

  let tipTo: PublicKey;
  if (mode.kind === 'sender') {
    tipTo = cfg.tipAccount;
  } else {
    const t = pickTip(crank);
    if (t === undefined) {
      logSkip('crank', debtUsd, engineRatio, 'no Jito tip accounts');
      return undefined;
    }
    tipTo = t;
  }

  const mk = async (repay: bigint, seize: bigint, tip: bigint, bh: string): Promise<SaveFireTx | undefined> => {
    const c: SaveFireCandidate = {
      obligation: pk,
      repayReserve: debtReserve!,
      withdrawReserve: coll!,
      collateralTokenProgram: ctp!,
      debtTokenProgram: debtTp,
      repayAmount: repay,
      seizeUnderlying: seize,
      depositReserves: [coll!.reserve],
      borrowReserves: [debtReserve!.reserve],
    };
    try {
      return await buildSaveFireTx(endpoint, c, cfg.authority, tipTo, tip, 100_000n, cfg.slippageBps, cfg.maxSwapAccounts, bh);
    } catch {
      return undefined;
    }
  };
  const gate = async (fire: VersionedTransaction): Promise<boolean> => {
    if (crankB64 === undefined) return simulateOk(endpoint, txToB64(fire));
    const [s, f] = crankB64;
    const bs = await simulateBundle(endpoint, [s, f, txToB64(fire)]);
    return bs !== undefined && bs.ranOk === 3;
  };

  const ph = '11111111111111111111111111111111111111111111';
  let chosen: { repay: bigint; fire: SaveFireTx } | undefined;
  for (const frac of repayFracs) {
    const repayUsd0 = debtUsd * frac;
    const repay = BigInt(Math.max(Math.trunc((repayUsd0 / Math.max(debtReserve.marketPrice, 1e-9)) * debtDec), 1));
    const seizedUsd0 = repayUsd0 * (1.0 + coll.liquidationBonusPct / 100.0);
    const seize = BigInt(Math.trunc((seizedUsd0 / Math.max(coll.marketPrice, 1e-9)) * 10 ** coll.mintDecimals));
    const fire = await mk(repay, seize, 0n, ph);
    if (fire === undefined) continue;
    if (await gate(fire.tx)) {
      chosen = { repay, fire };
      break;
    }
  }
  if (chosen === undefined) {
    logSkip(modeName(mode), debtUsd, engineRatio, 'no repay fraction passed sim (healthy at actionable price / too small)');
    return undefined;
  }
  const { repay, fire: firstFire } = chosen;

  const repayUsd = (Number(repay) / debtDec) * debtReserve.marketPrice;
  const usdcOut = (Number(firstFire.quotedDebtOut) / debtDec) * debtReserve.marketPrice;
  const estProfit = usdcOut - repayUsd;
  const solUsd = 150.0;
  const tipSol = Math.max((estProfit * Number(cfg.tipFractionBps)) / 10_000.0 / solUsd, cfg.minTipSol);
  const tipLamports = BigInt(Math.trunc(tipSol * 1e9));
  const log: DecisionLog = {
    t: now(),
    obligation: pk.toBase58(),
    protocol: 'save',
    mode: modeName(mode),
    debt_usd: debtUsd,
    ratio: engineRatio,
    repay_native: repay.toString(),
    quoted_usdc_out: usdcOut,
    est_profit_usdc: estProfit,
    fired: false,
    reason: '',
  };
  if (estProfit < cfg.minProfit + tipSol * solUsd) {
    log.reason = `below min profit (est $${estProfit.toFixed(2)})`;
    logDecision(runDir, log);
    return undefined;
  }

  const seizedUsd = repayUsd * (1.0 + coll.liquidationBonusPct / 100.0);
  const seize = BigInt(Math.trunc((seizedUsd / Math.max(coll.marketPrice, 1e-9)) * 10 ** coll.mintDecimals));
  const fire = await mk(repay, seize, tipLamports, ph);
  if (fire === undefined) {
    log.reason = 'final build failed';
    logDecision(runDir, log);
    return undefined;
  }
  if (!(await gate(fire.tx))) {
    log.reason = 'final fire sim revert (swap/repay would not cover the borrow)';
    logDecision(runDir, log);
    return undefined;
  }
  return { tx: fire.tx, mode, tipLamports, tipSol, estProfit, repay, debtUsd, ratio: engineRatio, built: performance.now() };
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
    obligation: pk.toBase58(),
    protocol: 'save',
    mode,
    debt_usd: cached.debtUsd,
    ratio: cached.ratio,
    repay_native: cached.repay.toString(),
    quoted_usdc_out: 0.0,
    est_profit_usdc: cached.estProfit,
    fired: false,
    reason: '',
  };
  const armedAgoMs = performance.now() - cached.built;
  console.log(
    `* SAVE LIQUIDATABLE [${mode}]  ${pk.toBase58().slice(0, 8)}  debt $${cached.debtUsd.toFixed(0)}  repay ${cached.repay}  est profit $${cached.estProfit.toFixed(2)}  tip ${cached.tipSol.toFixed(5)} SOL  (armed ${armedAgoMs.toFixed(0)}ms ago)`,
  );
  if (dryRun) {
    log.reason = `dry-run: would fire (${mode}, armed)`;
    logDecision(runDir, log);
    alert(webhook, 'save-dry', `DRY-RUN Save ${mode} liquidation ${pk.toBase58()} est profit $${cached.estProfit.toFixed(2)}`);
    return;
  }
  if (budget.wouldExceed(cached.tipSol)) {
    log.reason = 'daily tip cap';
    logDecision(runDir, log);
    alert(webhook, 'save-cap', 'daily tip cap reached');
    return;
  }
  if (await belowWalletFloor(endpoint, cfg.authority, walletMinSol)) {
    log.reason = 'wallet below floor';
    logDecision(runDir, log);
    alert(webhook, 'save-floor', 'wallet below floor — not firing');
    return;
  }
  const kpReq = kp!;
  const { repay, estProfit, tipLamports, tipSol } = cached;
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
    console.error(`[save] FIRED [${mode}] ${sig}${submit.bundleId ? ` bundle ${submit.bundleId}` : ''}`);
    logTrade<TradeLog>(runDir, {
      t: now(),
      obligation: pk.toBase58(),
      protocol: 'save',
      repay_native: repay.toString(),
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
          obligation: '',
          protocol: 'save',
          repay_native: '0',
          est_profit_usdc: 0.0,
          tip_lamports: '0',
          signature: sig,
          bundle: undefined,
          realized_usdc: pnl,
          error: undefined,
        });
        alert(webhook, 'save-landed', `Save liquidation landed ${sig}: realized $${pnl.toFixed(2)}`);
      },
      () => {
        void (async () => {
          const status = bundleId !== undefined ? await bundleStatus(crank.blockEngine, bundleId) : undefined;
          alert(webhook, 'save-miss', `Save liquidation ${sig} never confirmed (bundle status: ${status ?? ''})`);
        })();
      },
    );
  } else {
    console.error(`[save] send failed: ${submit.error}`);
    logTrade<TradeLog>(runDir, {
      t: now(),
      obligation: pk.toBase58(),
      protocol: 'save',
      repay_native: repay.toString(),
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
  const base: BaseCfg = loadBaseCfg({ runDir: 'runs', pollMs: 5000, rescanSecs: 30 });
  const { endpoint, dryRun, runDir } = base;
  const minDebt = Number.parseFloat(process.env.MIN_DEBT_USD ?? '') || 100.0;
  const armRatio = Number.parseFloat(process.env.ARM_RATIO ?? '') || 0.97;
  const armTtlMs = (Number.parseInt(process.env.ARM_TTL_SECS ?? '', 10) || 20) * 1000;
  const maxArm = Number.parseInt(process.env.MAX_ARM_PER_CYCLE ?? '', 10) || 8;
  const maxFire = Number.parseInt(process.env.MAX_FIRE_PER_CYCLE ?? '', 10) || 4;
  const maxSwapAccounts = Number.parseInt(process.env.MAX_SWAP_ACCOUNTS ?? '', 10) || 18;
  const watchRatio = Number.parseFloat(process.env.WATCH_RATIO ?? '') || 0.85;
  const ratioCap = Number.parseFloat(process.env.RATIO_CAP ?? '') || 3.0;
  const repayFracs = (process.env.REPAY_FRACS ?? '0.2,0.1,0.05')
    .split(',')
    .map((s) => Number.parseFloat(s.trim()))
    .filter((n) => Number.isFinite(n));

  const { kp, authority } = loadKeypair(dryRun);
  const cfg: Cfg = {
    authority,
    tipAccount: base.tipAccount,
    tipFractionBps: base.tipFractionBps,
    minTipSol: base.minTipSol,
    minProfit: base.minProfitUsd,
    slippageBps: base.slippageBps,
    maxSwapAccounts,
  };

  const lazerTable = pyth.newTable();
  const mintFeed = mintFeedExt();
  let lazerOn = false;
  const lazerToken = process.env.PYTH_LAZER_TOKEN;
  if (lazerToken) {
    lazer.spawnLazerThread(lazerToken, armFeeds(), lazerTable);
    lazerOn = true;
    console.error('[save] Pyth Lazer event-driven trigger ENABLED');
  } else {
    console.error('[save] WARNING: no PYTH_LAZER_TOKEN — falling back to slow poll (the 30s regression). Set the token for ms detection.');
  }

  const crank = await loadCrankCtx('[save]', lazerOn);

  const debtReserves = new Map<string, save.Reserve>();
  for (const resStr of save.DEBT_RESERVES) {
    const pk = new PublicKey(resStr);
    const raw = await getAccount(endpoint, pk);
    if (raw === undefined) throw new Error(`fetch debt reserve ${resStr}`);
    const r = save.decodeReserve(pk, raw);
    if (r === null) throw new Error(`decode debt reserve ${resStr}`);
    debtReserves.set(pk.toBase58(), r);
  }

  console.error(
    `[save] Solend liquidation executor ${dryRun ? '[DRY RUN]' : '[LIVE]'}  authority=${authority.toBase58()}  min_debt=$${minDebt} rescan=${base.rescanSecs}s tick_poll=${base.tickPollMs}ms lazer=${lazerOn} crank=${crank.on}`,
  );
  if (!dryRun) {
    const bal = await solBalance(endpoint, authority.toBase58());
    console.error(`[save] wallet balance: ${bal} SOL`);
    if (bal < base.walletMinSol) throw new Error('wallet below floor');
  }

  const engine = new SaveEngine(minDebt, ratioCap);
  const ctpCache = new Map<string, PublicKey>();
  let scan = await fullScanSave(endpoint, debtReserves, minDebt, ctpCache);
  if (scan === undefined) throw new Error('initial scan');
  let lastScan = performance.now();

  const budget = new DailyTipBudget(base.maxDailyTipSol);
  let freshBh = '11111111111111111111111111111111111111111111';
  let lastBh = performance.now() - 9999_000;
  const handled = new Map<string, number>();
  const simRejected = new Map<string, number>();
  const cache = new Map<string, CachedFire>();
  let lastTickUs = 0;
  let lastHb = performance.now() - 9999_000;
  let armDeferred = 0;
  let first = true;

  const lazerSnapshot = (): Map<number, number> => {
    const m = new Map<number, number>();
    for (const f of armFeeds()) {
      const p = pyth.get(lazerTable, f);
      if (p !== undefined) m.set(f, p.price);
    }
    return m;
  };

  for (;;) {
    if (first || performance.now() - lastScan >= base.rescanSecs * 1000) {
      if (!first) {
        const s = await fullScanSave(endpoint, debtReserves, minDebt, ctpCache);
        if (s !== undefined) scan = s;
      }
      lastScan = performance.now();
      const snap = lazerSnapshot();
      const armed = engine.rebuild(scan!.obls, scan!.reserves, mintFeed, watchRatio, snap);
      console.error(
        `[save] scan: ${scan!.obls.length} v1 USDC/USDT/wSOL-debt obligations (>= $${minDebt}) -> engine watch-set ${armed} (ratio >= ${watchRatio})`,
      );
      if (crank.on) {
        const feeds = new Set<string>();
        for (const w of engine.accounts) {
          if (!scan!.crankable.has(w.collReserve.toBase58())) continue;
          const fid = scan!.feedOf.get(w.collReserve.toBase58());
          if (fid !== undefined) feeds.add(fid.toString('hex'));
        }
        console.error(`[save] crank: ${scan!.crankable.size} crankable collateral reserves, ${feeds.size} feeds in Hermes cache`);
        crank.hermes.setFeeds([...feeds]);
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

    let snap: Map<number, number>;
    if (lazerOn) {
      const deadline = performance.now() + base.pollMs;
      for (;;) {
        let cur = 0;
        for (const f of armFeeds()) {
          const p = pyth.get(lazerTable, f);
          if (p !== undefined) cur = Math.max(cur, p.tsUs);
        }
        if (cur > lastTickUs) {
          lastTickUs = cur;
          break;
        }
        if (performance.now() >= deadline) break;
        await sleep(base.tickPollMs);
      }
      snap = lazerSnapshot();
    } else {
      await sleep(base.pollMs);
      snap = lazerSnapshot();
    }

    const liveFire = engine
      .onchainLiquidatableRanked()
      .filter(([pk]) => {
        const t = simRejected.get(pk.toBase58());
        return t === undefined || performance.now() - t >= base.simCooldownSecs * 1000;
      });
    const fireDeferred = Math.max(0, liveFire.length - maxFire);
    const crossed = liveFire.slice(0, maxFire).map(([pk]) => pk);

    if (lazerOn && base.heartbeatSecs > 0 && performance.now() - lastHb >= base.heartbeatSecs * 1000) {
      const totalFeeds = armFeeds().length;
      const lazerNear = engine.crossed(snap, armRatio).length;
      const lazerFlagged = engine.crossed(snap, 1.0).length;
      const onchainLiq = engine.onchainLiquidatableCount();
      const liveFireCt = engine
        .onchainLiquidatableRanked()
        .filter(([pk]) => {
          const t = simRejected.get(pk.toBase58());
          return t === undefined || performance.now() - t >= base.simCooldownSecs * 1000;
        }).length;
      let freshest = 0;
      for (const f of armFeeds()) {
        const p = pyth.get(lazerTable, f);
        if (p !== undefined) freshest = Math.max(freshest, p.tsUs);
      }
      const lagMs = Math.max(0, Math.trunc((nowUs() - freshest) / 1000));
      const defer = fireDeferred + armDeferred > 0 ? ` | DEFERRED fire ${fireDeferred}/arm ${armDeferred}` : '';
      console.error(
        `[hb] lazer feeds ${snap.size}/${totalFeeds} live | detect_lag ${lagMs}ms | watch ${engine.accounts.length} | lazer-flagged ${lazerFlagged} (>=arm(${armRatio}) ${lazerNear}) | on-chain liquidatable ${onchainLiq} | LIVE fire ${liveFireCt} (cap ${maxFire}) | cache ${cache.size}${defer} | ${lazer.status(lazerTable)}`,
      );
      lastHb = performance.now();
    }

    if (lazerOn) {
      const fireKeys = new Set(crossed.map((pk) => pk.toBase58()));
      for (const [key, c] of cache) {
        if (!fireKeys.has(key) || performance.now() - c.built >= armTtlMs) cache.delete(key);
      }
      const candidates = crossed
        .filter((pk) => !cache.has(pk.toBase58()))
        .filter((pk) => {
          const t = simRejected.get(pk.toBase58());
          return t === undefined || performance.now() - t >= base.simCooldownSecs * 1000;
        });
      armDeferred = Math.max(0, candidates.length - maxArm);
      for (const pk of candidates.slice(0, maxArm)) {
        const ratio = engine.onchainRatioOf(pk) ?? 0.0;
        const c = await tryArm(endpoint, runDir, cfg, crank, scan!, pk, repayFracs, ratio);
        if (c !== undefined) cache.set(pk.toBase58(), c);
        else simRejected.set(pk.toBase58(), performance.now());
      }
    }

    const toFire = crossed.filter((pk) => {
      const h = handled.get(pk.toBase58());
      return h === undefined || performance.now() - h >= base.handleCooldownSecs * 1000;
    });
    if (toFire.length === 0) continue;

    for (const pk of toFire) {
      const key = pk.toBase58();
      handled.set(key, performance.now());
      const ratio = engine.onchainRatioOf(pk) ?? 1.0;
      let cached = cache.get(key);
      if (cached !== undefined && performance.now() - cached.built < armTtlMs) {
        cache.delete(key);
      } else {
        cached = undefined;
        const rej = simRejected.get(key);
        if (rej !== undefined && performance.now() - rej < base.simCooldownSecs * 1000) {
          // still cooling down — skip inline arm this tick
        } else {
          const c = await tryArm(endpoint, runDir, cfg, crank, scan!, pk, repayFracs, ratio);
          if (c !== undefined) cached = c;
          else simRejected.set(key, performance.now());
        }
      }
      if (cached !== undefined) {
        const armedFromCache = performance.now() - cached.built > 0;
        const fireStart = nowUs();
        await fireCached(endpoint, runDir, base.senderUrl, cfg, crank, dryRun, pk, cached, freshBh, kp, budget, base.walletMinSol, base.webhook);
        const done = nowUs();
        logLatency(runDir, {
          t: now(),
          obligation: key,
          protocol: 'save',
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
