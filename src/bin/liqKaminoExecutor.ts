// Port of src/bin/liq_kamino_executor.rs
//
// Production Kamino (KLend) liquidation executor — EVENT-DRIVEN, DRY_RUN default.
//
// Mirrors the marginfi/Save executors: a Lazer WebSocket feeds an in-memory
// health engine (kaminoEngine.ts) that recomputes every obligation's
// bfDebt/threshold on each ~ms price tick with ZERO RPC.
//
// TWO-TIER gating (the overflag fix): Lazer NARROWS the set; the ON-CHAIN Scope
// price GATES the expensive work. KLend liquidations settle at the on-chain
// Scope oracle, and Lazer LEADS/diverges from Scope, so the Lazer-projected
// "liquidatable" set is mostly phantoms that are healthy on-chain. So:
//   full scan (RESCAN_SECS): v1 obligations + their reserves -> kaminoEngine
//     watch-set (stored on-chain health + per-side Lazer anchors)
//   ARM tier (cheap, Lazer): the near-threshold watch-set — zero RPC, no
//     Jupiter, no sim. Only narrows who's worth watching.
//   FIRE tier (expensive): ONLY obligations liquidatable at the on-chain Scope
//     price (engine.onChainLiquidatableRanked), ranked by USD deficit, capped
//     to MAX_FIRE_PER_CYCLE. These get the Jupiter quote + sim + submit.
//
// Kamino prices via Scope (its own oracle) which we cannot crank ourselves, so
// unlike Save there is no crank/bundle mode — a single Sender tx. Safety is
// profit-or-revert: the JupLend fixed-amount payback fails unless the
// seized-collateral swap covered the flash-borrow.
//
// v1.5 debt scope: any debt with a wired JupLend flash market (USDC/USDT/wSOL).
//
// Usage: HELIUS_RPC=<url> [DRY_RUN=1] [KEYPAIR_PATH=~/arb-keypair.json]
//        [PYTH_LAZER_TOKEN=... (required for event-driven)] [MIN_DEBT_USD=100]
//        [MIN_PROFIT_USD=0.5] [CLOSE_FACTOR=0.2] [MAX_BORROW_USD=5000]
//        [WATCH_RATIO=0.9] [ARM_RATIO=0.97] [RATIO_CAP=3] [RESCAN_SECS=30]
//        [TICK_POLL_MS=1] [POLL_MS=5000] [MAX_FIRE_PER_CYCLE=4]
//        [SIM_COOLDOWN_SECS=60] [HANDLE_COOLDOWN_SECS=20] [JUP_API_BASE=...]
//        [SLIPPAGE_BPS=100] [MAX_SWAP_ACCOUNTS=20]
//        npx tsx src/bin/liqKaminoExecutor.ts

import 'dotenv/config';
import { Keypair, PublicKey, type VersionedTransaction } from '@solana/web3.js';
import { decodeReserve, recompute, type Obligation, type Reserve } from '../lib/kamino.js';
import { Engine as KaminoEngine } from '../lib/kaminoEngine.js';
import { buildFireTx as buildKaminoFireTx, type KaminoFireCandidate } from '../lib/kaminoFire.js';
import { ReserveAccounts } from '../lib/kaminoIx.js';
import * as flashloan from '../lib/flashloan.js';
import { alert, logDecision, logTrade } from '../lib/observe.js';
import * as lazer from '../lib/lazer.js';
import * as pyth from '../lib/pyth.js';
import {
  b64,
  belowWalletFloor,
  DailyTipBudget,
  DEFAULT_AUTHORITY,
  getMultiple,
  latestBlockhash,
  loadBaseCfg,
  loadKeypair,
  logLatency,
  mintOwner,
  now,
  nowUs,
  rpc,
  signAndSubmit,
  simulateTxB64,
  sleep,
  solBalance,
  spawnPnlReadback,
  txToB64,
  type BaseCfg,
} from '../lib/liqExecutor.js';

