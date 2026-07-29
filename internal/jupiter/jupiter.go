// Package jupiter is the Jupiter Lend (Fluid) liquidation data layer —
// vault decoders + first-pass liquidatable detection.
//
// Jupiter Lend is a FLUID vault-tick protocol, fundamentally different from
// the obligation-based lenders (marginfi/Kamino/Save): liquidation is
// VAULT-LEVEL, not per-borrower. Each vault is an isolated (supply_token =
// collateral, borrow_token = debt) pair; a liquidator buys collateral off
// the vault's liquidation curve at a penalty discount (see the `liquidate`
// ix: debt_amt + col_per_unit_debt + absorb). There is NO obligation
// account.
//
// Layouts are Anchor/borsh (8-byte account discriminator =
// sha256("account:<Name>")[..8], then fields in declaration order, packed,
// little-endian), taken from the published IDL
// (jup-ag/jupiter-lend target/idl/vaults.json) and VERIFIED against 89 live
// mainnet VaultConfig/VaultState accounts.
//
// Program (Vaults/Borrow): jupr81YtYssSyPt8jbnGuiWon5f6x9TcDEFxYe3Bdzi
package jupiter

import (
	"arbengine/internal/solana"
)

const (
	VaultsProgram    = "jupr81YtYssSyPt8jbnGuiWon5f6x9TcDEFxYe3Bdzi"
	LiquidityProgram = "jupeiUmn818Jg1ekPURTpr4mFo29p46vygyykFJ3wZC"
)

// Anchor account discriminators (sha256("account:<Name>")[..8]).
var (
	VaultConfigDisc = [8]byte{0x63, 0x56, 0x2b, 0xd8, 0xb8, 0x66, 0x77, 0x4d}
	VaultStateDisc  = [8]byte{0xe4, 0xc4, 0x52, 0xa5, 0x62, 0xd2, 0xeb, 0x98}
)

// Common debt-token mints (the reachable debt set for our liquidator).
const (
	USDCMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	USDTMint = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"
	WSOLMint = "So11111111111111111111111111111111111111112"
)

func readPk(d []byte, o int) (solana.Pubkey, bool) {
	if o < 0 || o+32 > len(d) {
		return solana.Pubkey{}, false
	}
	pk, err := solana.PubkeyFromBytes(d[o : o+32])
	if err != nil {
		return solana.Pubkey{}, false
	}
	return pk, true
}

func u16le(d []byte, o int) (uint16, bool) {
	if o < 0 || o+2 > len(d) {
		return 0, false
	}
	return uint16(d[o]) | uint16(d[o+1])<<8, true
}

func u32le(d []byte, o int) (uint32, bool) {
	if o < 0 || o+4 > len(d) {
		return 0, false
	}
	return uint32(d[o]) | uint32(d[o+1])<<8 | uint32(d[o+2])<<16 | uint32(d[o+3])<<24, true
}

func u64le(d []byte, o int) (uint64, bool) {
	if o < 0 || o+8 > len(d) {
		return 0, false
	}
	v := uint64(0)
	for i := 0; i < 8; i++ {
		v |= uint64(d[o+i]) << (8 * i)
	}
	return v, true
}

func u128le(d []byte, o int) ([16]byte, bool) {
	if o < 0 || o+16 > len(d) {
		return [16]byte{}, false
	}
	var out [16]byte
	copy(out[:], d[o:o+16])
	return out, true
}

func i32le(d []byte, o int) (int32, bool) {
	v, ok := u32le(d, o)
	return int32(v), ok
}

// ── VaultConfig (219 bytes; VERIFIED against live accounts) ─────────────────
// disc[8] . vault_id u16@8 . supply_rate_magnifier i16@10 . borrow_rate_magnifier
// i16@12 . collateral_factor u16@14 . liquidation_threshold u16@16 .
// liquidation_max_limit u16@18 . withdraw_gap u16@20 . liquidation_penalty u16@22
// . borrow_fee u8@24 . vault_type u8@25 . oracle @26 . rebalancer @58 .
// liquidity_program @90 . oracle_program @122 . supply_token @154 .
// borrow_token @186 . bump u8@218.
type VaultConfig struct {
	VaultID uint16
	// CollateralFactor is a raw u16. INFERRED scale: units of 0.1%
	// (raw/1000 = fraction, raw/10 = percent) — matches known LTVs
	// (wSOL/USDC 850->85%, JUP/USDC 700->70%), but treat as inferred; the
	// fire path's sim is ground truth regardless.
	CollateralFactor     uint16
	LiquidationThreshold uint16
	LiquidationPenalty   uint16
	Oracle               solana.Pubkey
	OracleProgram        solana.Pubkey
	// SupplyToken is the collateral mint.
	SupplyToken solana.Pubkey
	// BorrowToken is the debt mint (what the liquidator repays).
	BorrowToken solana.Pubkey
}

