// Port of src/liq_engine.rs
//
// Event-driven liquidation engine: an in-memory watch-set that recomputes
// account health on every Pyth Lazer tick with ZERO RPC in the hot path,
// replacing the 5-second poll as the trigger.
//
// marginfi maintenance health is LINEAR in each bank's price:
//   weightedAssets      = sum_b (assetShares*assetShareValue/scale*wMaintA)*price_b
//   weightedLiabilities = sum_b (liabShares *liabShareValue /scale*wMaintL)*price_b
// so per account we precompute, once per rescan, the price COEFFICIENTS and
// split them by whether the bank's price comes from a Lazer feed (fast) or the
// on-chain baseline (slow-moving). A tick then costs O(#mapped feeds) per
// account — a few multiply-adds — instead of an RPC round-trip. The result is
// bit-for-bit the same number as liquidation.maintenanceHealth (unit-tested).

import { PublicKey } from '@solana/web3.js';
import { type Bank, type BankMap, type Health, healthRatio, type MarginfiAccount, type PriceMap, effectiveAssetWeightMaint } from './liquidation.js';

/**
 * One account, reduced to price coefficients. `const*` fold in the banks
 * priced off the on-chain baseline (captured at build time); `feed*` are the
 * per-Lazer-feed coefficients applied to live tick prices.
 */
export class WatchAccount {
  pubkey: PublicKey;
  private constA: number;
  private constL: number;
  private feedA: Map<number, number>;
  private feedL: Map<number, number>;
  /** Total weighted assets at build-time prices — for the min-collateral gate. */
  weightedAssetsSnapshot: number;
  /**
   * A balance whose bank or baseline price was unresolved -> health
   * INCOMPLETE, never trusted for a fire decision (mirrors
   * maintenanceHealth's `missing`).
   */
  complete: boolean;

  private constructor(
    pubkey: PublicKey,
    constA: number,
    constL: number,
    feedA: Map<number, number>,
    feedL: Map<number, number>,
    weightedAssetsSnapshot: number,
    complete: boolean,
  ) {
    this.pubkey = pubkey;
    this.constA = constA;
    this.constL = constL;
    this.feedA = feedA;
    this.feedL = feedL;
    this.weightedAssetsSnapshot = weightedAssetsSnapshot;
    this.complete = complete;
  }

  /**
   * Build the coefficient form. `baseline` = on-chain prices (authoritative
   * for anything not on a Lazer feed); `mintFeed` maps a bank's mint to its
   * Lazer feed id; `direct` is the subset of mints priced 1:1 by their feed
   * (everything else feed-mapped is anchor-scaled to baseline — see below).
   * `lazerNow` are the Lazer prices at build time, used to anchor non-1:1
   * banks and to seed weightedAssetsSnapshot consistently.
   */
  static build(
    acct: MarginfiAccount,
    banks: BankMap,
    baseline: PriceMap,
    mintFeed: Map<string, number>,
    direct: Set<string>,
    lazerNow: Map<number, number>,
  ): WatchAccount {
    let constA = 0.0;
    let constL = 0.0;
    const feedA = new Map<number, number>();
    const feedL = new Map<number, number>();
    let complete = true;
    // Liability banks for the emode intersection rule (matches maintenanceHealth).
    const liabBanks: Bank[] = acct.balances
      .filter((b) => b.liabilityShares > 0.0)
      .map((b) => banks.get(b.bankPk.toBase58()))
      .filter((b): b is Bank => b !== undefined);

    for (const b of acct.balances) {
      const bank = banks.get(b.bankPk.toBase58());
      if (bank === undefined) {
        if (b.assetShares > 0.0 || b.liabilityShares > 0.0) complete = false;
        continue;
      }
      const scale = 10 ** bank.mintDecimals;
      const coefA = b.assetShares > 0.0 ? (b.assetShares * bank.assetShareValue) / scale * effectiveAssetWeightMaint(bank, liabBanks) : 0.0;
      const coefL = b.liabilityShares > 0.0 ? (b.liabilityShares * bank.liabilityShareValue) / scale * bank.liabilityWeightMaint : 0.0;
      if (coefA === 0.0 && coefL === 0.0) continue;

      const mintKey = bank.mint.toBase58();
      const bankKey = b.bankPk.toBase58();
      const feed = mintFeed.get(mintKey);
      if (feed !== undefined && direct.has(mintKey)) {
        // 1:1 bank (its on-chain price IS the feed's asset): raw feed
        // coefficient. The feed's absolute level is the true price, so the
        // engine deliberately LEADS a stale on-chain oracle — the crank
        // edge depends on this.
        feedA.set(feed, (feedA.get(feed) ?? 0) + coefA);
        feedL.set(feed, (feedL.get(feed) ?? 0) + coefL);
      } else if (feed !== undefined) {
        // Feed-mapped but NOT 1:1 (LSTs -> SOL feed): anchor-scale to the on-
        // chain baseline. k = baseline/feed makes the bank's effective price
        // equal its oracle price at build time and track the feed's relative
        // movement afterwards. Unscaled, an LST reads 15-35% under its real
        // value (the raw SOL price) and healthy accounts look deep
        // underwater — the phantom-candidate bug. Without a live feed price
        // to anchor against, fold the baseline into the const part (correct
        // level, just not tick-driven until the next rescan).
        const bp = baseline.get(bankKey);
        const lp = lazerNow.get(feed);
        if (bp !== undefined && lp !== undefined && lp > 0.0) {
          const k = bp / lp;
          feedA.set(feed, (feedA.get(feed) ?? 0) + coefA * k);
          feedL.set(feed, (feedL.get(feed) ?? 0) + coefL * k);
        } else if (bp !== undefined) {
          constA += coefA * bp;
          constL += coefL * bp;
        } else {
          complete = false;
        }
      } else {
        // Bank priced off the on-chain baseline: fold its price in now.
        const price = baseline.get(bankKey);
        if (price !== undefined) {
          constA += coefA * price;
          constL += coefL * price;
        } else {
          complete = false;
        }
      }
    }
    // Snapshot weighted assets at build-time prices (baseline already folded;
    // add the Lazer-priced part at current Lazer prices).
    let wa = constA;
    for (const [feed, ca] of feedA) {
      wa += ca * (lazerNow.get(feed) ?? 0.0);
    }
    return new WatchAccount(
      acct.authority, // display only; keyed externally by account pk
      constA,
      constL,
      feedA,
      feedL,
      wa,
      complete,
    );
  }

