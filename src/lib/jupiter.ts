// Port of src/jupiter.rs
//
// Jupiter Lend (Fluid) liquidation data layer — vault decoders + first-pass
// liquidatable detection.
//
// Jupiter Lend is a FLUID vault-tick protocol, fundamentally different from the
// obligation-based lenders (marginfi/Kamino/Save): liquidation is VAULT-LEVEL,
// not per-borrower. Each vault is an isolated (supplyToken = collateral,
// borrowToken = debt) pair; a liquidator buys collateral off the vault's
// liquidation curve at a penalty discount (see the `liquidate` ix:
// debtAmt + colPerUnitDebt + absorb). There is NO obligation account.
//
// Layouts are Anchor/borsh (8-byte account discriminator = sha256("account:
// <Name>")[..8], then fields in declaration order, packed, little-endian),
// taken from the published IDL (jup-ag/jupiter-lend target/idl/vaults.json) and
// VERIFIED against 89 live mainnet VaultConfig/VaultState accounts.
//
// Program (Vaults/Borrow): jupr81YtYssSyPt8jbnGuiWon5f6x9TcDEFxYe3Bdzi

import { PublicKey } from '@solana/web3.js';

export const VAULTS_PROGRAM = 'jupr81YtYssSyPt8jbnGuiWon5f6x9TcDEFxYe3Bdzi';
export const LIQUIDITY_PROGRAM = 'jupeiUmn818Jg1ekPURTpr4mFo29p46vygyykFJ3wZC';

// Anchor account discriminators (sha256("account:<Name>")[..8]).
export const VAULT_CONFIG_DISC = Buffer.from([0x63, 0x56, 0x2b, 0xd8, 0xb8, 0x66, 0x77, 0x4d]);
export const VAULT_STATE_DISC = Buffer.from([0xe4, 0xc4, 0x52, 0xa5, 0x62, 0xd2, 0xeb, 0x98]);

// Common debt-token mints (the reachable debt set for our liquidator).
export const USDC_MINT = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v';
export const USDT_MINT = 'Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB';
export const WSOL_MINT = 'So11111111111111111111111111111111111111112';

function readPk(d: Buffer, o: number): PublicKey | undefined {
  if (o + 32 > d.length) return undefined;
  return new PublicKey(d.subarray(o, o + 32));
}
function u16le(d: Buffer, o: number): number | undefined {
  return o + 2 <= d.length ? d.readUInt16LE(o) : undefined;
}
function u32le(d: Buffer, o: number): number | undefined {
  return o + 4 <= d.length ? d.readUInt32LE(o) : undefined;
}
function u64le(d: Buffer, o: number): bigint | undefined {
  return o + 8 <= d.length ? d.readBigUInt64LE(o) : undefined;
}
function u128le(d: Buffer, o: number): bigint | undefined {
  if (o + 16 > d.length) return undefined;
  const lo = d.readBigUInt64LE(o);
  const hi = d.readBigUInt64LE(o + 8);
  return (hi << 64n) | lo;
}
function i32le(d: Buffer, o: number): number | undefined {
  return o + 4 <= d.length ? d.readInt32LE(o) : undefined;
}

// ── VaultConfig (219 bytes; VERIFIED against live accounts) ──────────────────
// disc[8] · vault_id u16@8 · supply_rate_magnifier i16@10 · borrow_rate_magnifier
// i16@12 · collateral_factor u16@14 · liquidation_threshold u16@16 ·
// liquidation_max_limit u16@18 · withdraw_gap u16@20 · liquidation_penalty u16@22
// · borrow_fee u8@24 · vault_type u8@25 · oracle @26 · rebalancer @58 ·
// liquidity_program @90 · oracle_program @122 · supply_token @154 ·
// borrow_token @186 · bump u8@218.
export class VaultConfig {
  vaultId!: number;
  /**
   * Raw u16. INFERRED scale: units of 0.1% (raw/1000 = fraction, raw/10 =
   * percent) — matches known LTVs (wSOL/USDC 850->85%, JUP/USDC 700->70%), but
   * treat as inferred; the fire path's sim is ground truth regardless.
   */
  collateralFactor!: number;
  liquidationThreshold!: number;
  liquidationPenalty!: number;
  oracle!: PublicKey;
  oracleProgram!: PublicKey;
  /** Collateral mint. */
  supplyToken!: PublicKey;
  /** Debt mint (what the liquidator repays). */
  borrowToken!: PublicKey;

