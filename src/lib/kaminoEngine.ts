// Port of src/kamino_engine.rs
//
// Off-chain, Lazer-driven health for Kamino (KLend) obligations — the
// event-driven trigger that replaces liq_kamino_executor's 30s poll. The Save
// census (45 USDC-debt liquidations in 48h, 0 caught) proved the poll fatal:
// competitors react to the oracle in milliseconds while we looked every 30s.
//
// Design — same anchor-on-stored-health approach as src/save_engine.rs. Kamino
// stores its own on-chain-correct health on the obligation as `Fraction`
// fixed-point: `bf_adjusted_debt` (the borrow-factor-adjusted debt value) and
// `unhealthy_borrow_value` (the liquidation threshold), with
//
//     liquidatable  <=>  bf_adjusted_debt >= unhealthy_borrow_value
//
// Those values are correct as of the obligation's last on-chain refresh. We
// ANCHOR on them and TRACK by the Lazer price RATIO: at rescan we snapshot each
// side's Lazer feed price; on every tick we scale the stored values by
// `lazer_now / lazer_at_rescan`. The debt side scales by the DEBT feed, the
// threshold side by the COLLATERAL feed. Exactly 1.0 at rescan (reproduces the
// on-chain values) and it tracks ms moves between rescans with ZERO RPC. The
// borrow-factor / liquidation-threshold multipliers are already baked into the
// stored values, so a proportional price move preserves them.
//
// Anchoring on the *Lazer feed* price (not the reserve's Scope `market_price`)
// is what makes LST collateral correct: mSOL/jitoSOL map to the SOL feed but
// their reserve price carries the staking premium — the ratio only cares about
// the feed's relative move.
//
// v1 scope: single deposit + single borrow, non-elevation (matches the fire
// path). The full on-chain fire-tx simulation remains the authoritative gate;
// this engine only decides WHO to spend that sim/arm budget on, fast.

import { PublicKey } from '@solana/web3.js';
import type { Obligation } from './kamino.js';

/** Lazer feed prices keyed by feed id. */
export type LazerPriceMap = Map<number, number>;
/** Reserve pubkey (base58) -> Lazer feed id. */
export type ReserveFeedMap = Map<string, number>;

function ratio(feed: number | undefined, anchor: number | undefined, lazer: LazerPriceMap): number {
  if (feed !== undefined && anchor !== undefined && anchor > 0.0) {
    const p = lazer.get(feed);
    return p !== undefined ? p / anchor : 1.0;
  }
  return 1.0;
}

/** One watched Kamino obligation reduced to price-ratio tracking. */
export class KaminoWatch {
  obligation: PublicKey;
  collReserve: PublicKey;
  debtReserve: PublicKey;
  /** Stored borrow-factor-adjusted debt (USD) — the value compared to threshold. */
  private bfDebtStored: number;
  /** Stored liquidation threshold (USD). */
  private unhealthyStored: number;
  /**
   * Lazer feed for each side (undefined = priced off a non-Lazer oracle, so it
   * can't move between rescans -> ratio stays 1.0).
   */
  private collFeed: number | undefined;
  private debtFeed: number | undefined;
  /**
   * Lazer feed price captured at rescan (the ratio anchor). undefined if the
   * feed had no live price at rescan.
   */
  private collAnchor: number | undefined;
  private debtAnchor: number | undefined;
  /** bf_adjusted_debt > 0 and unhealthy_borrow_value > 0 — else never trusted. */
  complete: boolean;

  private constructor(
    obligation: PublicKey,
    collReserve: PublicKey,
    debtReserve: PublicKey,
    bfDebtStored: number,
    unhealthyStored: number,
    collFeed: number | undefined,
    debtFeed: number | undefined,
    collAnchor: number | undefined,
    debtAnchor: number | undefined,
    complete: boolean,
  ) {
    this.obligation = obligation;
    this.collReserve = collReserve;
    this.debtReserve = debtReserve;
    this.bfDebtStored = bfDebtStored;
    this.unhealthyStored = unhealthyStored;
    this.collFeed = collFeed;
    this.debtFeed = debtFeed;
    this.collAnchor = collAnchor;
    this.debtAnchor = debtAnchor;
    this.complete = complete;
  }

