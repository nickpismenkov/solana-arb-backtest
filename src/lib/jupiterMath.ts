// Port of src/jupiter_math.rs
//
// Fluid (Jupiter Lend) tick<->price math + liquidate account-selection, reversed
// from the on-chain program source (code-423n4/2026-02-jupiter-lend) and the
// published TS SDK. Everything here is a line-for-line port of the authoritative
// Rust/TS, so the on-chain program is the spec.
//
// WHAT THIS SOLVES (the two pieces that kept `tryArm` returning undefined):
//
// 1. `colPerUnitDebt` — the liquidate ix's collateral-per-unit-debt arg.
//    HONEST FRAMING (verified against 8 real mainnet liquidate txs, see
//    jupiterFireProbe): this arg is NOT the price. It is a *minimum acceptable
//    collateral-per-debt slippage floor in 1e15 decimals*. The program computes
//    the ACTUAL price itself from the vault oracle (`get_ticks_from_oracle_price`)
//    and only reverts (VaultExcessSlippageLiquidation) if the realized
//    `actual_col*1e15/actual_debt < col_per_unit_debt`. Real liquidators pass
//    either 0 (accept the oracle price — seen in 2/8 txs) or a computed floor
//    (~1.1e13-1.5e13 — seen in the other 6). We reproduce the exact on-chain
//    formula in `computeColPerDebt`, and the executor sources the live value
//    from the program's own resolver revert (to=ADDRESS_DEAD ->
//    `VaultLiquidationResult:[col,debt,tick]`), which is exact ground truth.
//
// 2. `remainingAccountsIndices` + the tick/branch account set — layout
//    `[oracleSources, branches, ticks, tickHasDebt]` (verified: sums to the
//    remaining-account count on all 8 real txs). The PDAs and the selection rule
//    are ported from the SDK's `getRemainingAccountsLiquidate`.
//
// PRECISION NOTE: the Rust source hand-rolls 256-bit intermediates
// (`mul_u128_wide` + `div_256_by_128`) because u128 * u128 can overflow u128.
// JS `bigint` is arbitrary-precision, so `mulDivFloor`/`mulDivCeil` below use
// plain bigint arithmetic directly — the result is bit-identical to the Rust
// 256-bit path, just without needing the manual wide-multiply/long-division.

import { PublicKey } from '@solana/web3.js';

// ── mul_div (matches on-chain `safe_multiply_divide` semantics) ──────────────
// Fluid's ratio math multiplies two u128s (product can exceed u128) then divides
// by a u128. bigint handles the wide product natively.

/** floor((a * b) / d). Throws only on d==0n (callers guard). */
export function mulDivFloor(a: bigint, b: bigint, d: bigint): bigint {
  return (a * b) / d;
}

/** ceil((a * b) / d). */
export function mulDivCeil(a: bigint, b: bigint, d: bigint): bigint {
  const prod = a * b;
  const q = prod / d;
  const r = prod % d;
  return r !== 0n ? q + 1n : q;
}

// ── TickMath: ratioX48 = (1.0015^tick) * 2^48 ────────────────────────────────
// Exact port of crates/library/src/math/tick.rs.
export class TickMath {
  static readonly MIN_TICK = -16383;
  static readonly MAX_TICK = 16383;
  static readonly TICK_SPACING = 10015n;
  static readonly COLD_TICK = -2147483648; // i32::MIN

  private static readonly FACTOR00 = 0x10000000000000000n;
  private static readonly FACTOR01 = 0xff9dd7de423466c2n;
  private static readonly FACTOR02 = 0xff3bd55f4488ad27n;
  private static readonly FACTOR03 = 0xfe78410fd6498b74n;
  private static readonly FACTOR04 = 0xfcf2d9987c9be179n;
  private static readonly FACTOR05 = 0xf9ef02c4529258b0n;
  private static readonly FACTOR06 = 0xf402d288133a85a1n;
  private static readonly FACTOR07 = 0xe895615b5beb6386n;
  private static readonly FACTOR08 = 0xd34f17a00ffa00a8n;
  private static readonly FACTOR09 = 0xae6b7961714e2055n;
  private static readonly FACTOR10 = 0x76d6461f27082d75n;
  private static readonly FACTOR11 = 0x372a3bfe0745d8b7n;
  private static readonly FACTOR12 = 0xbe32cbee4897976n;
  private static readonly FACTOR13 = 0x8d4f70c9ff4925n;
  private static readonly FACTOR14 = 0x4e009ae55194n;