// DecodeVaultConfig decodes a VaultConfig account. Returns (config, true) on
// success, (zero, false) if the data is too short or the discriminator
// doesn't match.
func DecodeVaultConfig(d []byte) (VaultConfig, bool) {
	if len(d) < 219 {
		return VaultConfig{}, false
	}
	for i := 0; i < 8; i++ {
		if d[i] != VaultConfigDisc[i] {
			return VaultConfig{}, false
		}
	}
	vaultID, ok := u16le(d, 8)
	if !ok {
		return VaultConfig{}, false
	}
	collateralFactor, ok := u16le(d, 14)
	if !ok {
		return VaultConfig{}, false
	}
	liquidationThreshold, ok := u16le(d, 16)
	if !ok {
		return VaultConfig{}, false
	}
	liquidationPenalty, ok := u16le(d, 22)
	if !ok {
		return VaultConfig{}, false
	}
	oracle, ok := readPk(d, 26)
	if !ok {
		return VaultConfig{}, false
	}
	oracleProgram, ok := readPk(d, 122)
	if !ok {
		return VaultConfig{}, false
	}
	supplyToken, ok := readPk(d, 154)
	if !ok {
		return VaultConfig{}, false
	}
	borrowToken, ok := readPk(d, 186)
	if !ok {
		return VaultConfig{}, false
	}
	return VaultConfig{
		VaultID:              vaultID,
		CollateralFactor:     collateralFactor,
		LiquidationThreshold: liquidationThreshold,
		LiquidationPenalty:   liquidationPenalty,
		Oracle:               oracle,
		OracleProgram:        oracleProgram,
		SupplyToken:          supplyToken,
		BorrowToken:          borrowToken,
	}, true
}

// LiqThresholdFrac returns the liquidation threshold as a fraction
// (INFERRED scale, see field docs).
func (c VaultConfig) LiqThresholdFrac() float64 {
	return float64(c.LiquidationThreshold) / 1000.0
}

