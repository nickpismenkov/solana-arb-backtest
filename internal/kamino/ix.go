// Kamino (KLend) liquidation instructions, derived from a real mainnet
// liquidation tx — layouts are program-fixed, so one captured sample pins
// them.
//
// A Kamino liquidation is a 3-instruction sequence, all in one tx:
//
//	refresh_reserve(repay_reserve)  +  refresh_reserve(withdraw_reserve)
//	refresh_obligation(obligation, [reserves…])
//	liquidate_obligation_and_redeem_reserve_collateral_v2
package kamino

import (
	"encoding/binary"

	"github.com/gagliardetto/solana-go"
)

const FarmsProgram = "FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr"
const SysvarInstructions = "Sysvar1nstructions1111111111111111111111111"

// VERIFIED discriminators (from the captured tx).
var (
	discRefreshReserve    = [8]byte{0x02, 0xda, 0x8a, 0xeb, 0x4f, 0xc9, 0x19, 0x66}
	discRefreshObligation = [8]byte{0x21, 0x84, 0x93, 0xe4, 0x97, 0xc0, 0x48, 0x59}
	discLiquidateV2       = [8]byte{0xa2, 0xa1, 0x23, 0x8f, 0x1e, 0xbb, 0xb9, 0x67}
)

// Reserve account-field offsets (VERIFIED by locating known pubkeys in a real
// 8624-byte reserve).
const (
	rLendingMarket = 32
	rLiqMint       = 128
	rLiqSupply     = 160
	rFeeReceiver   = 192
	rCollMint      = 2560
	rCollSupply    = 2600
	rScopePrices   = 5112
)

func pk(s string) solana.PublicKey { return solana.MustPublicKeyFromBase58(s) }
func pkAt(d []byte, off int) solana.PublicKey {
	return solana.PublicKeyFromBytes(d[off : off+32])
}

// ReserveAccounts is every account of one reserve that a refresh/liquidate
// touches, pulled from the reserve account bytes.
type ReserveAccounts struct {
	Reserve          solana.PublicKey
	LendingMarket    solana.PublicKey
	LiquidityMint    solana.PublicKey
	LiquiditySupply  solana.PublicKey
	FeeReceiver      solana.PublicKey
	CollateralMint   solana.PublicKey
	CollateralSupply solana.PublicKey
	ScopePrices      solana.PublicKey
}

func DecodeReserveAccounts(reserve solana.PublicKey, data []byte) (*ReserveAccounts, bool) {
	if len(data) < rScopePrices+32 {
		return nil, false
	}
	return &ReserveAccounts{
		Reserve:          reserve,
		LendingMarket:    pkAt(data, rLendingMarket),
		LiquidityMint:    pkAt(data, rLiqMint),
		LiquiditySupply:  pkAt(data, rLiqSupply),
		FeeReceiver:      pkAt(data, rFeeReceiver),
		CollateralMint:   pkAt(data, rCollMint),
		CollateralSupply: pkAt(data, rCollSupply),
		ScopePrices:      pkAt(data, rScopePrices),
	}, true
}

// LendingMarketAuthority derives the lending_market_authority PDA (seed "lma").
func LendingMarketAuthority(lendingMarket solana.PublicKey) solana.PublicKey {
	addr, _, _ := solana.FindProgramAddress([][]byte{[]byte("lma"), lendingMarket.Bytes()}, pk(KlendProgram))
	return addr
}

// RefreshReserve. Scope-priced reserves pass the scope_prices account and
// leave pyth/switchboard slots as the KLend program (Anchor None).
func RefreshReserve(r *ReserveAccounts) solana.Instruction {
	klend := pk(KlendProgram)
	metas := solana.AccountMetaSlice{
		solana.NewAccountMeta(r.Reserve, true, false),
		solana.NewAccountMeta(r.LendingMarket, false, false),
		solana.NewAccountMeta(klend, false, false), // pyth = None
		solana.NewAccountMeta(klend, false, false), // switchboard price = None
		solana.NewAccountMeta(klend, false, false), // switchboard twap = None
		solana.NewAccountMeta(r.ScopePrices, false, false),
	}
	return solana.NewInstruction(klend, metas, discRefreshReserve[:])
}