  static readonly MIN_RATIOX48 = 6093n;
  static readonly MAX_RATIOX48 = 13002088133096036565414295n;
  static readonly ZERO_TICK_SCALED_RATIO = 0x1000000000000n; // 1 << 48
  private static readonly _1E13 = 10000000000000n;
  private static readonly U128_MAX = (1n << 128n) - 1n;

  private static mulShift64(n0: bigint, n1: bigint): bigint {
    // n0,n1 < 2^64 across the whole calc, so n0*n1 < 2^128 fits u128.
    return (n0 * n1) >> 64n;
  }

  static getRatioAtTick(tick: number): bigint | undefined {
    if (tick < TickMath.MIN_TICK || tick > TickMath.MAX_TICK) {
      return undefined;
    }
    const absTick = Math.abs(tick);
    let factor = TickMath.FACTOR00;
    if (absTick & 0x1) factor = TickMath.FACTOR01;
    if (absTick & 0x2) factor = TickMath.mulShift64(factor, TickMath.FACTOR02);
    if (absTick & 0x4) factor = TickMath.mulShift64(factor, TickMath.FACTOR03);
    if (absTick & 0x8) factor = TickMath.mulShift64(factor, TickMath.FACTOR04);
    if (absTick & 0x10) factor = TickMath.mulShift64(factor, TickMath.FACTOR05);
    if (absTick & 0x20) factor = TickMath.mulShift64(factor, TickMath.FACTOR06);
    if (absTick & 0x40) factor = TickMath.mulShift64(factor, TickMath.FACTOR07);
    if (absTick & 0x80) factor = TickMath.mulShift64(factor, TickMath.FACTOR08);
    if (absTick & 0x100) factor = TickMath.mulShift64(factor, TickMath.FACTOR09);
    if (absTick & 0x200) factor = TickMath.mulShift64(factor, TickMath.FACTOR10);
    if (absTick & 0x400) factor = TickMath.mulShift64(factor, TickMath.FACTOR11);
    if (absTick & 0x800) factor = TickMath.mulShift64(factor, TickMath.FACTOR12);
    if (absTick & 0x1000) factor = TickMath.mulShift64(factor, TickMath.FACTOR13);
    if (absTick & 0x2000) factor = TickMath.mulShift64(factor, TickMath.FACTOR14);

    let precision = 0n;
    if (tick >= 0) {
      factor = TickMath.U128_MAX / factor;
      if (factor % 0x10000n !== 0n) precision = 1n;
    }
    return (factor >> 16n) + precision;
  }

  /** Returns [tick, perfectRatioX48]. Mirrors on-chain get_tick_at_ratio. */
  static getTickAtRatio(ratioX48: bigint): [number, bigint] | undefined {
    if (ratioX48 < TickMath.MIN_RATIOX48 || ratioX48 > TickMath.MAX_RATIOX48) {
      return undefined;
    }
    const isNegative = ratioX48 < TickMath.ZERO_TICK_SCALED_RATIO;
    let factor = isNegative
      ? mulDivFloor(TickMath.ZERO_TICK_SCALED_RATIO, TickMath._1E13, ratioX48)
      : mulDivFloor(ratioX48, TickMath._1E13, TickMath.ZERO_TICK_SCALED_RATIO);
    let tick = 0;
    const steps: [bigint, number][] = [
      [2150859953785115391n, 0x2000],
      [4637736467054931n, 0x1000],
      [215354044936586n, 0x800],
      [46406254420777n, 0x400],
      [21542110950596n, 0x200],
      [14677230989051n, 0x100],
      [12114962232319n, 0x80],
      [11006798913544n, 0x40],
      [10491329235871n, 0x20],
      [10242718992470n, 0x10],
      [10120631893548n, 0x8],
      [10060135135051n, 0x4],
      [10030022500000n, 0x2],
    ];
    for (const [thresh, bit] of steps) {
      if (factor >= thresh) {
        tick |= bit;
        factor = mulDivFloor(factor, TickMath._1E13, thresh);
      }
    }
    // last step (2^0 = 1) does not divide-through in the reference for the
    // perfect-ratio branch, matching upstream.
    if (factor >= 10015000000000n) {
      tick |= 0x1;
      factor = mulDivFloor(factor, TickMath._1E13, 10015000000000n);
    }

    let perfect: bigint;
    if (isNegative) {
      tick = ~tick;
      perfect = mulDivFloor(ratioX48, factor, 10015000000000n);
    } else {
      perfect = mulDivFloor(ratioX48, TickMath._1E13, factor);
    }
    if (perfect > ratioX48) {
      return undefined;
    }
    return [tick, perfect];
  }
}

