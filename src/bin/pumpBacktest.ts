// Port of src/bin/pump_backtest.rs
//
// pump_backtest — replay a collected pump.fun event dataset chronologically
// and simulate candidate strategies against REAL data, reporting honest
// per-strategy EV, win-rate, and PnL distribution.
//
// It reads `runs/pump/events.jsonl` (produced by `pumpCollect`) and NEVER
// signs or submits a transaction. It shares no state with the liquidation
// engine.
//
// ════════════════════════════════════════════════════════════════════════════
//  #1 CORRECTNESS INVARIANT: NO LOOK-AHEAD BIAS — enforced STRUCTURALLY
// ════════════════════════════════════════════════════════════════════════════
// A decision at time T may only use information observable at or before T. We
// do not rely on discipline; the code makes the leak impossible by
// construction:
//
//  * Each mint's events are sorted once into strict chronological order.
//  * The ENTRY decision is computed in ONE dedicated pass over the prefix
//    slice `events[..k]` where `k` is chosen so every event in it has
//    `ms <= T_entry` (`T_entry = create_ms + detection_latency`). The entry's
//    curve price, the dev-allocation / dev-prebuy flags, the first-second
//    buyer count and the liquidity are ALL derived from that prefix and
//    nothing else. The suffix (the future) is simply not in scope for the
//    entry — it is a different slice.
//  * The EXIT is a forward-only walk over the suffix `events[k..]`. It
//    consumes events one at a time, updating the "as-of now" curve state to
//    each event's POST-trade reserves and then testing the exit rule. It
//    never indexes ahead. A take-profit fires only when the replayed price
//    ACTUALLY crossed the target at some t' > entry; a stop/dev-dump that
//    crashes the price first is seen first (in event order), so the strategy
//    eats the post-crash price. A time-based exit fires at `T_exit` using the
//    state as-of `T_exit` (the state BEFORE the first event past it), never
//    the crashing event's own price.
//  * (The Rust assert-backed unit tests pinning this boundary are not ported
//    — no test harness set up yet — but the invariant they proved is
//    preserved exactly in the simulate() logic below: an entry cannot see a
//    future price spike, and a spike after a later entry-time IS seen.)
//
// ════════════════════════════════════════════════════════════════════════════
//  COST MODEL (a backtest that ignores costs lies too) — all VERIFIED or configurable
// ════════════════════════════════════════════════════════════════════════════
//  * Detection latency: you cannot buy at the creation instant — faster bots
//    already did. Entry is at `create_ms + DETECTION_LATENCY_MS` (default
//    1200).
//  * Bonding-curve slippage: buys/sells move the price along the curve. We
//    use the constant-product reserves math from `../lib/pump.js` (VERIFIED
//    to the lamport against a captured trade) at the curve state as-of the
//    action.
//  * pump.fun trading fee: 125 bps (95 protocol + 30 creator), READ from a
//    real captured trade's `fee_basis_points`+`creator_fee_basis_points`.
//    Charged on top of the SOL that enters the curve on a buy, and off the
//    SOL received on a sell. Configurable via PUMP_FEE_BPS.
//  * Jito tip + network fee: sniping needs a competitive tip. Charged per
//    trade (entry and exit), configurable (ENTRY_TIP_SOL default 0.001,
//    EXIT_TIP_SOL default 0.0005, BASE_FEE_SOL default 0.000005).
//  * Rug / dev-dump: reconstructed from the real buy/sell path; if the price
//    collapses before our exit rule fires, we realize the loss at the real
//    post-dump curve price. On migration the bonding curve is gone, so we
//    exit at the last curve price (we have no PumpSwap price feed — see
//    caveats).
//  * Honeypot (sell-revert): NOT in the data (collector records successes
//    only). We do not pretend it is zero — it is flagged as unmodeled
//    downside.
//
// MODELING ASSUMPTION (stated honestly): our own buy/sell is priced against
// the curve as-of the action (we pay real slippage), but we do NOT perturb
// the recorded path for other traders (a small-trader assumption). For the
// buy sizes swept here this is a mild optimism on the exit side; larger
// sizes would need a full re-simulation of every participant.
//
// Usage: `EVENTS=runs/pump/events.jsonl tsx src/bin/pumpBacktest.ts`

import 'dotenv/config';
import * as fs from 'node:fs';
import * as readline from 'node:readline';
import { curveBuyTokensOut, curveSellSolOut } from '../lib/pump.js';