// RefreshObligation. Accounts: [lending_market, obligation(W)] then each of
// the obligation's reserves (deposits then borrows, in slot order) as
// read-only remaining accounts.
func RefreshObligation(lendingMarket, obligation solana.PublicKey, reserves []solana.PublicKey) solana.Instruction {
	metas := solana.AccountMetaSlice{
		solana.NewAccountMeta(lendingMarket, false, false),
		solana.NewAccountMeta(obligation, true, false),
	}
	for _, r := range reserves {
		metas = append(metas, solana.NewAccountMeta(r, false, false))
	}
	return solana.NewInstruction(pk(KlendProgram), metas, discRefreshObligation[:])
}

// LiquidateAndRedeemV2 builds
// liquidate_obligation_and_redeem_reserve_collateral_v2. Seizes and redeems
// in one ix. data = disc + liquidity_amount(u64) + min_acceptable_received(u64)
// + max_allowed_ltv_override_pct(u64). 25 accounts, VERIFIED layout.
func LiquidateAndRedeemV2(
	liquidator, obligation, lendingMarket solana.PublicKey,
	repay, withdraw *ReserveAccounts,
	userDestCollateral, userDestLiquidity, userSourceLiquidity solana.PublicKey,
	collateralTokenProgram, repayLiquidityTokenProgram, withdrawLiquidityTokenProgram solana.PublicKey,
	liquidityAmount, minAcceptableReceivedLiquidity, maxAllowedLtvOverridePct uint64,
) solana.Instruction {
	klend := pk(KlendProgram)
	data := make([]byte, 0, 32)
	data = append(data, discLiquidateV2[:]...)
	data = binary.LittleEndian.AppendUint64(data, liquidityAmount)
	data = binary.LittleEndian.AppendUint64(data, minAcceptableReceivedLiquidity)
	data = binary.LittleEndian.AppendUint64(data, maxAllowedLtvOverridePct)
	metas := solana.AccountMetaSlice{
		solana.NewAccountMeta(liquidator, false, true),
		solana.NewAccountMeta(obligation, true, false),
		solana.NewAccountMeta(lendingMarket, false, false),
		solana.NewAccountMeta(LendingMarketAuthority(lendingMarket), false, false),
		solana.NewAccountMeta(repay.Reserve, true, false),
		solana.NewAccountMeta(repay.LiquidityMint, false, false),
		solana.NewAccountMeta(repay.LiquiditySupply, true, false),
		solana.NewAccountMeta(withdraw.Reserve, true, false),
		solana.NewAccountMeta(withdraw.LiquidityMint, false, false),
		solana.NewAccountMeta(withdraw.CollateralMint, true, false),
		solana.NewAccountMeta(withdraw.CollateralSupply, true, false),
		solana.NewAccountMeta(withdraw.LiquiditySupply, true, false),
		solana.NewAccountMeta(withdraw.FeeReceiver, true, false),
		solana.NewAccountMeta(userDestCollateral, true, false),
		solana.NewAccountMeta(userDestLiquidity, true, false),
		solana.NewAccountMeta(userSourceLiquidity, true, false),
		solana.NewAccountMeta(collateralTokenProgram, false, false),
		solana.NewAccountMeta(repayLiquidityTokenProgram, false, false),
		solana.NewAccountMeta(withdrawLiquidityTokenProgram, false, false),
		solana.NewAccountMeta(pk(SysvarInstructions), false, false),
		solana.NewAccountMeta(klend, false, false),
		solana.NewAccountMeta(klend, false, false),
		solana.NewAccountMeta(klend, false, false),
		solana.NewAccountMeta(klend, false, false),
		solana.NewAccountMeta(pk(FarmsProgram), false, false),
	}
	return solana.NewInstruction(klend, metas, data)
}
