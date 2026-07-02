import 'dotenv/config';
import { PAIR, PARAMS, VENUES } from './config.js';
import { runSql, type Row } from './flipside.js';
import { probeSql, sampleSql, seriesSql } from './sql.js';
import { detect, type PricePoint } from './detect.js';

const fmtUsd = (n: number) =>
  n.toLocaleString('en-US', { style: 'currency', currency: 'USD', maximumFractionDigits: 0 });

async function probe() {
  console.log(`\nProbing yesterday's ${PAIR.base.symbol}/${PAIR.quote.symbol} swaps on Flipside…\n`);
  const byPlatform = await runSql(probeSql());
  console.log('Platforms that traded this pair yesterday (correct config.ts VENUES to match):');
  for (const r of byPlatform) {
    console.log(`  ${String(r.platform).padEnd(24)} ${String(r.swaps).padStart(10)} swaps`);
  }
  console.log('\nSample rows (check column names + that price ≈ $100–300):');
  const sample = await runSql(sampleSql());
  for (const r of sample.slice(0, 5)) {
    const price = priceOf(r);
    console.log(`  ${r.block_timestamp} ${String(r.platform).padEnd(16)} price≈${price?.toFixed(2)}`);
  }
  console.log('\nIf platforms/price look right, run: npm run backtest\n');
}

// Compute USDC-per-SOL from a raw swap row (used only in the probe sample).
function priceOf(r: Row): number | null {
  const fromMint = String(r.swap_from_mint);
  const fromAmt = Number(r.swap_from_amount);
  const toAmt = Number(r.swap_to_amount);
  if (!fromAmt || !toAmt) return null;
  return fromMint === PAIR.base.mint ? toAmt / fromAmt : fromAmt / toAmt;
}

async function run() {
  console.log(
    `\nBacktesting yesterday's ${PAIR.base.symbol}/${PAIR.quote.symbol} across [${VENUES.join(
      ', ',
    )}]\nAssumed size per opportunity: ${fmtUsd(PARAMS.sizeUsd)} · bucket: ${PARAMS.bucketSeconds}s\n`,
  );

  const rows = await runSql(seriesSql(PARAMS.bucketSeconds));
  const points: PricePoint[] = rows
    .map((r) => ({
      ts: Math.floor(new Date(String(r.ts)).getTime() / 1000),
      venue: String(r.platform),
      price: Number(r.price_usdc_per_sol),
    }))
    .filter((p) => Number.isFinite(p.ts) && Number.isFinite(p.price) && p.price > 0);

  if (points.length === 0) {
    console.log('No price points returned. Run `npm run probe` to check platforms/columns.');
    return;
  }

  // Data summary + sanity.
  const prices = points.map((p) => p.price);
  const perVenue = new Map<string, number>();
  for (const p of points) perVenue.set(p.venue, (perVenue.get(p.venue) ?? 0) + 1);
  console.log(`Price points: ${points.length}  (per venue: ${[...perVenue].map(([v, n]) => `${v}=${n}`).join(', ')})`);
  console.log(`Price range: $${Math.min(...prices).toFixed(2)}–$${Math.max(...prices).toFixed(2)}\n`);

  if (VENUES.length < 2) {
    console.log('Need at least two venues to compare. Update VENUES in config.ts.');
    return;
  }
  const pair: [string, string] = [VENUES[0], VENUES[1]];

  console.log('Theoretical opportunities if you won EVERY gap (one venue pair):\n');
  console.log('  cost(bps) | opportunities | theoretical net |   max spread | median spread');
  console.log('  ----------+---------------+-----------------+--------------+--------------');
  for (const costBps of PARAMS.costSweepBps) {
    const r = detect(points, pair, PARAMS.sizeUsd, costBps);
    console.log(
      `  ${String(costBps).padStart(8)} | ${String(r.count).padStart(13)} | ${fmtUsd(
        r.totalNetUsd,
      ).padStart(15)} | ${(r.maxSpreadBps.toFixed(1) + ' bps').padStart(12)} | ${(
        r.medianSpreadBps.toFixed(1) + ' bps'
      ).padStart(12)}`,
    );
  }

  console.log(
    `\nReminder: this is the CEILING (you won't win every gap), size is an assumption` +
      ` (${fmtUsd(PARAMS.sizeUsd)}), and price is last-trade proxy at ${PARAMS.bucketSeconds}s buckets` +
      ` — sub-bucket arbs are under-counted. Stage 2 measures who actually won.\n`,
  );
}

const cmd = process.argv[2];
const main = cmd === 'run' ? run : cmd === 'probe' ? probe : null;
if (!main) {
  console.log('Usage: npm run probe   (confirm schema/platforms)\n       npm run backtest (24h analysis)');
  process.exit(1);
}
main().catch((e) => {
  console.error('\nError:', e instanceof Error ? e.message : e);
  process.exit(1);
});
