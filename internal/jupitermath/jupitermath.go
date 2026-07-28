// Package jupitermath ports Fluid (Jupiter Lend) tick<->price math +
// liquidate account-selection, reversed from the on-chain program source
// (code-423n4/2026-02-jupiter-lend) and the published TS SDK. Everything
// here is a line-for-line port of the authoritative Rust/TS, so the
// on-chain program is the spec.
//
// WHAT THIS SOLVES (the two pieces that kept `try_arm` returning None):
//
//  1. `col_per_unit_debt` — the liquidate ix's collateral-per-unit-debt arg.
//     HONEST FRAMING (verified against 8 real mainnet liquidate txs, see
//     jupiter_fire_probe): this arg is NOT the price. It is a *minimum
//     acceptable collateral-per-debt slippage floor in 1e15 decimals*. The
//     program computes the ACTUAL price itself from the vault oracle
//     (`get_ticks_from_oracle_price`) and only reverts
//     (VaultExcessSlippageLiquidation) if the realized
//     `actual_col*1e15/actual_debt < col_per_unit_debt`. Real liquidators
//     pass either 0 (accept the oracle price — seen in 2/8 txs) or a
//     computed floor (~1.1e13-1.5e13 — seen in the other 6). We reproduce
//     the exact on-chain formula in ComputeColPerDebt, and the executor
//     sources the live value from the program's own resolver revert
//     (to=ADDRESS_DEAD -> `VaultLiquidationResult:[col,debt,tick]`), which
//     is exact ground truth.
//
//  2. `remaining_accounts_indices` + the tick/branch account set — layout
//     `[oracle_sources, branches, ticks, tick_has_debt]` (verified: sums to
//     the remaining-account count on all 8 real txs). The PDAs and the
//     selection rule are ported from the SDK's `getRemainingAccountsLiquidate`.
package jupitermath

import (
	"math/big"

	"arbengine/internal/solana"
)

// ── u256 mul_div (matches on-chain `safe_multiply_divide` semantics) ────────
//
// Fluid's ratio math multiplies two u128s (product can exceed u128) then
// divides by a u128. Go has no native u128, so we use math/big for the
// 256-bit-intermediate arithmetic; the results are bit-identical to the
// Rust wide-multiply/long-division implementation.

// MulDivFloor computes floor((a * b) / d). Panics only on d==0 (callers guard).
func MulDivFloor(a, b, d *big.Int) *big.Int {
	prod := new(big.Int).Mul(a, b)
	q := new(big.Int)
	q.Div(prod, d)
	return q
}

// MulDivCeil computes ceil((a * b) / d).
func MulDivCeil(a, b, d *big.Int) *big.Int {
	prod := new(big.Int).Mul(a, b)
	q, r := new(big.Int), new(big.Int)
	q.DivMod(prod, d, r)
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}

// ── TickMath: ratioX48 = (1.0015^tick) * 2^48 ───────────────────────────────
// Exact port of crates/library/src/math/tick.rs.

const (
	MinTick     int32 = -16383
	MaxTick     int32 = 16383
	TickSpacing       = 10015
	// ColdTick mirrors Rust's i32::MIN sentinel.
	ColdTick int32 = -2147483648
)

var (
	factor00 = mustBigFromHex("10000000000000000")
	factor01 = mustBigFromHex("ff9dd7de423466c2")
	factor02 = mustBigFromHex("ff3bd55f4488ad27")
	factor03 = mustBigFromHex("fe78410fd6498b74")
	factor04 = mustBigFromHex("fcf2d9987c9be179")
	factor05 = mustBigFromHex("f9ef02c4529258b0")
	factor06 = mustBigFromHex("f402d288133a85a1")
	factor07 = mustBigFromHex("e895615b5beb6386")
	factor08 = mustBigFromHex("d34f17a00ffa00a8")
	factor09 = mustBigFromHex("ae6b7961714e2055")
	factor10 = mustBigFromHex("76d6461f27082d75")
	factor11 = mustBigFromHex("372a3bfe0745d8b7")
	factor12 = mustBigFromHex("be32cbee4897976")
	factor13 = mustBigFromHex("8d4f70c9ff4925")
	factor14 = mustBigFromHex("4e009ae55194")
)