const KLEND = 'KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD';
const MAIN_MARKET = '7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF';
const OBLIGATION_SIZE = 3344;
/** [cu, cu_price, ata, ata, ata, borrow, refresh, refresh, refresh_ob, LIQUIDATE, ...] */
const LIQUIDATE_IX_INDEX = 9;

interface DecisionLog {
  t: number;
  obligation: string;
  protocol: 'kamino';
  ratio: number;
  debt_usd: number;
  repay_usd: number;
  quoted_usdc_out: number;
  est_profit_usdc: number;
  fire_sim_ok: boolean;
  fired: boolean;
  reason: string;
}
interface TradeLog {
  t: number;
  obligation: string;
  protocol: 'kamino';
  repay_usd: number;
  est_profit_usdc: number;
  tip_lamports: string;
  signature: string | undefined;
  realized_usdc: number | undefined;
  error: string | undefined;
}

/** Full-tx sim outcome, classified by where it stopped. */
type SimClass = { kind: 'clean' } | { kind: 'liquidateGate' } | { kind: 'otherRevert'; ix: number } | { kind: 'reject' };

async function simClass(endpoint: string, txB64: string): Promise<SimClass> {
  const res = await simulateTxB64(endpoint, txB64);
  if (res === undefined) return { kind: 'reject' };
  if (res.err == null) return { kind: 'clean' };
  const ixIdx = res.err?.InstructionError?.[0];
  if (typeof ixIdx !== 'number') return { kind: 'reject' };
  if (ixIdx === LIQUIDATE_IX_INDEX) return { kind: 'liquidateGate' };
  return { kind: 'otherRevert', ix: ixIdx };
}

interface KaminoScan {
  obls: Array<[PublicKey, Obligation]>;
  obIndex: Map<string, Obligation>;
  reserveFeed: Map<string, number>;
  reserveMint: Map<string, PublicKey>;
}

async function scanObligations(endpoint: string): Promise<Array<[PublicKey, Obligation]>> {
  const resp = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getProgramAccounts',
    params: [
      KLEND,
      {
        encoding: 'base64',
        dataSlice: { offset: 0, length: 2288 },
        filters: [{ dataSize: OBLIGATION_SIZE }, { memcmp: { offset: 32, bytes: MAIN_MARKET } }],
      },
    ],
  });
  const arr: any[] = Array.isArray(resp?.result) ? resp.result : [];
  const { decodeObligation } = await import('../lib/kamino.js');
  const out: Array<[PublicKey, Obligation]> = [];
  for (const e of arr) {
    const pkStr = e?.pubkey;
    const raw = b64(e?.account?.data);
    if (typeof pkStr !== 'string' || raw === undefined) continue;
    const o = decodeObligation(raw);
    if (o === null) continue;
    if (o.deposits.length !== 1 || o.borrows.length !== 1 || o.elevationGroup !== 0) continue;
    out.push([new PublicKey(pkStr), o]);
  }
  return out;
}

async function fullScanKamino(endpoint: string, minDebt: number, mintFeed: Map<string, number>): Promise<KaminoScan | undefined> {
  const obls = await scanObligations(endpoint);
  if (obls.length === 0) return undefined;
  const reservePkSet = new Set<string>();
  const reservePks: PublicKey[] = [];
  for (const [, o] of obls) {
    for (const [r] of [...o.deposits, ...o.borrows]) {
      const key = r.toBase58();
      if (!reservePkSet.has(key)) {
        reservePkSet.add(key);
        reservePks.push(r);
      }
    }
  }
  const raw = await getMultiple(endpoint, reservePks);
  const reserveFeed = new Map<string, number>();
  const reserveMint = new Map<string, PublicKey>();
  const reserves = new Map<string, Reserve>();
  for (const pk of reservePks) {
    const key = pk.toBase58();
    const d = raw.get(key);
    if (d === undefined) continue;
    const ra = ReserveAccounts.decode(pk, d);
    if (ra !== null) {
      reserveMint.set(key, ra.liquidityMint);
      const f = mintFeed.get(ra.liquidityMint.toBase58());
      if (f !== undefined) reserveFeed.set(key, f);
    }
    const r = decodeReserve(d);
    if (r !== null) reserves.set(key, r);
  }

  const obls2: Array<[PublicKey, Obligation]> = [];
  for (const [pk, o] of obls) {
    const rc = recompute(o, reserves);
    let o2 = o;
    if (rc.missing === 0 && !rc.elevation) {
      const borrowedValue = o.borrows.reduce((sum, [res, bamt]) => {
        const r = reserves.get(res.toBase58());
        if (r === undefined) return sum;
        return sum + (bamt / 10 ** r.mintDecimals) * r.marketPrice;
      }, 0.0);
      o2 = {
        ...o,
        bfAdjustedDebt: rc.bfAdjustedDebt,
        unhealthyBorrowValue: rc.unhealthyBorrowValue,
        allowedBorrowValue: rc.allowedBorrowValue,
        depositedValue: rc.depositedValue,
        borrowedValue,
      };
    }
    if (o2.borrowedValue >= minDebt) obls2.push([pk, o2]);
  }
  const obIndex = new Map(obls2.map(([pk, o]) => [pk.toBase58(), o]));
  return { obls: obls2, obIndex, reserveFeed, reserveMint };
}

