// Package kaminofire builds the Kamino atomic liquidation FIRE path — one
// flashloan-wrapped v0 tx:
//
//	[cu, cu_price, create ATAs, JupLend borrow USDC,
//	 refresh_reserve(repay), refresh_reserve(withdraw), refresh_obligation,
//	 liquidate_and_redeem_v2, Jupiter swap seized->USDC, JupLend payback, tip]
//
// Profit-or-revert, NO external capital: the flash-borrowed USDC repays the
// obligation's debt inside liquidate, which seizes discounted collateral and
// redeems it to the underlying liquidity token. The swap turns that back
// into USDC; the fixed-amount JupLend payback then fails unless the swap
// produced at least the borrowed amount — so a landed tx is always
// net-positive (the liquidation bonus), and an unprofitable one reverts for
// just the base fee.
//
// v1.5: the debt (repay reserve's liquidity) may be any asset with a wired
// JupLend flash market — USDC/USDT/wSOL. That mint is what JupLend
// flash-borrows and what the seized-collateral swap targets.
package kaminofire

import (
	"fmt"
	"os"

	"arbengine/internal/arb"
	"arbengine/internal/flashloan"
	"arbengine/internal/jup"
	"arbengine/internal/kaminoix"
	"arbengine/internal/solana"
)

const FireCULimit uint32 = 1_400_000

const USDCMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

// KaminoALT is the dedicated Kamino liquidation ALT (main-market static
// accounts + programs). Override via KAMINO_ALT. Set after
// liq_kamino_alt_print + on-chain create.
const KaminoALT = "6X77KtDupVYqU4SBjWsY93ycFW2bPm3AWpAuPWfxraKo"

// KaminoFireCandidate is a sim-confirmed Kamino liquidation ready to build.
type KaminoFireCandidate struct {
	Obligation      solana.Pubkey
	LendingMarket   solana.Pubkey
	RepayReserve    kaminoix.ReserveAccounts
	WithdrawReserve kaminoix.ReserveAccounts
	// ObligationReserves are the obligation's reserves in refresh order
	// (deposits then borrows).
	ObligationReserves []solana.Pubkey
	// WithdrawLiquidityMint is the seized-collateral underlying mint (=
	// withdraw reserve liquidity) + its program.
	WithdrawLiquidityMint          solana.Pubkey
	WithdrawLiquidityTokenProgram  solana.Pubkey
	WithdrawCollateralTokenProgram solana.Pubkey
	// RepayLiquidityTokenProgram is the repay side token program (USDC =
	// classic SPL in v1).
	RepayLiquidityTokenProgram solana.Pubkey
	// RepayAmount is the USDC to flash-borrow and repay into the obligation
	// (the close amount).
	RepayAmount uint64
	// SwapInAmount is the native underlying units to swap -> USDC. Computed
	// by the caller from the seized-collateral value and underlying price
	// (with a haircut so the ExactIn never exceeds the redeemed balance);
	// dust stays in the ATA.
	SwapInAmount uint64
}

// KaminoFireTx is the built, unsigned fire transaction.
type KaminoFireTx struct {
	Tx            solana.VersionedTransaction
	QuotedUSDCOut uint64
	TxBytes       int
}

