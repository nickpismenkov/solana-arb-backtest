// Fluid (Jupiter Lend) tick↔price math + liquidate account-selection,
// reversed from the on-chain program source and the published TS SDK.
//
//  1. col_per_unit_debt — the liquidate ix's collateral-per-unit-debt arg.
//     This arg is NOT the price. It is a *minimum acceptable collateral-per-
//     debt slippage floor in 1e15 decimals*. The program computes the ACTUAL
//     price itself from the vault oracle and only reverts if the realized
//     ratio falls below it.
//  2. remaining_accounts_indices + the tick/branch account set — layout
//     [oracle_sources, branches, ticks, tick_has_debt].
package jupiterlend

import (
	"encoding/binary"
	"math/big"

	"github.com/gagliardetto/solana-go"
)

// mulDivFloor is floor((a*b)/d), computed exactly via big.Int (replaces the
// original Rust's hand-rolled 256-bit long division — Go's math/big already
// does exact arbitrary-precision arithmetic).
func mulDivFloor(a, b, d *big.Int) *big.Int {
	p := new(big.Int).Mul(a, b)
	q := new(big.Int).Div(p, d) // Euclidean; a,b,d are always non-negative here
	return q
}

// mulDivCeil is ceil((a*b)/d).
func mulDivCeil(a, b, d *big.Int) *big.Int {
	p := new(big.Int).Mul(a, b)
	q, r := new(big.Int).QuoRem(p, d, new(big.Int))
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}

func bi(v uint64) *big.Int { return new(big.Int).SetUint64(v) }

