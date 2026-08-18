// Port of src/save.rs
//
// Save (formerly Solend) liquidation data layer + instruction builders.
//
// Save is the original SPL token-lending model — a NATIVE program, so each
// instruction is a one-byte tag (not an Anchor discriminator). Every layout
// below is derived from CAPTURED mainnet truth (the marginfi/Kamino lesson):
// the Reserve/Obligation packed layouts are cross-checked against the canonical
// Solend source AND real on-chain accounts, and the liquidate/refresh account
// orders are taken verbatim from real liquidation txs
// (4tQm9zcd… and 2inNexup…, both identical).
//
// ★ Save's USDC reserve reads the SAME Pyth sponsored feed (Dpw1EAVr…) that our
// self-crank pipeline already refreshes — so the crank front-run edge applies
// here too on Pyth-priced collateral, with no extra crank work.

import { PublicKey, TransactionInstruction, type AccountMeta } from '@solana/web3.js';

export const SOLEND_PROGRAM = 'So1endDq2YkqhipRh3WViPa8hdiSpxWy6z3Z6tMCpAo';
export const MAIN_POOL = '4UpD2fh7xH3VP9QQaXtsS1YY3bxzWhtfpks7FatyKvdY';

// Main-pool debt reserves the fire path repays. All three have a wired JupLend
// flash market (src/flashloan.rs) and are classic-SPL mints. Reserve pubkeys
// discovered live (getProgramAccounts, dataSize 619, memcmp lending_market =
// MAIN_POOL) and cross-checked against the reserve's decoded liquidity_mint.
export const USDC_RESERVE = 'BgxfHJDzm44T7XG68MYKx7YisTjZu73tVovyZSjJMpmw'; // 6dp
export const USDT_RESERVE = '8K9WC8xoh2rtQNY7iEGXtPvfbDCi563SdWhCAhuMP2xE'; // 6dp
export const WSOL_RESERVE = '8PbodeaosQP19SjYFx855UMqWxH2HynZLdBXmsrbac36'; // 9dp

// Isolated Solend pool with LIVE liquidation flow. Census 2026-07-15: over 48h
// this pool carried $321 of $341 (94%) of Solend's liquidation value while the
// main pool went effectively dead ($3). The fire path is pool-agnostic
// (lending_market_authority is a PDA of the market, and the tx uses the
// obligation's own lending_market), so covering a pool = scanning its
// obligations + registering its accepted-debt reserves. Only USDC/USDT here
// (same mints → same JupLend flash markets as the main pool; no wSOL reserve).
export const ISO_POOL_1 = '24FVbp6yRxP7qNNiVXHjAjwUabdvVfbJtDb3aJ5zCWwy';
export const ISO_POOL_1_USDC_RESERVE = '56v2DrnHB7kp5KkM4UboGymm8SxUJ8xRR9uYnU62uw4R';
export const ISO_POOL_1_USDT_RESERVE = 'AQTzHsJ5AHk1PN89o4mjPm7FHkMR7a7BxmY5jXgqnBTP';

/** Every Solend lending market we scan for liquidatable obligations. Add active
 * pools here (per an all-pools census) — the fire path needs no other change. */
export const SCAN_POOLS: readonly string[] = [MAIN_POOL, ISO_POOL_1];
/** Accepted-debt reserves across all SCAN_POOLS (USDC/USDT/wSOL mints, each
 * wired to a JupLend flash market). An obligation borrowing one of these is
 * fireable. */
export const DEBT_RESERVES: readonly string[] = [
  USDC_RESERVE,
  USDT_RESERVE,
  WSOL_RESERVE,
  ISO_POOL_1_USDC_RESERVE,
  ISO_POOL_1_USDT_RESERVE,
];

export const USDC_MINT = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v';
export const USDT_MINT = 'Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB';
export const WSOL_MINT = 'So11111111111111111111111111111111111111112';

/** Debt mints the widened fire path accepts (each has a JupLend flash market). */
export function isAcceptedDebtMint(mint: PublicKey): boolean {
  const s = mint.toBase58();
  return s === USDC_MINT || s === USDT_MINT || s === WSOL_MINT;
}

const TOKEN_PROGRAM = 'TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA';

// Instruction tags (Solend LendingInstruction enum).
const TAG_REFRESH_RESERVE = 3;
const TAG_REFRESH_OBLIGATION = 7;
const TAG_LIQUIDATE_AND_REDEEM = 17; // LiquidateObligationAndRedeemReserveCollateral

function pk(s: string): PublicKey {
  return new PublicKey(s);
}

