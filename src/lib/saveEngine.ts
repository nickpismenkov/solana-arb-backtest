// Port of src/save_engine.rs
//
// Off-chain, Lazer-driven health for Solend obligations — the event-driven
// trigger that replaces liq_save_executor's 30s stored-health poll. That poll
// lost every race (census: 45 USDC-debt Solend liquidations in 48h, 0 caught)
// because competitors react to the oracle in ms while we looked every 30s.
//
// Design (robust, avoids Solend's fiddly absolute price/amount scaling): ANCHOR
// on the obligation's own on-chain health — the STORED `borrowedValue` and
// `unhealthyBorrowValue`, correct as of Solend's last refresh — and TRACK it
// by the Lazer price RATIO. At rescan we snapshot each side's Lazer feed price;
// on every tick we scale the stored values by `lazerNow / lazerAtRescan`
// (exactly 1.0 at rescan, so it reproduces the on-chain values, and tracks
// ms-latency moves between rescans with ZERO RPC). Anchoring on the *Lazer*
// price (not the reserve `marketPrice`) is what makes LST collateral correct:
// mSOL/jitoSOL map to the SOL feed but their reserve price carries the staking
// premium — the ratio only cares about the feed's relative move.
//
// v1 scope: single deposit + single borrow (matches the fire path). The full
// on-chain simulateBundle remains the authoritative fire gate; this engine only
// decides WHO to spend that sim budget on, fast.

import { PublicKey } from '@solana/web3.js';
import { obligationFreshHealth, type Obligation, type Reserve } from './save.js';

/** One watched Solend obligation reduced to price-ratio tracking. */
export class SolendWatch {
  obligation: PublicKey;
  collReserve: PublicKey;
  debtReserve: PublicKey;
  private borrowedStored: number;
  private unhealthyStored: number;
  /** FRESH-price health at rescan — borrowed/unhealthy recomputed from the
   * freshly-fetched reserves via the cToken exchange rate (obligationFreshHealth),
   * i.e. the value Solend's own `liquidate` recomputes at settle time. The FIRE
   * tier gates on THIS, not on the lazily-stale stored verdict that over-reports
   * phantoms. Zeroed if a referenced reserve was missing. */
  private freshBorrowed: number;
  private freshUnhealthy: number;
  /** Lazer feed for each side (undefined = priced off a non-Lazer/baseline
   * oracle, so it can't move between rescans → ratio stays 1.0). */
  private collFeed: number | undefined;
  private debtFeed: number | undefined;
  /** Lazer feed price captured at rescan (the ratio anchor). undefined if the
   * feed had no live price at rescan. */
  private collAnchor: number | undefined;
  private debtAnchor: number | undefined;
  /** unhealthy_borrow_value > 0 and both reserves priced — else never trusted. */
  complete: boolean;

  private constructor(fields: {
    obligation: PublicKey;
    collReserve: PublicKey;
    debtReserve: PublicKey;
    borrowedStored: number;
    unhealthyStored: number;
    freshBorrowed: number;
    freshUnhealthy: number;
    collFeed: number | undefined;
    debtFeed: number | undefined;
    collAnchor: number | undefined;
    debtAnchor: number | undefined;
    complete: boolean;
  }) {
    this.obligation = fields.obligation;
    this.collReserve = fields.collReserve;
    this.debtReserve = fields.debtReserve;
    this.borrowedStored = fields.borrowedStored;
    this.unhealthyStored = fields.unhealthyStored;
    this.freshBorrowed = fields.freshBorrowed;
    this.freshUnhealthy = fields.freshUnhealthy;
    this.collFeed = fields.collFeed;
    this.debtFeed = fields.debtFeed;
    this.collAnchor = fields.collAnchor;
    this.debtAnchor = fields.debtAnchor;
    this.complete = fields.complete;
  }