const LAMPORTS_PER_SOL = 1e9;
/** Canonical pump.fun launch virtual-SOL reserve (= the constant virtual
 * offset, so `real_sol = virtual_sol - INIT_VIRTUAL_SOL`). VERIFIED in
 * pump.ts (ported from pump.rs tests). */
const INIT_VIRTUAL_SOL = 30_000_000_000n;
/** Safety cap so a position never rides forever in the sim. */
const MAX_HOLD_MS = 3_600_000;

// ── env helpers ──────────────────────────────────────────────────────────────

function envF64(k: string, def: number): number {
  const v = process.env[k];
  if (v === undefined) return def;
  const n = Number.parseFloat(v);
  return Number.isFinite(n) ? n : def;
}
function envU128(k: string, def: number): number {
  const v = process.env[k];
  if (v === undefined) return def;
  const n = Number.parseInt(v, 10);
  return Number.isFinite(n) ? n : def;
}

/** Costs shared by every simulated trade. All in SOL except the fee (bps). */
interface Costs {
  pumpFeeBps: number;
  entryTip: number;
  exitTip: number;
  baseFee: number;
}
function costsFromEnv(): Costs {
  return {
    pumpFeeBps: envF64('PUMP_FEE_BPS', 125.0),
    entryTip: envF64('ENTRY_TIP_SOL', 0.001),
    exitTip: envF64('EXIT_TIP_SOL', 0.0005),
    baseFee: envF64('BASE_FEE_SOL', 0.000005),
  };
}

// ── event model ──────────────────────────────────────────────────────────────

type Kind = 'Buy' | 'Sell' | 'Migrate';

/** One decoded JSONL record, reduced to what the sim needs. */
interface Ev {
  ms: number;
  kind: Kind;
  actor: string;
  /** Post-trade virtual reserves (for Create these are the initial reserves). */
  vsol: bigint;
  vtok: bigint;
  tok: bigint;
}

/** Everything known about one launch, built from its events. */
interface Mint {
  createMs: number;
  dev: string;
  initVsol: bigint;
  initVtok: bigint;
  totalSupply: bigint;
  migrated: boolean;
  /** Trades + migrate, strictly sorted by (ms, original order). */
  events: Ev[];
}

function newMint(createMs: number, dev: string): Mint {
  return {
    createMs,
    dev,
    initVsol: 0n,
    initVtok: 0n,
    totalSupply: 0n,
    migrated: false,
    events: [],
  };
}

function asU64(v: any): bigint {
  if (typeof v === 'number' && Number.isFinite(v) && v >= 0) return BigInt(Math.trunc(v));
  return 0n;
}

async function load(path: string): Promise<{ mints: Mint[]; tsMin: number; tsMax: number }> {
  if (!fs.existsSync(path)) {
    console.error(`cannot open ${path}: file not found`);
    process.exit(1);
  }
  const mints = new Map<string, Mint>();
  let tsMin = Number.POSITIVE_INFINITY;
  let tsMax = 0;

  const rl = readline.createInterface({
    input: fs.createReadStream(path, { encoding: 'utf8' }),
    crlfDelay: Infinity,
  });

  for await (const rawLine of rl) {
    const line = rawLine.trim();
    if (line.length === 0) continue;
    let v: any;
    try {
      v = JSON.parse(line);
    } catch {
      continue;
    }
    const ms = typeof v.unix_ms === 'number' ? v.unix_ms : 0;
    if (ms === 0) continue;
    tsMin = Math.min(tsMin, ms);
    tsMax = Math.max(tsMax, ms);
    const mint = typeof v.mint === 'string' ? v.mint : '';
    if (mint === '') continue;
    const et = typeof v.event_type === 'string' ? v.event_type : '';
    const actor = typeof v.actor === 'string' ? v.actor : '';
    switch (et) {
      case 'create': {
        let m = mints.get(mint);
        if (m === undefined) {
          m = newMint(ms, actor);
          mints.set(mint, m);
        }
        m.createMs = ms;
        m.dev = typeof v.dev === 'string' ? v.dev : actor;
        m.initVsol = v.init_virtual_sol_reserves !== undefined ? asU64(v.init_virtual_sol_reserves) : INIT_VIRTUAL_SOL;
        m.initVtok = asU64(v.init_virtual_token_reserves);
        m.totalSupply = asU64(v.token_total_supply);
        break;
      }
      case 'buy':
      case 'sell': {
        let m = mints.get(mint);
        if (m === undefined) {
          m = newMint(0, '');
          mints.set(mint, m);
        }
        m.events.push({
          ms,
          kind: et === 'buy' ? 'Buy' : 'Sell',
          actor,
          vsol: asU64(v.virtual_sol_reserves),
          vtok: asU64(v.virtual_token_reserves),
          tok: asU64(v.token_amount),
        });
        break;
      }
      case 'migrate': {
        let m = mints.get(mint);
        if (m === undefined) {
          m = newMint(0, '');
          mints.set(mint, m);
        }
        m.migrated = true;
        m.events.push({ ms, kind: 'Migrate', actor, vsol: 0n, vtok: 0n, tok: 0n });
        break;
      }
      default:
        break;
    }
  }

  const out = [...mints.values()];
  for (const m of out) {
    m.events.sort((a, b) => a.ms - b.ms);
  }
  if (tsMin === Number.POSITIVE_INFINITY) tsMin = 0;
  return { mints: out, tsMin, tsMax };
}

