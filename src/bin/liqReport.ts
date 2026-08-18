// Port of src/bin/liq_report.rs
//
// Digest of a liquidation executor run — read the JSONL ledgers and summarize
// so you can answer "is it working / did it earn?" at a glance without tailing
// the stream. Reads {RUN_DIR}/decisions.jsonl + trades.jsonl (both schemas
// tolerated). WATCH=1 reprints every REFRESH_SECS.
//
// Usage: [RUN_DIR=runs/liq] [WATCH=1] [REFRESH_SECS=30] tsx src/bin/liqReport.ts

import * as fs from 'node:fs';
import * as readline from 'node:readline';

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function readJsonl(path: string): Promise<any[]> {
  if (!fs.existsSync(path)) return [];
  const out: any[] = [];
  const rl = readline.createInterface({ input: fs.createReadStream(path), crlfDelay: Infinity });
  for await (const line of rl) {
    try {
      out.push(JSON.parse(line));
    } catch {
      // skip malformed lines
    }
  }
  return out;
}

function f(v: any, k: string): number {
  const x = v?.[k];
  return typeof x === 'number' ? x : 0.0;
}
function s(v: any, k: string): string | undefined {
  const x = v?.[k];
  return typeof x === 'string' ? x : undefined;
}

async function report(runDir: string): Promise<void> {
  const decisions = await readJsonl(`${runDir}/decisions.jsonl`);
  const trades = await readJsonl(`${runDir}/trades.jsonl`);

  // Liquidation decision rows across ALL executor schemas: marginfi keys the
  // borrower as "liquidatee", Save/Kamino as "obligation" — but all three have
  // "reason" + "fired" (and the arb-engine rows don't), so match on those.
  const liqDecisions = decisions.filter((d) => d?.reason !== undefined && d?.fired !== undefined);
  const fired = liqDecisions.filter((d) => d?.fired === true).length;
  const reasons = new Map<string, number>();
  for (const d of liqDecisions) {
    const r = s(d, 'reason') ?? '(none)';
    reasons.set(r, (reasons.get(r) ?? 0) + 1);
  }

  // Trades: liquidation trade rows (have "est_profit_usdc", unlike arb rows).
  // Submissions have a signature; landings have realized_usdc.
  const liqTrades = trades.filter((t) => t?.est_profit_usdc !== undefined);
  const submitted = liqTrades.filter((t) => s(t, 'signature') !== undefined);
  const landed = trades.filter((t) => t?.realized_usdc !== undefined && t?.realized_usdc !== null);
  const realized = landed.reduce((acc, t) => acc + f(t, 'realized_usdc'), 0.0);
  const errors = submitted.filter((t) => t?.error !== undefined && t?.error !== null);

  console.log(`═══ liquidation report (${runDir}) ═══`);
  console.log(`decisions logged: ${liqDecisions.length} (liquidation rows)`);
  console.log(`  fired:   ${fired}`);
  console.log(`  skipped: ${Math.max(0, liqDecisions.length - fired)}`);
  if (reasons.size > 0) {
    console.log('  reasons:');
    const sorted = Array.from(reasons.entries());
    sorted.sort((a, b) => b[1] - a[1]);
    for (const [r, n] of sorted.slice(0, 12)) {
      const text = r.length > 90 ? r.slice(0, 90) : r;
      console.log(`    ${String(n).padStart(5)}  ${text}`);
    }
  }
  console.log('trades:');
  console.log(`  submitted:  ${submitted.length}`);
  console.log(`  errored:    ${errors.length}`);
  console.log(`  landed:     ${landed.length}`);
  console.log(`  realized P&L: $${realized.toFixed(2)}`);
  if (landed.length === 0 && submitted.length === 0) {
    console.log('\n→ no fires yet. In a calm market that\'s expected — the bot only fires a');
    console.log('  marginfi-confirmed, profitable liquidation. Leave it running.');
  } else if (landed.length > 0) {
    console.log('\n→ ★ the strategy has landed real liquidations. That\'s the money question answered.');
  }
}

async function main(): Promise<void> {
  const runDir = process.env.RUN_DIR ?? 'runs/liq';
  const watch = process.env.WATCH === '1';
  const refresh = Number.parseInt(process.env.REFRESH_SECS ?? '', 10) || 30;
  if (!watch) {
    await report(runDir);
    return;
  }
  for (;;) {
    process.stdout.write('\x1b[2J\x1b[H'); // clear screen
    await report(runDir);
    await sleep(refresh * 1000);
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