func mustBigFromHex(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 16)
	if !ok {
		panic("jupitermath: bad hex literal " + s)
	}
	return n
}

// MinRatioX48 / MaxRatioX48 bound the valid input range for GetTickAtRatio.
var (
	MinRatioX48         = big.NewInt(6093)
	MaxRatioX48         = mustBigFromDec("13002088133096036565414295")
	ZeroTickScaledRatio = new(big.Int).Lsh(big.NewInt(1), 48) // 1 << 48
	oneE13              = mustBigFromDec("10000000000000")
)

func mustBigFromDec(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("jupitermath: bad decimal literal " + s)
	}
	return n
}

var (
	mask128 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
	u128Max = new(big.Int).Set(mask128)
)

// mulShift64 computes (n0*n1) >> 64. n0,n1 < 2^64 across the whole calc, so
// n0*n1 < 2^128 fits comfortably (mirrors the Rust wrapping_mul which never
// actually wraps here).
func mulShift64(n0, n1 *big.Int) *big.Int {
	prod := new(big.Int).Mul(n0, n1)
	return new(big.Int).Rsh(prod, 64)
}

// GetRatioAtTick mirrors on-chain get_ratio_at_tick. Returns (ratio, ok).
func GetRatioAtTick(tick int32) (*big.Int, bool) {
	if tick < MinTick || tick > MaxTick {
		return nil, false
	}
	absTick := uint32(tick)
	if tick < 0 {
		absTick = uint32(-tick)
	}
	factor := new(big.Int).Set(factor00)
	if absTick&0x1 != 0 {
		factor = new(big.Int).Set(factor01)
	}
	if absTick&0x2 != 0 {
		factor = mulShift64(factor, factor02)
	}
	if absTick&0x4 != 0 {
		factor = mulShift64(factor, factor03)
	}
	if absTick&0x8 != 0 {
		factor = mulShift64(factor, factor04)
	}
	if absTick&0x10 != 0 {
		factor = mulShift64(factor, factor05)
	}
	if absTick&0x20 != 0 {
		factor = mulShift64(factor, factor06)
	}
	if absTick&0x40 != 0 {
		factor = mulShift64(factor, factor07)
	}
	if absTick&0x80 != 0 {
		factor = mulShift64(factor, factor08)
	}
	if absTick&0x100 != 0 {
		factor = mulShift64(factor, factor09)
	}
	if absTick&0x200 != 0 {
		factor = mulShift64(factor, factor10)
	}
	if absTick&0x400 != 0 {
		factor = mulShift64(factor, factor11)
	}
	if absTick&0x800 != 0 {
		factor = mulShift64(factor, factor12)
	}
	if absTick&0x1000 != 0 {
		factor = mulShift64(factor, factor13)
	}
	if absTick&0x2000 != 0 {
		factor = mulShift64(factor, factor14)
	}

	precision := big.NewInt(0)
	if tick >= 0 {
		factor = new(big.Int).Div(u128Max, factor)
		mod := new(big.Int).Mod(factor, big.NewInt(0x10000))
		if mod.Sign() != 0 {
			precision = big.NewInt(1)
		}
	}
	ratio := new(big.Int).Add(new(big.Int).Rsh(factor, 16), precision)
	return ratio, true
}