// ── strategy config ──────────────────────────────────────────────────────────

/** Entry-time-observable filter (strategy 2). `undefined` fields = "don't care". */
interface Filter {
  /** Reject if dev's initial token allocation (fraction of supply) exceeds this. */
  maxDevAlloc?: number;
  /** Reject if the dev pre-bought at/near create. */
  rejectDevPrebuy?: boolean;
  /** Require at least this many DISTINCT buyers before T_entry. */
  minBuyers?: number;
  /** Require at least this much real SOL liquidity (SOL) at T_entry. */
  minLiqSol?: number;
}
const EMPTY_FILTER: Filter = {};

type Exit =
  /** Sell when price >= entry * (1 + pct/100). */
  | { kind: 'TakeProfit'; pct: number }
  /** Sell at entry + secs (at the state as-of that time). */
  | { kind: 'Hold'; secs: number }
  /** Sell when price drops pct% from the running peak. */
  | { kind: 'Trailing'; pct: number }
  /** Sell on the first sell by the dev wallet after entry. */
  | { kind: 'FirstDevSell' };

interface Strat {
  latencyMs: number;
  buySol: number;
  filter: Filter;
  exit: Exit;
}

/** Outcome of a single simulated round-trip. */
interface Trade {
  entryMs: number;
  pnlSol: number;
  pnlPct: number;
}

// ── the per-mint simulation (look-ahead-free by construction) ────────────────

/** Simulate one strategy on one launch. Returns `null` if the launch is not
 * tradeable (no create seen) or the filter rejects it. */
