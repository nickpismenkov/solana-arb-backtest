// Port of src/kamino_ix.rs
//
// Kamino (KLend) liquidation instructions, derived from a REAL mainnet
// liquidation tx (see kaminoLiqDecode) — layouts are program-fixed, so one
// captured sample pins them. Discriminators and the 25-account liquidate
// layout are verified against tx pBAF8kTU... (bSoL liquidator, USDC->bSOL).
//
// A Kamino liquidation is a 3-instruction sequence, all in one tx:
//   refresh_reserve(repay_reserve)  +  refresh_reserve(withdraw_reserve)
//   refresh_obligation(obligation, [reserves...])
//   liquidate_obligation_and_redeem_reserve_collateral_v2
// The liquidate seizes collateral and immediately redeems the cTokens to the
// underlying liquidity token into the liquidator's ATA (so the swap leg sells
// the underlying, not a cToken).
//
// Oracle wiring: main-market reserves price via Scope (scope_prices account
// at reserve offset 5112); the pyth/switchboard refresh slots are None
// (KLend-program placeholders). ReserveAccounts.decode pulls every account a
// refresh/liquidate needs straight out of the Reserve bytes.

import { PublicKey, TransactionInstruction, type AccountMeta } from '@solana/web3.js';

export const KLEND_PROGRAM = 'KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD';
export const FARMS_PROGRAM = 'FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr';
export const SYSVAR_INSTRUCTIONS = 'Sysvar1nstructions1111111111111111111111111';

// VERIFIED discriminators (from the captured tx).
const DISC_REFRESH_RESERVE = Buffer.from([0x02, 0xda, 0x8a, 0xeb, 0x4f, 0xc9, 0x19, 0x66]);
const DISC_REFRESH_OBLIGATION = Buffer.from([0x21, 0x84, 0x93, 0xe4, 0x97, 0xc0, 0x48, 0x59]);
const DISC_LIQUIDATE_V2 = Buffer.from([0xa2, 0xa1, 0x23, 0x8f, 0x1e, 0xbb, 0xb9, 0x67]);

// Reserve account-field offsets (VERIFIED by locating known pubkeys in a real
// 8624-byte reserve).
const R_LENDING_MARKET = 32;
const R_LIQ_MINT = 128;
const R_LIQ_SUPPLY = 160;
const R_FEE_RECEIVER = 192;
const R_COLL_MINT = 2560;
const R_COLL_SUPPLY = 2600;
const R_SCOPE_PRICES = 5112;

function pk(s: string): PublicKey {
  return new PublicKey(s);
}
function pkAt(d: Buffer, off: number): PublicKey {
  return new PublicKey(d.subarray(off, off + 32));
}
function meta(pubkey: PublicKey, isSigner: boolean, isWritable: boolean): AccountMeta {
  return { pubkey, isSigner, isWritable };
}

/**
 * Every account of one reserve that a refresh/liquidate touches, pulled from
 * the reserve account bytes.
 */
export interface ReserveAccounts {
  reserve: PublicKey;
  lendingMarket: PublicKey;
  liquidityMint: PublicKey;
  liquiditySupply: PublicKey;
  feeReceiver: PublicKey;
  collateralMint: PublicKey;
  collateralSupply: PublicKey;
  scopePrices: PublicKey;
}

export const ReserveAccounts = {
  decode(reserve: PublicKey, data: Buffer): ReserveAccounts | null {
    if (data.length < R_SCOPE_PRICES + 32) return null;
    return {
      reserve,
      lendingMarket: pkAt(data, R_LENDING_MARKET),
      liquidityMint: pkAt(data, R_LIQ_MINT),
      liquiditySupply: pkAt(data, R_LIQ_SUPPLY),
      feeReceiver: pkAt(data, R_FEE_RECEIVER),
      collateralMint: pkAt(data, R_COLL_MINT),
      collateralSupply: pkAt(data, R_COLL_SUPPLY),
      scopePrices: pkAt(data, R_SCOPE_PRICES),
    };
  },
};

/** lending_market_authority PDA (seed "lma"), VERIFIED against the captured tx. */
export function lendingMarketAuthority(lendingMarket: PublicKey): PublicKey {
  return PublicKey.findProgramAddressSync([Buffer.from('lma'), lendingMarket.toBuffer()], pk(KLEND_PROGRAM))[0];
}

/**
 * refresh_reserve. Scope-priced reserves pass the scope_prices account and
 * leave pyth/switchboard slots as the KLend program (Anchor `None`).
 * Accounts: [reserve(W), lending_market, pyth?, sb_price?, sb_twap?, scope?].
 */