// GetTickAtRatio mirrors on-chain get_tick_at_ratio. Returns (tick,
// perfectRatioX48, ok).
func GetTickAtRatio(ratioX48 *big.Int) (int32, *big.Int, bool) {
	if ratioX48.Cmp(MinRatioX48) < 0 || ratioX48.Cmp(MaxRatioX48) > 0 {
		return 0, nil, false
	}
	isNegative := ratioX48.Cmp(ZeroTickScaledRatio) < 0
	var factor *big.Int
	if isNegative {
		factor = MulDivFloor(ZeroTickScaledRatio, oneE13, ratioX48)
	} else {
		factor = MulDivFloor(ratioX48, oneE13, ZeroTickScaledRatio)
	}
	var tick int32
	steps := []struct {
		thresh string
		bit    int32
	}{
		{"2150859953785115391", 0x2000},
		{"4637736467054931", 0x1000},
		{"215354044936586", 0x800},
		{"46406254420777", 0x400},
		{"21542110950596", 0x200},
		{"14677230989051", 0x100},
		{"12114962232319", 0x80},
		{"11006798913544", 0x40},
		{"10491329235871", 0x20},
		{"10242718992470", 0x10},
		{"10120631893548", 0x8},
		{"10060135135051", 0x4},
		{"10030022500000", 0x2},
	}
	for _, s := range steps {
		thresh := mustBigFromDec(s.thresh)
		if factor.Cmp(thresh) >= 0 {
			tick |= s.bit
			factor = MulDivFloor(factor, oneE13, thresh)
		}
	}
	// last step (2^0 = 1) does not divide-through in the reference for the
	// perfect-ratio branch, matching upstream.
	tenBillion := mustBigFromDec("10015000000000")
	if factor.Cmp(tenBillion) >= 0 {
		tick |= 0x1
		factor = MulDivFloor(factor, oneE13, tenBillion)
	}

	var perfect *big.Int
	if isNegative {
		tick = ^tick
		perfect = MulDivFloor(ratioX48, factor, tenBillion)
	} else {
		perfect = MulDivFloor(ratioX48, oneE13, factor)
	}
	if perfect.Cmp(ratioX48) > 0 {
		return 0, nil, false
	}
	return tick, perfect, true
}

// ── col_per_debt (get_ticks_from_oracle_price) ───────────────────────────────
const (
	RateOutputDecimals uint32 = 15
	FourDecimals              = 10_000
	ThreeDecimals             = 1_000
)

var (
	tenPow24 = new(big.Int).Exp(big.NewInt(10), big.NewInt(24), nil)
	tenPow26 = new(big.Int).Exp(big.NewInt(10), big.NewInt(26), nil)
	tenPow30 = new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(RateOutputDecimals*2)), nil)
	tenPow8  = new(big.Int).Exp(big.NewInt(10), big.NewInt(8), nil)
	tenPow15 = new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(RateOutputDecimals)), nil)
)