  /**
   * Build for a v1 obligation (1 deposit, 1 borrow). `lazerNow` is the Lazer
   * snapshot at rescan, used to anchor the ratios.
   */
  static build(
    o: Obligation,
    obligation: PublicKey,
    reserves: Map<string, Reserve>,
    mintFeed: Map<string, number>,
    lazerNow: Map<number, number>,
  ): SolendWatch | null {
    if (o.deposits.length !== 1 || o.borrows.length !== 1) return null;
    const coll = reserves.get(o.deposits[0].reserve.toBase58());
    const debt = reserves.get(o.borrows[0].reserve.toBase58());
    if (coll === undefined || debt === undefined) return null;
    const collFeed = mintFeed.get(coll.liquidityMint.toBase58());
    const debtFeed = mintFeed.get(debt.liquidityMint.toBase58());
    const collAnchor = collFeed !== undefined ? lazerNow.get(collFeed) : undefined;
    const debtAnchor = debtFeed !== undefined ? lazerNow.get(debtFeed) : undefined;
    // Fresh-price health from the current reserves (cToken exchange rate) —
    // the phantom-free fire-tier verdict. Falls back to (0,0) only if a
    // reserve is unexpectedly absent, which leaves the obligation NOT
    // fire-eligible (safe: it stays merely watched).
    const fresh = obligationFreshHealth(o, reserves) ?? [0.0, 0.0];
    const [freshBorrowed, freshUnhealthy] = fresh;
    return new SolendWatch({
      obligation,
      collReserve: coll.reserve,
      debtReserve: debt.reserve,
      borrowedStored: o.borrowedValue,
      unhealthyStored: o.unhealthyBorrowValue,
      freshBorrowed,
      freshUnhealthy,
      collFeed,
      debtFeed,
      collAnchor,
      debtAnchor,
      complete: o.unhealthyBorrowValue > 0.0 && coll.marketPrice > 0.0 && debt.marketPrice > 0.0,
    });
  }

  private ratio(feed: number | undefined, anchor: number | undefined, lazer: Map<number, number>): number {
    if (feed !== undefined && anchor !== undefined && anchor > 0.0) {
      const p = lazer.get(feed);
      return p !== undefined ? p / anchor : 1.0;
    }
    return 1.0;
  }

  borrowed(lazer: Map<number, number>): number {
    return this.borrowedStored * this.ratio(this.debtFeed, this.debtAnchor, lazer);
  }
  unhealthy(lazer: Map<number, number>): number {
    return this.unhealthyStored * this.ratio(this.collFeed, this.collAnchor, lazer);
  }
  liquidatable(lazer: Map<number, number>): boolean {
    return this.complete && this.borrowed(lazer) > this.unhealthy(lazer);
  }
  /** borrowed/unhealthy at the given prices (>1.0 = underwater). */
  ratioNow(lazer: Map<number, number>): number {
    const u = this.unhealthy(lazer);
    return u <= 0.0 ? 0.0 : this.borrowed(lazer) / u;
  }
  /** True unless a Lazer-mapped side is missing a live price (then the ratio
   * would silently fall back to 1.0 and hide a move — don't trust it). */
  feedsReady(lazer: Map<number, number>): boolean {
    const ok = (f: number | undefined, a: number | undefined): boolean => {
      if (f !== undefined && a !== undefined) return lazer.has(f);
      return true;
    };
    return ok(this.collFeed, this.collAnchor) && ok(this.debtFeed, this.debtAnchor);
  }

  // ── FRESH-price on-chain health — FIRE-tier gate ───────────────────────────
  // The Lazer projection above decides WHO to re-check; these decide whether an
  // obligation is ACTUALLY liquidatable at the on-chain oracle price Solend
  // settles against — recomputed at rescan from the FRESH reserve prices + cToken
  // exchange rate (obligationFreshHealth), the exact value Solend's own
  // `liquidate` recomputes at settle time.
  //
  // We do NOT gate on the obligation's STORED borrowed/unhealthy: Solend refreshes
  // obligation health LAZILY, so hundreds read stale-HIGH on the collateral side
  // (priced when the collateral was worth more) and look liquidatable while a fresh
  // refresh proves them healthy — the "phantom" flood (live: 396 stored-liquidatable
  // vs 0 at fresh price in a calm market). Because fresh_health reproduces Solend's
  // refresh to 0.0000% on same-slot obligations, gating on it never UNDER-states
  // liquidatability relative to what Solend itself will compute — a genuinely
  // underwater position is still flagged — while the phantoms collapse away. The
  // executor still confirms every fire with a full sim as the ground-truth backstop.
  onchainLiquidatable(): boolean {
    return this.complete && this.freshUnhealthy > 0.0 && this.freshBorrowed > this.freshUnhealthy;
  }
  /** USD deficit (borrowed − unhealthy) at fresh prices; > 0 iff liquidatable. */
  onchainDeficit(): number {
    return this.freshBorrowed - this.freshUnhealthy;
  }
  /** Fresh-price ratio (borrowed / unhealthy); > 1 = underwater on-chain. */
  onchainRatio(): number {
    return this.freshUnhealthy <= 0.0 ? 0.0 : this.freshBorrowed / this.freshUnhealthy;
  }
}