// DebtLabel returns the debt-token label for the reachable-set check.
func (c VaultConfig) DebtLabel() string {
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

// DebtInScope reports whether this is debt we currently target (USDC/USDT/SOL).
func (c VaultConfig) DebtInScope() bool {
	return c.DebtLabel() != "other"
}

// ── VaultState (127 bytes; VERIFIED against live accounts) ──────────────────
// disc[8] . vault_id u16@8 . branch_liquidated u8@10 . topmost_tick i32@11 .
// current_branch_id u32@15 . total_branch_id u32@19 . total_supply u64@23 .
// total_borrow u64@31 . total_positions u32@39 . absorbed_debt_amount u128@43 .
// absorbed_col_amount u128@59 . absorbed_dust_debt u64@75 . liquidity_supply_
// exchange_price u64@83 . liquidity_borrow_exchange_price u64@91 . vault_supply_
// exchange_price u64@99 . vault_borrow_exchange_price u64@107 . next_position_id
// u32@115 . last_update_timestamp u64@119.
type VaultState struct {
	VaultID uint16
	// BranchLiquidated is nonzero while a branch is mid-liquidation.
	BranchLiquidated uint8
	// TopmostTick is the highest debt tick (Fluid liquidation curve position).
	TopmostTick int32
	// CurrentBranchID is the active branch id (root of the branch chain a
	// liquidation walks).
	CurrentBranchID uint32
	// TotalBranchID is the highest branch id ever created (new_branch =
	// this+1 when mid-liquidation).
	TotalBranchID  uint32
	TotalSupply    uint64
	TotalBorrow    uint64
	TotalPositions uint32
	// AbsorbedDebtAmount is the debt the vault has absorbed (bad-debt /
	// pending-liquidation bucket). Raw little-endian u128 bytes; use
	// AbsorbedDebtAmountBig for arithmetic.
	AbsorbedDebtAmount       [16]byte
	VaultSupplyExchangePrice uint64
	VaultBorrowExchangePrice uint64
	LastUpdateTimestamp      uint64
}

// DecodeVaultState decodes a VaultState account. Returns (state, true) on
// success, (zero, false) if the data is too short or the discriminator
// doesn't match.
func DecodeVaultState(d []byte) (VaultState, bool) {
	if len(d) < 127 {
		return VaultState{}, false
	}
	for i := 0; i < 8; i++ {
		if d[i] != VaultStateDisc[i] {
			return VaultState{}, false
		}
	}
	vaultID, ok := u16le(d, 8)
	if !ok {
		return VaultState{}, false
	}
	branchLiquidated := d[10]
	topmostTick, ok := i32le(d, 11)
	if !ok {
		return VaultState{}, false
	}
	currentBranchID, ok := u32le(d, 15)
	if !ok {
		return VaultState{}, false
	}
	totalBranchID, ok := u32le(d, 19)
	if !ok {
		return VaultState{}, false
	}
	totalSupply, ok := u64le(d, 23)
	if !ok {
		return VaultState{}, false
	}
	totalBorrow, ok := u64le(d, 31)
	if !ok {
		return VaultState{}, false
	}
	totalPositions, ok := u32le(d, 39)
	if !ok {
		return VaultState{}, false
	}
	absorbedDebtAmount, ok := u128le(d, 43)
	if !ok {
		return VaultState{}, false
	}
	vaultSupplyExchangePrice, ok := u64le(d, 99)
	if !ok {
		return VaultState{}, false
	}
	vaultBorrowExchangePrice, ok := u64le(d, 107)
	if !ok {
		return VaultState{}, false
	}
	lastUpdateTimestamp, ok := u64le(d, 119)
	if !ok {
		return VaultState{}, false
	}
	return VaultState{
		VaultID:                  vaultID,
		BranchLiquidated:         branchLiquidated,
		TopmostTick:              topmostTick,
		CurrentBranchID:          currentBranchID,
		TotalBranchID:            totalBranchID,
		TotalSupply:              totalSupply,
		TotalBorrow:              totalBorrow,
		TotalPositions:           totalPositions,
		AbsorbedDebtAmount:       absorbedDebtAmount,
		VaultSupplyExchangePrice: vaultSupplyExchangePrice,
		VaultBorrowExchangePrice: vaultBorrowExchangePrice,
		LastUpdateTimestamp:      lastUpdateTimestamp,
	}, true
}

// AbsorbedDebtAmountNonZero reports whether the raw little-endian u128
// AbsorbedDebtAmount is nonzero, without needing a big-int import at call
// sites.
func (s VaultState) AbsorbedDebtAmountNonZero() bool {
	for _, b := range s.AbsorbedDebtAmount {
		if b != 0 {
			return true
		}
	}
	return false
}

// ── Oracle account — the ordered price-source pubkeys (the `oracle_sources`) ─
// The vault's `oracle` (owned by `oracle_program`) is an Anchor `Oracle`
// account: disc[8] . nonce u16@8 . sources Vec<Sources> (u32 len@10,
// entries@14) . bump u8. Each `Sources` (borsh, in decl order) is
// `source: Pubkey(32) . invert: bool(1) . multiplier: u128(16) .
// divisor: u128(16) . source_type: enum u8(1)` = 66 bytes. The oracle CPI
// requires `remaining_accounts.len() == sources.len()` and checks
// `remaining_accounts[i].key() == sources[i].source` (verify_source), so
// the `oracle_sources` a liquidate must pass are exactly
// `[s.source for s in sources]` in order — fully derivable from on-chain
// state, no captured tx needed.
// (Source layout from code-423n4/2026-02-jupiter-lend
// programs/oracle/src/state.)
const OracleSourceStride = 66

// DecodeOracleSources decodes the ordered oracle_sources pubkeys from a raw
// Oracle account.
func DecodeOracleSources(raw []byte) ([]solana.Pubkey, bool) {
	n32, ok := u32le(raw, 10)
	if !ok {
		return nil, false
	}
	n := int(n32)
	out := make([]solana.Pubkey, 0, n)
	for i := 0; i < n; i++ {
		pk, ok := readPk(raw, 14+i*OracleSourceStride)
		if !ok {
			return nil, false
		}
		out = append(out, pk)
	}
	return out, true
}

// Vault is a vault = its config + state, keyed by vault_id.
type Vault struct {
	ConfigPubkey solana.Pubkey
	StatePubkey  solana.Pubkey
	Config       VaultConfig
	State        VaultState
}

// MaybeLiquidatable is the FIRST-PASS liquidatable signal. HONEST STATUS:
//   - CONFIDENT: absorbed_debt_amount > 0 means the vault currently holds
//     absorbed/pending-liquidation debt — a real, unambiguous signal that
//     something is (or just was) being liquidated in this vault.
//   - NOT USED: branch_liquidated is decoded but appears to be a historical
//     branch counter (nonzero on ~1/3 of vaults with zero absorbed debt),
//     so it is NOT a "liquidatable now" signal and is excluded from this
//     check.
//   - INFERRED / NOT YET IMPLEMENTED: precise "is there liquidatable debt
//     at the current price" needs Fluid's tick<->price math — comparing
//     topmost_tick to the tick implied by the live oracle price and
//     liquidation_threshold. That formula is NOT reversed here. This method
//     only surfaces vaults with absorbed debt; the executor's on-chain
//     liquidate simulation is the ground-truth gate (as with the other
//     protocols). Do NOT treat true as "definitely liquidatable," nor
//     false as "definitely healthy."
func (v Vault) MaybeLiquidatable() bool {
	return v.State.TotalBorrow > 0 && v.State.AbsorbedDebtAmountNonZero()
}