// ComputeColPerDebt is an exact port of `get_ticks_from_oracle_price`
// (utils/liquidate.rs).
//
// Inputs (all as the program sees them):
//   - exchangeRate: oracle liquidate exchange rate, 1e15 (RateOutputDecimals)
//   - supplyExPrice, borrowExPrice: vault exchange prices (1e12 precision)
//   - liquidationPenalty: VaultConfig raw (1e4 = 100%, e.g. 100 = 1%)
//   - liquidationThreshold, liquidationMaxLimit: VaultConfig raw (1e3 = 100%)
//
// Returns (colPerDebt, liquidationTick, maxTick, ok) where colPerDebt is the
// 1e15-scaled collateral-per-debt (already including the liquidation
// penalty) — i.e. the natural, protective value for the `col_per_unit_debt`
// arg.
func ComputeColPerDebt(
	exchangeRate, supplyExPrice, borrowExPrice *big.Int,
	liquidationPenalty, liquidationThreshold, liquidationMaxLimit uint16,
) (colPerDebt *big.Int, liquidationTick int32, maxTick int32, ok bool) {
	if exchangeRate.Sign() == 0 || exchangeRate.Cmp(tenPow24) > 0 || borrowExPrice.Sign() == 0 {
		return nil, 0, 0, false
	}
	debtPerCol := MulDivFloor(exchangeRate, supplyExPrice, borrowExPrice)
	if debtPerCol.Sign() == 0 {
		return nil, 0, 0, false
	}
	if debtPerCol.Cmp(tenPow26) > 0 {
		debtPerCol = new(big.Int).Set(tenPow26)
	}
	debtPerColCeil := MulDivCeil(exchangeRate, supplyExPrice, borrowExPrice)
	if debtPerColCeil.Cmp(tenPow26) > 0 {
		debtPerColCeil = new(big.Int).Set(tenPow26)
	}
	if debtPerColCeil.Sign() < 1 {
		debtPerColCeil = big.NewInt(1)
	}

	rawColPerDebt := new(big.Int).Div(tenPow30, debtPerColCeil)
	colPerDebt = new(big.Int).Mul(rawColPerDebt, new(big.Int).Add(big.NewInt(FourDecimals), big.NewInt(int64(liquidationPenalty))))
	colPerDebt = colPerDebt.Div(colPerDebt, big.NewInt(FourDecimals))

	liquidationRatio := MulDivFloor(debtPerCol, ZeroTickScaledRatio, tenPow15)
	thresholdRatio := new(big.Int).Mul(liquidationRatio, big.NewInt(int64(liquidationThreshold)))
	thresholdRatio.Div(thresholdRatio, big.NewInt(ThreeDecimals))
	liqTick, _, ok1 := GetTickAtRatio(thresholdRatio)
	if !ok1 {
		return nil, 0, 0, false
	}
	maxRatio := new(big.Int).Mul(liquidationRatio, big.NewInt(int64(liquidationMaxLimit)))
	maxRatio.Div(maxRatio, big.NewInt(ThreeDecimals))
	mTick, _, ok2 := GetTickAtRatio(maxRatio)
	if !ok2 {
		return nil, 0, 0, false
	}

	return colPerDebt, liqTick, mTick, true
}

// LiquidationTickFromPrice1e8 computes the SDK-style liquidation tick
// straight from an oracle price given in 1e8 (used by
// `getRemainingAccountsLiquidate` for account selection):
//
//	liquidationRatio       = price * 2^48 / 1e8
//	liquidationThreshold   = liquidationRatio * liquidation_threshold / 1e3
//	liquidationTick        = getTickAtRatio(liquidationThreshold)
func LiquidationTickFromPrice1e8(price1e8 *big.Int, liquidationThreshold uint16) (int32, bool) {
	liqRatio := MulDivFloor(price1e8, ZeroTickScaledRatio, tenPow8)
	thr := new(big.Int).Mul(liqRatio, big.NewInt(int64(liquidationThreshold)))
	thr.Div(thr, big.NewInt(ThreeDecimals))
	tick, _, ok := GetTickAtRatio(thr)
	return tick, ok
}

// LiquidationTickFromColPerDebt recovers the liquidation tick from a
// `col_per_unit_debt` value a real liquidator passed (~ the on-chain
// `col_per_debt`, which already includes the penalty), inverting
// ComputeColPerDebt. Lets us reconstruct the tick band for account
// selection without the oracle when a live price isn't handy (used by the
// probe; production uses LiquidationTickFromPrice1e8 off Lazer).
func LiquidationTickFromColPerDebt(colPerUnitDebt *big.Int, liquidationPenalty, liquidationThreshold uint16) (int32, bool) {
	if colPerUnitDebt.Sign() == 0 {
		return 0, false
	}
	rawColPerDebt := new(big.Int).Mul(colPerUnitDebt, big.NewInt(FourDecimals))
	rawColPerDebt.Div(rawColPerDebt, new(big.Int).Add(big.NewInt(FourDecimals), big.NewInt(int64(liquidationPenalty))))
	if rawColPerDebt.Sign() == 0 {
		return 0, false
	}
	debtPerCol := new(big.Int).Div(tenPow30, rawColPerDebt)
	liqRatio := MulDivFloor(debtPerCol, ZeroTickScaledRatio, tenPow15)
	thr := new(big.Int).Mul(liqRatio, big.NewInt(int64(liquidationThreshold)))
	thr.Div(thr, big.NewInt(ThreeDecimals))
	tick, _, ok := GetTickAtRatio(thr)
	return tick, ok
}

