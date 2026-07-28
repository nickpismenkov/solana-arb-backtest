// Package jupiterlend implements the Jupiter Lend (Fluid) liquidation data
// layer — vault decoders + first-pass liquidatable detection, tick/price
// math, and the liquidate instruction builder + fire-path.
//
// Jupiter Lend is a FLUID vault-tick protocol, fundamentally different from
// the obligation-based lenders (marginfi/Kamino/Save): liquidation is
// VAULT-LEVEL, not per-borrower. Each vault is an isolated (supply_token =
// collateral, borrow_token = debt) pair; a liquidator buys collateral off the
// vault's liquidation curve at a penalty discount. There is NO obligation
// account.
package jupiterlend

import (
	"encoding/binary"
	"math/big"

	"github.com/gagliardetto/solana-go"
)

const (
	VaultsProgram    = "jupr81YtYssSyPt8jbnGuiWon5f6x9TcDEFxYe3Bdzi"
	LiquidityProgram = "jupeiUmn818Jg1ekPURTpr4mFo29p46vygyykFJ3wZC"
	USDCMint         = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	USDTMint         = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"
	WSOLMint         = "So11111111111111111111111111111111111111112"
)

// Anchor account discriminators (sha256("account:<Name>")[..8]).
var (
	VaultConfigDisc = [8]byte{0x63, 0x56, 0x2b, 0xd8, 0xb8, 0x66, 0x77, 0x4d}
	VaultStateDisc  = [8]byte{0xe4, 0xc4, 0x52, 0xa5, 0x62, 0xd2, 0xeb, 0x98}
)

func readPk(d []byte, o int) (solana.PublicKey, bool) {
	if o+32 > len(d) {
		return solana.PublicKey{}, false
	}
	return solana.PublicKeyFromBytes(d[o : o+32]), true
}
func u16le(d []byte, o int) (uint16, bool) {
	if o+2 > len(d) {
		return 0, false
	}
	return binary.LittleEndian.Uint16(d[o : o+2]), true
}
func u32le(d []byte, o int) (uint32, bool) {
	if o+4 > len(d) {
		return 0, false
	}
	return binary.LittleEndian.Uint32(d[o : o+4]), true
}
func u64le(d []byte, o int) (uint64, bool) {
	if o+8 > len(d) {
		return 0, false
	}
	return binary.LittleEndian.Uint64(d[o : o+8]), true
}
func i32le(d []byte, o int) (int32, bool) {
	v, ok := u32le(d, o)
	return int32(v), ok
}

// ── VaultConfig (219 bytes; VERIFIED against live accounts) ──────────────────
type VaultConfig struct {
	VaultID              uint16
	CollateralFactor     uint16
	LiquidationThreshold uint16
	LiquidationPenalty   uint16
	Oracle               solana.PublicKey
	OracleProgram        solana.PublicKey
	SupplyToken          solana.PublicKey // collateral mint
	BorrowToken          solana.PublicKey // debt mint (what the liquidator repays)
}

func DecodeVaultConfig(d []byte) (*VaultConfig, bool) {
	if len(d) < 219 || !hasDisc(d, VaultConfigDisc) {
		return nil, false
	}
	vaultID, _ := u16le(d, 8)
	collFactor, _ := u16le(d, 14)
	liqThresh, _ := u16le(d, 16)
	liqPenalty, _ := u16le(d, 22)
	oracle, _ := readPk(d, 26)
	oracleProgram, _ := readPk(d, 122)
	supply, _ := readPk(d, 154)
	borrow, _ := readPk(d, 186)
	return &VaultConfig{
		VaultID: vaultID, CollateralFactor: collFactor, LiquidationThreshold: liqThresh,
		LiquidationPenalty: liqPenalty, Oracle: oracle, OracleProgram: oracleProgram,
		SupplyToken: supply, BorrowToken: borrow,
	}, true
}

func hasDisc(d []byte, disc [8]byte) bool {
	if len(d) < 8 {
		return false
	}
	var got [8]byte
	copy(got[:], d[:8])
	return got == disc
}

// LiqThresholdFrac is the liquidation threshold as a fraction (INFERRED scale).
func (c *VaultConfig) LiqThresholdFrac() float64 { return float64(c.LiquidationThreshold) / 1000.0 }

// DebtLabel is the debt-token label for the reachable-set check.
func (c *VaultConfig) DebtLabel() string {
	switch c.BorrowToken.String() {
	case USDCMint:
		return "USDC"
	case USDTMint:
		return "USDT"
	case WSOLMint:
		return "wSOL"
	default:
		return "other"
	}
}

// DebtInScope is the debt we currently target (USDC/USDT/SOL).
func (c *VaultConfig) DebtInScope() bool { return c.DebtLabel() != "other" }

