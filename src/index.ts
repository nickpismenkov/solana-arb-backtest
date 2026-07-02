import 'dotenv/config';
import { BASE, QUOTE, VENUES, PARAMS, yesterdayUtc } from './config.js';
import { gql } from './bitquery.js';
import { ohlcQuery, type OhlcResponse, type OhlcRow } from './query.js';
import { detect, type PricePoint } from './detect.js';

const fmtUsd = (n: number) =>
  n.toLocaleString('en-US', { style: 'currency', currency: 'USD', maximumFractionDigits: 0 });

// Split [after, before) into chunkHours-hour slices (ISO strings).
function chunks(after: string, before: string, chunkHours: number): [string, string][] {
  const out: [string, string][] = [];
  const step = chunkHours * 3600 * 1000;
  let t = new Date(after).getTime();
  const end = new Date(before).getTime();
  while (t < end) {
    const next = Math.min(t + step, end);
    out.push([new Date(t).toISOString(), new Date(next).toISOString()]);
    t = next;
  }
  return out;
}

async function fetchDay(after: string, before: string): Promise<OhlcRow[]> {
  const rows: OhlcRow[] = [];
  for (const [a, b] of chunks(after, before, PARAMS.chunkHours)) {
    const data = await gql<OhlcResponse>(
      ohlcQuery({
        venues: VENUES,
        after: a,
        before: b,
        intervalMinutes: PARAMS.intervalMinutes,
        minUsd: PARAMS.minUsd,
      }),
    );
    rows.push(...(data.Solana?.DEXTradeByTokens ?? []));
  }
  return rows;
}

