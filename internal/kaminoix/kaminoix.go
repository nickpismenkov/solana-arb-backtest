// Package kaminoix builds Kamino (KLend) liquidation instructions, derived
// from a REAL mainnet liquidation tx (see kamino_liq_decode) — layouts are
// program-fixed, so one captured sample pins them. Discriminators and the
// 25-account liquidate layout are verified against tx pBAF8kTU... (bSoL
// liquidator, USDC->bSOL).
//
// A Kamino liquidation is a 3-instruction sequence, all in one tx:
//
//	refresh_reserve(repay_reserve)  +  refresh_reserve(withdraw_reserve)
//	refresh_obligation(obligation, [reserves...])
//	liquidate_obligation_and_redeem_reserve_collateral_v2
//
// The liquidate seizes collateral and immediately redeems the cTokens to the
// underlying liquidity token into the liquidator's ATA (so the swap leg
// sells the underlying, not a cToken).
//
// Oracle wiring: main-market reserves price via Scope (scope_prices account
// at reserve offset 5112); the pyth/switchboard refresh slots are None
// (KLend-program placeholders). ReserveAccounts.Decode pulls every account a
// refresh/liquidate needs straight out of the Reserve bytes.
package kaminoix

import (
	"arbengine/internal/solana"
)

const (
	KlendProgram       = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"
	FarmsProgram       = "FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr"
	SysvarInstructions = "Sysvar1nstructions1111111111111111111111111"
)

// VERIFIED discriminators (from the captured tx).
var (
	discRefreshReserve    = [8]byte{0x02, 0xda, 0x8a, 0xeb, 0x4f, 0xc9, 0x19, 0x66}
	discRefreshObligation = [8]byte{0x21, 0x84, 0x93, 0xe4, 0x97, 0xc0, 0x48, 0x59}
	discLiquidateV2       = [8]byte{0xa2, 0xa1, 0x23, 0x8f, 0x1e, 0xbb, 0xb9, 0x67}
)

// Reserve account-field offsets (VERIFIED by locating known pubkeys in a
// real 8624-byte reserve).
const (
	rLendingMarket = 32
	rLiqMint       = 128
	rLiqSupply     = 160
	rFeeReceiver   = 192
	rCollMint      = 2560
	rCollSupply    = 2600
	rScopePrices   = 5112
)

func pk(s string) solana.Pubkey { return solana.MustPubkeyFromBase58(s) }

func pkAt(d []byte, off int) solana.Pubkey {
	pk, _ := solana.PubkeyFromBytes(d[off : off+32])
	return pk
}

// ReserveAccounts holds every account of one reserve that a
// refresh/liquidate touches, pulled from the reserve account bytes.
type ReserveAccounts struct {
	Reserve          solana.Pubkey
	LendingMarket    solana.Pubkey
	LiquidityMint    solana.Pubkey
	LiquiditySupply  solana.Pubkey
	FeeReceiver      solana.Pubkey
	CollateralMint   solana.Pubkey
	CollateralSupply solana.Pubkey
	ScopePrices      solana.Pubkey
}

