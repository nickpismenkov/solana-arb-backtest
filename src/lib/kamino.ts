// Port of src/kamino.rs
//
// Kamino Lend (klend) liquidation finder — Stage 1, read-only.
//
// Unlike marginfi (where we decode Pyth oracles and compute health), Kamino's
// `Obligation` STORES pre-computed USD health values as `Fraction` fixed-point
// (u128, 60 fractional bits). So we read liquidatability straight from the
// obligation — no oracle needed:
//
//     liquidatable  <=>  borrow_factor_adjusted_debt_value >= unhealthy_borrow_value
//
// Caveat: these values are as of the obligation's last on-chain refresh; the
// `stale` flag + last_update.slot tell us how fresh they are. Good enough for a
// finder; a live trigger would re-price via Scope (Kamino's oracle, not Pyth).
//
// All offsets VERIFIED against a real 3344-byte main-market obligation: the
// stored allowed/unhealthy values equal deposited_value x the init/liq LTVs
// (0.80 / 0.90), which only holds if every offset is correct.

import { PublicKey } from '@solana/web3.js';

export const KLEND_PROGRAM = 'KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD';
export const KAMINO_MAIN_MARKET = '7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF';

/** account:Obligation Anchor discriminator, and total account size. */
export const OBLIGATION_DISC = Buffer.from([168, 206, 141, 106, 88, 76, 172, 167]);
export const OBLIGATION_SIZE = 3344;

/** account:Reserve Anchor discriminator, and total account size. */
export const RESERVE_DISC = Buffer.from([43, 242, 204, 202, 26, 247, 59, 127]);
export const RESERVE_SIZE = 8624;

// Kamino `Fraction` = u128 with 60 fractional bits (value / 2^60).
const FRACTION_BITS = 60;

// VERIFIED offsets (offset 0 = 8-byte discriminator).
const O_LAST_UPDATE_SLOT = 16;
const O_STALE = 24;
const O_LENDING_MARKET = 32;
const O_OWNER = 64;
const O_DEPOSITS = 96; // [ObligationCollateral; 8], 136B each
const O_DEPOSITED_VALUE = 1192;
const O_BORROWS = 1208; // [ObligationLiquidity; 5], 200B each
const O_BF_ADJ_DEBT_VALUE = 2208;
const O_BORROWED_VALUE = 2224;
const O_ALLOWED_BORROW_VALUE = 2240;
const O_UNHEALTHY_BORROW_VALUE = 2256;
const O_ELEVATION_GROUP = 2285;
const O_HAS_DEBT = 2287;
const COLLATERAL_STRIDE = 136;
const COLL_DEPOSITED_AMOUNT = 32; // u64 cTokens
const LIQUIDITY_STRIDE = 200;
const LIQ_BORROWED_AMOUNT_SF = 88; // u128 Fraction, native smallest units

function fracAt(data: Buffer, off: number): number {
  // u128 LE -> f64, then / 2^60. Read as two u64 halves to avoid precision
  // loss building a single 128-bit bigint unnecessarily (bigint math is exact
  // here regardless, but this mirrors the Rust u128::from_le_bytes path).
  const lo = data.readBigUInt64LE(off);
  const hi = data.readBigUInt64LE(off + 8);
  const v = (hi << 64n) | lo;
  return Number(v) / 2 ** FRACTION_BITS;
}

function u64At(data: Buffer, off: number): bigint {
  return data.readBigUInt64LE(off);
}

function pubkeyAt(data: Buffer, off: number): PublicKey {
  return new PublicKey(data.subarray(off, off + 32));
}

const ZERO_PUBKEY = new PublicKey(new Uint8Array(32));

/** A decoded Kamino borrower position with its stored USD health values. */
export interface Obligation {
  owner: PublicKey;
  lendingMarket: PublicKey;
  lastUpdateSlot: bigint;
  stale: boolean;
  /** Total deposited collateral value (USD) — what's seizable. */
  depositedValue: number;
  /** Borrow-factor-adjusted debt — the value compared against the threshold. */
  bfAdjustedDebt: number;
  /** Raw borrowed market value (USD). */
  borrowedValue: number;
  /** Borrow allowed at init LTV (USD). */
  allowedBorrowValue: number;
  /** Liquidation threshold (USD) — cross it and you're liquidatable. */
  unhealthyBorrowValue: number;
  /** elevation group (0 = none; nonzero overrides reserve LTV/liq/bf params). */
  elevationGroup: number;
  /** Raw collateral positions: (deposit_reserve, deposited_amount cTokens). */
  deposits: Array<[PublicKey, bigint]>;
  /** Raw debt positions: (borrow_reserve, borrowed_amount native smallest units). */
  borrows: Array<[PublicKey, number]>;
}