// ── colPerDebt (get_ticks_from_oracle_price) ─────────────────────────────────
export const RATE_OUTPUT_DECIMALS = 15;
export const FOUR_DECIMALS = 10_000n;
export const THREE_DECIMALS = 1_000n;

const POW10_15 = 10n ** 15n;
const POW10_24 = 10n ** 24n;
const POW10_26 = 10n ** 26n;
const POW10_30 = 10n ** 30n; // 10^(RATE_OUTPUT_DECIMALS * 2)
const POW10_8 = 10n ** 8n;

/**
 * Exact port of `get_ticks_from_oracle_price` (utils/liquidate.rs).
 *
 * Inputs (all as the program sees them):
 * - `exchangeRate`: oracle liquidate exchange rate, 1e15 (RATE_OUTPUT_DECIMALS)
 * - `supplyExPrice`, `borrowExPrice`: vault exchange prices (1e12 precision)
 * - `liquidationPenalty`: VaultConfig raw (1e4 = 100%, e.g. 100 = 1%)
 * - `liquidationThreshold`, `liquidationMaxLimit`: VaultConfig raw (1e3 = 100%)
 *
 * Returns [colPerDebt, liquidationTick, maxTick] where `colPerDebt` is the
 * 1e15-scaled collateral-per-debt (already including the liquidation penalty)
 * — i.e. the natural, protective value for the `colPerUnitDebt` arg.
 */
export function computeColPerDebt(
  exchangeRate: bigint,
  supplyExPrice: bigint,
  borrowExPrice: bigint,
  liquidationPenalty: number,
  liquidationThreshold: number,
  liquidationMaxLimit: number,
): [bigint, number, number] | undefined {
  if (exchangeRate === 0n || exchangeRate > POW10_24 || borrowExPrice === 0n) {
    return undefined;
  }
  let debtPerCol = mulDivFloor(exchangeRate, supplyExPrice, borrowExPrice);
  if (debtPerCol === 0n) {
    return undefined;
  }
  if (debtPerCol > POW10_26) {
    debtPerCol = POW10_26;
  }
  let debtPerColCeil = mulDivCeil(exchangeRate, supplyExPrice, borrowExPrice);
  debtPerColCeil = debtPerColCeil < POW10_26 ? debtPerColCeil : POW10_26;
  debtPerColCeil = debtPerColCeil > 1n ? debtPerColCeil : 1n;

  const rawColPerDebt = POW10_30 / debtPerColCeil;
  const colPerDebt = (rawColPerDebt * (FOUR_DECIMALS + BigInt(liquidationPenalty))) / FOUR_DECIMALS;

  const liquidationRatio = mulDivFloor(debtPerCol, TickMath.ZERO_TICK_SCALED_RATIO, POW10_15);
  const thresholdRatio = (liquidationRatio * BigInt(liquidationThreshold)) / THREE_DECIMALS;
  const liqResult = TickMath.getTickAtRatio(thresholdRatio);
  if (liqResult === undefined) return undefined;
  const liquidationTick = liqResult[0];
  const maxRatio = (liquidationRatio * BigInt(liquidationMaxLimit)) / THREE_DECIMALS;
  const maxResult = TickMath.getTickAtRatio(maxRatio);
  if (maxResult === undefined) return undefined;
  const maxTick = maxResult[0];

  return [colPerDebt, liquidationTick, maxTick];
}

/**
 * SDK-style liquidation tick straight from an oracle price given in 1e8 (used by
 * `getRemainingAccountsLiquidate` for account selection):
 *   liquidationRatio       = price * 2^48 / 1e8
 *   liquidationThreshold   = liquidationRatio * liquidation_threshold / 1e3
 *   liquidationTick        = getTickAtRatio(liquidationThreshold)
 */
export function liquidationTickFromPrice1e8(price1e8: bigint, liquidationThreshold: number): number | undefined {
  const liqRatio = mulDivFloor(price1e8, TickMath.ZERO_TICK_SCALED_RATIO, POW10_8);
  const thr = (liqRatio * BigInt(liquidationThreshold)) / THREE_DECIMALS;
  const result = TickMath.getTickAtRatio(thr);
  return result?.[0];
}