// ── PDA derivations (seeds from programs/vaults/src/state/seeds.rs) ─────────
const VaultsProgramID = "jupr81YtYssSyPt8jbnGuiWon5f6x9TcDEFxYe3Bdzi"

var vaultsProgram = solana.MustPubkeyFromBase58(VaultsProgramID)

// BranchPDA derives the `branch` account PDA for a vault/branch id pair.
func BranchPDA(vaultID uint16, branchID uint32) solana.Pubkey {
	pk, _ := solana.FindProgramAddress([][]byte{
		[]byte("branch"),
		le16(vaultID),
		le32(branchID),
	}, vaultsProgram)
	return pk
}

// TickPDA derives the `tick` account PDA. SDK: tick + MaxTick, encoded as
// u32 le (4 bytes).
func TickPDA(vaultID uint16, tick int32) solana.Pubkey {
	key := uint32(tick + MaxTick)
	pk, _ := solana.FindProgramAddress([][]byte{
		[]byte("tick"),
		le16(vaultID),
		le32(key),
	}, vaultsProgram)
	return pk
}

// TickHasDebtPDA derives the `tick_has_debt` account PDA.
func TickHasDebtPDA(vaultID uint16, index uint8) solana.Pubkey {
	pk, _ := solana.FindProgramAddress([][]byte{
		[]byte("tick_has_debt"),
		le16(vaultID),
		{index},
	}, vaultsProgram)
	return pk
}

// NewBranchID computes the `new_branch` account's branch_id passed to
// `liquidate`. The vaults handler VALIDATES its `branch_id` against a value
// computed from vault state (`ErrorCodes::VaultNewBranchInvalid` on
// mismatch):
//   - mid-liquidation (branchLiquidated == 1): a fresh branch,
//     id = totalBranchID + 1 (see VaultState::update_topmost_tick /
//     update_branch_info_by_one in programs/vaults/src/state/vault_state.rs);
//   - otherwise the current branch is reused, id = currentBranchID.
//
// So new_branch = BranchPDA(vaultID, NewBranchID(state)).
func NewBranchID(branchLiquidated uint8, currentBranchID, totalBranchID uint32) uint32 {
	if branchLiquidated == 1 {
		return totalBranchID + 1
	}
	return currentBranchID
}

// ── Fluid Liquidity-program PDAs ─────────────────────────────────────────────
// Seeds reversed from the on-chain Anchor source (audit repo
// code-423n4/2026-02-jupiter-lend, programs/liquidity/src/state/{seeds,context}.rs)
// and VALIDATED against live mainnet accounts (owner == liquidity program;
// token accounts == SPL accounts owned by `liquidity_pda`). The vaults
// `liquidate` CPIs into the liquidity program, which RE-DERIVES each of
// these via its own `#[account(seeds=[...])]` constraints — so a wrong seed
// reverts (ConstraintSeeds) at simulation, making the on-chain program the
// ground-truth check.
const LiquidityProgramID = "jupeiUmn818Jg1ekPURTpr4mFo29p46vygyykFJ3wZC"

var liqProgram = solana.MustPubkeyFromBase58(LiquidityProgramID)

// LiquidityPDA is the liquidity program's global singleton state PDA. Also
// the token authority (ATA owner) for every vault token account. Seeds
// [b"liquidity"].
func LiquidityPDA() solana.Pubkey {
	pk, _ := solana.FindProgramAddress([][]byte{[]byte("liquidity")}, liqProgram)
	return pk
}

// ReservePDA derives {supply,borrow}_token_reserves_liquidity — per-mint
// TokenReserve. Seeds [b"reserve", mint].
func ReservePDA(mint solana.Pubkey) solana.Pubkey {
	pk, _ := solana.FindProgramAddress([][]byte{[]byte("reserve"), mint.Bytes()}, liqProgram)
	return pk
}

