// Port of src/lazer.rs
//
// Pyth Lazer pre-positioning for the liquidation executors.
//
// Liquidation eligibility is gated by the protocol's ON-CHAIN oracle, so Lazer
// can't make an account liquidatable sooner than the chain sees it. Its value
// is a LEADING signal: Lazer ticks at ms latency, minutes ahead of the
// reserve/bank oracle crank. Blending Lazer prices into the health recompute
// lets the executor ARM (pre-select + keep hot) exactly the accounts about to
// cross the threshold, so when the on-chain oracle catches up the fire tx is
// already built and only needs sign+submit. The FIRE decision itself stays
// gated by full on-chain simulation — Lazer never affects safety, only which
// accounts we spend sim budget on.
//
// Feed ids are Lazer's numeric ids (SOL=6, BTC=1, ETH=2, USDC=7). Only the
// volatile majors matter for arming — a borrower crosses the threshold when
// its volatile collateral drops or volatile debt rises; stables don't move.

import type { BankMap, PriceMap } from './liquidation.js';
import { get, newTable, spawnLazer, type PriceTable } from './pyth.js';

// Lazer numeric feed ids (VERIFIED live in pyth_probe: SOL=6, USDC=7, BTC=1,
// ETH=2; the rest verified against the Lazer symbol registry
// history.pyth-lazer.dourolabs.app/history/v1/symbols).
export const LAZER_SOL = 6;
export const LAZER_BTC = 1;
export const LAZER_ETH = 2;
export const LAZER_USDC = 7;
export const LAZER_USDT = 8;
export const LAZER_BONK = 9;
export const LAZER_WIF = 10;
export const LAZER_PYTH = 3;
// JUP/W exist on Lazer but do NOT support the `real_time` channel — including
// them errors the ENTIRE subscription ("Feeds do not support channel
// real_time: 92, 102", verified live 2026-07-14) and the stream goes dark.
// Their banks stay baseline-priced; do not add them to armFeedIds unless
// the channel support changes or a separate fixed-rate subscription is wired.
export const LAZER_JUP = 92;
export const LAZER_W = 102;

/**
 * Every feed the executors subscribe to and arm on. The list is CENSUS-DRIVEN:
 * a 7-day scan of landed marginfi liquidations (2026-07-14) showed BONK
 * collateral in 91% of them, with PYTH/WIF next among Lazer-covered assets —
 * while the old majors-only list (SOL/BTC/ETH/USDC) missed all of them
 * between 300s rescans. Stables (USDC/USDT) included so stable-debt accounts
 * are fully priced by Lazer. Known gaps (baseline-priced): HNT (no Lazer
 * feed), JUP/W (no real_time channel — see above).
 */
export function armFeedIds(): number[] {
  return [LAZER_SOL, LAZER_BTC, LAZER_ETH, LAZER_USDC, LAZER_USDT, LAZER_BONK, LAZER_WIF, LAZER_PYTH];
}

/**
 * Mint (base58) -> Lazer feed id for assets whose price Lazer leads.
 * SOL-correlated LSTs map to SOL (their on-chain valuation moves with SOL,
 * scaled by the LST exchange rate — see `oneToOneMints` for why that scale
 * matters).
 */
export function mintFeedMap(): Map<string, number> {
  return new Map<string, number>([
    ['So11111111111111111111111111111111111111112', LAZER_SOL], // wSOL
    ['mSoLzYCxHdYgdzU16g5QSh3i5K3z3KZK7ytfqcJm7So', LAZER_SOL], // mSOL
    ['J1toso1uCk3RLmjorhTtrVwY9HJ7X8V9yYac6Y7kGCPn', LAZER_SOL], // jitoSOL
    ['bSo13r4TkiE4KumL71LsHTPpL2euBYLFx6h9HP3piy1', LAZER_SOL], // bSOL
    ['7dHbWXmci3dT8UFYWYZweBLXgycu7Y3iL6trKn1Y7ARj', LAZER_SOL], // stSOL
    ['LSTxxxnJzKDFSLr4dUkPcmCf5VyryEqzPLz5j4bpxFp', LAZER_SOL], // LST (Marinade)
    ['5oVNBeEEQvYi1cX3ir8Dx5n1P7pdxydbGF2X4TxVusJm', LAZER_SOL], // INF
    ['jupSoLaHXQiZZTSfEWMTRRgpnyFm8f6sZdosWBjx93v', LAZER_SOL], // jupSOL
    ['he1iusmfkpAdwvxLNGV8Y1iSbj4rUy6yMhEA3fotn9A', LAZER_SOL], // hSOL (Helius)
    ['EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v', LAZER_USDC], // USDC
    ['Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB', LAZER_USDT], // USDT
    ['cbbtcf3aa214zXHbiAZQwf4122FBYbraNdFqgw4iMij', LAZER_BTC], // cbBTC
    ['3NZ9JMVBmGAqocybic2c7LQCJScmgsAZ6vQqTDzcqmJh', LAZER_BTC], // wBTC (Wormhole)
    ['7vfCXTUXx5WJV5JADk17DUJ4ksgau7utNKj4b963voxs', LAZER_ETH], // wETH (Wormhole)
    ['DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263', LAZER_BONK], // BONK
    ['EKpQGSJtjMFqKZ9KQanSqYXRcF8fBopzLHYxdM65zcjm', LAZER_WIF], // WIF
    ['HZ1JovNiVvGrGNiiYvEozEVgZ58xaU3RKwX8eACQBCt3', LAZER_PYTH], // PYTH
    // JUP/W deliberately unmapped: their feeds aren't in armFeedIds (no
    // real_time channel), and a mapped-but-unsubscribed feed would leave
    // accounts permanently !feeds_ready instead of baseline-priced.
  ]);
}