  /**
   * Build for a v1 obligation (1 deposit, 1 borrow, non-elevation). `reserveFeed`
   * maps each reserve pubkey (base58) -> its Lazer feed id (via the reserve's
   * liquidity mint); `lazerNow` is the Lazer snapshot at rescan, used to anchor
   * the ratios.
   */
  static build(o: Obligation, obligation: PublicKey, reserveFeed: ReserveFeedMap, lazerNow: LazerPriceMap): KaminoWatch | null {
    if (o.deposits.length !== 1 || o.borrows.length !== 1 || o.elevationGroup !== 0) return null;
    const collReserve = o.deposits[0][0];
    const debtReserve = o.borrows[0][0];
    const collFeed = reserveFeed.get(collReserve.toBase58());
    const debtFeed = reserveFeed.get(debtReserve.toBase58());
    const collAnchor = collFeed !== undefined ? lazerNow.get(collFeed) : undefined;
    const debtAnchor = debtFeed !== undefined ? lazerNow.get(debtFeed) : undefined;
    return new KaminoWatch(
      obligation,
      collReserve,
      debtReserve,
      o.bfAdjustedDebt,
      o.unhealthyBorrowValue,
      collFeed,
      debtFeed,
      collAnchor,
      debtAnchor,
      o.unhealthyBorrowValue > 0.0 && o.bfAdjustedDebt > 0.0,
    );
  }

  /** Borrow-factor-adjusted debt scaled to the current debt-feed price. */
  bfDebt(lazer: LazerPriceMap): number {
    return this.bfDebtStored * ratio(this.debtFeed, this.debtAnchor, lazer);
  }
  /** Liquidation threshold scaled to the current collateral-feed price. */
  unhealthy(lazer: LazerPriceMap): number {
    return this.unhealthyStored * ratio(this.collFeed, this.collAnchor, lazer);
  }
  liquidatable(lazer: LazerPriceMap): boolean {
    return this.complete && this.bfDebt(lazer) >= this.unhealthy(lazer);
  }
  /** bf_debt / unhealthy — >= 1.0 = underwater; how close otherwise. */
  ratioNow(lazer: LazerPriceMap): number {
    const u = this.unhealthy(lazer);
    if (u <= 0.0) return 0.0;
    return this.bfDebt(lazer) / u;
  }

  /**
   * On-chain (Scope) ratio as of the LAST RESCAN — the stored bf_debt /
   * unhealthy with NO Lazer scaling. KLend liquidations settle at the on-chain
   * Scope price, not the Lazer-projected one, so this (not `ratioNow`) is the
   * authoritative gate for spending the expensive quote/sim/submit budget.
   */
  onChainRatio(): number {
    if (this.unhealthyStored <= 0.0) return 0.0;
    return this.bfDebtStored / this.unhealthyStored;
  }
  /** Liquidatable at the on-chain Scope price captured at the last rescan. */
  onChainLiquidatable(): boolean {
    return this.complete && this.bfDebtStored >= this.unhealthyStored && this.unhealthyStored > 0.0;
  }
  /**
   * USD deficit at the on-chain price (bf_debt - threshold); ranking key for the
   * fire tier so the biggest real opportunity wins the capped budget.
   */
  onChainDeficit(): number {
    return this.bfDebtStored - this.unhealthyStored;
  }
  /**
   * True unless a Lazer-mapped side is missing a live price (then the ratio
   * would silently fall back to 1.0 and hide a move — don't trust it).
   */
  feedsReady(lazer: LazerPriceMap): boolean {
    const ok = (f: number | undefined, a: number | undefined): boolean => {
      if (f !== undefined && a !== undefined) return lazer.has(f);
      return true;
    };
    return ok(this.collFeed, this.collAnchor) && ok(this.debtFeed, this.debtAnchor);
  }
}