// BuildFireTx builds the unsigned Kamino fire tx. Quotes the
// seized-underlying->USDC swap live (Jupiter), so call only for a
// sim-confirmed candidate.
func BuildFireTx(
	rpcEndpoint string,
	c *KaminoFireCandidate,
	authority solana.Pubkey,
	tipAccount *solana.Pubkey,
	tipLamports uint64,
	priorityMicroLamports uint64,
	slippageBps uint32,
	maxSwapAccounts int,
	blockhash solana.Hash,
) (KaminoFireTx, error) {
	// Debt asset = the repay reserve's liquidity mint (USDC/USDT/wSOL). It's
	// the flash-borrow asset, the swap target, and the payback token.
	debtMint := c.RepayReserve.LiquidityMint
	debtTp := c.RepayLiquidityTokenProgram
	if !flashloan.HasMarket(debtMint) {
		return KaminoFireTx{}, fmt.Errorf("no JupLend flash market for debt mint %s", debtMint.String())
	}

	// ATAs: debt asset (borrow dest + repay source + swap out), seized
	// underlying (swap in), collateral cToken (transient redeem target).
	debtAta := flashloan.AtaFor(authority, debtMint, debtTp)
	seizedAta := flashloan.AtaFor(authority, c.WithdrawLiquidityMint, c.WithdrawLiquidityTokenProgram)
	collAta := flashloan.AtaFor(authority, c.WithdrawReserve.CollateralMint, c.WithdrawCollateralTokenProgram)

	// Swap the redeemed underlying -> debt asset. SwapInAmount is the
	// caller's native-unit estimate of the seized collateral (with a
	// haircut so the ExactIn stays within the redeemed balance). The fixed
	// payback is the real profit-or-revert guard regardless of any
	// swap-sizing slack.
	//
	// Same-mint case (seized underlying == debt mint): no swap — the
	// redeemed liquidity IS the debt asset. Jupiter rejects equal in/out
	// mints, so skip.
	sameMint := c.WithdrawLiquidityMint == debtMint
	var swapIxs []solana.Instruction
	var quotedOut uint64
	var swapAlts []solana.Pubkey
	if sameMint {
		quotedOut = c.SwapInAmount
	} else {
		quote, err := jup.Quote(c.WithdrawLiquidityMint, debtMint, c.SwapInAmount, slippageBps, maxSwapAccounts)
		if err != nil {
			return KaminoFireTx{}, err
		}
		plan, err := jup.SwapInstructions(quote, authority, false)
		if err != nil {
			return KaminoFireTx{}, err
		}
		swapIxs = plan.Instructions
		quotedOut = plan.QuotedOut
		swapAlts = plan.ALTAddresses
	}

	altAddrs := append([]solana.Pubkey{}, swapAlts...)
	if a, ok := kaminoALTOverride(); ok {
		if pk, err := solana.PubkeyFromBase58(a); err == nil {
			altAddrs = append(altAddrs, pk)
		}
	}
	alts, err := jup.FetchALTs(rpcEndpoint, altAddrs)
	if err != nil {
		return KaminoFireTx{}, err
	}

	borrowIx, ok := flashloan.Borrow(authority, debtMint, c.RepayAmount)
	if !ok {
		return KaminoFireTx{}, fmt.Errorf("no JupLend flash market for debt mint %s", debtMint.String())
	}
	paybackIx, ok := flashloan.Payback(authority, debtMint, c.RepayAmount)
	if !ok {
		return KaminoFireTx{}, fmt.Errorf("no JupLend flash market for debt mint %s", debtMint.String())
	}

	ixs := []solana.Instruction{
		arb.CuLimitIx(FireCULimit),
		arb.CuPriceIx(priorityMicroLamports),
		flashloan.CreateAtaIdempotentFor(authority, debtMint, debtTp),
		flashloan.CreateAtaIdempotentFor(authority, c.WithdrawLiquidityMint, c.WithdrawLiquidityTokenProgram),
		flashloan.CreateAtaIdempotentFor(authority, c.WithdrawReserve.CollateralMint, c.WithdrawCollateralTokenProgram),
		borrowIx,
		kaminoix.RefreshReserve(&c.RepayReserve),
		kaminoix.RefreshReserve(&c.WithdrawReserve),
		kaminoix.RefreshObligation(c.LendingMarket, c.Obligation, c.ObligationReserves),
		kaminoix.LiquidateAndRedeemV2(kaminoix.LiquidateAndRedeemV2Params{
			Liquidator:                     authority,
			Obligation:                     c.Obligation,
			LendingMarket:                  c.LendingMarket,
			Repay:                          &c.RepayReserve,
			Withdraw:                       &c.WithdrawReserve,
			UserDestCollateral:             collAta,
			UserDestLiquidity:              seizedAta,
			UserSourceLiquidity:            debtAta,
			CollateralTokenProgram:         c.WithdrawCollateralTokenProgram,
			RepayLiquidityTokenProgram:     c.RepayLiquidityTokenProgram,
			WithdrawLiquidityTokenProgram:  c.WithdrawLiquidityTokenProgram,
			LiquidityAmount:                c.RepayAmount,
			MinAcceptableReceivedLiquidity: 0,
			MaxAllowedLtvOverridePct:       0,
		}),
	}
	ixs = append(ixs, swapIxs...)
	// Fixed-amount payback = the guard: reverts unless the swap covered it.
	ixs = append(ixs, paybackIx)
	if tipAccount != nil && tipLamports > 0 {
		ixs = append(ixs, arb.TransferIx(authority, *tipAccount, tipLamports))
	}

	msg, err := solana.CompileV0(authority, ixs, alts, blockhash)
	if err != nil {
		return KaminoFireTx{}, fmt.Errorf("compile v0: %w", err)
	}
	tx := solana.VersionedTransaction{
		Signatures: []solana.Signature{{}},
		Message:    solana.VersionedMessage{IsV0: true, V0: msg},
	}
	txBytes, err := tx.MarshalBinary()
	if err != nil {
		return KaminoFireTx{}, err
	}
	return KaminoFireTx{Tx: tx, QuotedUSDCOut: quotedOut, TxBytes: len(txBytes)}, nil
}

// kaminoALTOverride mirrors the Rust env lookup: KAMINO_ALT env var if set,
// else the KaminoALT constant if non-empty.
func kaminoALTOverride() (string, bool) {
	if v, ok := os.LookupEnv("KAMINO_ALT"); ok {
		return v, true
	}
	if KaminoALT != "" {
		return KaminoALT, true
	}
	return "", false
}
