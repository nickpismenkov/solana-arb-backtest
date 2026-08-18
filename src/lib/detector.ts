// Port of src/detector.rs
//
// Feed-agnostic fee-adjusted arb detector — a direct port of the TS version.
// Feed it price ticks from any source (gRPC now, ShredStream later) and it
// emits arb open/close with lifetime (slots + ms) = the reaction budget. A
// "real" arb requires the cross-venue gap to exceed the SUM of both fees.

export interface Tick {
  venue: string;
  price: number; // quote per base (USDC per SOL)
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
  peakNetBps: number;
}

export type TickResult =
  | { kind: 'open'; netBps: number }
  | { kind: 'close'; event: ArbEvent }
  | { kind: 'none' };

interface OpenState {
  buy: string;
  sell: string;
  openSlot: number;
  openTsMs: number;
  peakNetBps: number;
  lastSlot: number;
  lastTsMs: number;
}

export class Detector {
  private readonly venues: [string, string];
  readonly thresholdBps: number;
  private lastA: number | undefined;
  private lastB: number | undefined;
  private open: OpenState | undefined;

  constructor(a: string, b: string, feeA: number, feeB: number) {
    this.venues = [a, b];
    this.thresholdBps = feeA + feeB;
    this.lastA = undefined;
    this.lastB = undefined;
    this.open = undefined;
  }

  onTick(t: Tick): TickResult {
    const [a, b] = this.venues;
    if (t.venue === a) {
      this.lastA = t.price;
    } else if (t.venue === b) {
      this.lastB = t.price;
    }
    const pa = this.lastA;
    const pb = this.lastB;
    if (pa === undefined || pb === undefined) {
      return { kind: 'none' };
    }

    const signedGapBps = ((pb - pa) / pa) * 10_000.0; // + => B dearer
    let net: number;
    let buy: string;
    let sell: string;
    if (signedGapBps > this.thresholdBps) {
      net = signedGapBps - this.thresholdBps;
      buy = a;
      sell = b;
    } else if (signedGapBps < -this.thresholdBps) {
      net = -signedGapBps - this.thresholdBps;
      buy = b;
      sell = a;
    } else {
      net = 0.0;
      buy = '';
      sell = '';
    }

    if (net > 0.0) {
      if (this.open === undefined) {
        this.open = {
          buy,
          sell,
          openSlot: t.slot,
          openTsMs: t.tsMs,
          peakNetBps: net,
          lastSlot: t.slot,
          lastTsMs: t.tsMs,
        };
        return { kind: 'open', netBps: net };
      } else {
        this.open.peakNetBps = Math.max(this.open.peakNetBps, net);
        this.open.lastSlot = t.slot;
        this.open.lastTsMs = t.tsMs;
        return { kind: 'none' };
      }
    }

    if (this.open !== undefined) {
      const o = this.open;
      this.open = undefined;
      const closeSlot = t.slot !== 0 ? t.slot : o.lastSlot;
      return {
        kind: 'close',
        event: {
          buyVenue: o.buy,
          sellVenue: o.sell,
          openSlot: o.openSlot,
          closeSlot,
          lifetimeSlots: Math.max(0, closeSlot - o.openSlot),
          lifetimeMs: Math.max(0, t.tsMs - o.openTsMs),
          peakNetBps: o.peakNetBps,
        },
      };
    }
    return { kind: 'none' };
  }
}

export function medianF64(v: number[]): number {
  if (v.length === 0) return 0.0;
  const sorted = [...v].sort((x, y) => x - y);
  return sorted[Math.floor(sorted.length / 2)];
}

export function medianU128(v: number[]): number {
  if (v.length === 0) return 0;
  const sorted = [...v].sort((x, y) => x - y);
  return sorted[Math.floor(sorted.length / 2)];
}
