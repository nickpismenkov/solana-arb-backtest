// Port of src/liquidation.rs
//
// marginfi v2 liquidation engine — Stage 1: read-only health finder.
//
// Decodes on-chain `Bank` and `MarginfiAccount` state, computes each
// borrower's maintenance health, and flags who is liquidatable (weighted
// assets < weighted liabilities). No money moves here — this is the finder
// that feeds the liquidate-tx builder (Stage 2).
//
// Every byte offset below is VERIFIED: the Bank layout is checked field-by-
// field against real mainnet USDC-bank bytes (share values, weights, oracle
// setup all sane), and the MarginfiAccount/Balance layout is size-asserted
// against the marginfi-v2 source (Balance=104, MarginfiAccount=2312,
// Bank=1864) with the head (group@8, authority@40, lending_account@72)
// confirmed on-chain. `WrappedI80F48` is a 16-byte LE i128 fixed-point
// (value / 2^48).

import { PublicKey } from '@solana/web3.js';

function readI128LE(data: Buffer, offset: number): bigint {
  let result = 0n;
  for (let i = 15; i >= 0; i--) {
    result = (result << 8n) | BigInt(data[offset + i]);
  }
  const SIGN_BIT = 1n << 127n;
  if (result & SIGN_BIT) result -= 1n << 128n;
  return result;
}

/** Convert a WrappedI80F48 (16-byte LE i128, 48 fractional bits) to a number, at `data[offset..]`. */
export function i80f48ToF64(data: Buffer, offset: number): number {
  return Number(readI128LE(data, offset)) / Number(1n << 48n);
}

function readPubkey(data: Buffer, off: number): PublicKey {
  return new PublicKey(data.subarray(off, off + 32));
}

// ── Bank (VERIFIED against mainnet bytes; total account size 1864) ──────────
export const BANK_DISC = Buffer.from([142, 49, 166, 242, 50, 66, 97, 188]); // sha256("account:Bank")[..8]

/**
 * One emode (elevation-mode) rule on a *liability* bank: when this bank is
 * borrowed and the account holds collateral whose bank has `collateralTag`,
 * the collateral's asset weights are REPLACED by these boosted values.
 * VERIFIED against USDC bank 2s37akK2 (tag 619 → maint 0.99, tag 871 → maint
 * 0.92) which is exactly what flips ratio-1.30 accounts healthy per marginfi.
 */
export interface EmodeEntry {
  collateralTag: number;
  assetWeightInit: number;
  assetWeightMaint: number;
}

/** The risk parameters we need from a Bank to price a position's health. */
export interface Bank {
  mint: PublicKey;
  mintDecimals: number;
  assetShareValue: number;
  liabilityShareValue: number;
  assetWeightInit: number;
  assetWeightMaint: number;
  liabilityWeightInit: number;
  liabilityWeightMaint: number;
  /** OracleSetup enum: 1=PythLegacy, 2=SwitchboardV2, 3=PythPushOracle, … */
  oracleSetup: number;
  /** oracleKeys[0]: for Pyth Push this is the feed id / PriceUpdateV2 ref. */
  oracleKey: PublicKey;
  /**
   * BankConfig.oracle_max_age (seconds) — the chain rejects a price older
   * than this with a stale-oracle error. VERIFIED @800: USDC=300, wSOL=70,
   * BONK=120. 0 = use the program default.
   */
  oracleMaxAge: number;
  /**
   * This bank's emode class (0 = not emode-eligible as collateral). VERIFIED
   * @920: Amtw3n7G→619, USDC/other stables→57481.
   */
  emodeTag: number;
  /** Boosts this bank grants to collateral when borrowed as a liability. */
  emodeEntries: EmodeEntry[];
}

// Emode layout in the Bank account (VERIFIED against mainnet USDC + collateral
// banks): the bank's own tag is a u16 @920; the boost-entry array starts @1264,
// each entry 40 bytes (collateral_tag u16 @0, asset_weight_init @8,
// asset_weight_maint @24). Unused/garbage slots are rejected by weight range.
const BANK_EMODE_TAG = 920;
const BANK_EMODE_ENTRIES = 1264;
const EMODE_ENTRY_SIZE = 40;
// BankConfig.oracle_max_age (u16 seconds) — VERIFIED @800 across banks
// (USDC=300, wSOL=70, BONK=120).
const BANK_ORACLE_MAX_AGE = 800;