function simulate(m: Mint, s: Strat, c: Costs): Trade | null {
  if (m.createMs === 0 || m.initVtok === 0n) {
    return null; // we only trade launches whose birth (and initial curve) we saw
  }
  const tEntry = m.createMs + s.latencyMs;

  // ── ENTRY PASS: only the prefix with ms <= T_entry is in scope. ──────────
  // Curve state as-of T_entry starts at the create's initial reserves and is
  // advanced by every trade at or before T_entry.
  let vsol = m.initVsol;
  let vtok = m.initVtok;
  const buyers = new Set<string>();
  let devAllocTokens = 0n;
  let devPrebought = false;
  const firstSec = m.createMs + 1000;
  let k = 0;
  for (const e of m.events) {
    if (e.ms > tEntry) break;
    k += 1;
    if (e.kind === 'Buy') {
      if (e.ms <= firstSec) buyers.add(e.actor);
      if (e.actor === m.dev) {
        devPrebought = true;
        devAllocTokens += e.tok;
      }
      vsol = e.vsol;
      vtok = e.vtok;
    } else if (e.kind === 'Sell') {
      vsol = e.vsol;
      vtok = e.vtok;
    } else {
      return null; // already graduated before we could enter
    }
  }

  // ── FILTER (uses only the prefix-derived features above) ─────────────────
  const f = s.filter;
  if (f.maxDevAlloc !== undefined) {
    const alloc = m.totalSupply > 0n ? Number(devAllocTokens) / Number(m.totalSupply) : 0.0;
    if (alloc > f.maxDevAlloc) return null;
  }
  if (f.rejectDevPrebuy === true && devPrebought) return null;
  if (f.minBuyers !== undefined && buyers.size < f.minBuyers) return null;
  if (f.minLiqSol !== undefined) {
    const realSol = Number(vsol > INIT_VIRTUAL_SOL ? vsol - INIT_VIRTUAL_SOL : 0n) / LAMPORTS_PER_SOL;
    if (realSol < f.minLiqSol) return null;
  }
  if (vtok === 0n) return null;

  // ── OPEN POSITION at the as-of-T_entry curve state. ──────────────────────
  // Buy budget `buySol` is total SOL out of pocket for the buy incl. pump
  // fee; the SOL that actually enters the curve is the budget net of the fee.
  const feeMul = 1.0 + c.pumpFeeBps / 1e4;
  const solIntoCurve = BigInt(Math.trunc((s.buySol / feeMul) * LAMPORTS_PER_SOL));
  const positionTokens = curveBuyTokensOut(vsol, vtok, solIntoCurve);
  if (positionTokens === 0n) return null;
  const entryCost = s.buySol + c.entryTip + c.baseFee;
  const entryPrice = Number(vsol) / Number(vtok); // raw SOL-lamports per raw token

  // ── EXIT PASS: forward-only walk of the suffix events[k..]. ──────────────
  let peak = entryPrice;
  const tHardExit = tEntry + MAX_HOLD_MS;
  const tTimedExit = s.exit.kind === 'Hold' ? Math.min(tEntry + s.exit.secs * 1000, tHardExit) : tHardExit;

  // exit_state is the curve reserves at which we finally sell.
  let exitVsol = vsol;
  let exitVtok = vtok;

  for (let i = k; i < m.events.length; i++) {
    const e = m.events[i];
    // (a) Time-based exit fires BEFORE consuming this event, using the state
    //     as-of the exit instant (i.e. the state BEFORE this later event) —
    //     never this event's own (possibly crashing) price. Look-ahead-free.
    if (e.ms >= tTimedExit) {
      break;
    }
    if (e.kind === 'Migrate') {
      // Curve gone; realize at the last curve state (no PumpSwap feed).
      break;
    }
    // (b) React to the event: advance curve to its POST-trade state.
    exitVsol = e.vsol;
    exitVtok = e.vtok;
    const price = Number(exitVsol) / Number(exitVtok);
    if (price > peak) peak = price;
    let hit: boolean;
    switch (s.exit.kind) {
      case 'TakeProfit':
        hit = price >= entryPrice * (1.0 + s.exit.pct / 100.0);
        break;
      case 'Trailing':
        hit = price <= peak * (1.0 - s.exit.pct / 100.0);
        break;
      case 'FirstDevSell':
        hit = e.kind === 'Sell' && e.actor === m.dev;
        break;
      case 'Hold':
        hit = false; // handled by the timed branch above
        break;
    }
    if (hit) break;
  }
  // If we ran out of events still open, exit at the last known curve state
  // (equal to the entry state if nothing traded) — a real, forced close.

  // ── REALIZE the sell into the exit-state curve. ──────────────────────────
  // We price OTHERS' trades against the recorded (unperturbed) path, but we
  // DO carry our own buy's footprint: our SOL is still sitting in the pool
  // and our tokens are still out of it. So we sell back into the recorded
  // reserves with our own delta added (+solIntoCurve to vsol, -positionTokens
  // from vtok). Without this we would double-charge curve convexity — pay
  // entry slippage AND exit slippage on a static curve — inventing a loss
  // that isn't there. With it, a flat market round-trips to ~0 (minus
  // fees/tips) and a real loss comes only from OTHERS moving the price (dev
  // dumps etc.), which is the honest cost.
  const adjVsol = exitVsol + solIntoCurve;
  const adjVtokRaw = exitVtok - positionTokens;
  const adjVtok = adjVtokRaw > 1n ? adjVtokRaw : 1n;
  const solOutCurve = curveSellSolOut(adjVsol, adjVtok, positionTokens);
  const netSol = (Number(solOutCurve) / LAMPORTS_PER_SOL) * (1.0 - c.pumpFeeBps / 1e4) - c.exitTip - c.baseFee;
  const pnlSol = netSol - entryCost;
  return {
    entryMs: tEntry,
    pnlSol,
    pnlPct: (100.0 * pnlSol) / entryCost,
  };
}

// ── aggregate stats ──────────────────────────────────────────────────────────

function pct(sorted: number[], p: number): number {
  if (sorted.length === 0) return NaN;
  const idx = Math.round((sorted.length - 1) * p);
  return sorted[Math.min(idx, sorted.length - 1)];
}

interface Stats {
  n: number;
  winRate: number;
  meanSol: number;
  medianSol: number;
  meanPct: number;
  totalSol: number;
  maxDd: number;
  p10: number;
  p90: number;
}