/**
 * Mints (base58) whose on-chain price IS the feed price (1 token = 1 feed
 * unit). LSTs are deliberately absent: an LST is worth (exchange rate)x SOL —
 * pricing it at the RAW SOL feed undervalues the collateral by 15-35% and
 * makes healthy LST-collateral accounts look deep underwater (the
 * phantom-candidate bug found 2026-07-14). Consumers that substitute the feed
 * price directly (`blend`) must restrict themselves to this set;
 * coefficient-based consumers (liqEngine) anchor-scale mapped banks to the
 * on-chain baseline instead.
 */
export function oneToOneMints(): Set<string> {
  return new Set<string>([
    'So11111111111111111111111111111111111111112', // wSOL
    'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v', // USDC
    'Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB', // USDT
    'cbbtcf3aa214zXHbiAZQwf4122FBYbraNdFqgw4iMij', // cbBTC
    '3NZ9JMVBmGAqocybic2c7LQCJScmgsAZ6vQqTDzcqmJh', // wBTC
    '7vfCXTUXx5WJV5JADk17DUJ4ksgau7utNKj4b963voxs', // wETH
    'DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263', // BONK
    'EKpQGSJtjMFqKZ9KQanSqYXRcF8fBopzLHYxdM65zcjm', // WIF
    'HZ1JovNiVvGrGNiiYvEozEVgZ58xaU3RKwX8eACQBCt3', // PYTH
  ]);
}

/**
 * Blend Lazer prices over an on-chain baseline: start from the on-chain
 * PriceMap (authoritative for anything Lazer doesn't cover) and override a
 * bank's price with the fresher Lazer tick when we have both a mint mapping
 * and a recent tick. Only 1:1 mints are overridden — an LST priced at the raw
 * SOL feed would be undervalued by its exchange rate, so LST banks keep the
 * on-chain baseline here (the tick-driven engine anchor-scales them instead).
 * Returns the blended map + how many banks Lazer led.
 */
export function blend(
  banks: BankMap,
  onChain: PriceMap,
  table: PriceTable,
  map: Map<string, number>,
): [PriceMap, number] {
  const direct = oneToOneMints();
  const out: PriceMap = new Map(onChain);
  let led = 0;
  for (const [bankPk, bank] of banks) {
    const mintKey = bank.mint.toBase58();
    if (!direct.has(mintKey)) continue;
    const feed = map.get(mintKey);
    if (feed === undefined) continue;
    const p = get(table, feed);
    if (p === undefined) continue;
    out.set(bankPk, p.price);
    led += 1;
  }
  return [out, led];
}

/**
 * Spawn the Lazer WS feed, writing into `table`. Returns immediately; the
 * caller's event loop keeps the process alive while the background task runs.
 */
export function spawnLazerThread(token: string, feedIds: number[], table: PriceTable): void {
  spawnLazer(token, feedIds, table);
}

/** Compact log line describing which Lazer majors are live (for boot output). */
export function status(table: PriceTable): string {
  const f = (id: number, name: string): string | undefined => {
    const p = get(table, id);
    return p === undefined ? undefined : `${name}=$${p.price.toFixed(2)}`;
  };
  const g = (id: number, name: string): string | undefined => {
    const p = get(table, id);
    return p === undefined ? undefined : `${name}=$${p.price.toFixed(6)}`;
  };
  return [f(LAZER_SOL, 'SOL'), f(LAZER_BTC, 'BTC'), f(LAZER_ETH, 'ETH'), f(LAZER_USDC, 'USDC'), g(LAZER_BONK, 'BONK'), f(LAZER_JUP, 'JUP')]
    .filter((x): x is string => x !== undefined)
    .join(' ');
}