export function refreshReserve(r: ReserveAccounts): TransactionInstruction {
  const klend = pk(KLEND_PROGRAM);
  return new TransactionInstruction({
    programId: klend,
    keys: [
      meta(r.reserve, false, true),
      meta(r.lendingMarket, false, false),
      meta(klend, false, false), // pyth = None
      meta(klend, false, false), // switchboard price = None
      meta(klend, false, false), // switchboard twap = None
      meta(r.scopePrices, false, false), // scope
    ],
    data: DISC_REFRESH_RESERVE,
  });
}

/**
 * refresh_obligation. Accounts: [lending_market, obligation(W)] then each of
 * the obligation's reserves (deposits then borrows, in slot order) as
 * read-only remaining accounts.
 */
export function refreshObligation(lendingMarket: PublicKey, obligation: PublicKey, reserves: PublicKey[]): TransactionInstruction {
  const keys: AccountMeta[] = [meta(lendingMarket, false, false), meta(obligation, false, true)];
  for (const r of reserves) keys.push(meta(r, false, false));
  return new TransactionInstruction({ programId: pk(KLEND_PROGRAM), keys, data: DISC_REFRESH_OBLIGATION });
}

/**
 * liquidate_obligation_and_redeem_reserve_collateral_v2. Seizes and redeems in
 * one ix. data = disc + liquidity_amount(u64) + min_acceptable_received(u64) +
 * max_allowed_ltv_override_pct(u64). 25 accounts, VERIFIED layout:
 *   [0] liquidator (signer)          [13] user_dest_collateral (W)
 *   [1] obligation (W)               [14] user_dest_liquidity (W)
 *   [2] lending_market               [15] user_source_liquidity (W, the repay)
 *   [3] lending_market_authority     [16] collateral_token_program
 *   [4] repay_reserve (W)            [17] repay_liquidity_token_program
 *   [5] repay_liquidity_mint         [18] withdraw_liquidity_token_program
 *   [6] repay_liquidity_supply (W)   [19] instructions sysvar
 *   [7] withdraw_reserve (W)         [20..23] KLend placeholders (opt None)
 *   [8] withdraw_liquidity_mint      [24] farms program
 *   [9] withdraw_collateral_mint (W)
 *   [10] withdraw_collateral_supply (W)
 *   [11] withdraw_liquidity_supply (W)
 *   [12] withdraw_fee_receiver (W)
 */
export function liquidateAndRedeemV2(
  liquidator: PublicKey,
  obligation: PublicKey,
  lendingMarket: PublicKey,
  repay: ReserveAccounts,
  withdraw: ReserveAccounts,
  userDestCollateral: PublicKey,
  userDestLiquidity: PublicKey,
  userSourceLiquidity: PublicKey,
  collateralTokenProgram: PublicKey,
  repayLiquidityTokenProgram: PublicKey,
  withdrawLiquidityTokenProgram: PublicKey,
  liquidityAmount: bigint,
  minAcceptableReceivedLiquidity: bigint,
  maxAllowedLtvOverridePct: bigint,
): TransactionInstruction {
  const klend = pk(KLEND_PROGRAM);
  const data = Buffer.alloc(32);
  DISC_LIQUIDATE_V2.copy(data, 0);
  data.writeBigUInt64LE(liquidityAmount, 8);
  data.writeBigUInt64LE(minAcceptableReceivedLiquidity, 16);
  data.writeBigUInt64LE(maxAllowedLtvOverridePct, 24);
  const keys: AccountMeta[] = [
    meta(liquidator, true, false),
    meta(obligation, false, true),
    meta(lendingMarket, false, false),
    meta(lendingMarketAuthority(lendingMarket), false, false),
    meta(repay.reserve, false, true),
    meta(repay.liquidityMint, false, false),
    meta(repay.liquiditySupply, false, true),
    meta(withdraw.reserve, false, true),
    meta(withdraw.liquidityMint, false, false),
    meta(withdraw.collateralMint, false, true),
    meta(withdraw.collateralSupply, false, true),
    meta(withdraw.liquiditySupply, false, true),
    meta(withdraw.feeReceiver, false, true),
    meta(userDestCollateral, false, true),
    meta(userDestLiquidity, false, true),
    meta(userSourceLiquidity, false, true),
    meta(collateralTokenProgram, false, false),
    meta(repayLiquidityTokenProgram, false, false),
    meta(withdrawLiquidityTokenProgram, false, false),
    meta(pk(SYSVAR_INSTRUCTIONS), false, false),
    meta(klend, false, false),
    meta(klend, false, false),
    meta(klend, false, false),
    meta(klend, false, false),
    meta(pk(FARMS_PROGRAM), false, false),
  ];
  return new TransactionInstruction({ programId: klend, keys, data });
}