// ── VaultState (127 bytes; VERIFIED against live accounts) ───────────────────
type VaultState struct {
	VaultID                  uint16
	BranchLiquidated         uint8
	TopmostTick              int32
	CurrentBranchID          uint32
	TotalBranchID            uint32
	TotalSupply              uint64
	TotalBorrow              uint64
	TotalPositions           uint32
	AbsorbedDebtAmount       *[2]uint64 // 128-bit little-endian limbs (lo, hi)
	VaultSupplyExchangePrice uint64
	VaultBorrowExchangePrice uint64
	LastUpdateTimestamp      uint64
}

func u128le(d []byte, o int) (*[2]uint64, bool) {
	if o+16 > len(d) {
		return nil, false
	}
	return &[2]uint64{binary.LittleEndian.Uint64(d[o : o+8]), binary.LittleEndian.Uint64(d[o+8 : o+16])}, true
}

func isZeroU128(v *[2]uint64) bool { return v[0] == 0 && v[1] == 0 }

// U128String renders a 128-bit little-endian limb pair (lo, hi) as a decimal
// string, matching Rust's u128 Display used when printing
// absorbed_debt_amount.
func U128String(v *[2]uint64) string {
	if v == nil {
		return "0"
	}
	return U128ToBigInt(v).String()
}

// U128ToBigInt converts a (lo, hi) little-endian limb pair to a big.Int.
func U128ToBigInt(v *[2]uint64) *big.Int {
	if v == nil {
		return big.NewInt(0)
	}
	lo := new(big.Int).SetUint64(v[0])
	hi := new(big.Int).SetUint64(v[1])
	hi.Lsh(hi, 64)
	hi.Add(hi, lo)
	return hi
}

// BigIntToU128 converts a non-negative big.Int (must fit in 128 bits) to a
// (lo, hi) little-endian limb pair.
func BigIntToU128(v *big.Int) *[2]uint64 {
	mask64 := new(big.Int).SetUint64(^uint64(0))
	lo := new(big.Int).And(v, mask64)
	hi := new(big.Int).Rsh(v, 64)
	hi.And(hi, mask64)
	return &[2]uint64{lo.Uint64(), hi.Uint64()}
}

// DecodeU128LE decodes a little-endian 128-bit value at offset o in d into a
// (lo, hi) limb pair.
func DecodeU128LE(d []byte, o int) (*[2]uint64, bool) {
	return u128le(d, o)
}

func DecodeVaultState(d []byte) (*VaultState, bool) {
	if len(d) < 127 || !hasDisc(d, VaultStateDisc) {
		return nil, false
	}
	vaultID, _ := u16le(d, 8)
	branchLiquidated := d[10]
	topmostTick, _ := i32le(d, 11)
	currentBranchID, _ := u32le(d, 15)
	totalBranchID, _ := u32le(d, 19)
	totalSupply, _ := u64le(d, 23)
	totalBorrow, _ := u64le(d, 31)
	totalPositions, _ := u32le(d, 39)
	absorbedDebt, _ := u128le(d, 43)
	supplyEx, _ := u64le(d, 99)
	borrowEx, _ := u64le(d, 107)
	lastUpdate, _ := u64le(d, 119)
	return &VaultState{
		VaultID: vaultID, BranchLiquidated: branchLiquidated, TopmostTick: topmostTick,
		CurrentBranchID: currentBranchID, TotalBranchID: totalBranchID,
		TotalSupply: totalSupply, TotalBorrow: totalBorrow, TotalPositions: totalPositions,
		AbsorbedDebtAmount: absorbedDebt, VaultSupplyExchangePrice: supplyEx,
		VaultBorrowExchangePrice: borrowEx, LastUpdateTimestamp: lastUpdate,
	}, true
}

// ── Oracle account — the ordered price-source pubkeys ────────────────────────
const OracleSourceStride = 66

func DecodeOracleSources(raw []byte) ([]solana.PublicKey, bool) {
	n, ok := u32le(raw, 10)
	if !ok {
		return nil, false
	}
	out := make([]solana.PublicKey, 0, n)
	for i := uint32(0); i < n; i++ {
		pk, ok := readPk(raw, 14+int(i)*OracleSourceStride)
		if !ok {
			return nil, false
		}
		out = append(out, pk)
	}
	return out, true
}

// Vault is its config + state, keyed by vault_id.
type Vault struct {
	ConfigPubkey solana.PublicKey
	StatePubkey  solana.PublicKey
	Config       *VaultConfig
	State        *VaultState
}

// MaybeLiquidatable is the FIRST-PASS liquidatable signal.
//
// CONFIDENT: absorbed_debt_amount > 0 means the vault currently holds
// absorbed/pending-liquidation debt — a real, unambiguous signal.
// NOT USED: branch_liquidated is decoded but appears to be a historical
// branch counter, so it's excluded from this check.
// INFERRED / NOT YET IMPLEMENTED: precise "is there liquidatable debt at the
// current price" needs Fluid's tick↔price math; the executor's on-chain
// liquidate simulation is the ground-truth gate.
func (v *Vault) MaybeLiquidatable() bool {
	return v.State.TotalBorrow > 0 && !isZeroU128(v.State.AbsorbedDebtAmount)
}