function summarize(trades: Trade[]): Stats {
  const n = trades.length;
  if (n === 0) {
    return { n: 0, winRate: 0, meanSol: 0, medianSol: 0, meanPct: 0, totalSol: 0, maxDd: 0, p10: 0, p90: 0 };
  }
  const wins = trades.filter((t) => t.pnlSol > 0).length;
  const totalSol = trades.reduce((a, t) => a + t.pnlSol, 0);
  const meanSol = totalSol / n;
  const meanPct = trades.reduce((a, t) => a + t.pnlPct, 0) / n;

  const solSorted = trades.map((t) => t.pnlSol).sort((a, b) => a - b);
  const medianSol = pct(solSorted, 0.5);
  const pctSorted = trades.map((t) => t.pnlPct).sort((a, b) => a - b);

  // Max drawdown of the equity curve, trades ordered by entry time.
  const byEntry = [...trades].sort((a, b) => a.entryMs - b.entryMs);
  let equity = 0;
  let peak = 0;
  let maxDd = 0;
  for (const t of byEntry) {
    equity += t.pnlSol;
    peak = Math.max(peak, equity);
    maxDd = Math.max(maxDd, peak - equity);
  }

  return {
    n,
    winRate: (100.0 * wins) / n,
    meanSol,
    medianSol,
    meanPct,
    totalSol,
    maxDd,
    p10: pct(pctSorted, 0.1),
    p90: pct(pctSorted, 0.9),
  };
}

function exitLabel(e: Exit): string {
  switch (e.kind) {
    case 'TakeProfit':
      return `TP+${e.pct.toFixed(0)}%`;
    case 'Hold':
      return `hold${e.secs}s`;
    case 'Trailing':
      return `trail${e.pct.toFixed(0)}%`;
    case 'FirstDevSell':
      return 'devSell';
  }
}

function filterLabel(f: Filter): string {
  const parts: string[] = [];
  if (f.maxDevAlloc !== undefined) parts.push(`devAlloc<${(f.maxDevAlloc * 100).toFixed(0)}%`);
  if (f.rejectDevPrebuy === true) parts.push('noDevPrebuy');
  if (f.minBuyers !== undefined) parts.push(`>=${f.minBuyers}buyers`);
  if (f.minLiqSol !== undefined) parts.push(`>=${f.minLiqSol}SOL`);
  return parts.length === 0 ? 'none' : parts.join('+');
}

/** One row of the results table. */
interface Row {
  label: string;
  st: Stats;
}

function runSweep(mints: Mint[], strats: Strat[], c: Costs, labelOf: (s: Strat) => string): Row[] {
  const rows: Row[] = [];
  for (const s of strats) {
    const trades: Trade[] = [];
    for (const m of mints) {
      const t = simulate(m, s, c);
      if (t !== null) trades.push(t);
    }
    rows.push({ label: labelOf(s), st: summarize(trades) });
  }
  rows.sort((a, b) => b.st.meanSol - a.st.meanSol);
  return rows;
}

function printTable(title: string, rows: Row[]): void {
  console.log(`\n══ ${title} ══`);
  console.log(
    `${'params'.padEnd(34)} ${'trades'.padStart(6)} ${'win%'.padStart(7)} ${'mean SOL'.padStart(11)} ${'med SOL'.padStart(10)} ${'mean%'.padStart(9)} ${'total SOL'.padStart(10)} ${'p10%'.padStart(8)} ${'p90%'.padStart(8)} ${'maxDD SOL'.padStart(9)}`,
  );
  console.log('-'.repeat(120));
  for (const r of rows) {
    const s = r.st;
    if (s.n === 0) {
      console.log(`${r.label.padEnd(34)} ${String(0).padStart(6)} ${'(no trades)'.padStart(7)}`);
      continue;
    }
    console.log(
      `${r.label.padEnd(34)} ${String(s.n).padStart(6)} ${`${s.winRate.toFixed(1)}%`.padStart(7)} ${s.meanSol.toFixed(5).padStart(11)} ${s.medianSol.toFixed(5).padStart(10)} ${`${s.meanPct.toFixed(1)}%`.padStart(8)} ${s.totalSol.toFixed(4).padStart(10)} ${s.p10.toFixed(1).padStart(8)} ${s.p90.toFixed(1).padStart(8)} ${s.maxDd.toFixed(4).padStart(9)}`,
    );
  }
}