function readPk(d: Buffer, o: number): PublicKey | null {
  if (o < 0 || o + 32 > d.length) return null;
  return new PublicKey(d.subarray(o, o + 32));
}

function u64le(d: Buffer, o: number): bigint | null {
  if (o < 0 || o + 8 > d.length) return null;
  return d.readBigUInt64LE(o);
}

/** Solend Decimal / scaled values are u128 with WAD = 1e18. */
function wad(d: Buffer, o: number): number | null {
  if (o < 0 || o + 16 > d.length) return null;
  let result = 0n;
  for (let i = 15; i >= 0; i--) {
    result = (result << 8n) | BigInt(d[o + i]);
  }
  return Number(result) / 1e18;
}

// ── Reserve (619 bytes, layout from Solend reserve.rs Pack + verified on the
//    USDC reserve). ───────────────────────────────────────────────────────────
const R_LENDING_MARKET = 10;
const R_LIQ_MINT = 42;
const R_MINT_DECIMALS = 74;
const R_LIQ_SUPPLY = 75;
const R_PYTH_ORACLE = 107;
const R_SB_ORACLE = 139;
const R_AVAILABLE_AMOUNT = 171; // u64: liquidity available in the reserve (base units)
const R_BORROWED_WADS = 179; // Decimal (WAD): liquidity borrowed, accrues interest
const R_CUM_BORROW_RATE = 195; // Decimal (WAD): cumulative borrow rate (monotone ↑)
const R_MARKET_PRICE = 211; // Decimal (WAD): USD price per whole token
const R_COLL_MINT = 227;
const R_COLL_MINT_SUPPLY = 259; // u64: cToken mint total supply (base units)
const R_COLL_SUPPLY = 267;
const R_LTV = 300;
const R_LIQ_BONUS = 301;
const R_LIQ_THRESHOLD = 302;
const R_FEE_RECEIVER = 339;
// accumulated_protocol_fees_wads lives in the reserve's trailing padding region
// (added after the original 619-byte layout froze), NOT inline after
// cumulative_borrow_rate. Verified live: subtracting it moves the exchange rate
// by <1e-4 %, but total_supply() nets it out, so we reproduce it exactly.
const R_ACCUM_PROTOCOL_FEES_WADS = 373; // Decimal (WAD)

/** Every account a refresh/liquidate touches for one reserve, pulled from the
 * reserve bytes — mirrors Kamino's ReserveAccounts pattern. */
export interface Reserve {
  reserve: PublicKey;
  lendingMarket: PublicKey;
  liquidityMint: PublicKey;
  mintDecimals: number;
  liquiditySupply: PublicKey;
  pythOracle: PublicKey;
  switchboardOracle: PublicKey;
  collateralMint: PublicKey;
  collateralSupply: PublicKey;
  feeReceiver: PublicKey;
  marketPrice: number;
  liquidationThresholdPct: number;
  liquidationBonusPct: number;
  loanToValuePct: number;
  // ── cToken exchange-rate inputs (fresh-price collateral valuation) ──────────
  /** Liquidity available in the reserve (base units). */
  availableAmount: bigint;
  /** Liquidity borrowed (whole native units; WAD decoded), accrues interest. */
  borrowedAmount: number;
  /** Cumulative borrow rate (WAD decoded), monotone ↑ — accrues each borrow's
   * principal to "now" when reproducing Solend's refresh_obligation. */
  cumulativeBorrowRate: number;
  /** Protocol fees accrued but not yet claimed (whole units; WAD decoded).
   * Netted out of total_supply, exactly like Solend's own math. */
  accumulatedProtocolFees: number;
  /** cToken mint total supply (base units) — denominator of the exchange rate. */
  collateralMintTotalSupply: bigint;
}