export function decodeBank(data: Buffer): Bank | null {
  if (data.length < 1864 || !data.subarray(0, 8).equals(BANK_DISC)) return null;
  // Emode entries: scan slots, keep only sane weight pairs (a real boost
  // has 0 < init <= maint <= 1.5; leftover/other-field bytes fail this).
  const emodeEntries: EmodeEntry[] = [];
  let e = 0;
  while (BANK_EMODE_ENTRIES + (e + 1) * EMODE_ENTRY_SIZE <= data.length && e < 16) {
    const base = BANK_EMODE_ENTRIES + e * EMODE_ENTRY_SIZE;
    const tag = data.readUInt16LE(base);
    const init = i80f48ToF64(data, base + 8);
    const maint = i80f48ToF64(data, base + 24);
    if (tag !== 0 && init > 0.0 && init <= maint && maint <= 1.5) {
      emodeEntries.push({ collateralTag: tag, assetWeightInit: init, assetWeightMaint: maint });
    }
    e += 1;
  }
  return {
    mint: readPubkey(data, 8),
    mintDecimals: data[40],
    // group @41 (verified, unused here)
    assetShareValue: i80f48ToF64(data, 80),
    liabilityShareValue: i80f48ToF64(data, 96),
    // BankConfig @296
    assetWeightInit: i80f48ToF64(data, 296),
    assetWeightMaint: i80f48ToF64(data, 312),
    liabilityWeightInit: i80f48ToF64(data, 328),
    liabilityWeightMaint: i80f48ToF64(data, 344),
    oracleSetup: data[609],
    oracleKey: readPubkey(data, 610),
    oracleMaxAge: data.readUInt16LE(BANK_ORACLE_MAX_AGE),
    emodeTag: data.readUInt16LE(BANK_EMODE_TAG),
    emodeEntries,
  };
}

/**
 * Boosted maint weight this liability bank grants to collateral of `tag`, if
 * any. Used by the emode intersection rule.
 */
function emodeBoost(bank: Bank, tag: number): number | undefined {
  return bank.emodeEntries.find((e) => e.collateralTag === tag)?.assetWeightMaint;
}

/**
 * The effective maintenance asset weight for one collateral bank, applying
 * marginfi's emode rule: emode boosts the collateral's weight ONLY if the
 * collateral is emode-tagged AND *every* liability bank the account borrows
 * grants a boost for that tag (intersection); then the boost is the min
 * across those liabilities (most conservative). Otherwise the base maint
 * weight. Falls back to base weight whenever emode doesn't cleanly apply, so
 * we over-flag rather than under-flag (the sim gate is the fire backstop).
 */
export function effectiveAssetWeightMaint(collateral: Bank, liabilityBanks: Bank[]): number {
  const base = collateral.assetWeightMaint;
  if (collateral.emodeTag === 0 || liabilityBanks.length === 0) return base;
  let boost = Infinity;
  for (const l of liabilityBanks) {
    const w = emodeBoost(l, collateral.emodeTag);
    if (w === undefined) return base; // a borrowed liability doesn't grant emode -> no boost
    boost = Math.min(boost, w);
  }
  return Number.isFinite(boost) ? Math.max(boost, base) : base;
}

// ── MarginfiAccount / Balance (size-asserted; head verified on-chain) ───────
export const MARGINFI_ACCOUNT_DISC = Buffer.from([67, 178, 130, 109, 126, 114, 28, 42]); // sha256("account:MarginfiAccount")[..8]
export const MA_SIZE = 2312;
const MA_GROUP = 8;
const MA_AUTHORITY = 40;
const LENDING_ACCOUNT = 72; // balances[0] start
const BALANCE_SIZE = 104;
const MAX_BALANCES = 16;
// within a Balance:
const BAL_ACTIVE = 0;
const BAL_BANK_PK = 1;
const BAL_ASSET_SHARES = 40;
const BAL_LIABILITY_SHARES = 56;