function verdict(strategy: string, rows: Row[], minTrades: number): void {
  // Best row (rows are pre-sorted by mean_sol desc) that clears a trade-count bar.
  const best = rows.find((r) => r.st.n >= minTrades);
  if (best === undefined) {
    console.log(`  VERDICT [${strategy}]: INCONCLUSIVE — no parameter set reached ${minTrades} trades in this sample.`);
  } else if (best.st.meanSol > 0) {
    console.log(
      `  VERDICT [${strategy}]: best positive-EV set = \`${best.label}\` (mean ${best.st.meanSol >= 0 ? '+' : ''}${best.st.meanSol.toFixed(5)} SOL/trade over ${best.st.n} trades, win ${best.st.winRate.toFixed(1)}%). Treat as HYPOTHESIS until confirmed on the multi-hour set.`,
    );
  } else {
    console.log(
      `  VERDICT [${strategy}]: LOSING after costs. Best set \`${best.label}\` still ${best.st.meanSol >= 0 ? '+' : ''}${best.st.meanSol.toFixed(5)} SOL/trade over ${best.st.n} trades.`,
    );
  }
}

// ── strategy 3: smart-money follow (strict train/test split) ─────────────────

function smartMoney(mints: Mint[], splitMs: number, c: Costs): void {
  console.log('\n\n╔══ STRATEGY 3: SMART-MONEY FOLLOW (train first-half / test second-half) ══╗');
  // TRAIN: realized net SOL cash-flow per wallet, first half ONLY.
  const spent = new Map<string, number>();
  const recv = new Map<string, number>();
  const tradesCt = new Map<string, number>();
  for (const m of mints) {
    for (const e of m.events) {
      if (e.ms >= splitMs) continue;
      if (e.kind === 'Buy') {
        // SOL that left the buyer ~ curve delta they caused; use tok*price
        // proxy is fragile, so approximate with the reserve-implied sol: we
        // don't have per-event sol for reserves here, so use price*tok.
        const price = Number(e.vsol) / Number(e.vtok > 0n ? e.vtok : 1n);
        spent.set(e.actor, (spent.get(e.actor) ?? 0) + (price * Number(e.tok)) / LAMPORTS_PER_SOL);
        tradesCt.set(e.actor, (tradesCt.get(e.actor) ?? 0) + 1);
      } else if (e.kind === 'Sell') {
        const price = Number(e.vsol) / Number(e.vtok > 0n ? e.vtok : 1n);
        recv.set(e.actor, (recv.get(e.actor) ?? 0) + (price * Number(e.tok)) / LAMPORTS_PER_SOL);
        tradesCt.set(e.actor, (tradesCt.get(e.actor) ?? 0) + 1);
      }
    }
  }
  const smart = new Set<string>();
  for (const [w, n] of tradesCt) {
    if (n >= 4) {
      const net = (recv.get(w) ?? 0) - (spent.get(w) ?? 0);
      if (net > 0) smart.add(w);
    }
  }
  console.log(
    `  trained on ${tradesCt.size} wallets active in first half; ${smart.size} tagged 'smart' (>=4 trades, positive net cash-flow).`,
  );
  if (smart.size === 0) {
    console.log("  VERDICT [smart-money]: INCONCLUSIVE — no wallet met the smart bar in this sample.");
    return;
  }

  // TEST: in the second half, mirror a smart wallet's FIRST buy of a mint.
  // Enter at buy_ms + latency (our latency), exit by a fixed rule (hold 15s).
  // Only mints whose create we saw (so entry curve state is honest).
  const latency = envU128('DETECTION_LATENCY_MS', 1200);
  const trades: Trade[] = [];
  for (const m of mints) {
    if (m.createMs === 0 || m.initVtok === 0n) continue;
    // find first smart buy in the second half for this mint
    const sig = m.events.find((e) => e.ms >= splitMs && e.kind === 'Buy' && smart.has(e.actor));
    if (sig === undefined) continue;
    const s: Strat = {
      latencyMs: Math.max(sig.ms + latency - m.createMs, 0),
      buySol: envF64('BUY_SOL', 0.5),
      filter: EMPTY_FILTER,
      exit: { kind: 'Hold', secs: 15 },
    };
    const t = simulate(m, s, c);
    if (t !== null) trades.push(t);
  }
  const st = summarize(trades);
  const rows: Row[] = [{ label: `follow-smart(hold15s, ${smart.size} wallets)`, st }];
  printTable('smart-money follow — second-half test', rows);
  verdict('smart-money', rows, 10);
  console.log(
    "  NOTE: wallet 'profit' here is a first-half realized-cash-flow proxy (buys vs sells\n  reconstructed from reserve-implied prices); it is a train signal, not ground-truth PnL.",
  );
}

// ── strategy 4: migration play ───────────────────────────────────────────────