/**
 * Recover the liquidation tick from a `colPerUnitDebt` value a real
 * liquidator passed (~ the on-chain `colPerDebt`, which already includes the
 * penalty), inverting `computeColPerDebt`. Lets us reconstruct the tick band
 * for account selection without the oracle when a live price isn't handy (used
 * by the probe; production uses `liquidationTickFromPrice1e8` off Lazer).
 */
export function liquidationTickFromColPerDebt(colPerUnitDebt: bigint, liquidationPenalty: number, liquidationThreshold: number): number | undefined {
  if (colPerUnitDebt === 0n) return undefined;
  const rawColPerDebt = (colPerUnitDebt * FOUR_DECIMALS) / (FOUR_DECIMALS + BigInt(liquidationPenalty));
  if (rawColPerDebt === 0n) return undefined;
  const debtPerCol = POW10_30 / rawColPerDebt;
  const liqRatio = mulDivFloor(debtPerCol, TickMath.ZERO_TICK_SCALED_RATIO, POW10_15);
  const thr = (liqRatio * BigInt(liquidationThreshold)) / THREE_DECIMALS;
  const result = TickMath.getTickAtRatio(thr);
  return result?.[0];
}

// ── PDA derivations (seeds from programs/vaults/src/state/seeds.rs) ──────────
export const VAULTS_PROGRAM_ID = 'jupr81YtYssSyPt8jbnGuiWon5f6x9TcDEFxYe3Bdzi';

function program(): PublicKey {
  return new PublicKey(VAULTS_PROGRAM_ID);
}

function u16leBytes(n: number): Buffer {
  const b = Buffer.alloc(2);
  b.writeUInt16LE(n, 0);
  return b;
}
function u32leBytes(n: number): Buffer {
  const b = Buffer.alloc(4);
  b.writeUInt32LE(n, 0);
  return b;
}

export function branchPda(vaultId: number, branchId: number): PublicKey {
  return PublicKey.findProgramAddressSync([Buffer.from('branch'), u16leBytes(vaultId), u32leBytes(branchId)], program())[0];
}

export function tickPda(vaultId: number, tick: number): PublicKey {
  // SDK: tick + MAX_TICK, encoded as u32 le (4 bytes).
  const key = (tick + TickMath.MAX_TICK) >>> 0;
  return PublicKey.findProgramAddressSync([Buffer.from('tick'), u16leBytes(vaultId), u32leBytes(key)], program())[0];
}

export function tickHasDebtPda(vaultId: number, index: number): PublicKey {
  return PublicKey.findProgramAddressSync([Buffer.from('tick_has_debt'), u16leBytes(vaultId), Buffer.from([index])], program())[0];
}

/**
 * The `newBranch` account passed to `liquidate` is a plain (seed-unchecked)
 * `#[account(mut)]`; the vaults handler VALIDATES its `branch_id` against a value
 * computed from vault state (`ErrorCodes::VaultNewBranchInvalid` on mismatch):
 *   - mid-liquidation (`branchLiquidated == 1`): a fresh branch,
 *     `id = totalBranchId + 1` (see VaultState::update_topmost_tick /
 *     update_branch_info_by_one in programs/vaults/src/state/vault_state.rs);
 *   - otherwise the current branch is reused, `id = currentBranchId`.
 * So `newBranch = branchPda(vaultId, newBranchId(state))`.
 */
export function newBranchId(branchLiquidated: number, currentBranchId: number, totalBranchId: number): number {
  return branchLiquidated === 1 ? totalBranchId + 1 : currentBranchId;
}

// ── Fluid Liquidity-program PDAs ──────────────────────────────────────────────
// Seeds reversed from the on-chain Anchor source (audit repo
// code-423n4/2026-02-jupiter-lend, programs/liquidity/src/state/{seeds,context}.rs)
// and VALIDATED against live mainnet accounts (owner == liquidity program;
// token accounts == SPL accounts owned by `liquidityPda`). The vaults `liquidate`
// CPIs into the liquidity program, which RE-DERIVES each of these via its own
// `#[account(seeds=[...])]` constraints — so a wrong seed reverts (ConstraintSeeds)
// at simulation, making the on-chain program the ground-truth check.
export const LIQUIDITY_PROGRAM_ID = 'jupeiUmn818Jg1ekPURTpr4mFo29p46vygyykFJ3wZC';
function liqProgram(): PublicKey {
  return new PublicKey(LIQUIDITY_PROGRAM_ID);
}