/** In-memory watch-set, rebuilt on rescan, queried on every Lazer tick. */
export class Engine {
  accounts: KaminoWatch[] = [];
  minDebt: number;
  /**
   * Reject obligations whose ratio exceeds this — an absurd ratio (debt >>
   * threshold) means the collateral is mis-priced near zero (dust / dead
   * feed), never a real opportunity. Without it, deficit-ranking would put
   * these un-fireable accounts FIRST (huge debt - ~0 threshold) and starve
   * the genuine near-threshold ones. Census-proven fix from the old poller.
   */
  ratioCap: number;

  constructor(minDebt: number, ratioCap: number) {
    this.minDebt = minDebt;
    this.ratioCap = ratioCap;
  }

  /**
   * Rebuild from decoded obligations. Keeps v1-shaped, >= min_debt (borrowed
   * market value), complete obligations near threshold (watchRatio <= ratio
   * <= ratioCap at build prices).
   */
  rebuild(obls: Array<[PublicKey, Obligation]>, reserveFeed: ReserveFeedMap, watchRatio: number, lazerNow: LazerPriceMap): number {
    this.accounts = [];
    for (const [pk, o] of obls) {
      if (o.borrowedValue < this.minDebt) continue;
      const w = KaminoWatch.build(o, pk, reserveFeed, lazerNow);
      if (w === null) continue;
      const r = w.ratioNow(lazerNow);
      if (w.complete && r >= watchRatio && r <= this.ratioCap) {
        this.accounts.push(w);
      }
    }
    return this.accounts.length;
  }

  /** Liquidatable obligations (fireRatio <= ratio <= ratioCap) at these prices. */
  crossed(lazer: LazerPriceMap, fireRatio: number): PublicKey[] {
    return this.accounts
      .filter((w) => w.complete && w.feedsReady(lazer))
      .filter((w) => {
        const r = w.ratioNow(lazer);
        return r >= fireRatio && r <= this.ratioCap;
      })
      .map((w) => w.obligation);
  }

  /**
   * Same, ranked by USD deficit (bf_debt - unhealthy) desc — biggest real
   * opportunity first, with the mis-priced-dust tail excluded by ratioCap.
   */
  crossedRanked(lazer: LazerPriceMap, fireRatio: number): Array<[PublicKey, number]> {
    const v: Array<[PublicKey, number]> = [];
    for (const w of this.accounts) {
      if (!(w.complete && w.feedsReady(lazer))) continue;
      const r = w.ratioNow(lazer);
      if (r >= fireRatio && r <= this.ratioCap) {
        v.push([w.obligation, w.bfDebt(lazer) - w.unhealthy(lazer)]);
      }
    }
    v.sort((a, b) => b[1] - a[1]);
    return v;
  }

  /**
   * FIRE tier: obligations liquidatable at the ON-CHAIN Scope price captured at
   * the last rescan (stored health — NOT the Lazer projection), ranked by USD
   * deficit desc, with the mis-priced-dust tail excluded by ratioCap. Only
   * these are worth an expensive Jupiter quote + sim + submit. In a calm market
   * this is near-empty even while the Lazer-projected `crossed` set is large —
   * that gap is exactly the phantom-flag bug this split fixes.
   */
  onChainLiquidatableRanked(): Array<[PublicKey, number]> {
    const v: Array<[PublicKey, number]> = [];
    for (const w of this.accounts) {
      if (w.onChainLiquidatable() && w.onChainRatio() <= this.ratioCap) {
        v.push([w.obligation, w.onChainDeficit()]);
      }
    }
    v.sort((a, b) => b[1] - a[1]);
    return v;
  }

  /** Count of on-chain-liquidatable obligations (for the heartbeat's real-fire tell). */
  onChainLiquidatableCount(): number {
    return this.accounts.filter((w) => w.onChainLiquidatable() && w.onChainRatio() <= this.ratioCap).length;
  }

  /** Look up a watched obligation's reserves (for building the fire/refresh). */
  reservesOf(obligation: PublicKey): [PublicKey, PublicKey] | null {
    const w = this.accounts.find((w) => w.obligation.equals(obligation));
    return w ? [w.collReserve, w.debtReserve] : null;
  }
}

// (tests omitted — see #[cfg(test)] mod tests in kamino_engine.rs for the
// rescan-anchoring, ratio-cap, and on-chain-vs-Lazer-phantom fixtures this
// module is expected to satisfy.)
