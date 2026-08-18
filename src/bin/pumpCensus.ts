// Port of src/bin/pump_census.rs
//
// pump_census — read `runs/pump/events.jsonl` (produced by `pumpCollect`) and
// report the ground-truth opportunity size for pump.fun launches:
//   * launches per hour (and the observation window)
//   * % of launches that graduate vs die (within the window)
//   * median / distribution of time-to-graduation
//   * distribution of peak-price-multiple after launch
//   * a dev-dump rug proxy (launches where the dev wallet sells, how fast)
//
// This answers "is there even money here, and where" BEFORE any capital risk.
// It is a pure read of the collected file — no RPC, no chain writes.
//
// Usage: `tsx src/bin/pumpCensus.ts [-- path/to/events.jsonl]`
//        env: PUMP_OUT (default runs/pump/events.jsonl)

import * as fs from 'node:fs';
import * as readline from 'node:readline';

/** Per-mint rollup built from the event stream. */
interface Token {
  createMs: number | null;
  dev: string | null;
  /** price_in_sol implied by the create's initial reserves. */
  initPrice: number;
  trades: number;
  buys: number;
  sells: number;
  peakPrice: number;
  migrateMs: number | null;
  /** First time the dev wallet was seen selling (ms). */
  devFirstSellMs: number | null;
  devSold: boolean;
}

function newToken(): Token {
  return {
    createMs: null,
    dev: null,
    initPrice: 0,
    trades: 0,
    buys: 0,
    sells: 0,
    peakPrice: 0,
    migrateMs: null,
    devFirstSellMs: null,
    devSold: false,
  };
}

function pct(sorted: number[], p: number): number {
  if (sorted.length === 0) return NaN;
  const idx = Math.round((sorted.length - 1) * p);
  return sorted[Math.min(idx, sorted.length - 1)];
}

/** Reject records whose shape or internal consistency betrays a torn write. */
function recordIsSane(v: any): boolean {
  const et = typeof v?.event_type === 'string' ? v.event_type : undefined;
  if (et === undefined) return false;
  if (et !== 'create' && et !== 'buy' && et !== 'sell' && et !== 'migrate') return false;
  // base58 pubkey = 32-44 chars, signature = 86-88 chars
  const mint = typeof v.mint === 'string' ? v.mint : undefined;
  const mintOk = mint !== undefined && mint.length >= 32 && mint.length <= 44;
  const sig = typeof v.signature === 'string' ? v.signature : undefined;
  const sigOk = sig !== undefined && sig.length >= 80 && sig.length <= 90;
  const unixMs = typeof v.unix_ms === 'number' ? v.unix_ms : 0;
  if (!mintOk || !sigOk || unixMs === 0) return false;
  if (et === 'buy' || et === 'sell') {
    // Zero values are legitimate (e.g. dust sells into an emptied curve,
    // vsr = 0 -> price 0); torn lines betray themselves by MISSING fields or
    // by a price that disagrees with the reserves it was derived from.
    const vs = typeof v.virtual_sol_reserves === 'number' ? v.virtual_sol_reserves : undefined;
    const vt = typeof v.virtual_token_reserves === 'number' ? v.virtual_token_reserves : undefined;
    const p = typeof v.price_in_sol === 'number' ? v.price_in_sol : undefined;
    if (vs === undefined || vt === undefined || p === undefined) return false;
    if (!Number.isFinite(p) || p < 0 || vs < 0 || vt < 0) return false;
    if (vt > 0) {
      const recomputed = vs / 1e9 / (vt / 1e6);
      if (Math.abs(p - recomputed) > recomputed * 0.01) return false;
    }
  }
  return true;
}