export function decodeReserve(reserve: PublicKey, d: Buffer): Reserve | null {
  if (d.length < 619 || d[0] !== 1) return null;
  const lendingMarket = readPk(d, R_LENDING_MARKET);
  const liquidityMint = readPk(d, R_LIQ_MINT);
  const liquiditySupply = readPk(d, R_LIQ_SUPPLY);
  const pythOracle = readPk(d, R_PYTH_ORACLE);
  const switchboardOracle = readPk(d, R_SB_ORACLE);
  const collateralMint = readPk(d, R_COLL_MINT);
  const collateralSupply = readPk(d, R_COLL_SUPPLY);
  const feeReceiver = readPk(d, R_FEE_RECEIVER);
  const marketPrice = wad(d, R_MARKET_PRICE);
  const availableAmount = u64le(d, R_AVAILABLE_AMOUNT);
  const borrowedAmount = wad(d, R_BORROWED_WADS);
  const cumulativeBorrowRate = wad(d, R_CUM_BORROW_RATE);
  const accumulatedProtocolFees = wad(d, R_ACCUM_PROTOCOL_FEES_WADS);
  const collateralMintTotalSupply = u64le(d, R_COLL_MINT_SUPPLY);
  if (
    lendingMarket === null ||
    liquidityMint === null ||
    liquiditySupply === null ||
    pythOracle === null ||
    switchboardOracle === null ||
    collateralMint === null ||
    collateralSupply === null ||
    feeReceiver === null ||
    marketPrice === null ||
    availableAmount === null ||
    borrowedAmount === null ||
    cumulativeBorrowRate === null ||
    accumulatedProtocolFees === null ||
    collateralMintTotalSupply === null
  ) {
    return null;
  }
  return {
    reserve,
    lendingMarket,
    liquidityMint,
    mintDecimals: d[R_MINT_DECIMALS],
    liquiditySupply,
    pythOracle,
    switchboardOracle,
    collateralMint,
    collateralSupply,
    feeReceiver,
    marketPrice,
    loanToValuePct: d[R_LTV],
    liquidationBonusPct: d[R_LIQ_BONUS],
    liquidationThresholdPct: d[R_LIQ_THRESHOLD],
    availableAmount,
    borrowedAmount,
    cumulativeBorrowRate,
    accumulatedProtocolFees,
    collateralMintTotalSupply,
  };
}

/**
 * cToken → underlying multiplier (Solend `collateral_exchange_rate`, inverted
 * to liquidity-per-cToken so a deposit valuation is `ctokens × rate × price`).
 *
 *   total_supply = available + borrowed − accumulated_protocol_fees
 *   rate         = total_supply / cToken_mint_total_supply
 *
 * Solend's `CollateralExchangeRate` is cTokens-per-liquidity; the liquidity a
 * deposit is worth is `collateral_to_liquidity(amount) = amount / rate`, i.e.
 * `amount × total_supply / mint_supply` — what we return here. Starts at 1.0
 * (INITIAL_COLLATERAL_RATE) and only grows with interest; a reserve with no
 * cTokens minted (or degenerate liquidity) falls back to 1.0.
 */
export function ctokenExchangeRate(r: Reserve): number {
  if (r.collateralMintTotalSupply === 0n) return 1.0;
  const totalSupply = Number(r.availableAmount) + r.borrowedAmount - r.accumulatedProtocolFees;
  const mint = Number(r.collateralMintTotalSupply);
  if (totalSupply <= 0.0 || mint <= 0.0) return 1.0;
  return totalSupply / mint;
}

// ── Obligation (1300 bytes, layout from Solend obligation.rs Pack + verified
//    on a real main-pool obligation). ────────────────────────────────────────
const O_LENDING_MARKET = 10;
const O_OWNER = 42;
const O_DEPOSITED_VALUE = 74;
const O_BORROWED_VALUE = 90;
const O_UNHEALTHY_BORROW_VALUE = 122;
const O_DEPOSITS_LEN = 202;
const O_BORROWS_LEN = 203;
const O_DATA_FLAT = 204;
const COLLATERAL_LEN = 88; // reserve(32) + deposited_amount u64(8) + market_value(16) + pad(32)
const LIQUIDITY_LEN = 112; // reserve(32) + cum_rate(16) + borrowed_wads(16) + market_value(16) + pad(32)

export interface Deposit {
  reserve: PublicKey;
  /** cToken (collateral) amount deposited. */
  depositedAmount: bigint;
  marketValue: number;
}

export interface Borrow {
  reserve: PublicKey;
  /** Borrow's cumulative_borrow_rate snapshot at the obligation's last refresh;
   * dividing the reserve's current rate by this accrues interest to "now". */
  cumulativeBorrowRate: number;
  borrowedAmountWads: number;
  marketValue: number;
}

export interface Obligation {
  lendingMarket: PublicKey;
  owner: PublicKey;
  depositedValue: number;
  borrowedValue: number;
  unhealthyBorrowValue: number;
  deposits: Deposit[];
  borrows: Borrow[];
}

