// Stage 2b — the "cost to win": Jito tips paid by the top SOL/USDC bots.
// If the clearing tip per bundle is large relative to the per-trade edge, the
// margin is already competed away; that's the bar a newcomer must outbid.

import 'dotenv/config';
import { BASE, QUOTE, VENUES, yesterdayUtc } from './config.js';
import { gql } from './bitquery.js';

const JITO_TIP_ACCOUNTS = [
  '96gYZGLnJYVFmbjzopPSU6QiEV5fGqZNyN9nmNhvrZU5',
  'HFqU5x63VTqvQss8hp11i4wVV8bD44PvwucfZ2bU7gRe',
  'Cw8CFyM9FkoMi7K7Crf6HNQqf4uEMzpKw6QNghXLvLkY',
  'ADaUMid9yfUytqMBgopwjb2DTLSokTSzL1zt6iGPaS49',
  'DfXygSm4jCyNCybVYYK6DwvWqjKee8pbDmJGcLWNDXjh',
  'ADuUkR4vqLUMWXxW9gh6D6L8pMSawimctcNZ5pGwDcEt',
  'DttWaMuVvTiduZRnguLF7jNxTgiMBZ1hyAumKUiL2KRL',
  '3AVi9Tg9Uo68tJfuvoKvqKNWKkC5wPdSSdeBnizKZ6jT',
];

const short = (a: string) => `${a.slice(0, 4)}…${a.slice(-4)}`;
const SOL_USD = 73; // rough, from Stage 1

const day = yesterdayUtc();
const venueList = VENUES.map((v) => `"${v}"`).join(', ');
const tipList = JITO_TIP_ACCOUNTS.map((a) => `"${a}"`).join(', ');

async function main() {
  console.log(`\nStage 2b — Jito tip economics for top SOL/USDC bots, ${day.label} (UTC)\n`);

  // Top 15 signers (same as Stage 2).
  const lb = await gql<{ Solana: { DEXTradeByTokens: { Transaction: { Signer: string }; cnt: string }[] } }>(`
    query { Solana { DEXTradeByTokens(
      where: { Trade: { Currency: { MintAddress: { is: "${BASE.mint}" } },
                        Side: { Currency: { MintAddress: { is: "${QUOTE.mint}" } } },
                        Dex: { ProtocolFamily: { in: [${venueList}] } } },
               Block: { Time: { after: "${day.after}", before: "${day.before}" } } }
      orderBy: { descendingByField: "cnt" }
      limit: { count: 15 }
    ) { Transaction { Signer } cnt: count } } }`);
  const top = lb.Solana.DEXTradeByTokens.map((r) => ({ signer: r.Transaction.Signer, trades: Number(r.cnt) }));
  const signerList = top.map((t) => `"${t.signer}"`).join(', ');

  // Jito tips those signers paid (SOL transfers to tip accounts), grouped by sender.
  const tips = await gql<{
    Solana: { Transfers: { Transfer: { Sender: { Address: string } }; bundles: string; tipSol: string }[] };
  }>(`
    query { Solana { Transfers(
      where: { Transfer: { Sender: { Address: { in: [${signerList}] } },
                           Receiver: { Address: { in: [${tipList}] } } },
               Block: { Time: { after: "${day.after}", before: "${day.before}" } } }
      limit: { count: 100 }
    ) {
      Transfer { Sender { Address } }
      bundles: count
      tipSol: sum(of: Transfer_Amount)
    } } }`);

  const tipBySigner = new Map<string, { bundles: number; sol: number }>();
  for (const r of tips.Solana.Transfers) {
    tipBySigner.set(r.Transfer.Sender.Address, { bundles: Number(r.bundles), sol: Number(r.tipSol) });
  }

  console.log('  signer          trades     jito bundles    total tip     mean tip/bundle');
  console.log('  --------------- ---------- --------------- ------------- ---------------');
  let totalTipSol = 0;
  let totalBundles = 0;
  for (const t of top) {
    const tp = tipBySigner.get(t.signer);
    const sol = tp?.sol ?? 0;
    const b = tp?.bundles ?? 0;
    totalTipSol += sol;
    totalBundles += b;
    const mean = b > 0 ? sol / b : 0;
    console.log(
      `  ${short(t.signer).padEnd(15)} ${String(t.trades).padStart(10)} ${String(b).padStart(15)} ${(
        sol.toFixed(3) + ' SOL'
      ).padStart(13)} ${(mean.toFixed(6) + ' SOL').padStart(15)}`,
    );
  }

  const meanTip = totalBundles > 0 ? totalTipSol / totalBundles : 0;
  console.log(
    `\n  Top-15 total Jito tips: ${totalTipSol.toFixed(2)} SOL (~$${(totalTipSol * SOL_USD).toFixed(
      0,
    )}) across ${totalBundles.toLocaleString()} bundles`,
  );
  console.log(
    `  Mean tip / landed bundle: ${meanTip.toFixed(6)} SOL (~$${(meanTip * SOL_USD).toFixed(4)})`,
  );
  console.log(
    `\nThat mean tip is the floor you must outbid to land a bundle ahead of them —\n` +
      `per opportunity, on top of gas. Compare to the per-trade edge from Stage 1.\n`,
  );
}

main().catch((e) => {
  console.error('\nError:', e instanceof Error ? e.message : e);
  process.exit(1);
});