  static decode(d: Buffer): VaultConfig | undefined {
    if (d.length < 219 || !d.subarray(0, 8).equals(VAULT_CONFIG_DISC)) return undefined;
    const vaultId = u16le(d, 8);
    const collateralFactor = u16le(d, 14);
    const liquidationThreshold = u16le(d, 16);
    const liquidationPenalty = u16le(d, 22);
    const oracle = readPk(d, 26);
    const oracleProgram = readPk(d, 122);
    const supplyToken = readPk(d, 154);
    const borrowToken = readPk(d, 186);
    if (
      vaultId === undefined ||
      collateralFactor === undefined ||
      liquidationThreshold === undefined ||
      liquidationPenalty === undefined ||
      oracle === undefined ||
      oracleProgram === undefined ||
      supplyToken === undefined ||
      borrowToken === undefined
    ) {
      return undefined;
    }
    const v = new VaultConfig();
    v.vaultId = vaultId;
    v.collateralFactor = collateralFactor;
    v.liquidationThreshold = liquidationThreshold;
    v.liquidationPenalty = liquidationPenalty;
    v.oracle = oracle;
    v.oracleProgram = oracleProgram;
    v.supplyToken = supplyToken;
    v.borrowToken = borrowToken;
    return v;
  }

  /** Liquidation threshold as a fraction (INFERRED scale, see field docs). */
  liqThresholdFrac(): number {
    return this.liquidationThreshold / 1000;
  }

  /** Debt-token label for the reachable-set check. */
  debtLabel(): 'USDC' | 'USDT' | 'wSOL' | 'other' {
    const s = this.borrowToken.toBase58();
    if (s === USDC_MINT) return 'USDC';
    if (s === USDT_MINT) return 'USDT';
    if (s === WSOL_MINT) return 'wSOL';
    return 'other';
  }

  /** Debt we currently target (USDC/USDT/SOL). */
  debtInScope(): boolean {
    return this.debtLabel() !== 'other';
  }
}

// ── VaultState (127 bytes; VERIFIED against live accounts) ───────────────────
// disc[8] · vault_id u16@8 · branch_liquidated u8@10 · topmost_tick i32@11 ·
// current_branch_id u32@15 · total_branch_id u32@19 · total_supply u64@23 ·
// total_borrow u64@31 · total_positions u32@39 · absorbed_debt_amount u128@43 ·
// absorbed_col_amount u128@59 · absorbed_dust_debt u64@75 · liquidity_supply_
// exchange_price u64@83 · liquidity_borrow_exchange_price u64@91 · vault_supply_
// exchange_price u64@99 · vault_borrow_exchange_price u64@107 · next_position_id
// u32@115 · last_update_timestamp u64@119.
export class VaultState {
  vaultId!: number;
  /** Nonzero while a branch is mid-liquidation. */
  branchLiquidated!: number;
  /** Highest debt tick (Fluid liquidation curve position). */
  topmostTick!: number;
  /** Active branch id (root of the branch chain a liquidation walks). */
  currentBranchId!: number;
  /** Highest branch id ever created (newBranch = this+1 when mid-liquidation). */
  totalBranchId!: number;
  totalSupply!: bigint;
  totalBorrow!: bigint;
  totalPositions!: number;
  /** Debt the vault has absorbed (bad-debt / pending-liquidation bucket). */
  absorbedDebtAmount!: bigint;
  vaultSupplyExchangePrice!: bigint;
  vaultBorrowExchangePrice!: bigint;
  lastUpdateTimestamp!: number;

