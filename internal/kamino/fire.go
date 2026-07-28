// Kamino atomic liquidation FIRE path — one flashloan-wrapped v0 tx:
//
//	[cu, cu_price, create ATAs, JupLend borrow USDC,
//	 refresh_reserve(repay), refresh_reserve(withdraw), refresh_obligation,
//	 liquidate_and_redeem_v2, Jupiter swap seized→USDC, JupLend payback, tip]
//
// Profit-or-revert, NO external capital: the flash-borrowed USDC repays the
// obligation's debt inside liquidate, which seizes discounted collateral and
// redeems it to the underlying liquidity token. The swap turns that back
// into USDC; the fixed-amount JupLend payback then fails unless the swap
// produced at least the borrowed amount.
package kamino

import (
	"fmt"
	"os"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/arb"
	"solana-arb-backtest-go/internal/flashloan"
	"solana-arb-backtest-go/internal/jupswap"
)

const FireCULimit uint32 = 1_400_000

// KaminoALT is the dedicated Kamino liquidation ALT (main-market static
// accounts + programs). Override via KAMINO_ALT.
const KaminoALT = "6X77KtDupVYqU4SBjWsY93ycFW2bPm3AWpAuPWfxraKo"

type FireCandidate struct {
	Obligation      solana.PublicKey
	LendingMarket   solana.PublicKey
	RepayReserve    *ReserveAccounts
	WithdrawReserve *ReserveAccounts
	// obligation's reserves in refresh order (deposits then borrows).
	ObligationReserves []solana.PublicKey
	// seized-collateral underlying mint (= withdraw reserve liquidity) + its program.
	WithdrawLiquidityMint          solana.PublicKey
	WithdrawLiquidityTokenProgram  solana.PublicKey
	WithdrawCollateralTokenProgram solana.PublicKey
	// repay side token program (USDC = classic SPL in v1).
	RepayLiquidityTokenProgram solana.PublicKey
	// USDC to flash-borrow and repay into the obligation (the close amount).
	RepayAmount uint64
	// Native underlying units to swap → USDC.
	SwapInAmount uint64
}

type FireTx struct {
	Tx            *solana.Transaction
	QuotedUSDCOut uint64
	TxBytes       int
}

// BuildFireTx builds the unsigned Kamino fire tx. Quotes the
// seized-underlying→USDC swap live (Jupiter), so call only for a
// sim-confirmed candidate.
func BuildFireTx(
	rpcEndpoint string,
	c *FireCandidate,
	authority solana.PublicKey,
	tipAccount *solana.PublicKey,
	tipLamports uint64,
	priorityMicroLamports uint64,
	slippageBps uint32,
	maxSwapAccounts int,
	blockhash solana.Hash,
) (*FireTx, error) {
	// Debt asset = the repay reserve's liquidity mint (USDC/USDT/wSOL). It's
	// the flash-borrow asset, the swap target, and the payback token.
	debtMint := c.RepayReserve.LiquidityMint
	debtTp := c.RepayLiquidityTokenProgram
	if !flashloan.HasMarket(debtMint) {
		return nil, fmt.Errorf("no JupLend flash market for debt mint %s", debtMint)
	}

	// ATAs: debt asset (borrow dest + repay source + swap out), seized
	// underlying (swap in), collateral cToken (transient redeem target).
	debtAta := flashloan.AtaFor(authority, debtMint, debtTp)
	seizedAta := flashloan.AtaFor(authority, c.WithdrawLiquidityMint, c.WithdrawLiquidityTokenProgram)
	collAta := flashloan.AtaFor(authority, c.WithdrawReserve.CollateralMint, c.WithdrawCollateralTokenProgram)

	// Swap the redeemed underlying → debt asset. Same-mint case (seized
	// underlying == debt mint): no swap — the redeemed liquidity IS the debt
	// asset. Jupiter rejects equal in/out mints, so skip.
	sameMint := c.WithdrawLiquidityMint.Equals(debtMint)
	var swapIxs []solana.Instruction
	var quotedOut uint64
	var swapAlts []solana.PublicKey
	if sameMint {
		quotedOut = c.SwapInAmount
	} else {
		quote, err := jupswap.Quote(c.WithdrawLiquidityMint, debtMint, c.SwapInAmount, slippageBps, maxSwapAccounts)
		if err != nil {
			return nil, err
		}
		plan, err := jupswap.SwapInstructions(quote, authority, false)
		if err != nil {
			return nil, err
		}
		swapIxs, quotedOut, swapAlts = plan.Instructions, plan.QuotedOut, plan.AltAddresses
	}

	altAddrs := append([]solana.PublicKey{}, swapAlts...)
	kaminoAlt := os.Getenv("KAMINO_ALT")
	if kaminoAlt == "" {
		kaminoAlt = KaminoALT
	}
	if kaminoAlt != "" {
		if p, err := solana.PublicKeyFromBase58(kaminoAlt); err == nil {
			altAddrs = append(altAddrs, p)
		}
	}
	alts, err := jupswap.FetchAlts(rpcEndpoint, altAddrs)
	if err != nil {
		return nil, err
	}

	borrowIx, ok := flashloan.Borrow(authority, debtMint, c.RepayAmount)
	if !ok {
		return nil, fmt.Errorf("no JupLend flash market for debt mint %s", debtMint)
	}
	paybackIx, ok := flashloan.Payback(authority, debtMint, c.RepayAmount)
	if !ok {
		return nil, fmt.Errorf("no JupLend flash market for debt mint %s", debtMint)
	}

	ixs := []solana.Instruction{
		arb.CuLimitIx(FireCULimit),
		arb.CuPriceIx(priorityMicroLamports),
		flashloan.CreateAtaIdempotentFor(authority, debtMint, debtTp),
		flashloan.CreateAtaIdempotentFor(authority, c.WithdrawLiquidityMint, c.WithdrawLiquidityTokenProgram),
		flashloan.CreateAtaIdempotentFor(authority, c.WithdrawReserve.CollateralMint, c.WithdrawCollateralTokenProgram),
		borrowIx,
		RefreshReserve(c.RepayReserve),
		RefreshReserve(c.WithdrawReserve),
		RefreshObligation(c.LendingMarket, c.Obligation, c.ObligationReserves),
		LiquidateAndRedeemV2(
			authority, c.Obligation, c.LendingMarket, c.RepayReserve, c.WithdrawReserve,
			collAta, seizedAta, debtAta,
			c.WithdrawCollateralTokenProgram, c.RepayLiquidityTokenProgram, c.WithdrawLiquidityTokenProgram,
			c.RepayAmount, 0, 0,
		),
	}
	ixs = append(ixs, swapIxs...)
	// Fixed-amount payback = the guard: reverts unless the swap covered it.
	ixs = append(ixs, paybackIx)
	if tipAccount != nil && tipLamports > 0 {
		ixs = append(ixs, arb.TransferIx(authority, *tipAccount, tipLamports))
	}

	tx, err := arb.CompileV0(authority, ixs, alts, blockhash)
	if err != nil {
		return nil, err
	}
	txBytes, err := tx.MarshalBinary()
	if err != nil {
		return nil, err
	}
	return &FireTx{Tx: tx, QuotedUSDCOut: quotedOut, TxBytes: len(txBytes)}, nil
}
