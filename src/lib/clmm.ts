// Port of src/clmm.rs
//
// In-memory concentrated-liquidity (Uniswap-v3-style) math for Orca Whirlpool
// and Raydium CLMM. Given a pool's current sqrtPrice + active liquidity, apply
// a swap in closed form (WITHIN the current tick — constant liquidity) to get
// the exact output and post-swap price, then compute the optimal cross-venue
// arb size and its exact profit. Pure arithmetic — hot-path safe.
//
// LIMITATION: assumes the swap stays inside the current tick's liquidity range
// (no tick crossing). Valid for small/moderate sizes on deep pools; large
// sizes need tick-array liquidity (Stage 1b). clmm_probe verifies where the
// within-tick assumption holds vs real on-chain quotes.

import { PublicKey } from '@solana/web3.js';

/**
 * Normalized pool state for swap math. sqrtP is the raw Q64.64 sqrt price as
 * a float (sqrt of token1/token0 in raw base units); liquidity is raw L.
 */
export interface ClmmState {
  sqrtP: number;
  liquidity: number;
  mint0: PublicKey;
  mint1: PublicKey;
  dec0: number;
  dec1: number;
  feeBps: number;
}

function u128LE(d: Uint8Array, o: number): bigint {
  const view = new DataView(d.buffer, d.byteOffset, d.byteLength);
  const lo = view.getBigUint64(o, true);
  const hi = view.getBigUint64(o + 8, true);
  return (hi << 64n) | lo;
}
function pkAt(d: Uint8Array, o: number): PublicKey {
  return new PublicKey(d.subarray(o, o + 32));
}

/**
 * Orca Whirlpool: liquidity u128@49, sqrtPrice u128@65, mintA@101, mintB@181.
 * Decimals come from config (caller passes base/quote decimals by mint).
 */
export function clmmFromOrca(d: Uint8Array, decA: number, decB: number, feeBps: number): ClmmState | undefined {
  if (d.length < 213) return undefined;
  return {
    liquidity: Number(u128LE(d, 49)),
    sqrtP: Number(u128LE(d, 65)) / 2 ** 64,
    mint0: pkAt(d, 101),
    mint1: pkAt(d, 181),
    dec0: decA,
    dec1: decB,
    feeBps,
  };
}

/**
 * Raydium CLMM: mint0@73, mint1@105, decimals@233/234, liquidity u128@237,
 * sqrtPriceX64 u128@253.
 */
export function clmmFromRay(d: Uint8Array, feeBps: number): ClmmState | undefined {
  if (d.length < 269) return undefined;
  return {
    liquidity: Number(u128LE(d, 237)),
    sqrtP: Number(u128LE(d, 253)) / 2 ** 64,
    mint0: pkAt(d, 73),
    mint1: pkAt(d, 105),
    dec0: d[233],
    dec1: d[234],
    feeBps,
  };
}

/**
 * Apply a swap of `amountIn` raw units of the input token (token0 if
 * `zeroForOne`, else token1). Returns raw `amountOut` of the other token.
 * Within-tick closed form. Fee taken off the input first.
 */
export function applySwap(s: ClmmState, zeroForOne: boolean, amountIn: number): number {
  if (amountIn <= 0.0 || s.liquidity <= 0.0) return 0.0;
  const amt = amountIn * (1.0 - s.feeBps / 1e4);
  const l = s.liquidity;
  const sp = s.sqrtP;
  if (zeroForOne) {
    // token0 in → price decreases. 1/sp_new = 1/sp + amt/L
    const spNew = (l * sp) / (l + amt * sp);
    return l * (sp - spNew); // token1 out
  } else {
    // token1 in → price increases. sp_new = sp + amt/L
    const spNew = sp + amt / l;
    return l * (1.0 / sp - 1.0 / spNew); // token0 out
  }
}

/**
 * Post-swap copy (mutates sqrtP as if `amountIn` were swapped). Used to
 * predict a victim's effect before building our arb.
 */
export function afterSwap(s: ClmmState, zeroForOne: boolean, amountIn: number): ClmmState {
  const out: ClmmState = { ...s };
  if (amountIn > 0.0 && s.liquidity > 0.0) {
    const amt = amountIn * (1.0 - s.feeBps / 1e4);
    out.sqrtP = zeroForOne
      ? (s.liquidity * s.sqrtP) / (s.liquidity + amt * s.sqrtP)
      : s.sqrtP + amt / s.liquidity;
  }
  return out;
}

/**
 * Apply a decoded victim swap: `sellBase` = the victim sells base (base is
 * input, price of base falls); else the victim buys base (quote is input).
 * `amountInRaw` is the exact-in amount in the input token's raw units.
 */
export function afterBaseSwap(s: ClmmState, base: PublicKey, sellBase: boolean, amountInRaw: number): ClmmState {
  const baseIs0 = s.mint0.equals(base);
  // sellBase → base is input; zeroForOne iff base is token0.
  // buyBase  → quote is input; zeroForOne iff quote is token0 (= !baseIs0).
  const zeroForOne = sellBase ? baseIs0 : !baseIs0;
  return afterSwap(s, zeroForOne, amountInRaw);
}

/** UI price of token0 in token1 (e.g. USDC per SOL if token0=SOL). */
export function uiPrice(s: ClmmState): number {
  return s.sqrtP * s.sqrtP * 10 ** (s.dec0 - s.dec1);
}

/**
 * Simulate the full round trip for a given USDC borrow amount (raw USDC units):
 * buy base on `buy` pool, sell it on `sell` pool. Returns net USDC profit (raw,
 * can be negative). `base` = the base mint (e.g. wSOL). Both pools must be the
 * same token pair.
 */
export function roundTripProfit(buy: ClmmState, sell: ClmmState, base: PublicKey, borrowUsdc: number): number {
  // Buy base with USDC on `buy`: input is USDC.
  const usdcIs0Buy = !buy.mint0.equals(base);
  const baseOut = applySwap(buy, usdcIs0Buy, borrowUsdc);
  if (baseOut <= 0.0) return -borrowUsdc;
  // Sell that base for USDC on `sell`: input is base.
  const baseIs0Sell = sell.mint0.equals(base);
  const usdcOut = applySwap(sell, baseIs0Sell, baseOut);
  return usdcOut - borrowUsdc;
}

/**
 * Best cross-venue arb between two pools. Returns [optimal borrow raw USDC,
 * net profit raw USDC, buyASellB]. `buyASellB = true` means buy base on
 * `a`, sell on `b`; false is the reverse. Ternary search over the concave
 * profit curve; `maxUsdc` caps the search.
 */
export function optimalArb(a: ClmmState, b: ClmmState, base: PublicKey, maxUsdc: number): [number, number, boolean] {
  const opt = (buy: ClmmState, sell: ClmmState): [number, number] => {
    let lo = 0.0;
    let hi = maxUsdc;
    for (let i = 0; i < 60; i++) {
      const m1 = lo + (hi - lo) / 3.0;
      const m2 = hi - (hi - lo) / 3.0;
      if (roundTripProfit(buy, sell, base, m1) < roundTripProfit(buy, sell, base, m2)) {
        lo = m1;
      } else {
        hi = m2;
      }
    }
    const size = (lo + hi) / 2.0;
    return [size, roundTripProfit(buy, sell, base, size)];
  };
  const [s1, p1] = opt(a, b);
  const [s2, p2] = opt(b, a);
  if (p1 >= p2) return [s1, p1, true];
  return [s2, p2, false];
}

export function wsol(): PublicKey {
  return new PublicKey('So11111111111111111111111111111111111111112');
}