/**
 * `liquidity` — the liquidity program's global singleton state PDA. Also the
 * token authority (ATA owner) for every vault token account. Seeds `[b"liquidity"]`.
 */
export function liquidityPda(): PublicKey {
  return PublicKey.findProgramAddressSync([Buffer.from('liquidity')], liqProgram())[0];
}
/**
 * `{supply,borrow}_token_reserves_liquidity` — per-mint `TokenReserve`.
 * Seeds `[b"reserve", mint]`.
 */
export function reservePda(mint: PublicKey): PublicKey {
  return PublicKey.findProgramAddressSync([Buffer.from('reserve'), mint.toBuffer()], liqProgram())[0];
}
/** `{supply,borrow}_rate_model` — per-mint `RateModel`. Seeds `[b"rate_model", mint]`. */
export function rateModelPda(mint: PublicKey): PublicKey {
  return PublicKey.findProgramAddressSync([Buffer.from('rate_model'), mint.toBuffer()], liqProgram())[0];
}
/**
 * `vault_supply_position_on_liquidity` — the vault's supply user-position at the
 * liquidity layer. `protocol` = the vault's `vaultConfig` PDA (the vaultConfig
 * is the PDA that signs the liquidity CPI). Seeds
 * `[b"user_supply_position", supplyMint, vaultConfig]`.
 */
export function userSupplyPositionPda(supplyMint: PublicKey, vaultConfig: PublicKey): PublicKey {
  return PublicKey.findProgramAddressSync([Buffer.from('user_supply_position'), supplyMint.toBuffer(), vaultConfig.toBuffer()], liqProgram())[0];
}
/** `vault_borrow_position_on_liquidity`. Seeds `[b"user_borrow_position", borrowMint, vaultConfig]`. */
export function userBorrowPositionPda(borrowMint: PublicKey, vaultConfig: PublicKey): PublicKey {
  return PublicKey.findProgramAddressSync([Buffer.from('user_borrow_position'), borrowMint.toBuffer(), vaultConfig.toBuffer()], liqProgram())[0];
}
/**
 * `supply_token_claim_account` — a `UserClaim`. Seeds `[b"user_claim", user, mint]`.
 * On the liquidate hot path the liquidity program only enforces `has_one = mint`
 * (not the seeds), and the vault passes it as an unchecked account with
 * `user = vaultConfig`; validated live by the probe (existence + owner + mint).
 */
export function userClaimPda(user: PublicKey, mint: PublicKey): PublicKey {
  return PublicKey.findProgramAddressSync([Buffer.from('user_claim'), user.toBuffer(), mint.toBuffer()], liqProgram())[0];
}

// ── tick_has_debt bitmap: index math + next-tick walk ────────────────────────
export const TICK_HAS_DEBT_ARRAY_SIZE = 8;
export const TICK_HAS_DEBT_CHILDREN_SIZE = 32; // bytes
export const TICK_HAS_DEBT_CHILDREN_SIZE_IN_BITS = TICK_HAS_DEBT_CHILDREN_SIZE * 8; // 256
export const TICKS_PER_TICK_HAS_DEBT = TICK_HAS_DEBT_ARRAY_SIZE * 256; // 2048

/** Which tick_has_debt array (0..15) a tick lives in. */
export function indexForTick(tick: number): number {
  const idx = Math.floor((tick - TickMath.MIN_TICK) / TICKS_PER_TICK_HAS_DEBT);
  return Math.min(idx, 15);
}
function firstTickForIndex(index: number): number {
  return TickMath.MIN_TICK + index * TICKS_PER_TICK_HAS_DEBT;
}

/** [arrayIndex, mapIndex, byteIndex, bitIndex] for a tick — mirrors getTickIndices. */
export function tickIndices(tick: number): [number, number, number, number] {
  const arrayIndex = indexForTick(tick);
  const first = firstTickForIndex(arrayIndex);
  const withinArray = tick - first; // 0..2047
  const mapIndex = Math.floor(withinArray / TICK_HAS_DEBT_CHILDREN_SIZE_IN_BITS);
  const withinMap = withinArray % TICK_HAS_DEBT_CHILDREN_SIZE_IN_BITS;
  return [arrayIndex, mapIndex, Math.floor(withinMap / 8), withinMap % 8];
}

