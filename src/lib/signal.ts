// Port of src/signal.rs
//
// Hot-path signal layer. A lock-free price cache (updated by a background gRPC
// task) and a pure local edge calc — so the reaction path reads memory and
// does arithmetic only, never RPC. The on-chain exact-out guard is the real
// safety; this is just the go/no-go heuristic that keeps us from blind-firing
// (and picks the direction). Prices here are gRPC/Turbine-lagged by design —
// acceptable for a heuristic, backstopped by the guard.

/**
 * Latest prices (quote per base) for both venues. There is no JS equivalent
 * of Rust's cross-thread atomics contended from a hot path, so this is a
 * plain mutable holder — single-threaded Node has no torn-read hazard here.
 */
export class PriceCache {
  private orcaPriceVal = NaN;
  private orcaSlotVal = 0;
  private rayPriceVal = NaN;
  private raySlotVal = 0;

  setOrca(price: number, slot: number): void {
    this.orcaPriceVal = price;
    this.orcaSlotVal = slot;
  }

  setRay(price: number, slot: number): void {
    this.rayPriceVal = price;
    this.raySlotVal = slot;
  }

  /** [orcaPrice, rayPrice, orcaSlot, raySlot]. Prices are NaN until seeded. */
  get(): [number, number, number, number] {
    return [this.orcaPriceVal, this.rayPriceVal, this.orcaSlotVal, this.raySlotVal];
  }
}

/**
 * Local round-trip edge estimate. Returns [orcaFirst, edgeBps] for the more
 * profitable direction. orcaFirst=true means buy base on Orca, sell on Ray.
 * First-order (ignores price impact) — a go/no-go heuristic; the guard handles
 * the exact economics on chain.
 */
export function localEdge(
  orcaPrice: number,
  rayPrice: number,
  orcaFeeBps: number,
  rayFeeBps: number,
): [boolean, number] {
  if (!(Number.isFinite(orcaPrice) && Number.isFinite(rayPrice)) || orcaPrice <= 0.0 || rayPrice <= 0.0) {
    return [true, -Infinity];
  }
  const keep = (1.0 - orcaFeeBps / 10_000.0) * (1.0 - rayFeeBps / 10_000.0);
  // orcaFirst: buy base on Orca (cost orcaPrice), sell on Ray (recv rayPrice).
  const edgeOf = ((rayPrice / orcaPrice) * keep - 1.0) * 10_000.0;
  // rayFirst: buy base on Ray, sell on Orca.
  const edgeRf = ((orcaPrice / rayPrice) * keep - 1.0) * 10_000.0;
  if (edgeOf >= edgeRf) {
    return [true, edgeOf];
  } else {
    return [false, edgeRf];
  }
}