interface Cfg {
  authority: PublicKey;
  tipAccount: PublicKey;
  tipFractionBps: bigint;
  minTipSol: number;
  minProfit: number;
  closeFactor: number;
  maxBorrowUsd: number;
  slippageBps: number;
  maxSwapAccounts: number;
}

interface CachedFire {
  tx: VersionedTransaction;
  tipLamports: bigint;
  tipSol: number;
  estProfit: number;
  repayUsd: number;
  debtUsd: number;
  ratio: number;
  clean: boolean;
  built: number;
}

async function tryArm(
  endpoint: string,
  runDir: string,
  cfg: Cfg,
  scan: KaminoScan,
  pk: PublicKey,
  engineRatio: number,
  tpCache: Map<string, PublicKey>,
): Promise<CachedFire | undefined> {
  const market = new PublicKey(MAIN_MARKET);
  const o = scan.obIndex.get(pk.toBase58());
  if (o === undefined) return undefined;
  if (o.deposits.length !== 1 || o.borrows.length !== 1 || o.elevationGroup !== 0) return undefined;
  const withdrawPk = o.deposits[0]![0];
  const repayPk = o.borrows[0]![0];

  const log: DecisionLog = {
    t: now(),
    obligation: pk.toBase58(),
    protocol: 'kamino',
    ratio: engineRatio,
    debt_usd: 0.0,
    repay_usd: 0.0,
    quoted_usdc_out: 0.0,
    est_profit_usdc: 0.0,
    fire_sim_ok: false,
    fired: false,
    reason: '',
  };
  const skip = (reason: string): void => {
    log.reason = reason;
    logDecision(runDir, log);
  };

  const raw = await getMultiple(endpoint, [withdrawPk, repayPk]);
  const wrData = raw.get(withdrawPk.toBase58());
  const rrData = raw.get(repayPk.toBase58());
  if (wrData === undefined || rrData === undefined) {
    skip('reserve fetch failed');
    return undefined;
  }
  const wr = ReserveAccounts.decode(withdrawPk, wrData);
  const rr = ReserveAccounts.decode(repayPk, rrData);
  if (wr === null || rr === null) {
    skip('reserve accounts decode failed');
    return undefined;
  }
  const wrRes = decodeReserve(wrData);
  const rrRes = decodeReserve(rrData);
  if (wrRes === null || rrRes === null) {
    skip('reserve decode failed');
    return undefined;
  }
  if (!flashloan.hasMarket(rr.liquidityMint)) {
    skip('debt mint has no wired flash market');
    return undefined;
  }

  const debtDec = rrRes.mintDecimals;
  const debtPrice = Math.max(rrRes.marketPrice, 1e-9);
  const borrowAmt = o.borrows[0]![1];
  const debtUsd = (borrowAmt / 10 ** debtDec) * rrRes.marketPrice;
  const repayUsd = Math.max(Math.min(debtUsd * cfg.closeFactor, cfg.maxBorrowUsd), 1.0);
  const repayAmount = BigInt(Math.trunc((repayUsd / debtPrice) * 10 ** debtDec));
  const bonus = 1.05;
  const seizedNative = ((repayUsd * bonus) / Math.max(wrRes.marketPrice, 1e-9)) * 10 ** wrRes.mintDecimals;
  const swapInAmount = BigInt(Math.trunc(seizedNative * 0.995));
  log.debt_usd = debtUsd;
  log.repay_usd = repayUsd;

  const withdrawLiquidityTp = await mintOwner(endpoint, wr.liquidityMint, tpCache);
  const withdrawCollateralTp = await mintOwner(endpoint, wr.collateralMint, tpCache);
  const repayLiquidityTp = await mintOwner(endpoint, rr.liquidityMint, tpCache);
  if (withdrawLiquidityTp === undefined || withdrawCollateralTp === undefined || repayLiquidityTp === undefined) {
    skip('mint owner lookup failed');
    return undefined;
  }
  const cand: KaminoFireCandidate = {
    obligation: pk,
    lendingMarket: market,
    repayReserve: rr,
    withdrawReserve: wr,
    obligationReserves: [withdrawPk, repayPk],
    withdrawLiquidityMint: wr.liquidityMint,
    withdrawLiquidityTokenProgram: withdrawLiquidityTp,
    withdrawCollateralTokenProgram: withdrawCollateralTp,
    repayLiquidityTokenProgram: repayLiquidityTp,
    repayAmount,
    swapInAmount,
  };
  const ph = '11111111111111111111111111111111111111111111';

  let fire;
  try {
    fire = await buildKaminoFireTx(endpoint, cand, cfg.authority, null, 0n, 100_000n, cfg.slippageBps, cfg.maxSwapAccounts, ph);
  } catch (e) {
    skip(`build: ${e}`);
    return undefined;
  }
  const quotedUsd = (Number(fire.quotedUsdcOut) / 10 ** debtDec) * debtPrice;
  const estProfit = quotedUsd - repayUsd;
  log.quoted_usdc_out = quotedUsd;
  log.est_profit_usdc = estProfit;
  const solUsd = 150.0;
  const tipSol = Math.max((estProfit * Number(cfg.tipFractionBps)) / 10_000.0 / solUsd, cfg.minTipSol);
  const tipLamports = BigInt(Math.trunc(tipSol * 1e9));
  if (estProfit < cfg.minProfit + tipSol * solUsd) {
    skip(`below min profit (est $${estProfit.toFixed(2)}, tip $${(tipSol * solUsd).toFixed(2)})`);
    return undefined;
  }

  try {
    fire = await buildKaminoFireTx(endpoint, cand, cfg.authority, cfg.tipAccount, tipLamports, 100_000n, cfg.slippageBps, cfg.maxSwapAccounts, ph);
  } catch (e) {
    skip(`rebuild: ${e}`);
    return undefined;
  }
  const cls = await simClass(endpoint, txToB64(fire.tx));
  const clean = cls.kind === 'clean';
  log.fire_sim_ok = cls.kind === 'clean' || cls.kind === 'liquidateGate';
  if (cls.kind === 'otherRevert') {
    skip(`sim revert at ix ${cls.ix} (wiring) — not arming`);
    return undefined;
  }
  if (cls.kind === 'reject') {
    skip('sim rejected by RPC');
    return undefined;
  }
  log.reason = clean ? 'armed (clean — liquidatable on-chain now)' : 'armed (ahead — reverts at liquidate gate until Scope crosses)';
  logDecision(runDir, log);
  return { tx: fire.tx, tipLamports, tipSol, estProfit, repayUsd, debtUsd, ratio: engineRatio, clean, built: performance.now() };
}