async function run() {
  const day = yesterdayUtc();
  console.log(
    `\nBacktesting ${BASE.symbol}/${QUOTE.symbol} for ${day.label} (UTC)\n` +
      `Venues: ${VENUES.join(', ')}  ·  compare: ${PARAMS.venuePair.join(' vs ')}\n` +
      `Assumed size/opportunity: ${fmtUsd(PARAMS.sizeUsd)}  ·  bucket: ${PARAMS.intervalMinutes}m\n`,
  );

  const rows = await fetchDay(day.after, day.before);
  if (rows.length === 0) {
    console.log('No rows returned. Check VENUES / token mints in config.ts.');
    return;
  }

  // Per-venue close series (for persistent-gap detection) + per-minute OHLC map
  // keyed by "minute|venue" (for the intra-minute upper bound).
  const closePoints: PricePoint[] = [];
  const ohlc = new Map<string, { high: number; low: number }>();
  const minutes = new Set<string>();
  const perVenue = new Map<string, number>();
  const prices: number[] = [];

  for (const r of rows) {
    const venue = r.Trade.Dex.ProtocolFamily;
    const ts = Math.floor(new Date(r.Block.Time).getTime() / 1000);
    const close = Number(r.Trade.close);
    if (!Number.isFinite(close) || close <= 0) continue;
    closePoints.push({ ts, venue, price: close });
    ohlc.set(`${r.Block.Time}|${venue}`, { high: Number(r.Trade.high), low: Number(r.Trade.low) });
    minutes.add(r.Block.Time);
    perVenue.set(venue, (perVenue.get(venue) ?? 0) + 1);
    prices.push(close, Number(r.Trade.high), Number(r.Trade.low));
  }

  console.log(
    `Buckets: ${rows.length}  (${[...perVenue].map(([v, n]) => `${v}=${n}`).join(', ')})`,
  );
  console.log(`Price range: $${Math.min(...prices).toFixed(2)}–$${Math.max(...prices).toFixed(2)}\n`);

  const [A, B] = PARAMS.venuePair;

  // (1) Persistent gaps: forward-filled close of A vs close of B, gap must exceed
  // the threshold. Conservative — only catches divergence that shows in closes.
  console.log('(1) Persistent cross-venue gaps (minute closes, forward-filled):\n');
  console.log('  cost(bps) | opportunities | theoretical net |  max spread');
  console.log('  ----------+---------------+-----------------+------------');
  for (const costBps of PARAMS.costSweepBps) {
    const r = detect(closePoints, [A, B], PARAMS.sizeUsd, costBps, PARAMS.sanityMaxBps);
    console.log(
      `  ${String(costBps).padStart(8)} | ${String(r.count).padStart(13)} | ${fmtUsd(
        r.totalNetUsd,
      ).padStart(15)} | ${(r.maxSpreadBps.toFixed(1) + ' bps').padStart(11)}`,
    );
  }

  // (2) Intra-minute upper bound: within each minute both venues traded, best
  // cross spread using each venue's high/low. Over-counts (high/low may not be
  // simultaneous), so it's a ceiling on what a same-minute reaction could catch.
  console.log('\n(2) Intra-minute UPPER BOUND (per-minute high/low, both venues present):\n');
  const both = [...minutes].filter((m) => ohlc.has(`${m}|${A}`) && ohlc.has(`${m}|${B}`));
  console.log('  cost(bps) | opportunities | theoretical net |  max spread');
  console.log('  ----------+---------------+-----------------+------------');
  for (const costBps of PARAMS.costSweepBps) {
    let count = 0;
    let net = 0;
    let maxBps = 0;
    for (const m of both) {
      const a = ohlc.get(`${m}|${A}`)!;
      const b = ohlc.get(`${m}|${B}`)!;
      const opt1 = (a.high - b.low) / b.low; // buy B.low, sell A.high
      const opt2 = (b.high - a.low) / a.low; // buy A.low, sell B.high
      const spreadBps = Math.max(opt1, opt2) * 10_000;
      if (spreadBps > PARAMS.sanityMaxBps) continue; // data artifact, not an arb
      if (spreadBps > costBps) {
        count++;
        net += ((spreadBps - costBps) / 10_000) * PARAMS.sizeUsd;
        maxBps = Math.max(maxBps, spreadBps);
      }
    }
    console.log(
      `  ${String(costBps).padStart(8)} | ${String(count).padStart(13)} | ${fmtUsd(net).padStart(
        15,
      )} | ${(maxBps.toFixed(1) + ' bps').padStart(11)}`,
    );
  }

  console.log(
    `\nCaveats: minute resolution (sub-minute arbs invisible in #1, and #2's high/low\n` +
      `may not be simultaneous). Size is an assumption (${fmtUsd(
        PARAMS.sizeUsd,
      )}). Both are ceilings — you\n` +
      `won't win every gap. Stage 2 measures who actually won + their tips.\n`,
  );
}

async function probe() {
  const day = yesterdayUtc();
  console.log(`\nVenues trading ${BASE.symbol}/${QUOTE.symbol} on ${day.label} (top by trades):\n`);
  const q = `
    query {
      Solana {
        DEXTradeByTokens(
          where: {
            Trade: { Currency: { MintAddress: { is: "${BASE.mint}" } },
                     Side: { Currency: { MintAddress: { is: "${QUOTE.mint}" } } } },
            Block: { Time: { after: "${day.after}", before: "${day.before}" } }
          }
          orderBy: { descendingByField: "trades" }
          limit: { count: 15 }
        ) { Trade { Dex { ProtocolFamily } } trades: count }
      }
    }`;
  const data = await gql<{ Solana: { DEXTradeByTokens: { Trade: { Dex: { ProtocolFamily: string } }; trades: string }[] } }>(q);
  for (const r of data.Solana.DEXTradeByTokens) {
    console.log(`  ${r.Trade.Dex.ProtocolFamily.padEnd(20)} ${r.trades.padStart(10)} trades`);
  }
  console.log('\nSet VENUES / venuePair in config.ts to real AMMs (not aggregators), then: npm run backtest\n');
}

const cmd = process.argv[2];
const main = cmd === 'run' ? run : cmd === 'probe' ? probe : null;
if (!main) {
  console.log('Usage: npm run probe | npm run backtest');
  process.exit(1);
}
main().catch((e) => {
  console.error('\nError:', e instanceof Error ? e.message : e);
  process.exit(1);
});