/** Decode a klend Obligation account. Mirrors `Obligation::decode`. */
export function decodeObligation(data: Buffer): Obligation | null {
  if (data.length < O_HAS_DEBT + 1 || !data.subarray(0, 8).equals(OBLIGATION_DISC)) {
    return null;
  }
  // Raw positions (skip empty slots = zeroed reserve pubkey).
  const deposits: Array<[PublicKey, bigint]> = [];
  for (let i = 0; i < 8; i++) {
    const base = O_DEPOSITS + i * COLLATERAL_STRIDE;
    const reserve = pubkeyAt(data, base);
    if (reserve.equals(ZERO_PUBKEY)) continue;
    const amt = u64At(data, base + COLL_DEPOSITED_AMOUNT);
    if (amt === 0n) continue;
    deposits.push([reserve, amt]);
  }
  const borrows: Array<[PublicKey, number]> = [];
  for (let i = 0; i < 5; i++) {
    const base = O_BORROWS + i * LIQUIDITY_STRIDE;
    const reserve = pubkeyAt(data, base);
    if (reserve.equals(ZERO_PUBKEY)) continue;
    const amt = fracAt(data, base + LIQ_BORROWED_AMOUNT_SF); // native smallest units
    if (amt === 0.0) continue;
    borrows.push([reserve, amt]);
  }
  return {
    owner: pubkeyAt(data, O_OWNER),
    lendingMarket: pubkeyAt(data, O_LENDING_MARKET),
    lastUpdateSlot: u64At(data, O_LAST_UPDATE_SLOT),
    stale: data[O_STALE] !== 0,
    depositedValue: fracAt(data, O_DEPOSITED_VALUE),
    bfAdjustedDebt: fracAt(data, O_BF_ADJ_DEBT_VALUE),
    borrowedValue: fracAt(data, O_BORROWED_VALUE),
    allowedBorrowValue: fracAt(data, O_ALLOWED_BORROW_VALUE),
    unhealthyBorrowValue: fracAt(data, O_UNHEALTHY_BORROW_VALUE),
    elevationGroup: data[O_ELEVATION_GROUP],
    deposits,
    borrows,
  };
}

/** Kamino's own liquidation gate. */
export function obligationLiquidatable(o: Obligation): boolean {
  return o.bfAdjustedDebt >= o.unhealthyBorrowValue && o.unhealthyBorrowValue > 0.0;
}

/** debt / threshold — >= 1.0 means liquidatable; how close otherwise. */
export function obligationRatio(o: Obligation): number {
  if (o.unhealthyBorrowValue === 0.0) return 0.0;
  return o.bfAdjustedDebt / o.unhealthyBorrowValue;
}

// ── Reserve (8624 bytes) — cached price + params to recompute CURRENT health ──
// Offsets VERIFIED against the mainnet SOL reserve (price $82.20, dec 9).
const R_LAST_UPDATE_SLOT = 16;
const R_STALE = 24;
const R_AVAILABLE_AMOUNT = 224; // u64 native
const R_BORROWED_AMOUNT_SF = 232; // u128 Fraction native
const R_MARKET_PRICE_SF = 248; // u128 Fraction USD/whole-token
const R_MINT_DECIMALS = 272; // u64
const R_ACC_PROTOCOL_FEES_SF = 344;
const R_ACC_REFERRER_FEES_SF = 360;
const R_PENDING_REFERRER_FEES_SF = 376;
const R_COLL_MINT_TOTAL_SUPPLY = 2592; // u64 cToken supply
const R_LTV_PCT = 4872; // u8
const R_LIQ_THRESHOLD_PCT = 4873; // u8
const R_BORROW_FACTOR_PCT = 5008; // u64

/** A reserve's cached price + params needed to value obligation positions. */
export interface Reserve {
  mintDecimals: number;
  /** USD per whole token (from the reserve's cached, refresh_reserve'd oracle). */
  marketPrice: number;
  priceSlot: bigint;
  priceStale: boolean;
  /** underlying liquidity tokens per cToken (>= 1, grows with interest). */
  exchangeRate: number;
  ltvPct: number;
  liqThresholdPct: number;
  borrowFactorPct: bigint;
}