async function fireCached(
  endpoint: string,
  runDir: string,
  senderUrl: string,
  cfg: Cfg,
  dryRun: boolean,
  pk: PublicKey,
  cached: CachedFire,
  freshBh: string,
  kp: Keypair | undefined,
  budget: DailyTipBudget,
  walletMinSol: number,
  webhook: string | undefined,
): Promise<void> {
  const log: DecisionLog = {
    t: now(),
    obligation: pk.toBase58(),
    protocol: 'kamino',
    ratio: cached.ratio,
    debt_usd: cached.debtUsd,
    repay_usd: cached.repayUsd,
    quoted_usdc_out: 0.0,
    est_profit_usdc: cached.estProfit,
    fire_sim_ok: true,
    fired: false,
    reason: '',
  };
  const armedAgoMs = performance.now() - cached.built;
  console.log(
    `* KAMINO LIQUIDATABLE ${pk.toBase58().slice(0, 8)}  debt $${cached.debtUsd.toFixed(0)}  repay $${cached.repayUsd.toFixed(2)}  est profit $${cached.estProfit.toFixed(2)}  tip ${cached.tipSol.toFixed(5)} SOL  (${cached.clean ? 'clean' : 'ahead'} armed ${armedAgoMs.toFixed(0)}ms ago)`,
  );
  if (dryRun) {
    log.reason = `dry-run: would fire (armed, ${cached.clean ? 'clean' : 'ahead'})`;
    logDecision(runDir, log);
    alert(webhook, 'kliq-dry', `DRY-RUN Kamino liq ${pk.toBase58()} est profit $${cached.estProfit.toFixed(2)}`);
    return;
  }
  if (budget.wouldExceed(cached.tipSol)) {
    log.reason = 'daily tip cap';
    logDecision(runDir, log);
    alert(webhook, 'kliq-cap', 'daily tip cap reached');
    return;
  }
  if (await belowWalletFloor(endpoint, cfg.authority, walletMinSol)) {
    log.reason = 'wallet below floor';
    logDecision(runDir, log);
    alert(webhook, 'kliq-floor', 'wallet below floor — not firing');
    return;
  }
  const kpReq = kp!;
  const { repayUsd, estProfit, tipLamports, tipSol } = cached;
  log.fired = true;
  log.reason = 'fired (armed cache)';
  logDecision(runDir, log);
  const submit = await signAndSubmit(cached.tx, kpReq, freshBh, senderUrl, undefined);
  if (submit.ok) {
    const sig = submit.signature;
    console.error(`[kexec] FIRED ${sig}`);
    logTrade<TradeLog>(runDir, {
      t: now(),
      obligation: pk.toBase58(),
      protocol: 'kamino',
      repay_usd: repayUsd,
      est_profit_usdc: estProfit,
      tip_lamports: tipLamports.toString(),
      signature: sig,
      realized_usdc: undefined,
      error: undefined,
    });
    spawnPnlReadback(
      endpoint,
      sig,
      cfg.authority.toBase58(),
      (pnl) => {
        budget.add(tipSol);
        logTrade<TradeLog>(runDir, {
          t: now(),
          obligation: '',
          protocol: 'kamino',
          repay_usd: 0.0,
          est_profit_usdc: 0.0,
          tip_lamports: '0',
          signature: sig,
          realized_usdc: pnl,
          error: undefined,
        });
        alert(webhook, 'kliq-landed', `Kamino liq landed ${sig}: realized $${pnl.toFixed(2)}`);
      },
      () => alert(webhook, 'kliq-miss', `Kamino liq ${sig} never confirmed`),
    );
  } else {
    console.error(`[kexec] send failed: ${submit.error}`);
    logTrade<TradeLog>(runDir, {
      t: now(),
      obligation: pk.toBase58(),
      protocol: 'kamino',
      repay_usd: repayUsd,
      est_profit_usdc: estProfit,
      tip_lamports: tipLamports.toString(),
      signature: undefined,
      realized_usdc: undefined,
      error: submit.error,
    });
  }
}