/**
 * childrenBits[byte] for (array `index`, map `mapIndex`) out of raw account
 * bytes of a TickHasDebtArray. Layout (repr(C,packed)): disc[8] vaultId[2]
 * index[1] then tickHasDebt[8] x childrenBits[32].
 */
function childrenByte(raw: Buffer, mapIndex: number, byteIndex: number): number | undefined {
  const off = 11 + mapIndex * TICK_HAS_DEBT_CHILDREN_SIZE + byteIndex;
  return off < raw.length ? raw.readUInt8(off) : undefined;
}

function mostSignificantBit(byte: number): number {
  // leading zeros within 8 bits
  if (byte === 0) return 8;
  let lz = 0;
  let mask = 0x80;
  while ((byte & mask) === 0 && lz < 8) {
    lz += 1;
    mask >>= 1;
  }
  return lz;
}

/** Reader: fetch raw bytes of tick_has_debt array `index` for `vaultId`. */
export type ArrayFetcher = (index: number) => Buffer | undefined;

/**
 * Port of the SDK `findNextTickWithDebt`: given `startTick`, walk down the
 * bitmaps to the next lower tick that has debt, using `fetch` to load raw
 * TickHasDebtArray bytes by index. Returns MIN_TICK if none.
 */
export function findNextTickWithDebt(startTick: number, fetch: ArrayFetcher): number {
  const [arrayIndex, mapIndex, byteIndex, bitIndex] = tickIndices(startTick);
  let currentArray = arrayIndex;
  let currentMap = mapIndex;
  let raw = fetch(currentArray);
  if (raw === undefined) return TickMath.MIN_TICK;
  const buf = Buffer.from(raw);
  clearBits(buf, mapIndex, byteIndex, bitIndex);
  raw = buf;
  for (;;) {
    const nt = nextTopTick(raw, currentArray, currentMap);
    if (nt !== undefined) {
      return nt;
    }
    if (currentArray === 0) return TickMath.MIN_TICK;
    currentArray -= 1;
    currentMap = TICK_HAS_DEBT_ARRAY_SIZE - 1;
    const r = fetch(currentArray);
    if (r === undefined) return TickMath.MIN_TICK;
    raw = r;
  }
}

function clearBits(raw: Buffer, mapIndex: number, byteIndex: number, bitIndex: number): void {
  const base = 11 + mapIndex * TICK_HAS_DEBT_CHILDREN_SIZE;
  if (base + TICK_HAS_DEBT_CHILDREN_SIZE > raw.length) return;
  if (bitIndex > 0) {
    const mask = (1 << bitIndex) - 1;
    raw[base + byteIndex] = (raw[base + byteIndex] as number) & mask;
  } else {
    raw[base + byteIndex] = 0;
  }
  for (let b = byteIndex + 1; b < TICK_HAS_DEBT_CHILDREN_SIZE; b++) {
    raw[base + b] = 0;
  }
}

function nextTopTick(raw: Buffer, arrayIndex: number, startMap: number): number | undefined {
  for (let map = startMap; map >= 0; map--) {
    for (let byteIdx = TICK_HAS_DEBT_CHILDREN_SIZE - 1; byteIdx >= 0; byteIdx--) {
      const byte = childrenByte(raw, map, byteIdx);
      if (byte === undefined) return undefined;
      if (byte !== 0) {
        const bitPos = 7 - mostSignificantBit(byte);
        const tickWithinMap = byteIdx * 8 + bitPos;
        const mapFirst = firstTickForIndex(arrayIndex) + map * TICK_HAS_DEBT_CHILDREN_SIZE_IN_BITS;
        return mapFirst + tickWithinMap;
      }
    }
  }
  return undefined;
}

// ── Branch account decode (only what selection needs) ────────────────────────
// repr(C,packed): disc[8] vault_id u16@8 branch_id u32@10 status u8@14
// minima_tick i32@15 minima_tick_partials u32@19 debt_liquidity u64@23
// debt_factor u64@31 connected_branch_id u32@39 connected_minima_tick i32@43
export class BranchLite {
  branchId!: number;
  status!: number;
  connectedBranchId!: number;

  static decode(raw: Buffer): BranchLite | undefined {
    if (raw.length < 43) return undefined;
    const b = new BranchLite();
    b.branchId = raw.readUInt32LE(10);
    b.status = raw.readUInt8(14);
    b.connectedBranchId = raw.readUInt32LE(39);
    return b;
  }
}