function migrationPlay(mints: Mint[], c: Costs): void {
  console.log('\n\n╔══ STRATEGY 4: MIGRATION PLAY (near-graduation ride) ══╗');
  const migrated = mints.filter((m) => m.migrated).length;
  const migratedSeenBirth = mints.filter((m) => m.migrated && m.createMs !== 0).length;
  console.log(`  migrations in dataset: ${migrated} (of which ${migratedSeenBirth} we also saw born).`);

  // Near-graduation ride: enter the first time real_sol >= 75 SOL (observable),
  // exit at migration (or last curve state). This is the only migration edge
  // we can price WITHOUT a PumpSwap feed — the post-graduation pop is NOT
  // modelled.
  const latency = envU128('DETECTION_LATENCY_MS', 1200);
  const trades: Trade[] = [];
  for (const m of mints) {
    if (m.createMs === 0 || m.initVtok === 0n) continue;
    const thresh = INIT_VIRTUAL_SOL + BigInt(75) * BigInt(LAMPORTS_PER_SOL);
    const cross = m.events.find((e) => e.vsol >= thresh && e.kind === 'Buy');
    if (cross === undefined) continue;
    const s: Strat = {
      latencyMs: Math.max(cross.ms + latency - m.createMs, 0),
      buySol: envF64('BUY_SOL', 0.5),
      filter: EMPTY_FILTER,
      exit: { kind: 'Hold', secs: 60 },
    };
    const t = simulate(m, s, c);
    if (t !== null) trades.push(t);
  }
  const st = summarize(trades);
  if (st.n === 0) {
    console.log(
      '  Too few launches reached the near-graduation band in this sample to backtest.\n  VERDICT [migration]: NOT MEANINGFULLY TESTABLE with this dataset (and we have no\n  PumpSwap price feed for the post-graduation pop — the real migration edge).',
    );
    return;
  }
  const rows: Row[] = [{ label: 'near-grad ride (>=75 SOL, hold60s)', st }];
  printTable('migration — pre-graduation ride only', rows);
  verdict('migration', rows, 10);
  console.log(
    "  CAVEAT: this prices ONLY the bonding-curve ride up to migration. The post-migration\n  PumpSwap pop — the part most 'migration plays' target — needs a PumpSwap price feed the\n  collector does not yet capture.",
  );
}

// ── main ─────────────────────────────────────────────────────────────────────

function buildSnipeSweep(latency: number, sizes: number[]): Strat[] {
  const exits: Exit[] = [
    { kind: 'TakeProfit', pct: 50.0 },
    { kind: 'TakeProfit', pct: 100.0 },
    { kind: 'TakeProfit', pct: 300.0 },
    { kind: 'Hold', secs: 5 },
    { kind: 'Hold', secs: 15 },
    { kind: 'Hold', secs: 30 },
    { kind: 'Trailing', pct: 30.0 },
    { kind: 'Trailing', pct: 50.0 },
    { kind: 'FirstDevSell' },
  ];
  const v: Strat[] = [];
  for (const sz of sizes) {
    for (const e of exits) {
      v.push({ latencyMs: latency, buySol: sz, filter: EMPTY_FILTER, exit: e });
    }
  }
  return v;
}

function buildFilteredSweep(latency: number, size: number): Strat[] {
  const filters: Filter[] = [
    { minBuyers: 3 },
    { minBuyers: 5 },
    { minBuyers: 10 },
    { rejectDevPrebuy: true },
    { maxDevAlloc: 0.05 },
    { minLiqSol: 2.0 },
    { minBuyers: 5, rejectDevPrebuy: true },
    { minBuyers: 10, minLiqSol: 3.0 },
  ];
  const exits: Exit[] = [
    { kind: 'Hold', secs: 15 },
    { kind: 'TakeProfit', pct: 100.0 },
    { kind: 'FirstDevSell' },
    { kind: 'Trailing', pct: 30.0 },
  ];
  const v: Strat[] = [];
  for (const f of filters) {
    for (const e of exits) {
      v.push({ latencyMs: latency, buySol: size, filter: f, exit: e });
    }
  }
  return v;
}