/** One active position slot on a MarginfiAccount (raw shares, not yet priced). */
export interface Balance {
  bankPk: PublicKey;
  assetShares: number;
  liabilityShares: number;
}

/** A decoded borrower: authority + its active balances. */
export interface MarginfiAccount {
  group: PublicKey;
  authority: PublicKey;
  balances: Balance[];
}

/**
 * Decode balances from account data. Accepts either the full 2312-byte
 * account or a leading dataSlice that covers all balances (>= 1736 bytes).
 */
export function decodeMarginfiAccount(data: Buffer): MarginfiAccount | null {
  if (
    data.length < LENDING_ACCOUNT + MAX_BALANCES * BALANCE_SIZE ||
    !data.subarray(0, 8).equals(MARGINFI_ACCOUNT_DISC)
  ) {
    return null;
  }
  const balances: Balance[] = [];
  for (let i = 0; i < MAX_BALANCES; i++) {
    const base = LENDING_ACCOUNT + i * BALANCE_SIZE;
    // active flag is a u8 bool; skip empty slots.
    if (data[base + BAL_ACTIVE] === 0) continue;
    const assetShares = i80f48ToF64(data, base + BAL_ASSET_SHARES);
    const liabilityShares = i80f48ToF64(data, base + BAL_LIABILITY_SHARES);
    if (assetShares === 0.0 && liabilityShares === 0.0) continue;
    balances.push({
      bankPk: readPubkey(data, base + BAL_BANK_PK),
      assetShares,
      liabilityShares,
    });
  }
  return {
    group: readPubkey(data, MA_GROUP),
    authority: readPubkey(data, MA_AUTHORITY),
    balances,
  };
}

/**
 * Bank pubkeys of ALL `active`-flag balances, in slot order — INCLUDING
 * zero-share ones. marginfi's liquidate health-check requires an oracle for
 * every active balance (not just the funded ones), so the observation list
 * must cover all of these or it fails WrongNumberOfOracleAccounts (6051).
 * `decodeMarginfiAccount` drops zero-share balances (fine for health/
 * selection, wrong for the obs list).
 */
export function activeBankPks(data: Buffer): PublicKey[] {
  const v: PublicKey[] = [];
  if (data.length < LENDING_ACCOUNT + MAX_BALANCES * BALANCE_SIZE) return v;
  for (let i = 0; i < MAX_BALANCES; i++) {
    const base = LENDING_ACCOUNT + i * BALANCE_SIZE;
    if (data[base + BAL_ACTIVE] === 0) continue;
    v.push(readPubkey(data, base + BAL_BANK_PK));
  }
  return v;
}

// ── Pyth PriceUpdateV2 (on-chain pull oracle — what marginfi reads) ─────────
// disc(8) · write_authority(32)@8 · verification_level@40 · price_message · …
// `verification_level` is a Borsh enum: tag@40 is 1 for Full (1 byte total) or
// 0 for Partial (2 bytes: tag + num_signatures). So price_message starts at 41
// (Full) or 42 (Partial). VERIFIED against the USDC oracle (Full → $0.9998).
// price_message: feed_id(32) · price:i64 · conf:u64 · exponent:i32 · publish_time:i64.
export const PRICE_UPDATE_V2_DISC = Buffer.from([34, 241, 35, 99, 157, 126, 244, 205]); // sha256("account:PriceUpdateV2")[..8]

export interface PriceUpdateV2 {
  feedId: Buffer;
  usdPrice: number;
  publishTime: bigint;
}

