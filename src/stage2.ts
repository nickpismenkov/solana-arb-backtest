// Stage 2 — who's actually competing for SOL/USDC flow, and how concentrated
// it is. If a handful of bots dominate and trade across multiple venues, the
// cross-venue arb from Stage 1 is already locked up and a newcomer's realistic
// capture rate is ~0.
//
// We measure the actual population of players (by transaction signer) rather
// than trying to match each Stage-1 gap to its exact capture — the leaderboard
// + concentration + multi-venue presence answers "how contested" directly.

import 'dotenv/config';
import { BASE, QUOTE, VENUES, yesterdayUtc } from './config.js';
import { gql } from './bitquery.js';

const short = (a: string) => `${a.slice(0, 4)}…${a.slice(-4)}`;
const pct = (n: number, d: number) => (d > 0 ? ((n / d) * 100).toFixed(1) + '%' : '—');

const day = yesterdayUtc();
const WINDOW = `Block: { Time: { after: "${day.after}", before: "${day.before}" } }`;
const venueList = VENUES.map((v) => `"${v}"`).join(', ');
// Dex lives INSIDE Trade (sibling of Currency/Side), not at the where top level.
const TRADE = `Trade: { Currency: { MintAddress: { is: "${BASE.mint}" } }, Side: { Currency: { MintAddress: { is: "${QUOTE.mint}" } } }, Dex: { ProtocolFamily: { in: [${venueList}] } } }`;

interface SignerRow {
  Transaction: { Signer: string };
  cnt: string;
}
interface VenueRow {
  Transaction: { Signer: string };
  Trade: { Dex: { ProtocolFamily: string } };
  cnt: string;
}

async function main() {
  console.log(
    `\nStage 2 — SOL/USDC competition on ${day.label} (UTC), venues: ${VENUES.join(', ')}\n`,
  );

  // Total trades (single aggregate row) for share denominators.
  const totalData = await gql<{ Solana: { DEXTradeByTokens: { cnt: string }[] } }>(`
    query { Solana { DEXTradeByTokens(
      where: { ${TRADE}, ${WINDOW} }
    ) { cnt: count } } }`);
  const total = Number(totalData.Solana.DEXTradeByTokens[0]?.cnt ?? 0);

  // Top signers by trade count.
  const lbData = await gql<{ Solana: { DEXTradeByTokens: SignerRow[] } }>(`
    query { Solana { DEXTradeByTokens(
      where: { ${TRADE}, ${WINDOW} }
      orderBy: { descendingByField: "cnt" }
      limit: { count: 25 }
    ) { Transaction { Signer } cnt: count } } }`);
  const leaders = lbData.Solana.DEXTradeByTokens.map((r) => ({
    signer: r.Transaction.Signer,
    trades: Number(r.cnt),
  }));

  // Venue breakdown for the top signers (which venues each plays = arb signature).
  const topSigners = leaders.slice(0, 15).map((l) => l.signer);
  const signerList = topSigners.map((s) => `"${s}"`).join(', ');
  const vData = await gql<{ Solana: { DEXTradeByTokens: VenueRow[] } }>(`
    query { Solana { DEXTradeByTokens(
      where: { ${TRADE}, Transaction: { Signer: { in: [${signerList}] } }, ${WINDOW} }
      limit: { count: 500 }
    ) { Transaction { Signer } Trade { Dex { ProtocolFamily } } cnt: count } } }`);
  const venuesBySigner = new Map<string, Set<string>>();
  for (const r of vData.Solana.DEXTradeByTokens) {
    const s = r.Transaction.Signer;
    if (!venuesBySigner.has(s)) venuesBySigner.set(s, new Set());
    venuesBySigner.get(s)!.add(r.Trade.Dex.ProtocolFamily);
  }

  console.log(`Total SOL/USDC trades (${VENUES.join('+')}): ${total.toLocaleString()}\n`);
  console.log('Top signers (bots) by trade count:\n');
  console.log('  #  signer          trades     share   venues');
  console.log('  -- --------------- ---------- ------- ---------------------------');
  leaders.forEach((l, i) => {
    const vs = venuesBySigner.get(l.signer);
    const vstr = vs ? `${vs.size} (${[...vs].join(', ')})` : '';
    console.log(
      `  ${String(i + 1).padStart(2)} ${short(l.signer).padEnd(15)} ${String(l.trades).padStart(
        10,
      )} ${pct(l.trades, total).padStart(7)}   ${vstr}`,
    );
  });

  const top5 = leaders.slice(0, 5).reduce((s, l) => s + l.trades, 0);
  const top10 = leaders.slice(0, 10).reduce((s, l) => s + l.trades, 0);
  const multiVenue = [...venuesBySigner.values()].filter((v) => v.size >= 2).length;

  console.log('\nConcentration:');
  console.log(`  Top 5 signers  = ${pct(top5, total)} of all trades`);
  console.log(`  Top 10 signers = ${pct(top10, total)} of all trades`);
  console.log(`  Multi-venue among top 15 = ${multiVenue}/15 (trade ≥2 venues → cross-venue bots)`);

  console.log(
    `\nRead: the more one-sided this is (few signers, high share, multi-venue), the more\n` +
      `the cross-venue arb is already captured by incumbents — i.e. a newcomer's realistic\n` +
      `capture of the Stage-1 ceiling is near zero. Jito-tip levels + same-slot-vs-later\n` +
      `timing (true "who won each gap") is Stage 2b.\n`,
  );
}

main().catch((e) => {
  console.error('\nError:', e instanceof Error ? e.message : e);
  process.exit(1);
});