/** In-memory watch-set, rebuilt on rescan, queried on every Lazer tick. */
export class Engine {
  accounts: SolendWatch[] = [];
  minDebt: number;
  /** Reject obligations whose ratio exceeds this — an absurd ratio (borrowed ≫
   * unhealthy) means the collateral is mis-priced near zero (dust / dead
   * feed), never a real opportunity. Without it, deficit-ranking would put
   * these un-fireable accounts FIRST (huge borrowed − ~0 unhealthy) and starve
   * the genuine near-threshold ones. Census-proven fix from the old poller. */
  ratioCap: number;

  constructor(minDebt: number, ratioCap: number) {
    this.minDebt = minDebt;
    this.ratioCap = ratioCap;
  }

  /**
   * Rebuild from decoded obligations. Keeps v1-shaped, ≥ minDebt, complete
   * obligations near threshold (watchRatio ≤ ratio ≤ ratioCap at build prices).
   */
  rebuild(
    obls: Array<[PublicKey, Obligation]>,
    reserves: Map<string, Reserve>,
    mintFeed: Map<string, number>,
    watchRatio: number,
    lazerNow: Map<number, number>,
  ): number {
    this.accounts = [];
    for (const [pk, o] of obls) {
      if (o.borrowedValue < this.minDebt) continue;
      const w = SolendWatch.build(o, pk, reserves, mintFeed, lazerNow);
      if (w === null) continue;
      const r = w.ratioNow(lazerNow);
      if (w.complete && r >= watchRatio && r <= this.ratioCap) {
        this.accounts.push(w);
      }
    }
    return this.accounts.length;
  }

  /** Liquidatable obligations (fireRatio ≤ ratio ≤ ratioCap) at these prices. */
  crossed(lazer: Map<number, number>, fireRatio: number): PublicKey[] {
    return this.accounts
      .filter((w) => w.complete && w.feedsReady(lazer))
      .filter((w) => {
        const r = w.ratioNow(lazer);
        return r >= fireRatio && r <= this.ratioCap;
      })
      .map((w) => w.obligation);
  }

  /**
   * Same, ranked by USD deficit (borrowed − unhealthy) desc — biggest real
   * opportunity first, with the mis-priced-dust tail excluded by ratioCap.
   */
  crossedRanked(lazer: Map<number, number>, fireRatio: number): Array<[PublicKey, number]> {
    const v: Array<[PublicKey, number]> = [];
    for (const w of this.accounts) {
      if (!(w.complete && w.feedsReady(lazer))) continue;
      const r = w.ratioNow(lazer);
      if (r >= fireRatio && r <= this.ratioCap) {
        v.push([w.obligation, w.borrowed(lazer) - w.unhealthy(lazer)]);
      }
    }
    v.sort((a, b) => b[1] - a[1]);
    return v;
  }

  /**
   * FIRE tier — obligations liquidatable at the ON-CHAIN oracle price (stored
   * health from the last rescan), ranked by USD deficit desc so the biggest
   * real opportunity wins the capped sim budget. Lazer only NARROWS who to
   * watch; the on-chain price GATES the expensive sim. The mis-priced-dust tail
   * (borrowed ≫ ~0 unhealthy) is excluded by ratioCap, same as crossedRanked.
   */
  onchainLiquidatableRanked(): Array<[PublicKey, number]> {
    const v: Array<[PublicKey, number]> = [];
    for (const w of this.accounts) {
      if (w.onchainLiquidatable() && w.onchainRatio() <= this.ratioCap) {
        v.push([w.obligation, w.onchainDeficit()]);
      }
    }
    v.sort((a, b) => b[1] - a[1]);
    return v;
  }

  /** Count of on-chain-liquidatable obligations — the REAL fire-candidate count
   * for the heartbeat, distinct from the (much larger) Lazer-flagged set. */
  onchainLiquidatableCount(): number {
    return this.accounts.filter((w) => w.onchainLiquidatable() && w.onchainRatio() <= this.ratioCap).length;
  }

  /** The on-chain (stored) ratio for a specific watched obligation. */
  onchainRatioOf(obligation: PublicKey): number | undefined {
    const w = this.accounts.find((a) => a.obligation.equals(obligation));
    return w?.onchainRatio();
  }

  /** Look up a watched obligation's reserves (for building the fire/refresh). */
  reservesOf(obligation: PublicKey): [PublicKey, PublicKey] | undefined {
    const w = this.accounts.find((a) => a.obligation.equals(obligation));
    return w !== undefined ? [w.collReserve, w.debtReserve] : undefined;
  }
}

// (tests omitted)
