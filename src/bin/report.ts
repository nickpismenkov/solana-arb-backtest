// Port of src/bin/report.rs
//
// Rollup of the executor ledgers (decisions.jsonl + trades.jsonl): decodable
// victims evaluated, profitable predictions, fires, landings, realized P&L,
// tips paid. Reads the JSONL the executor writes; safe to run while it's live.
//
// Usage: RUN_DIR=runs tsx src/bin/report.ts

import { readFileSync } from 'node:fs';

function readJsonl(path: string): any[] {
  let text: string;
  try {
    text = readFileSync(path, 'utf8');
  } catch {
    return [];
  }
  const out: any[] = [];
  for (const line of text.split('\n')) {
    if (line.length === 0) continue;
    try {
      out.push(JSON.parse(line));
    } catch {
      // ignore
    }
  }
  return out;
}

function main(): void {
  const dir = process.env.RUN_DIR ?? 'runs';
  const decisions = readJsonl(`${dir}/decisions.jsonl`);
  const trades = readJsonl(`${dir}/trades.jsonl`);

  // Decisions: one per decodable victim we evaluated (routed/CPI skipped).
  const evaluated = decisions.length;
  const profitable = decisions.filter((d) => d.reason === 'profitable').length;
  const below = decisions.filter((d) => d.reason === 'below_threshold').length;
  const fired = decisions.filter((d) => d.fired === true).length; // live submits (0 in dry run)

  // Trades: submit errors, and confirmed on-chain landings (realized_usdc set).
  const submitErrors = trades.filter((t) => typeof t.error === 'string').length;
  const landed = trades.filter((t) => typeof t.realized_usdc === 'number');
  const realized = landed.reduce((a, t) => a + (t.realized_usdc as number), 0);
  const tipsSol = landed.reduce((a, t) => a + (typeof t.tip_lamports === 'number' ? t.tip_lamports : 0), 0) / 1e9;

  console.log(`\n──────── executor rollup (${dir}) ────────`);
  console.log(`decodable victims evaluated:  ${evaluated}`);
  console.log(`  predicted PROFITABLE:       ${profitable}   (below threshold: ${below})`);
  console.log(`  fired live:                 ${fired}   (0 in dry run)`);
  console.log(`submit errors:                ${submitErrors}`);
  console.log(`LANDED on-chain:              ${landed.length}`);
  console.log(`realized P&L:                 ${realized >= 0 ? '+' : ''}${realized.toFixed(4)} USDC`);
  console.log(`tips paid (landed only):      ${tipsSol.toFixed(6)} SOL`);
  if (landed.length === 0 && profitable > 0) {
    console.log(`\n→ found ${profitable} profitable predictions but nothing landed = losing the race (or dry run).`);
  } else if (profitable === 0) {
    console.log('\n→ no profitable predictions = no capturable edge in this window (or market quiet).');
  }
  console.log();
}

main();