/** Decode a klend Reserve account. Mirrors `Reserve::decode`. */
export function decodeReserve(data: Buffer): Reserve | null {
  if (data.length < R_BORROW_FACTOR_PCT + 8 || !data.subarray(0, 8).equals(RESERVE_DISC)) {
    return null;
  }
  // total_supply() (native units): available + borrowed - fees.
  const totalSupply =
    Number(u64At(data, R_AVAILABLE_AMOUNT)) +
    fracAt(data, R_BORROWED_AMOUNT_SF) -
    fracAt(data, R_ACC_PROTOCOL_FEES_SF) -
    fracAt(data, R_ACC_REFERRER_FEES_SF) -
    fracAt(data, R_PENDING_REFERRER_FEES_SF);
  const ctokenSupply = Number(u64At(data, R_COLL_MINT_TOTAL_SUPPLY));
  const exchangeRate = ctokenSupply > 0.0 && totalSupply > 0.0 ? totalSupply / ctokenSupply : 1.0; // INITIAL_COLLATERAL_RATE
  return {
    mintDecimals: Number(u64At(data, R_MINT_DECIMALS)),
    marketPrice: fracAt(data, R_MARKET_PRICE_SF),
    priceSlot: u64At(data, R_LAST_UPDATE_SLOT),
    priceStale: data[R_STALE] !== 0,
    exchangeRate,
    ltvPct: data[R_LTV_PCT],
    liqThresholdPct: data[R_LIQ_THRESHOLD_PCT],
    borrowFactorPct: u64At(data, R_BORROW_FACTOR_PCT),
  };
}

/** Health recomputed from CURRENT reserve prices (replicates refresh_obligation). */
export interface Recomputed {
  depositedValue: number;
  allowedBorrowValue: number;
  unhealthyBorrowValue: number;
  bfAdjustedDebt: number;
  /** positions whose reserve we couldn't resolve — result is INCOMPLETE if > 0. */
  missing: number;
  /** obligation uses an elevation group -> reserve-config LTV/liq/bf are WRONG here. */
  elevation: boolean;
  /** worst (oldest) reserve-price slot used — how fresh the recompute is. */
  oldestPriceSlot: bigint;
}

export function recomputedLiquidatable(r: Recomputed): boolean {
  return r.bfAdjustedDebt >= r.unhealthyBorrowValue && r.unhealthyBorrowValue > 0.0;
}
export function recomputedRatio(r: Recomputed): number {
  if (r.unhealthyBorrowValue === 0.0) return 0.0;
  return r.bfAdjustedDebt / r.unhealthyBorrowValue;
}
/** Trust only when fully priced and not elevation-group-dependent. */
export function recomputedTrustworthy(r: Recomputed): boolean {
  return r.missing === 0 && !r.elevation;
}

/** Reserves keyed by pubkey (base58) — see conventions.md HashMap<Pubkey,T> rule. */
export type ReserveMap = Map<string, Reserve>;

/**
 * Recompute an obligation's health at current reserve prices. Caveat: uses the
 * stored borrowed_amount without re-accruing interest to the current slot
 * (slightly under-counts debt -> conservative, won't false-positive from this).
 */
export function recompute(ob: Obligation, reserves: ReserveMap): Recomputed {
  let deposited = 0.0;
  let allowed = 0.0;
  let unhealthy = 0.0;
  let bfDebt = 0.0;
  let missing = 0;
  let oldest = -1n; // sentinel for u64::MAX, resolved to 0n at the end if untouched
  let oldestSet = false;
  const trackOldest = (slot: bigint): void => {
    if (!oldestSet || slot < oldest) {
      oldest = slot;
      oldestSet = true;
    }
  };
  for (const [res, camt] of ob.deposits) {
    const r = reserves.get(res.toBase58());
    if (r === undefined) {
      missing += 1;
      continue;
    }
    trackOldest(r.priceSlot);
    const underlying = Number(camt) * r.exchangeRate;
    const val = (underlying * r.marketPrice) / 10 ** r.mintDecimals;
    deposited += val;
    allowed += (val * r.ltvPct) / 100.0;
    unhealthy += (val * r.liqThresholdPct) / 100.0;
  }
  for (const [res, bamt] of ob.borrows) {
    const r = reserves.get(res.toBase58());
    if (r === undefined) {
      missing += 1;
      continue;
    }
    trackOldest(r.priceSlot);
    const val = (bamt / 10 ** r.mintDecimals) * r.marketPrice;
    bfDebt += (val * Number(r.borrowFactorPct)) / 100.0;
  }
  return {
    depositedValue: deposited,
    allowedBorrowValue: allowed,
    unhealthyBorrowValue: unhealthy,
    bfAdjustedDebt: bfDebt,
    missing,
    elevation: ob.elevationGroup !== 0,
    oldestPriceSlot: oldestSet ? oldest : 0n,
  };
}

// (tests omitted — see #[cfg(test)] mod tests in kamino.rs for the two
// health-threshold fixtures this module is expected to satisfy.)