export function decodeObligation(d: Buffer): Obligation | null {
  if (d.length < 1300 || d[0] !== 1) return null;
  const nDep = d[O_DEPOSITS_LEN];
  const nBor = d[O_BORROWS_LEN];
  const deposits: Deposit[] = [];
  let off = O_DATA_FLAT;
  for (let i = 0; i < nDep; i++) {
    const reserve = readPk(d, off);
    const depositedAmount = u64le(d, off + 32);
    const marketValue = wad(d, off + 40);
    if (reserve === null || depositedAmount === null || marketValue === null) return null;
    deposits.push({ reserve, depositedAmount, marketValue });
    off += COLLATERAL_LEN;
  }
  const borrows: Borrow[] = [];
  for (let i = 0; i < nBor; i++) {
    const reserve = readPk(d, off);
    const cumulativeBorrowRate = wad(d, off + 32);
    const borrowedAmountWads = wad(d, off + 48);
    const marketValue = wad(d, off + 64);
    if (reserve === null || cumulativeBorrowRate === null || borrowedAmountWads === null || marketValue === null) {
      return null;
    }
    borrows.push({ reserve, cumulativeBorrowRate, borrowedAmountWads, marketValue });
    off += LIQUIDITY_LEN;
  }
  const lendingMarket = readPk(d, O_LENDING_MARKET);
  const owner = readPk(d, O_OWNER);
  const depositedValue = wad(d, O_DEPOSITED_VALUE);
  const borrowedValue = wad(d, O_BORROWED_VALUE);
  const unhealthyBorrowValue = wad(d, O_UNHEALTHY_BORROW_VALUE);
  if (lendingMarket === null || owner === null || depositedValue === null || borrowedValue === null || unhealthyBorrowValue === null) {
    return null;
  }
  return { lendingMarket, owner, depositedValue, borrowedValue, unhealthyBorrowValue, deposits, borrows };
}

/**
 * Liquidatable per Solend's own on-chain math: borrowed value has crossed
 * the (deposit-weighted) unhealthy threshold. Both fields are refreshed
 * on-chain, so this is the protocol's verdict — the fire is still sim-gated.
 */
export function obligationLiquidatable(o: Obligation): boolean {
  return o.unhealthyBorrowValue > 0.0 && o.borrowedValue > o.unhealthyBorrowValue;
}

/** How far over the threshold (>1.0 = underwater), for ranking. */
export function obligationHealthRatio(o: Obligation): number {
  return o.unhealthyBorrowValue === 0.0 ? 0.0 : o.borrowedValue / o.unhealthyBorrowValue;
}

/**
 * Recompute (borrowed_value, unhealthy_borrow_value) at the reserves' CURRENT
 * on-chain prices — a faithful reproduction of Solend's own
 * `refresh_obligation` math. Returns `null` if any referenced reserve is
 * missing from `reserves`.
 *
 * WHY: the STORED borrowed/unhealthy are refreshed LAZILY (only when someone
 * touches the obligation), so hundreds read stale — usually stale-HIGH on the
 * collateral side (priced when the collateral was worth more), which makes a
 * genuinely-healthy position look liquidatable ("phantom"). The fire tier must
 * gate on the value Solend's own `liquidate` will recompute at settle time,
 * which is exactly this.
 *
 * EXACTNESS (validated live): for obligations refreshed in the same slot as
 * their reserves (fresh price == the price Solend last refreshed at) this
 * reproduces the stored values to 0.0000%. The pieces:
 *   collateral: `deposited_ctokens × exchange_rate × price × liq_threshold`
 *               where exchange_rate = ctokenExchangeRate(reserve).
 *   debt:       `borrowed_wads × (reserve_cum_rate / borrow_cum_rate) / 10^dec
 *               × price` — the ratio accrues interest since the last refresh
 *               (accrual ≥ 1, so debt is never understated; clamped for safety).
 * Solend applies a per-asset borrow weight, but for the accepted debt set
 * (USDC/USDT/wSOL) it is 1.0 (verified against freshly-refreshed obligations).
 */
export function obligationFreshHealth(o: Obligation, reserves: Map<string, Reserve>): [number, number] | null {
  let unhealthy = 0.0;
  for (const d of o.deposits) {
    const r = reserves.get(d.reserve.toBase58());
    if (r === undefined) return null;
    const underlying = (Number(d.depositedAmount) * ctokenExchangeRate(r)) / 10 ** r.mintDecimals;
    unhealthy += (underlying * r.marketPrice * r.liquidationThresholdPct) / 100.0;
  }
  let borrowed = 0.0;
  for (const b of o.borrows) {
    const r = reserves.get(b.reserve.toBase58());
    if (r === undefined) return null;
    const accrual = b.cumulativeBorrowRate > 0.0 ? Math.max(r.cumulativeBorrowRate / b.cumulativeBorrowRate, 1.0) : 1.0;
    const native = (b.borrowedAmountWads * accrual) / 10 ** r.mintDecimals;
    borrowed += native * r.marketPrice;
  }
  return [borrowed, unhealthy];
}

