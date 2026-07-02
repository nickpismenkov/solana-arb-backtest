// Feed-agnostic fee-adjusted arb detector. You feed it price ticks from ANY
// source (gRPC now, ShredStream later) and it emits arb open/close events with
// lifetime (slots + ms) = the reaction budget. A "real" arb requires the
// cross-venue gap to exceed the SUM of both venues' fees (net of fees).

export interface Tick {
  venue: string;
  price: number; // quote per base (e.g. USDC per SOL)
  slot: number;
  tsMs: number;
}

export interface ArbEvent {
  buyVenue: string;
  sellVenue: string;
  openSlot: number;
  closeSlot: number;
  lifetimeSlots: number;
  lifetimeMs: number;
  peakNetBps: number; // peak edge beyond both fees
}

export interface DetectorOpts {
  venues: [string, string];
  feeBps: Record<string, number>; // per-venue swap fee in bps
}

type OpenState = {
  buyVenue: string;
  sellVenue: string;
  openSlot: number;
  openTsMs: number;
  peakNetBps: number;
  lastSlot: number;
  lastTsMs: number;
};

export type TickResult =
  | { type: 'none'; netBps: number }
  | { type: 'open'; netBps: number }
  | { type: 'close'; event: ArbEvent };

export class Detector {
  readonly thresholdBps: number;
  private last: Record<string, number> = {};
  private open: OpenState | null = null;

  constructor(private opts: DetectorOpts) {
    const [a, b] = opts.venues;
    this.thresholdBps = (opts.feeBps[a] ?? 0) + (opts.feeBps[b] ?? 0);
  }

  onTick(t: Tick): TickResult {
    this.last[t.venue] = t.price;
    const [A, B] = this.opts.venues;
    const pa = this.last[A];
    const pb = this.last[B];
    if (pa === undefined || pb === undefined) return { type: 'none', netBps: 0 };

    const signedGapBps = ((pb - pa) / pa) * 10_000; // + => B dearer
    let net = 0;
    let buy = '';
    let sell = '';
    if (signedGapBps > this.thresholdBps) {
      net = signedGapBps - this.thresholdBps;
      buy = A;
      sell = B;
    } else if (signedGapBps < -this.thresholdBps) {
      net = -signedGapBps - this.thresholdBps;
      buy = B;
      sell = A;
    }

    if (net > 0) {
      if (!this.open) {
        this.open = {
          buyVenue: buy,
          sellVenue: sell,
          openSlot: t.slot,
          openTsMs: t.tsMs,
          peakNetBps: net,
          lastSlot: t.slot,
          lastTsMs: t.tsMs,
        };
        return { type: 'open', netBps: net };
      }
      this.open.peakNetBps = Math.max(this.open.peakNetBps, net);
      this.open.lastSlot = t.slot;
      this.open.lastTsMs = t.tsMs;
      return { type: 'none', netBps: net };
    }

    if (this.open) {
      const o = this.open;
      this.open = null;
      const closeSlot = t.slot || o.lastSlot;
      return {
        type: 'close',
        event: {
          buyVenue: o.buyVenue,
          sellVenue: o.sellVenue,
          openSlot: o.openSlot,
          closeSlot,
          lifetimeSlots: Math.max(0, closeSlot - o.openSlot),
          lifetimeMs: Math.max(0, t.tsMs - o.openTsMs),
          peakNetBps: o.peakNetBps,
        },
      };
    }
    return { type: 'none', netBps: 0 };
  }
}

const median = (a: number[]) => (a.length ? [...a].sort((x, y) => x - y)[Math.floor(a.length / 2)] : 0);

export function report(events: ArbEvent[], thresholdBps: number, runSec: number): void {
  console.log(`\n──────── shadow report (${runSec.toFixed(0)}s) ────────`);
  console.log(`Arb threshold (both fees): ${thresholdBps} bps`);
  console.log(`Real fee-adjusted arbs detected: ${events.length}`);
  if (!events.length) {
    console.log('  → none: pair arbed to within fees at this feed resolution.\n');
    return;
  }
  const nets = events.map((e) => e.peakNetBps);
  const slots = events.map((e) => e.lifetimeSlots);
  const ms = events.map((e) => e.lifetimeMs);
  const le1 = events.filter((e) => e.lifetimeSlots <= 1).length;
  console.log(`  peak net edge: median ${median(nets).toFixed(1)} bps, max ${Math.max(...nets).toFixed(1)} bps`);
  console.log(`  LIFETIME (reaction budget): median ${median(slots)} slots / ${median(ms)} ms, max ${Math.max(...slots)} slots`);
  console.log(`  closed within ≤1 slot: ${le1}/${events.length} (${((le1 / events.length) * 100).toFixed(0)}%) → need co-located/ShredStream speed`);
  console.log();
}