// RateModelPDA derives {supply,borrow}_rate_model — per-mint RateModel.
// Seeds [b"rate_model", mint].
func RateModelPDA(mint solana.Pubkey) solana.Pubkey {
	pk, _ := solana.FindProgramAddress([][]byte{[]byte("rate_model"), mint.Bytes()}, liqProgram)
	return pk
}

// UserSupplyPositionPDA derives vault_supply_position_on_liquidity — the
// vault's supply user-position at the liquidity layer. vaultConfig = the
// vault's vault_config PDA (the vault_config is the PDA that signs the
// liquidity CPI). Seeds [b"user_supply_position", supply_mint, vault_config].
func UserSupplyPositionPDA(supplyMint, vaultConfig solana.Pubkey) solana.Pubkey {
	pk, _ := solana.FindProgramAddress([][]byte{
		[]byte("user_supply_position"), supplyMint.Bytes(), vaultConfig.Bytes(),
	}, liqProgram)
	return pk
}

// UserBorrowPositionPDA derives vault_borrow_position_on_liquidity. Seeds
// [b"user_borrow_position", borrow_mint, vault_config].
func UserBorrowPositionPDA(borrowMint, vaultConfig solana.Pubkey) solana.Pubkey {
	pk, _ := solana.FindProgramAddress([][]byte{
		[]byte("user_borrow_position"), borrowMint.Bytes(), vaultConfig.Bytes(),
	}, liqProgram)
	return pk
}

// UserClaimPDA derives supply_token_claim_account — a UserClaim. Seeds
// [b"user_claim", user, mint]. On the liquidate hot path the liquidity
// program only enforces has_one = mint (not the seeds), and the vault
// passes it as an unchecked account with user = vault_config; validated
// live by the probe (existence + owner + mint).
func UserClaimPDA(user, mint solana.Pubkey) solana.Pubkey {
	pk, _ := solana.FindProgramAddress([][]byte{
		[]byte("user_claim"), user.Bytes(), mint.Bytes(),
	}, liqProgram)
	return pk
}

func le16(v uint16) []byte { return []byte{byte(v), byte(v >> 8)} }
func le32(v uint32) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)} }

// ── tick_has_debt bitmap: index math + next-tick walk ────────────────────────
const (
	TickHasDebtArraySize                = 8
	TickHasDebtChildrenSize             = 32                          // bytes
	TickHasDebtChildrenSizeInBits       = TickHasDebtChildrenSize * 8 // 256
	TicksPerTickHasDebt           int32 = TickHasDebtArraySize * 256  // 2048
)

// IndexForTick reports which tick_has_debt array (0..15) a tick lives in.
func IndexForTick(tick int32) uint8 {
	idx := (tick - MinTick) / TicksPerTickHasDebt
	if idx > 15 {
		return 15
	}
	return uint8(idx)
}

func firstTickForIndex(index uint8) int32 {
	return MinTick + int32(index)*TicksPerTickHasDebt
}

// TickIndices computes (arrayIndex, mapIndex, byteIndex, bitIndex) for a
// tick — mirrors getTickIndices.
func TickIndices(tick int32) (arrayIndex uint8, mapIndex, byteIndex, bitIndex int) {
	arrayIndex = IndexForTick(tick)
	first := firstTickForIndex(arrayIndex)
	withinArray := int(tick - first) // 0..2047
	mapIndex = withinArray / TickHasDebtChildrenSizeInBits
	withinMap := withinArray % TickHasDebtChildrenSizeInBits
	return arrayIndex, mapIndex, withinMap / 8, withinMap % 8
}

// childrenByte returns children_bits[byte] for (array index, mapIndex) out
// of raw account bytes of a TickHasDebtArray. Layout (repr(C,packed)):
// disc[8] vault_id[2] index[1] then tick_has_debt[8] x children_bits[32].
func childrenByte(raw []byte, mapIndex, byteIndex int) (byte, bool) {
	off := 11 + mapIndex*TickHasDebtChildrenSize + byteIndex
	if off < 0 || off >= len(raw) {
		return 0, false
	}
	return raw[off], true
}