async function main(): Promise<void> {
  const base: BaseCfg = loadBaseCfg({ runDir: 'runs', pollMs: 5000, rescanSecs: 30 });
  const { endpoint, dryRun, runDir } = base;
  const minDebt = Number.parseFloat(process.env.MIN_DEBT_USD ?? '') || 100.0;
  const ratioCap = Number.parseFloat(process.env.RATIO_CAP ?? '') || 3.0;
  const closeFactor = Number.parseFloat(process.env.CLOSE_FACTOR ?? '') || 0.2;
  const maxBorrowUsd = Number.parseFloat(process.env.MAX_BORROW_USD ?? '') || 5000.0;
  const watchRatio = Number.parseFloat(process.env.WATCH_RATIO ?? '') || 0.9;
  const armRatio = Number.parseFloat(process.env.ARM_RATIO ?? '') || 0.97;
  const maxFire = Number.parseInt(process.env.MAX_FIRE_PER_CYCLE ?? '', 10) || 4;
  const maxSwapAccounts = Number.parseInt(process.env.MAX_SWAP_ACCOUNTS ?? '', 10) || 20;

  const { kp, authority } = loadKeypair(dryRun);
  const cfg: Cfg = {
    authority,
    tipAccount: base.tipAccount,
    tipFractionBps: base.tipFractionBps,
    minTipSol: base.minTipSol,
    minProfit: base.minProfitUsd,
    closeFactor,
    maxBorrowUsd,
    slippageBps: base.slippageBps,
    maxSwapAccounts,
  };

  const lazerTable = pyth.newTable();
  const mintFeed = lazer.mintFeedMap();
  let lazerOn = false;
  const lazerToken = process.env.PYTH_LAZER_TOKEN;
  if (lazerToken) {
    lazer.spawnLazerThread(lazerToken, lazer.armFeedIds(), lazerTable);
    lazerOn = true;
    console.error('[kexec] Pyth Lazer event-driven trigger ENABLED');
  } else {
    console.error('[kexec] WARNING: no PYTH_LAZER_TOKEN — falling back to slow poll (the regression). Set the token for ms detection.');
  }

  console.error(
    `[kexec] Kamino liquidation executor ${dryRun ? '[DRY RUN]' : '[LIVE]'}  authority=${authority.toBase58()}  min_debt=$${minDebt} min_profit=$${base.minProfitUsd} rescan=${base.rescanSecs}s tick_poll=${base.tickPollMs}ms lazer=${lazerOn}`,
  );
  if (!dryRun) {
    const bal = await solBalance(endpoint, authority.toBase58());
    console.error(`[kexec] wallet balance: ${bal} SOL`);
    if (bal < base.walletMinSol) throw new Error(`wallet below floor ${base.walletMinSol}`);
  }

  const engine = new KaminoEngine(minDebt, ratioCap);
  let scan = await fullScanKamino(endpoint, minDebt, mintFeed);
  if (scan === undefined) throw new Error('initial scan');
  let lastScan = performance.now();
  const tpCache = new Map<string, PublicKey>();

  const budget = new DailyTipBudget(base.maxDailyTipSol);
  let freshBh = '11111111111111111111111111111111111111111111';
  let lastBh = performance.now() - 9999_000;
  const handled = new Map<string, number>();
  const simRejected = new Map<string, number>();
  let lastTickUs = 0;
  let lastHb = performance.now() - 9999_000;
  let fireDeferred = 0;
  const loggedUnwired = new Set<string>();
  let first = true;

  const lazerSnapshot = (): Map<number, number> => {
    const m = new Map<number, number>();
    for (const f of lazer.armFeedIds()) {
      const p = pyth.get(lazerTable, f);
      if (p !== undefined) m.set(f, p.price);
    }
    return m;
  };

  for (;;) {
    if (first || performance.now() - lastScan >= base.rescanSecs * 1000) {
      if (!first) {
        const s = await fullScanKamino(endpoint, minDebt, mintFeed);
        if (s !== undefined) scan = s;
      }
      lastScan = performance.now();
      const snap = lazerSnapshot();
      const armed = engine.rebuild(scan!.obls, scan!.reserveFeed, watchRatio, snap);
      console.error(`[kexec] scan: ${scan!.obls.length} v1 obligations (>= $${minDebt}) -> engine watch-set ${armed} (ratio >= ${watchRatio})`);
      let unwiredNow = 0;
      for (const w of engine.accounts) {
        const mint = scan!.reserveMint.get(w.debtReserve.toBase58());
        if (mint === undefined || flashloan.hasMarket(mint)) continue;
        unwiredNow += 1;
        const key = mint.toBase58();
        if (!loggedUnwired.has(key)) {
          loggedUnwired.add(key);
          console.error(`[kexec] unwired debt mint (no JupLend flash market) — will skip: ${key}`);
        }
      }
      if (unwiredNow > 0) {
        console.error(`[kexec] ${unwiredNow}/${engine.accounts.length} watch-set obligations have an unwired debt mint (excluded from fire candidates)`);
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
        for (const f of lazer.armFeedIds()) {
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

    if (lazerOn && base.heartbeatSecs > 0 && performance.now() - lastHb >= base.heartbeatSecs * 1000) {
      const totalFeeds = lazer.armFeedIds().length;
      const near = engine.crossed(snap, armRatio).length;
      const lazerFlagged = engine.crossed(snap, 1.0).length;
      const onChain = engine.onChainLiquidatableCount();
      let freshest = 0;
      for (const f of lazer.armFeedIds()) {
        const p = pyth.get(lazerTable, f);
        if (p !== undefined) freshest = Math.max(freshest, p.tsUs);
      }
      const lagMs = Math.max(0, Math.trunc((nowUs() - freshest) / 1000));
      const defer = fireDeferred > 0 ? ` | DEFERRED fire ${fireDeferred}/cycle` : '';
      console.error(
        `[hb] lazer feeds ${snap.size}/${totalFeeds} live | detect_lag ${lagMs}ms | watch ${engine.accounts.length} | ${near} within arm(${armRatio}) | lazer-flagged ${lazerFlagged} | on-chain liquidatable ${onChain} | fire-cap ${maxFire}${defer} | ${lazer.status(lazerTable)}`,
      );
      lastHb = performance.now();
    }

    const fireRanked = engine.onChainLiquidatableRanked();
    const isWired = (pk: PublicKey): boolean => {
      const rr = engine.reservesOf(pk);
      if (rr === null) return false;
      const [, debt] = rr;
      const mint = scan!.reserveMint.get(debt.toBase58());
      return mint !== undefined && flashloan.hasMarket(mint);
    };
    const fireCandidates = fireRanked
      .map(([pk]) => pk)
      .filter(isWired)
      .filter((pk) => {
        const h = handled.get(pk.toBase58());
        return h === undefined || performance.now() - h >= base.handleCooldownSecs * 1000;
      })
      .filter((pk) => {
        const t = simRejected.get(pk.toBase58());
        return t === undefined || performance.now() - t >= base.simCooldownSecs * 1000;
      });
    fireDeferred = Math.max(0, fireCandidates.length - maxFire);
    for (const pk of fireCandidates.slice(0, maxFire)) {
      handled.set(pk.toBase58(), performance.now());
      const w = engine.accounts.find((w2) => w2.obligation.equals(pk));
      const ratio = w?.onChainRatio() ?? 1.0;
      const fireStart = nowUs();
      const c = await tryArm(endpoint, runDir, cfg, scan!, pk, ratio, tpCache);
      if (c !== undefined) {
        await fireCached(endpoint, runDir, base.senderUrl, cfg, dryRun, pk, c, freshBh, kp, budget, base.walletMinSol, base.webhook);
        const done = nowUs();
        if (lazerOn) {
          logLatency(runDir, {
            t: now(),
            obligation: pk.toBase58(),
            protocol: 'kamino',
            clean: c.clean,
            appeared_us: lastTickUs,
            detected_lag_ms: Math.max(0, Math.trunc((fireStart - lastTickUs) / 1000)),
            submit_lag_ms: Math.max(0, Math.trunc((done - lastTickUs) / 1000)),
            fire_submit_ms: Math.trunc((done - fireStart) / 1000),
            armed: false,
            dry_run: dryRun,
          });
        }
      } else {
        simRejected.set(pk.toBase58(), performance.now());
      }
    }
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