/** Decode a Pyth PriceUpdateV2 account. Price is scaled by its exponent to whole-token USD. */
export function decodePriceUpdateV2(data: Buffer): PriceUpdateV2 | null {
  if (data.length < 134 || !data.subarray(0, 8).equals(PRICE_UPDATE_V2_DISC)) return null;
  // Branch on verification_level tag to locate price_message.
  let pm: number;
  if (data[40] === 1) pm = 41; // Full
  else if (data[40] === 0) pm = 42; // Partial (tag + num_signatures)
  else return null;
  const feedId = Buffer.from(data.subarray(pm, pm + 32));
  const price = data.readBigInt64LE(pm + 32);
  const exponent = data.readInt32LE(pm + 48);
  const publishTime = data.readBigInt64LE(pm + 52);
  const usd = Number(price) * 10 ** exponent;
  return { feedId, usdPrice: usd, publishTime };
}

// Switchboard On-Demand PullFeed (owner SBondMDrcV3K…, 3208 bytes). marginfi
// oracle_setup 4 (SwitchboardPull) and 7 use these. The feed result is an
// i128 fixed-point (÷ 1e18) at offset 56 — VERIFIED on-chain: SOL feed reads
// $80.98, a USDS feed $0.9999. disc = sha256("account:PullFeedAccountData")[..8].
export const SWITCHBOARD_PULL_DISC = Buffer.from([0xc4, 0x1b, 0x6c, 0xc4, 0x0a, 0xd7, 0xdb, 0x28]);
const SB_PULL_RESULT = 56;

export function decodeSwitchboardPull(data: Buffer): number | null {
  if (data.length < SB_PULL_RESULT + 16 || !data.subarray(0, 8).equals(SWITCHBOARD_PULL_DISC)) return null;
  const v = readI128LE(data, SB_PULL_RESULT);
  const usd = Number(v) / 1e18;
  return Number.isFinite(usd) && usd > 0.0 ? usd : null;
}

// The Solana slot the current Switchboard result was produced at, offset 40 in
// the PullFeedAccountData (VERIFIED empirically across all 1508 live feeds
// 2026-07-14: fresh feeds read ~350 slots behind head, dead feeds read 0/behind
// millions). marginfi gates liquidation on this via the bank's oracle_max_age;
// a result too far behind head reverts the liquidate ix with SwitchboardStalePrice
// (6049) — which is exactly what made ~6.3k accounts show as "liquidatable" to a
// finder that read the price but not its age.
const SB_PULL_RESULT_SLOT = 40;

export function decodeSwitchboardPullSlot(data: Buffer): bigint | null {
  if (data.length < SB_PULL_RESULT_SLOT + 8 || !data.subarray(0, 8).equals(SWITCHBOARD_PULL_DISC)) return null;
  return data.readBigUInt64LE(SB_PULL_RESULT_SLOT);
}

/**
 * Fallback staleness ceiling (in slots) for a Switchboard oracle whose bank
 * reports oracleMaxAge = 0 (meaning "use the program default"). Generous by
 * design — see the asymmetry note on `maxStaleSlotsFor`. Override with
 * MAX_SB_STALE_SLOTS.
 */
export const DEFAULT_MAX_SB_STALE_SLOTS = 5000n;

/**
 * Approximate Solana slot rate. Real cadence is ~2.3–2.5 slots/s; using the
 * high end makes the slot budget slightly LARGER (more lenient), which is the
 * safe direction here.
 */
export const SLOTS_PER_SEC = 2.5;

/**
 * Extra leniency over the chain's own threshold. The failure asymmetry is
 * one-sided: gating TIGHTER than the chain would make us SKIP an account the
 * chain would accept (missed money); gating LOOSER only lets a stale account
 * reach the sim gate, which rejects it (6049) and the caller backs it off. So
 * we deliberately allow ~2x the chain's max-age before filtering.
 */
export const STALE_SAFETY_FACTOR = 2.0;

/**
 * Per-bank Switchboard staleness ceiling in SLOTS, derived from the bank's
 * on-chain oracleMaxAge (seconds) so we mirror exactly what the program will
 * accept — plus the safety factor. `oracleMaxAgeSecs === 0` -> program
 * default -> the generous fallback (caller-overridable).
 */
export function maxStaleSlotsFor(oracleMaxAgeSecs: number, defaultSlots: bigint): bigint {
  if (oracleMaxAgeSecs === 0) return defaultSlots;
  return BigInt(Math.trunc(oracleMaxAgeSecs * SLOTS_PER_SEC * STALE_SAFETY_FACTOR));
}