/**
 * Liquidatable at CURRENT on-chain prices (fresh_health), the value Solend's
 * `liquidate` recomputes at settle time — the phantom-free fire-tier verdict.
 */
export function obligationFreshLiquidatable(o: Obligation, reserves: Map<string, Reserve>): boolean {
  const fh = obligationFreshHealth(o, reserves);
  if (fh === null) return false;
  const [b, u] = fh;
  return u > 0.0 && b > u;
}

/**
 * lending_market_authority PDA — seed = the lending market pubkey (VERIFIED:
 * derives DdZR6zR… for the main pool, matching the captured liquidation tx).
 */
export function lendingMarketAuthority(lendingMarket: PublicKey): PublicKey {
  return PublicKey.findProgramAddressSync([lendingMarket.toBuffer()], pk(SOLEND_PROGRAM))[0];
}

// ── Instruction builders (tags + account orders from the captured txs) ────────

function meta(pubkey: PublicKey, isSigner: boolean, isWritable: boolean): AccountMeta {
  return { pubkey, isSigner, isWritable };
}

/** refresh_reserve (tag 3): [reserve(w), pyth_oracle, switchboard_oracle]. */
export function refreshReserve(r: Reserve): TransactionInstruction {
  return new TransactionInstruction({
    programId: pk(SOLEND_PROGRAM),
    keys: [meta(r.reserve, false, true), meta(r.pythOracle, false, false), meta(r.switchboardOracle, false, false)],
    data: Buffer.from([TAG_REFRESH_RESERVE]),
  });
}

/** refresh_obligation (tag 7): [obligation(w), then each deposit reserve, then
 * each borrow reserve — in obligation order]. */
export function refreshObligation(obligation: PublicKey, depositReserves: PublicKey[], borrowReserves: PublicKey[]): TransactionInstruction {
  const keys: AccountMeta[] = [meta(obligation, false, true)];
  for (const r of [...depositReserves, ...borrowReserves]) {
    keys.push(meta(r, false, true));
  }
  return new TransactionInstruction({ programId: pk(SOLEND_PROGRAM), keys, data: Buffer.from([TAG_REFRESH_OBLIGATION]) });
}

/**
 * liquidate_obligation_and_redeem_reserve_collateral (tag 17). Repays
 * `liquidityAmount` of the borrow (the debt asset — USDC/USDT/wSOL) and
 * seizes+redeems the withdraw reserve's collateral to underlying, into the
 * liquidator's accounts. The account order is asset-agnostic and verbatim from
 * the captured txs (15 accounts).
 */
export function liquidateAndRedeem(
  liquidityAmount: bigint,
  sourceLiquidity: PublicKey, // liquidator debt-asset ATA (repay)
  destCollateral: PublicKey, // liquidator cToken ATA
  destLiquidity: PublicKey, // liquidator underlying ATA (redeemed collateral)
  repayReserve: Reserve, // the borrow (debt) reserve
  withdrawReserve: Reserve, // the collateral reserve being seized
  obligation: PublicKey,
  lendingMarket: PublicKey,
  userTransferAuthority: PublicKey, // signer
): TransactionInstruction {
  const data = Buffer.alloc(9);
  data.writeUInt8(TAG_LIQUIDATE_AND_REDEEM, 0);
  data.writeBigUInt64LE(liquidityAmount, 1);
  const keys: AccountMeta[] = [
    meta(sourceLiquidity, false, true), // 0
    meta(destCollateral, false, true), // 1
    meta(destLiquidity, false, true), // 2
    meta(repayReserve.reserve, false, true), // 3
    meta(repayReserve.liquiditySupply, false, true), // 4
    meta(withdrawReserve.reserve, false, true), // 5
    meta(withdrawReserve.collateralMint, false, true), // 6
    meta(withdrawReserve.collateralSupply, false, true), // 7
    meta(withdrawReserve.liquiditySupply, false, true), // 8
    meta(withdrawReserve.feeReceiver, false, true), // 9
    meta(obligation, false, true), // 10
    meta(lendingMarket, false, false), // 11
    meta(lendingMarketAuthority(lendingMarket), false, false), // 12
    meta(userTransferAuthority, true, true), // 13 signer
    meta(pk(TOKEN_PROGRAM), false, false), // 14
  ];
  return new TransactionInstruction({ programId: pk(SOLEND_PROGRAM), keys, data });
}

// (tests omitted)
