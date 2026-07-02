// Opportunity detection over a per-venue price series.
//
// Method: walk the merged, time-ordered price points forward, forward-filling
// each venue's last price. Whenever the two venues' prices diverge by more than
// the cost threshold, an arb existed (buy on the cheaper venue, sell on the
// dearer). We count each *opening* of such a gap as one opportunity (not every
// tick it stays open) and take the max spread while it's open for the gross.
//
// Caveats, on purpose:
//  - Price = last *executed trade* price per bucket, a proxy for the pool mid.
//  - Gross assumes a fixed notional (PARAMS.sizeUsd); real capturable size is
//    bounded by pool depth, which trade data alone doesn't give us.
//  - This is the theoretical ceiling if you won *every* gap — Stage 2 measures
//    who actually won.

export interface PricePoint {
  ts: number; // epoch seconds
  venue: string;
  price: number; // USDC per SOL
}

export interface Opportunity {
  openTs: number;
  closeTs: number;
  durationSec: number;
  buyVenue: string;
  sellVenue: string;
  maxSpreadBps: number;
  grossUsd: number; // maxSpread * size
  netUsd: number; // (maxSpread - cost) * size
}

export interface DetectResult {
  costBps: number;
  count: number;
  totalNetUsd: number;
  totalGrossUsd: number;
  medianSpreadBps: number;
  maxSpreadBps: number;
  opportunities: Opportunity[];
}

export function detect(
  points: PricePoint[],
  venues: [string, string],
  sizeUsd: number,
  costBps: number,
): DetectResult {
  const [A, B] = venues;
  const sorted = [...points].sort((a, b) => a.ts - b.ts);
  const last: Record<string, number | undefined> = {};

  let open:
    | { openTs: number; lastTs: number; buyVenue: string; sellVenue: string; maxSpreadBps: number }
    | null = null;
  const opps: Opportunity[] = [];

  const closeOpen = () => {
    if (!open) return;
    const grossUsd = (open.maxSpreadBps / 10_000) * sizeUsd;
    const netUsd = ((open.maxSpreadBps - costBps) / 10_000) * sizeUsd;
    opps.push({
      openTs: open.openTs,
      closeTs: open.lastTs,
      durationSec: Math.max(0, open.lastTs - open.openTs),
      buyVenue: open.buyVenue,
      sellVenue: open.sellVenue,
      maxSpreadBps: open.maxSpreadBps,
      grossUsd,
      netUsd,
    });
    open = null;
  };

  for (const p of sorted) {
    last[p.venue] = p.price;
    const pa = last[A];
    const pb = last[B];
    if (pa === undefined || pb === undefined) continue;

    const spreadBps = (Math.abs(pa - pb) / Math.min(pa, pb)) * 10_000;
    const buyVenue = pa < pb ? A : B;
    const sellVenue = pa < pb ? B : A;

    if (spreadBps > costBps) {
      if (!open) open = { openTs: p.ts, lastTs: p.ts, buyVenue, sellVenue, maxSpreadBps: spreadBps };
      else {
        open.lastTs = p.ts;
        if (spreadBps > open.maxSpreadBps) {
          open.maxSpreadBps = spreadBps;
          open.buyVenue = buyVenue;
          open.sellVenue = sellVenue;
        }
      }
    } else {
      closeOpen();
    }
  }
  closeOpen();

  const spreads = opps.map((o) => o.maxSpreadBps).sort((a, b) => a - b);
  const median = spreads.length ? spreads[Math.floor(spreads.length / 2)] : 0;

  return {
    costBps,
    count: opps.length,
    totalNetUsd: opps.reduce((s, o) => s + o.netUsd, 0),
    totalGrossUsd: opps.reduce((s, o) => s + o.grossUsd, 0),
    medianSpreadBps: median,
    maxSpreadBps: spreads.length ? spreads[spreads.length - 1] : 0,
    opportunities: opps,
  };
}