/**
 * USD price from an oracle, treating a Switchboard feed whose result slot is
 * more than `maxStaleSlots` behind `currentSlot` as UNAVAILABLE (null -> the
 * health calc counts it missing and never trusts the account). Pyth feeds
 * pass through unchanged. `currentSlot === 0n` disables the gate.
 */
export function decodeOraclePriceFresh(data: Buffer, currentSlot: bigint, maxStaleSlots: bigint): number | null {
  const pyth = decodePriceUpdateV2(data);
  if (pyth) return pyth.usdPrice;
  const usd = decodeSwitchboardPull(data);
  if (usd === null) return null;
  if (currentSlot > 0n) {
    const slot = decodeSwitchboardPullSlot(data);
    if (slot === null) return null;
    const behind = currentSlot > slot ? currentSlot - slot : 0n;
    if (behind > maxStaleSlots) return null; // stale -- the chain would revert with 6049
  }
  return usd;
}

/**
 * USD price from any oracle account we can decode, dispatching on disc:
 * Pyth PriceUpdateV2 (setups 1/3/5/6) or Switchboard On-Demand PullFeed
 * (setups 4/7).
 */
export function decodeOraclePrice(data: Buffer): number | null {
  const pyth = decodePriceUpdateV2(data);
  if (pyth) return pyth.usdPrice;
  return decodeSwitchboardPull(data);
}

// ── Health ──────────────────────────────────────────────────────────────────

/** The USD price of one whole token for a bank's mint. Keyed by bank pubkey (base58). */
export type PriceMap = Map<string, number>;
export type BankMap = Map<string, Bank>;

export interface Health {
  weightedAssets: number;
  weightedLiabilities: number;
}

/** Maintenance health value. Liquidatable when < 0. */
export function healthValue(h: Health): number {
  return h.weightedAssets - h.weightedLiabilities;
}
export function healthLiquidatable(h: Health): boolean {
  return healthValue(h) < 0.0;
}
/** weightedLiabilities / weightedAssets — ratio >= 1.0 means underwater. */
export function healthRatio(h: Health): number {
  return h.weightedAssets === 0.0 ? Infinity : h.weightedLiabilities / h.weightedAssets;
}

/**
 * Compute maintenance-weighted health for an account. `missing` counts
 * balances whose bank or price we couldn't resolve — if it's > 0 the health
 * is INCOMPLETE and must NOT be trusted for a liquidation decision (skipping
 * an unpriced collateral bank makes a healthy account look underwater).
 */
export interface HealthResult {
  health: Health;
  missing: number;
}

export function maintenanceHealth(acct: MarginfiAccount, banks: BankMap, prices: PriceMap): HealthResult {
  // Liability banks of this account (needed for the emode intersection rule).
  const liabBanks: Bank[] = acct.balances
    .filter((b) => b.liabilityShares > 0.0)
    .map((b) => banks.get(b.bankPk.toBase58()))
    .filter((b): b is Bank => b !== undefined);
  let wa = 0.0;
  let wl = 0.0;
  let missing = 0;
  for (const b of acct.balances) {
    const key = b.bankPk.toBase58();
    const bank = banks.get(key);
    const price = prices.get(key);
    if (bank === undefined || price === undefined) {
      missing += 1;
      continue;
    }
    const scale = 10 ** bank.mintDecimals;
    if (b.assetShares > 0.0) {
      const ui = (b.assetShares * bank.assetShareValue) / scale;
      // Emode: collateral weight may be boosted by the borrowed liabilities.
      const w = effectiveAssetWeightMaint(bank, liabBanks);
      wa += ui * price * w;
    }
    if (b.liabilityShares > 0.0) {
      const ui = (b.liabilityShares * bank.liabilityShareValue) / scale;
      wl += ui * price * bank.liabilityWeightMaint;
    }
  }
  return { health: { weightedAssets: wa, weightedLiabilities: wl }, missing };
}