func pow10Big(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// TickMath — exact port of crates/library/src/math/tick.rs.
const (
	MinTick  int32 = -16383
	MaxTick  int32 = 16383
	ColdTick int32 = -1 << 31
)

var (
	tickSpacing128      = big.NewInt(10015)
	minRatioX48         = big.NewInt(6093)
	maxRatioX48, _      = new(big.Int).SetString("13002088133096036565414295", 10)
	zeroTickScaledRatio = new(big.Int).Lsh(big.NewInt(1), 48) // 1 << 48
	oneE13              = big.NewInt(10000000000000)
)

var factors = []*big.Int{
	hex("0x10000000000000000"),
	hex("0xff9dd7de423466c2"),
	hex("0xff3bd55f4488ad27"),
	hex("0xfe78410fd6498b74"),
	hex("0xfcf2d9987c9be179"),
	hex("0xf9ef02c4529258b0"),
	hex("0xf402d288133a85a1"),
	hex("0xe895615b5beb6386"),
	hex("0xd34f17a00ffa00a8"),
	hex("0xae6b7961714e2055"),
	hex("0x76d6461f27082d75"),
	hex("0x372a3bfe0745d8b7"),
	hex("0xbe32cbee4897976"),
	hex("0x8d4f70c9ff4925"),
	hex("0x4e009ae55194"),
}

func hex(s string) *big.Int {
	v, _ := new(big.Int).SetString(s[2:], 16)
	return v
}

var maxU128, _ = new(big.Int).SetString("340282366920938463463374607431768211455", 10)

func mulShift64(n0, n1 *big.Int) *big.Int {
	p := new(big.Int).Mul(n0, n1)
	return p.Rsh(p, 64)
}

// GetRatioAtTick returns (1.0015^tick) * 2^48 as an exact integer, mirroring
// the on-chain program.
func GetRatioAtTick(tick int32) (*big.Int, bool) {
	if tick < MinTick || tick > MaxTick {
		return nil, false
	}
	absTick := tick
	if absTick < 0 {
		absTick = -absTick
	}
	factor := new(big.Int).Set(factors[0])
	bitFactor := func(mask int32, idx int) {
		if absTick&mask != 0 {
			if idx == 1 {
				factor = new(big.Int).Set(factors[1])
			} else {
				factor = mulShift64(factor, factors[idx])
			}
		}
	}
	bitFactor(0x1, 1)
	bitFactor(0x2, 2)
	bitFactor(0x4, 3)
	bitFactor(0x8, 4)
	bitFactor(0x10, 5)
	bitFactor(0x20, 6)
	bitFactor(0x40, 7)
	bitFactor(0x80, 8)
	bitFactor(0x100, 9)
	bitFactor(0x200, 10)
	bitFactor(0x400, 11)
	bitFactor(0x800, 12)
	bitFactor(0x1000, 13)
	bitFactor(0x2000, 14)

	precision := big.NewInt(0)
	if tick >= 0 {
		factor = new(big.Int).Div(maxU128, factor)
		mod := new(big.Int).Mod(factor, big.NewInt(0x10000))
		if mod.Sign() != 0 {
			precision = big.NewInt(1)
		}
	}
	result := new(big.Int).Rsh(factor, 16)
	result.Add(result, precision)
	return result, true
}

// GetTickAtRatio returns (tick, perfectRatioX48). Mirrors on-chain get_tick_at_ratio.
func GetTickAtRatio(ratioX48 *big.Int) (int32, *big.Int, bool) {
	if ratioX48.Cmp(minRatioX48) < 0 || ratioX48.Cmp(maxRatioX48) > 0 {
		return 0, nil, false
	}
	isNegative := ratioX48.Cmp(zeroTickScaledRatio) < 0
	var factor *big.Int
	if isNegative {
		factor = mulDivFloor(zeroTickScaledRatio, oneE13, ratioX48)
	} else {
		factor = mulDivFloor(ratioX48, oneE13, zeroTickScaledRatio)
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
		thresh, _ := new(big.Int).SetString(s.thresh, 10)
		if factor.Cmp(thresh) >= 0 {
			tick |= s.bit
			factor = mulDivFloor(factor, oneE13, thresh)
		}
	}
	tenThousand15 := big.NewInt(10015000000000)
	if factor.Cmp(tenThousand15) >= 0 {
		tick |= 0x1
		factor = mulDivFloor(factor, oneE13, tenThousand15)
	}

	var perfect *big.Int
	if isNegative {
		tick = ^tick
		perfect = mulDivFloor(ratioX48, factor, tenThousand15)
	} else {
		perfect = mulDivFloor(ratioX48, oneE13, factor)
	}
	if perfect.Cmp(ratioX48) > 0 {
		return 0, nil, false
	}
	return tick, perfect, true
}

// ── col_per_debt (get_ticks_from_oracle_price) ───────────────────────────────
const RateOutputDecimals = 15

var (
	fourDecimals  = big.NewInt(10_000)
	threeDecimals = big.NewInt(1_000)
)

// ComputeColPerDebt is an exact port of get_ticks_from_oracle_price
// (utils/liquidate.rs). Returns (colPerDebt, liquidationTick, maxTick).
func ComputeColPerDebt(
	exchangeRate, supplyExPrice, borrowExPrice *big.Int,
	liquidationPenalty, liquidationThreshold, liquidationMaxLimit uint16,
) (*big.Int, int32, int32, bool) {
	e24 := pow10Big(24)
	if exchangeRate.Sign() == 0 || exchangeRate.Cmp(e24) > 0 || borrowExPrice.Sign() == 0 {
		return nil, 0, 0, false
	}
	e26 := pow10Big(26)
	debtPerCol := mulDivFloor(exchangeRate, supplyExPrice, borrowExPrice)
	if debtPerCol.Sign() == 0 {
		return nil, 0, 0, false
	}
	if debtPerCol.Cmp(e26) > 0 {
		debtPerCol = e26
	}
	debtPerColCeil := mulDivCeil(exchangeRate, supplyExPrice, borrowExPrice)
	if debtPerColCeil.Cmp(e26) > 0 {
		debtPerColCeil = e26
	}
	if debtPerColCeil.Sign() < 1 {
		debtPerColCeil = big.NewInt(1)
	}

	rawColPerDebt := new(big.Int).Div(pow10Big(RateOutputDecimals*2), debtPerColCeil)
	colPerDebt := new(big.Int).Mul(rawColPerDebt, new(big.Int).Add(fourDecimals, bi(uint64(liquidationPenalty))))
	colPerDebt.Div(colPerDebt, fourDecimals)

	liquidationRatio := mulDivFloor(debtPerCol, zeroTickScaledRatio, pow10Big(RateOutputDecimals))
	thresholdRatio := new(big.Int).Mul(liquidationRatio, bi(uint64(liquidationThreshold)))
	thresholdRatio.Div(thresholdRatio, threeDecimals)
	liquidationTick, _, ok := GetTickAtRatio(thresholdRatio)
	if !ok {
		return nil, 0, 0, false
	}
	maxRatio := new(big.Int).Mul(liquidationRatio, bi(uint64(liquidationMaxLimit)))
	maxRatio.Div(maxRatio, threeDecimals)
	maxTick, _, ok := GetTickAtRatio(maxRatio)
	if !ok {
		return nil, 0, 0, false
	}
	return colPerDebt, liquidationTick, maxTick, true
}

// LiquidationTickFromPrice1e8 is the SDK-style liquidation tick straight from
// an oracle price given in 1e8 (used by getRemainingAccountsLiquidate for
// account selection).
func LiquidationTickFromPrice1e8(price1e8 *big.Int, liquidationThreshold uint16) (int32, bool) {
	liqRatio := mulDivFloor(price1e8, zeroTickScaledRatio, pow10Big(8))
	thr := new(big.Int).Mul(liqRatio, bi(uint64(liquidationThreshold)))
	thr.Div(thr, threeDecimals)
	tick, _, ok := GetTickAtRatio(thr)
	return tick, ok
}

// LiquidationTickFromColPerDebt recovers the liquidation tick from a
// col_per_unit_debt value a real liquidator passed, inverting ComputeColPerDebt.
func LiquidationTickFromColPerDebt(colPerUnitDebt *big.Int, liquidationPenalty, liquidationThreshold uint16) (int32, bool) {
	if colPerUnitDebt.Sign() == 0 {
		return 0, false
	}
	rawColPerDebt := new(big.Int).Mul(colPerUnitDebt, fourDecimals)
	rawColPerDebt.Div(rawColPerDebt, new(big.Int).Add(fourDecimals, bi(uint64(liquidationPenalty))))
	if rawColPerDebt.Sign() == 0 {
		return 0, false
	}
	debtPerCol := new(big.Int).Div(pow10Big(RateOutputDecimals*2), rawColPerDebt)
	liqRatio := mulDivFloor(debtPerCol, zeroTickScaledRatio, pow10Big(RateOutputDecimals))
	thr := new(big.Int).Mul(liqRatio, bi(uint64(liquidationThreshold)))
	thr.Div(thr, threeDecimals)
	tick, _, ok := GetTickAtRatio(thr)
	return tick, ok
}

// ── PDA derivations (seeds from programs/vaults/src/state/seeds.rs) ───────────
func vaultsProgram() solana.PublicKey    { return solana.MustPublicKeyFromBase58(VaultsProgram) }
func liquidityProgram() solana.PublicKey { return solana.MustPublicKeyFromBase58(LiquidityProgram) }

func le16(v uint16) []byte { b := make([]byte, 2); b[0] = byte(v); b[1] = byte(v >> 8); return b }
func le32(v uint32) []byte {
	b := make([]byte, 4)
	for i := 0; i < 4; i++ {
		b[i] = byte(v >> (8 * i))
	}
	return b
}

func BranchPDA(vaultID uint16, branchID uint32) solana.PublicKey {
	addr, _, _ := solana.FindProgramAddress([][]byte{[]byte("branch"), le16(vaultID), le32(branchID)}, vaultsProgram())
	return addr
}

// VaultStatePDA derives a vault's VaultState account from its vault_id.
func VaultStatePDA(vaultID uint16) solana.PublicKey {
	addr, _, _ := solana.FindProgramAddress([][]byte{[]byte("vault_state"), le16(vaultID)}, vaultsProgram())
	return addr
}

// TickPDA: SDK: tick + MAX_TICK, encoded as u32 le (4 bytes).
func TickPDA(vaultID uint16, tick int32) solana.PublicKey {
	key := uint32(tick + MaxTick)
	addr, _, _ := solana.FindProgramAddress([][]byte{[]byte("tick"), le16(vaultID), le32(key)}, vaultsProgram())
	return addr
}

func TickHasDebtPDA(vaultID uint16, index uint8) solana.PublicKey {
	addr, _, _ := solana.FindProgramAddress([][]byte{[]byte("tick_has_debt"), le16(vaultID), {index}}, vaultsProgram())
	return addr
}

// NewBranchID: the new_branch account passed to liquidate is validated
// against a value computed from vault state:
//   - mid-liquidation (branch_liquidated == 1): a fresh branch, id = total_branch_id + 1;
//   - otherwise the current branch is reused, id = current_branch_id.
func NewBranchID(branchLiquidated uint8, currentBranchID, totalBranchID uint32) uint32 {
	if branchLiquidated == 1 {
		return totalBranchID + 1
	}
	return currentBranchID
}

// ── Fluid Liquidity-program PDAs ─────────────────────────────────────────────
func LiquidityPDA() solana.PublicKey {
	addr, _, _ := solana.FindProgramAddress([][]byte{[]byte("liquidity")}, liquidityProgram())
	return addr
}
func ReservePDA(mint solana.PublicKey) solana.PublicKey {
	addr, _, _ := solana.FindProgramAddress([][]byte{[]byte("reserve"), mint.Bytes()}, liquidityProgram())
	return addr
}
func RateModelPDA(mint solana.PublicKey) solana.PublicKey {
	addr, _, _ := solana.FindProgramAddress([][]byte{[]byte("rate_model"), mint.Bytes()}, liquidityProgram())
	return addr
}
func UserSupplyPositionPDA(supplyMint, vaultConfig solana.PublicKey) solana.PublicKey {
	addr, _, _ := solana.FindProgramAddress([][]byte{[]byte("user_supply_position"), supplyMint.Bytes(), vaultConfig.Bytes()}, liquidityProgram())
	return addr
}
func UserBorrowPositionPDA(borrowMint, vaultConfig solana.PublicKey) solana.PublicKey {
	addr, _, _ := solana.FindProgramAddress([][]byte{[]byte("user_borrow_position"), borrowMint.Bytes(), vaultConfig.Bytes()}, liquidityProgram())
	return addr
}
func UserClaimPDA(user, mint solana.PublicKey) solana.PublicKey {
	addr, _, _ := solana.FindProgramAddress([][]byte{[]byte("user_claim"), user.Bytes(), mint.Bytes()}, liquidityProgram())
	return addr
}

// ── tick_has_debt bitmap: index math + next-tick walk ────────────────────────
const (
	TickHasDebtArraySize                = 8
	TickHasDebtChildrenSize             = 32                          // bytes
	TickHasDebtChildrenSizeInBits       = TickHasDebtChildrenSize * 8 // 256
	TicksPerTickHasDebt           int32 = TickHasDebtArraySize * 256  // 2048
)

// IndexForTick is which tick_has_debt array (0..15) a tick lives in.
func IndexForTick(tick int32) uint8 {
	idx := (tick - MinTick) / TicksPerTickHasDebt
	if idx > 15 {
		idx = 15
	}
	return uint8(idx)
}
func firstTickForIndex(index uint8) int32 {
	return MinTick + int32(index)*TicksPerTickHasDebt
}

// TickIndices returns (arrayIndex, mapIndex, byteIndex, bitIndex) for a tick
// — mirrors getTickIndices.
func TickIndices(tick int32) (uint8, int, int, int) {
	arrayIndex := IndexForTick(tick)
	first := firstTickForIndex(arrayIndex)
	withinArray := int(tick - first) // 0..2047
	mapIndex := withinArray / TickHasDebtChildrenSizeInBits
	withinMap := withinArray % TickHasDebtChildrenSizeInBits
	return arrayIndex, mapIndex, withinMap / 8, withinMap % 8
}

func childrenByte(raw []byte, mapIndex, byteIndex int) (byte, bool) {
	off := 11 + mapIndex*TickHasDebtChildrenSize + byteIndex
	if off >= len(raw) {
		return 0, false
	}
	return raw[off], true
}

func mostSignificantBit(b byte) int {
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

// ArrayFetcher fetches raw bytes of a tick_has_debt array by index.
type ArrayFetcher func(index uint8) ([]byte, bool)

// FindNextTickWithDebt ports the SDK findNextTickWithDebt: given startTick,
// walk down the bitmaps to the next lower tick that has debt.
func FindNextTickWithDebt(startTick int32, fetch ArrayFetcher) int32 {
	arrayIndex, mapIndex, byteIndex, bitIndex := TickIndices(startTick)
	currentArray := arrayIndex
	currentMap := mapIndex
	raw, ok := fetch(currentArray)
	if !ok {
		return MinTick
	}
	buf := append([]byte{}, raw...)
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
		mask := byte((1 << bitIndex) - 1)
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
type BranchLite struct {
	BranchID          uint32
	Status            uint8
	ConnectedBranchID uint32
}

func DecodeBranchLite(raw []byte) (*BranchLite, bool) {
	if len(raw) < 43 {
		return nil, false
	}
	return &BranchLite{
		BranchID:          binary.LittleEndian.Uint32(raw[10:14]),
		Status:            raw[14],
		ConnectedBranchID: binary.LittleEndian.Uint32(raw[39:43]),
	}, true
}