  static decode(d: Buffer): VaultState | undefined {
    if (d.length < 127 || !d.subarray(0, 8).equals(VAULT_STATE_DISC)) return undefined;
    const vaultId = u16le(d, 8);
    const branchLiquidated = d.length > 10 ? d.readUInt8(10) : undefined;
    const topmostTick = i32le(d, 11);
    const currentBranchId = u32le(d, 15);
    const totalBranchId = u32le(d, 19);
    const totalSupply = u64le(d, 23);
    const totalBorrow = u64le(d, 31);
    const totalPositions = u32le(d, 39);
    const absorbedDebtAmount = u128le(d, 43);
    const vaultSupplyExchangePrice = u64le(d, 99);
    const vaultBorrowExchangePrice = u64le(d, 107);
    const lastUpdateTimestamp = u64le(d, 119);
    if (
      vaultId === undefined ||
      branchLiquidated === undefined ||
      topmostTick === undefined ||
      currentBranchId === undefined ||
      totalBranchId === undefined ||
      totalSupply === undefined ||
      totalBorrow === undefined ||
      totalPositions === undefined ||
      absorbedDebtAmount === undefined ||
      vaultSupplyExchangePrice === undefined ||
      vaultBorrowExchangePrice === undefined ||
      lastUpdateTimestamp === undefined
    ) {
      return undefined;
    }
    const v = new VaultState();
    v.vaultId = vaultId;
    v.branchLiquidated = branchLiquidated;
    v.topmostTick = topmostTick;
    v.currentBranchId = currentBranchId;
    v.totalBranchId = totalBranchId;
    v.totalSupply = totalSupply;
    v.totalBorrow = totalBorrow;
    v.totalPositions = totalPositions;
    v.absorbedDebtAmount = absorbedDebtAmount;
    v.vaultSupplyExchangePrice = vaultSupplyExchangePrice;
    v.vaultBorrowExchangePrice = vaultBorrowExchangePrice;
    v.lastUpdateTimestamp = Number(lastUpdateTimestamp);
    return v;
  }
}

// ── Oracle account — the ordered price-source pubkeys (the `oracleSources`) ──
// The vault's `oracle` (owned by `oracleProgram`) is an Anchor `Oracle` account:
//   disc[8] · nonce u16@8 · sources Vec<Sources> (u32 len@10, entries@14) · bump u8.
// Each `Sources` (borsh, in decl order) is `source: Pubkey(32) · invert: bool(1) ·
// multiplier: u128(16) · divisor: u128(16) · source_type: enum u8(1)` = 66 bytes.
// The oracle CPI requires `remaining_accounts.len() == sources.len()` and checks
// `remaining_accounts[i].key() == sources[i].source` (verify_source), so the
// `oracleSources` a liquidate must pass are exactly `[s.source for s in sources]`
// in order — fully derivable from on-chain state, no captured tx needed.
// (Source layout from code-423n4/2026-02-jupiter-lend programs/oracle/src/state.)
export const ORACLE_SOURCE_STRIDE = 66;

export function decodeOracleSources(raw: Buffer): PublicKey[] | undefined {
  const n = u32le(raw, 10);
  if (n === undefined) return undefined;
  const out: PublicKey[] = [];
  for (let i = 0; i < n; i++) {
    const pk = readPk(raw, 14 + i * ORACLE_SOURCE_STRIDE);
    if (pk === undefined) return undefined;
    out.push(pk);
  }
  return out;
}

/** A vault = its config + state, keyed by vaultId. */
export class Vault {
  configPubkey!: PublicKey;
  statePubkey!: PublicKey;
  config!: VaultConfig;
  state!: VaultState;

  /**
   * FIRST-PASS liquidatable signal. HONEST STATUS:
   *  - CONFIDENT: `absorbedDebtAmount > 0` means the vault currently holds
   *    absorbed/pending-liquidation debt — a real, unambiguous signal that
   *    something is (or just was) being liquidated in this vault.
   *  - NOT USED: `branchLiquidated` is decoded but appears to be a historical
   *    branch counter (nonzero on ~1/3 of vaults with zero absorbed debt), so
   *    it is NOT a "liquidatable now" signal and is excluded from this check.
   *  - INFERRED / NOT YET IMPLEMENTED: precise "is there liquidatable debt at
   *    the current price" needs Fluid's tick<->price math — comparing
   *    `topmostTick` to the tick implied by the live oracle price and
   *    `liquidationThreshold`. That formula is NOT reversed here. This method
   *    only surfaces vaults with absorbed debt; the executor's on-chain
   *    liquidate simulation is the ground-truth gate (as with the other
   *    protocols). Do NOT treat `true` as "definitely liquidatable," nor
   *    `false` as "definitely healthy."
   */
  maybeLiquidatable(): boolean {
    return this.state.totalBorrow > 0n && this.state.absorbedDebtAmount > 0n;
  }
}