func mostSignificantBit(b byte) int {
	// leading zeros within 8 bits
	if b == 0 {
		return 8
	}
	lz := 0
	mask := byte(0x80)
	for b&mask == 0 && lz < 8 {
		lz++
		mask >>= 1
	}
	return lz
}

// ArrayFetcher reads raw bytes of tick_has_debt array `index` for a vault.
type ArrayFetcher func(index uint8) ([]byte, bool)

// FindNextTickWithDebt is a port of the SDK `findNextTickWithDebt`: given
// startTick, walk down the bitmaps to the next lower tick that has debt,
// using fetch to load raw TickHasDebtArray bytes by index. Returns MinTick
// if none.
func FindNextTickWithDebt(startTick int32, fetch ArrayFetcher) int32 {
	arrayIndex, mapIndex, byteIndex, bitIndex := TickIndices(startTick)
	currentArray := arrayIndex
	currentMap := mapIndex
	raw, ok := fetch(currentArray)
	if !ok {
		return MinTick
	}
	buf := append([]byte(nil), raw...)
	clearBits(buf, mapIndex, byteIndex, bitIndex)
	raw = buf
	for {
		if nt, ok := nextTopTick(raw, currentArray, currentMap); ok {
			return nt
		}
		if currentArray == 0 {
			return MinTick
		}
		currentArray--
		currentMap = TickHasDebtArraySize - 1
		r, ok := fetch(currentArray)
		if !ok {
			return MinTick
		}
		raw = r
	}
}

func clearBits(raw []byte, mapIndex, byteIndex, bitIndex int) {
	base := 11 + mapIndex*TickHasDebtChildrenSize
	if base+TickHasDebtChildrenSize > len(raw) {
		return
	}
	if bitIndex > 0 {
		mask := byte((uint16(1) << uint(bitIndex)) - 1)
		raw[base+byteIndex] &= mask
	} else {
		raw[base+byteIndex] = 0
	}
	for b := byteIndex + 1; b < TickHasDebtChildrenSize; b++ {
		raw[base+b] = 0
	}
}

func nextTopTick(raw []byte, arrayIndex uint8, startMap int) (int32, bool) {
	for m := startMap; m >= 0; m-- {
		for byteIdx := TickHasDebtChildrenSize - 1; byteIdx >= 0; byteIdx-- {
			b, ok := childrenByte(raw, m, byteIdx)
			if !ok {
				return 0, false
			}
			if b != 0 {
				bitPos := 7 - mostSignificantBit(b)
				tickWithinMap := byteIdx*8 + bitPos
				mapFirst := firstTickForIndex(arrayIndex) + int32(m*TickHasDebtChildrenSizeInBits)
				return mapFirst + int32(tickWithinMap), true
			}
		}
	}
	return 0, false
}

// ── Branch account decode (only what selection needs) ────────────────────────
// repr(C,packed): disc[8] vault_id u16@8 branch_id u32@10 status u8@14
// minima_tick i32@15 minima_tick_partials u32@19 debt_liquidity u64@23
// debt_factor u64@31 connected_branch_id u32@39 connected_minima_tick i32@43
type BranchLite struct {
	BranchID          uint32
	Status            uint8
	ConnectedBranchID uint32
}

// DecodeBranchLite decodes the subset of a Branch account selection needs.
func DecodeBranchLite(raw []byte) (BranchLite, bool) {
	if len(raw) < 43 {
		return BranchLite{}, false
	}
	return BranchLite{
		BranchID:          u32le(raw, 10),
		Status:            raw[14],
		ConnectedBranchID: u32le(raw, 39),
	}, true
}

func u32le(d []byte, o int) uint32 {
	return uint32(d[o]) | uint32(d[o+1])<<8 | uint32(d[o+2])<<16 | uint32(d[o+3])<<24
}