async function main(): Promise<void> {
  const path = process.env.EVENTS ?? process.env.PUMP_OUT ?? 'runs/pump/events.jsonl';
  const latency = envU128('DETECTION_LATENCY_MS', 1200);
  const sizes = [0.2, 0.5, 1.0];
  const costs = costsFromEnv();

  const { mints, tsMin, tsMax } = await load(path);
  const spanH = Math.max(0, tsMax - tsMin) / 3_600_000.0;
  const launches = mints.filter((m) => m.createMs !== 0).length;

  console.log('═══════════════════════════════════════════════════════════════════════');
  console.log(` pump.fun BACKTEST — ${path}`);
  console.log('═══════════════════════════════════════════════════════════════════════');
  console.log(
    `window ${(spanH * 60).toFixed(1)} min (${spanH.toFixed(3)} h) | mints ${mints.length} | launches (birth seen) ${launches} | migrations ${mints.filter((m) => m.migrated).length}`,
  );
  console.log(
    `costs: latency ${latency}ms | pump fee ${costs.pumpFeeBps.toFixed(0)}bps | entry tip ${costs.entryTip} SOL | exit tip ${costs.exitTip} SOL | base fee ${costs.baseFee} SOL`,
  );
  console.log(`buy sizes swept: [${sizes.join(', ')}] SOL`);
  if (spanH < 0.5) {
    console.log(
      `\n⚠️  SAMPLE IS TINY (${(spanH * 60).toFixed(1)} min). This run only proves the ENGINE works end-to-end.\n⚠️  It CANNOT support any strategy conclusion — most launches' fates fall outside the\n⚠️  window (survivorship), and a handful of trades is noise. The real verdict needs\n⚠️  the multi-hour dataset.`,
    );
  }

  // ── STRATEGY 1: snipe (no filter) ────────────────────────────────────────
  console.log('\n\n╔══ STRATEGY 1: SNIPE (buy every launch at entry, exit by rule) ══╗');
  const s1 = buildSnipeSweep(latency, sizes);
  const rows1 = runSweep(mints, s1, costs, (s) => `${s.buySol.toFixed(1)}SOL ${exitLabel(s.exit)}`);
  printTable('snipe sweep (sorted by mean SOL/trade)', rows1);
  verdict('snipe', rows1, 20);

  // ── STRATEGY 2: filtered snipe (0.5 SOL) ─────────────────────────────────
  console.log('\n\n╔══ STRATEGY 2: FILTERED SNIPE (enter only launches passing an entry-time filter) ══╗');
  const s2 = buildFilteredSweep(latency, 0.5);
  const rows2 = runSweep(mints, s2, costs, (s) => `[${filterLabel(s.filter)}] ${exitLabel(s.exit)}`);
  printTable('filtered-snipe sweep (0.5 SOL, sorted by mean SOL/trade)', rows2);
  verdict('filtered-snipe', rows2, 15);

  // ── STRATEGY 3 & 4 ───────────────────────────────────────────────────────
  const split = tsMin + Math.floor(Math.max(0, tsMax - tsMin) / 2);
  smartMoney(mints, split, costs);
  migrationPlay(mints, costs);

  // ── honest caveats ───────────────────────────────────────────────────────
  console.log('\n\n═══ CAVEATS (read before trusting any number above) ═══');
  console.log(' 1. HONEYPOTS UNMODELLED: the collector records only SUCCESSFUL txs, so sell-revert');
  console.log('    honeypots are invisible here. Real snipe PnL is WORSE than shown by that unknown.');
  console.log(' 2. SURVIVORSHIP / WINDOW: launches whose rug or peak falls outside the capture window');
  console.log("    are scored on a truncated life. A short window flatters 'graduated' and undercounts rugs.");
  console.log(' 3. NO PATH PERTURBATION: our own buy/sell is priced with real slippage but does not');
  console.log('    move the recorded path for others (small-trader assumption; optimistic on exit fills).');
  console.log(' 4. NO PUMPSWAP FEED: post-graduation prices are not captured, so the migration pop is unpriced.');
  console.log(' 5. PAST ≠ FUTURE: a filter fit to one window can fail the next; treat any positive set as a');
  console.log('    hypothesis to re-test on fresh data, not a green light to deploy capital.');
  console.log(` 6. DATASET SIZE: with ${launches} launches over ${spanH.toFixed(2)}h, only high-frequency strategies have enough`);
  console.log('    trades to escape noise. Sub-20-trade rows are anecdotes.');
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});

// (tests omitted — no test harness set up yet; the Rust `#[cfg(test)]` module
// pinned the slippage math AND the no-look-ahead replay ordering via
// assert-backed synthetic-mint scenarios: entry_cannot_see_future_price_spike,
// entry_after_spike_time_does_see_it, take_profit_uses_real_crossing_not_peak,
// dev_dump_before_exit_is_a_loss, filter_rejects_when_too_few_early_buyers.
// The invariants they proved are preserved by the simulate() logic above:
// the entry pass only scans events[..k] with ms <= T_entry, and the exit pass
// walks events[k..] strictly forward, testing the timed-exit boundary before
// consuming each event and updating price state only from POST-trade
// reserves in event order.)