// DecodeReserveAccounts decodes a ReserveAccounts from a raw Reserve
// account's bytes.
func DecodeReserveAccounts(reserve solana.Pubkey, data []byte) (*ReserveAccounts, bool) {
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

// LendingMarketAuthority derives the lending_market_authority PDA (seed
// "lma"), VERIFIED against the captured tx.
func LendingMarketAuthority(lendingMarket solana.Pubkey) solana.Pubkey {
	addr, _ := solana.FindProgramAddress([][]byte{[]byte("lma"), lendingMarket.Bytes()}, pk(KlendProgram))
	return addr
}

// RefreshReserve builds refresh_reserve. Scope-priced reserves pass the
// scope_prices account and leave pyth/switchboard slots as the KLend
// program (Anchor None).
// Accounts: [reserve(W), lending_market, pyth?, sb_price?, sb_twap?, scope?].
func RefreshReserve(r *ReserveAccounts) solana.Instruction {
	klend := pk(KlendProgram)
	return solana.Instruction{
		ProgramID: klend,
		Accounts: []solana.AccountMeta{
			solana.Writable(r.Reserve),
			solana.ReadonlyMeta(r.LendingMarket),
			solana.ReadonlyMeta(klend),         // pyth = None
			solana.ReadonlyMeta(klend),         // switchboard price = None
			solana.ReadonlyMeta(klend),         // switchboard twap = None
			solana.ReadonlyMeta(r.ScopePrices), // scope
		},
		Data: append([]byte{}, discRefreshReserve[:]...),
	}
}

// RefreshObligation builds refresh_obligation. Accounts: [lending_market,
// obligation(W)] then each of the obligation's reserves (deposits then
// borrows, in slot order) as read-only remaining accounts.
func RefreshObligation(lendingMarket, obligation solana.Pubkey, reserves []solana.Pubkey) solana.Instruction {
	accounts := []solana.AccountMeta{
		solana.ReadonlyMeta(lendingMarket),
		solana.Writable(obligation),
	}
	for _, r := range reserves {
		accounts = append(accounts, solana.ReadonlyMeta(r))
	}
	return solana.Instruction{
		ProgramID: pk(KlendProgram),
		Accounts:  accounts,
		Data:      append([]byte{}, discRefreshObligation[:]...),
	}
}

// LiquidateAndRedeemV2Params bundles the arguments to
// LiquidateAndRedeemV2 (Go doesn't take 14 positional args gracefully; the
// Rust liquidate_and_redeem_v2 signature is preserved 1:1 in field order).
type LiquidateAndRedeemV2Params struct {
	Liquidator                     solana.Pubkey
	Obligation                     solana.Pubkey
	LendingMarket                  solana.Pubkey
	Repay                          *ReserveAccounts
	Withdraw                       *ReserveAccounts
	UserDestCollateral             solana.Pubkey
	UserDestLiquidity              solana.Pubkey
	UserSourceLiquidity            solana.Pubkey
	CollateralTokenProgram         solana.Pubkey
	RepayLiquidityTokenProgram     solana.Pubkey
	WithdrawLiquidityTokenProgram  solana.Pubkey
	LiquidityAmount                uint64
	MinAcceptableReceivedLiquidity uint64
	MaxAllowedLtvOverridePct       uint64
}

// LiquidateAndRedeemV2 builds
// liquidate_obligation_and_redeem_reserve_collateral_v2. Seizes and redeems
// in one ix. data = disc + liquidity_amount(u64) + min_acceptable_received(u64)
// + max_allowed_ltv_override_pct(u64). 25 accounts, VERIFIED layout:
//
//	[0] liquidator (signer)          [13] user_dest_collateral (W)
//	[1] obligation (W)               [14] user_dest_liquidity (W)
//	[2] lending_market               [15] user_source_liquidity (W, the repay)
//	[3] lending_market_authority     [16] collateral_token_program
//	[4] repay_reserve (W)            [17] repay_liquidity_token_program
//	[5] repay_liquidity_mint         [18] withdraw_liquidity_token_program
//	[6] repay_liquidity_supply (W)   [19] instructions sysvar
//	[7] withdraw_reserve (W)         [20..23] KLend placeholders (opt None)
//	[8] withdraw_liquidity_mint      [24] farms program
//	[9] withdraw_collateral_mint (W)
//	[10] withdraw_collateral_supply (W)
//	[11] withdraw_liquidity_supply (W)
//	[12] withdraw_fee_receiver (W)
func LiquidateAndRedeemV2(p LiquidateAndRedeemV2Params) solana.Instruction {
	klend := pk(KlendProgram)
	data := make([]byte, 0, 32)
	data = append(data, discLiquidateV2[:]...)
	data = appendU64LE(data, p.LiquidityAmount)
	data = appendU64LE(data, p.MinAcceptableReceivedLiquidity)
	data = appendU64LE(data, p.MaxAllowedLtvOverridePct)
	accounts := []solana.AccountMeta{
		solana.SignerMeta(p.Liquidator),
		solana.Writable(p.Obligation),
		solana.ReadonlyMeta(p.LendingMarket),
		solana.ReadonlyMeta(LendingMarketAuthority(p.LendingMarket)),
		solana.Writable(p.Repay.Reserve),
		solana.ReadonlyMeta(p.Repay.LiquidityMint),
		solana.Writable(p.Repay.LiquiditySupply),
		solana.Writable(p.Withdraw.Reserve),
		solana.ReadonlyMeta(p.Withdraw.LiquidityMint),
		solana.Writable(p.Withdraw.CollateralMint),
		solana.Writable(p.Withdraw.CollateralSupply),
		solana.Writable(p.Withdraw.LiquiditySupply),
		solana.Writable(p.Withdraw.FeeReceiver),
		solana.Writable(p.UserDestCollateral),
		solana.Writable(p.UserDestLiquidity),
		solana.Writable(p.UserSourceLiquidity),
		solana.ReadonlyMeta(p.CollateralTokenProgram),
		solana.ReadonlyMeta(p.RepayLiquidityTokenProgram),
		solana.ReadonlyMeta(p.WithdrawLiquidityTokenProgram),
		solana.ReadonlyMeta(pk(SysvarInstructions)),
		solana.ReadonlyMeta(klend),
		solana.ReadonlyMeta(klend),
		solana.ReadonlyMeta(klend),
		solana.ReadonlyMeta(klend),
		solana.ReadonlyMeta(pk(FarmsProgram)),
	}
	return solana.Instruction{ProgramID: klend, Accounts: accounts, Data: data}
}

func appendU64LE(b []byte, v uint64) []byte {
	return append(b,
		byte(v), byte(v>>8), byte(v>>16), byte(v>>24),
		byte(v>>32), byte(v>>40), byte(v>>48), byte(v>>56),
	)
}