async function main(): Promise<void> {
  const path = process.argv[2] ?? process.env.PUMP_OUT ?? 'runs/pump/events.jsonl';

  if (!fs.existsSync(path)) {
    console.error(`cannot open ${path}: file not found`);
    process.exit(1);
  }

  const tokens = new Map<string, Token>();
  let nEvents = 0;
  let nCreate = 0;
  let nBuy = 0;
  let nSell = 0;
  let nMigrate = 0;
  let nSkipped = 0;
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
      nSkipped += 1;
      continue;
    }
    // Torn-line guard: interleaved writers have produced lines that still
    // parse as JSON but carry woven-together garbage fields. Require a sane
    // base shape, and on trades a price consistent with the raw reserves.
    if (!recordIsSane(v)) {
      nSkipped += 1;
      continue;
    }
    nEvents += 1;
    const ts = typeof v.unix_ms === 'number' ? v.unix_ms : 0;
    if (ts > 0) {
      tsMin = Math.min(tsMin, ts);
      tsMax = Math.max(tsMax, ts);
    }
    const mint = typeof v.mint === 'string' ? v.mint : '';
    if (mint === '') continue;
    const et = typeof v.event_type === 'string' ? v.event_type : '';
    let t = tokens.get(mint);
    if (t === undefined) {
      t = newToken();
      tokens.set(mint, t);
    }
    switch (et) {
      case 'create': {
        nCreate += 1;
        t.createMs = ts;
        t.dev = typeof v.dev === 'string' ? v.dev : null;
        const vs = typeof v.init_virtual_sol_reserves === 'number' ? v.init_virtual_sol_reserves : 0;
        const vt = typeof v.init_virtual_token_reserves === 'number' ? v.init_virtual_token_reserves : 0;
        if (vt > 0) {
          // price_in_sol with 6 decimals: (vs/1e9)/(vt/1e6)
          t.initPrice = vs / 1e9 / (vt / 1e6);
        }
        break;
      }
      case 'buy':
      case 'sell': {
        if (et === 'buy') {
          nBuy += 1;
          t.buys += 1;
        } else {
          nSell += 1;
          t.sells += 1;
        }
        t.trades += 1;
        const p = typeof v.price_in_sol === 'number' ? v.price_in_sol : 0;
        if (p > t.peakPrice) t.peakPrice = p;
        // dev-dump proxy: this sell's actor is the create's dev wallet.
        if (et === 'sell') {
          const actor = typeof v.actor === 'string' ? v.actor : undefined;
          if (t.dev !== null && actor !== undefined && t.dev === actor) {
            t.devSold = true;
            if (t.devFirstSellMs === null) t.devFirstSellMs = ts;
          }
        }
        break;
      }
      case 'migrate': {
        nMigrate += 1;
        if (t.migrateMs === null) t.migrateMs = ts;
        break;
      }
      default:
        break;
    }
  }

  if (nEvents === 0) {
    console.error(`no events in ${path}`);
    process.exit(1);
  }

  if (tsMin === Number.POSITIVE_INFINITY) tsMin = 0;
  const spanMs = Math.max(0, tsMax - tsMin);
  const spanHours = spanMs / 3_600_000;

  console.log(`═══ pump.fun census — ${path} ═══`);
  console.log(
    `events ${nEvents}  (create ${nCreate}, buy ${nBuy}, sell ${nSell}, migrate ${nMigrate})  [skipped ${nSkipped} torn/malformed lines]`,
  );
  console.log(
    `observation window: ${(spanMs / 60_000).toFixed(2)} min (${spanHours.toFixed(3)} h)  |  distinct mints seen: ${tokens.size}`,
  );
  if (spanHours > 0) {
    console.log(`launch rate: ${(nCreate / spanHours).toFixed(1)} launches/hour  (${nCreate} creates in window)`);
    console.log(
      `migration rate: ${(nMigrate / spanHours).toFixed(2)} migrations/hour  (${nMigrate} migrates in window)`,
    );
  }

  // ── Launch-cohort analysis: only mints whose CREATE we captured in-window ──
  const cohort: Token[] = [...tokens.values()].filter((t) => t.createMs !== null);
  console.log(`\n── launch cohort (create seen in-window): ${cohort.length} ──`);
  if (cohort.length === 0) {
    console.log('(no creates captured in this window — run the collector longer)');
    return;
  }

  const graduated = cohort.filter((t) => t.migrateMs !== null).length;
  const noTrades = cohort.filter((t) => t.trades === 0).length;
  const devDumped = cohort.filter((t) => t.devSold).length;

  console.log(
    `graduated (migrated) in-window: ${graduated}/${cohort.length} = ${((100 * graduated) / cohort.length).toFixed(2)}%`,
  );
  console.log(
    `died-so-far proxy (0 trades after launch): ${noTrades}/${cohort.length} = ${((100 * noTrades) / cohort.length).toFixed(2)}%`,
  );
  console.log("NOTE: window is short, so 'graduated %' is a floor and 'died %' is a\n");
  console.log('      ceiling — most launches\' fate falls outside the capture window.');

  // time-to-graduation for the ones that did migrate in-window
  const ttg: number[] = cohort
    .filter((t) => t.createMs !== null && t.migrateMs !== null && (t.migrateMs as number) >= (t.createMs as number))
    .map((t) => ((t.migrateMs as number) - (t.createMs as number)) / 1000);
  ttg.sort((a, b) => a - b);
  if (ttg.length > 0) {
    console.log(
      `\ntime-to-graduation (s): p50 ${pct(ttg, 0.5).toFixed(0)}  p25 ${pct(ttg, 0.25).toFixed(0)}  p75 ${pct(ttg, 0.75).toFixed(0)}  min ${ttg[0].toFixed(0)}  max ${ttg[ttg.length - 1].toFixed(0)}  (n=${ttg.length})`,
    );
  } else {
    console.log('\ntime-to-graduation: no create→migrate pair fully inside the window');
  }

  // peak price multiple vs the launch price, for launches that traded
  const mult: number[] = cohort
    .filter((t) => t.trades > 0 && t.initPrice > 0 && t.peakPrice > 0)
    .map((t) => t.peakPrice / t.initPrice);
  mult.sort((a, b) => a - b);
  if (mult.length > 0) {
    console.log(`\npeak price multiple (peak / launch price), launches that traded (n=${mult.length}):`);
    console.log(
      `  p10 ${pct(mult, 0.1).toFixed(2)}x  p50 ${pct(mult, 0.5).toFixed(2)}x  p75 ${pct(mult, 0.75).toFixed(2)}x  p90 ${pct(mult, 0.9).toFixed(2)}x  p99 ${pct(mult, 0.99).toFixed(2)}x  max ${mult[mult.length - 1].toFixed(2)}x`,
    );
    for (const thr of [2.0, 5.0, 10.0, 50.0]) {
      const c = mult.filter((m) => m >= thr).length;
      console.log(`  ≥${thr.toFixed(0).padStart(4)}x : ${String(c).padStart(5)} / ${mult.length} = ${((100 * c) / mult.length).toFixed(1)}%`);
    }
  }

  // dev-dump rug proxy
  console.log(
    `\ndev-dump proxy: ${devDumped}/${cohort.length} = ${((100 * devDumped) / cohort.length).toFixed(2)}% of launches had the dev wallet SELL in-window`,
  );
  const devDt: number[] = cohort
    .filter(
      (t) => t.createMs !== null && t.devFirstSellMs !== null && (t.devFirstSellMs as number) >= (t.createMs as number),
    )
    .map((t) => ((t.devFirstSellMs as number) - (t.createMs as number)) / 1000);
  devDt.sort((a, b) => a - b);
  if (devDt.length > 0) {
    console.log(
      `  dev time-to-first-sell (s): p50 ${pct(devDt, 0.5).toFixed(1)}  p25 ${pct(devDt, 0.25).toFixed(1)}  min ${devDt[0].toFixed(1)}  (n=${devDt.length})`,
    );
  }
  console.log(
    '  (sell-revert honeypots are NOT visible here: the collector records only\n   successful txs. That rug flavour needs a failed-tx scan — future work.)',
  );
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