  /**
   * Health at the given Lazer prices (baseline banks are already folded). A
   * feed with no live price falls back to 0 -> treated as unresolved, so
   * callers should only trust `complete` accounts with all feeds present.
   */
  health(lazer: Map<number, number>): Health {
    let wa = this.constA;
    let wl = this.constL;
    for (const [feed, ca] of this.feedA) wa += ca * (lazer.get(feed) ?? 0.0);
    for (const [feed, cl] of this.feedL) wl += cl * (lazer.get(feed) ?? 0.0);
    return { weightedAssets: wa, weightedLiabilities: wl };
  }

  /**
   * True once we have a live Lazer price for every feed this account depends
   * on (else the linear health is missing a term and must not be trusted).
   */
  feedsReady(lazer: Map<number, number>): boolean {
    for (const f of this.feedA.keys()) if (!lazer.has(f)) return false;
    for (const f of this.feedL.keys()) if (!lazer.has(f)) return false;
    return true;
  }
}

/** The in-memory watch-set. Rebuilt on rescan; queried on every tick. */
export class Engine {
  accounts: Array<[PublicKey, WatchAccount]> = [];
  minCollateral: number;

  constructor(minCollateral: number) {
    this.minCollateral = minCollateral;
  }

  /**
   * Rebuild from decoded accounts. `watchRatio` pre-filters to accounts
   * already near threshold (at build-time prices) so ticks evaluate a small
   * hot set. Returns how many were kept.
   */
  rebuild(
    accts: Array<[PublicKey, MarginfiAccount]>,
    banks: BankMap,
    baseline: PriceMap,
    mintFeed: Map<string, number>,
    direct: Set<string>,
    lazerNow: Map<number, number>,
    watchRatio: number,
  ): number {
    this.accounts = [];
    for (const [pkey, a] of accts) {
      const wa = WatchAccount.build(a, banks, baseline, mintFeed, direct, lazerNow);
      if (!wa.complete || wa.weightedAssetsSnapshot < this.minCollateral) continue;
      const h = wa.health(lazerNow);
      if (healthRatio(h) >= watchRatio) this.accounts.push([pkey, wa]);
    }
    return this.accounts.length;
  }

  /**
   * Accounts that are liquidatable (ratio >= fireRatio, health complete, all
   * feeds priced) at the given Lazer prices. Pure arithmetic, no RPC.
   */
  crossed(lazer: Map<number, number>, fireRatio: number): PublicKey[] {
    const out: PublicKey[] = [];
    for (const [pkey, wa] of this.accounts) {
      if (wa.complete && wa.feedsReady(lazer) && healthRatio(wa.health(lazer)) >= fireRatio) out.push(pkey);
    }
    return out;
  }

  /**
   * Same set as `crossed`, but each paired with a priority score (the USD
   * deficit = weightedLiabilities - weightedAssets) and sorted most-urgent
   * first. A larger score means deeper underwater / bigger position, so the
   * caller can cap per-cycle sim work to the top-K without starving the
   * biggest real opportunities. For the arm-set (score < 0, not yet crossed)
   * this ranks the accounts closest to crossing first.
   */
  crossedRanked(lazer: Map<number, number>, fireRatio: number): Array<[PublicKey, number]> {
    const v: Array<[PublicKey, number]> = [];
    for (const [pkey, wa] of this.accounts) {
      if (!(wa.complete && wa.feedsReady(lazer))) continue;
      const h = wa.health(lazer);
      if (healthRatio(h) >= fireRatio) v.push([pkey, h.weightedLiabilities - h.weightedAssets]);
    }
    v.sort((a, b) => b[1] - a[1]);
    return v;
  }
}
