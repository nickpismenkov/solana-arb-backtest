// The atomic Save (Solend) liquidation FIRE path — one flash-loan-wrapped v0 tx:
//
//	[cu_limit, cu_price, create ATAs,
//	 JupLend borrow(debt)               (capital, no inventory)
//	 save refresh_reserve(repay=debt) · refresh_reserve(withdraw=collateral)
//	   · refresh_obligation
//	 save liquidate_obligation_and_redeem  (repay debt, seize collateral,
//	   redeem cTokens → underlying into our ATA)
//	 Jupiter swap collateral-underlying → debt  (skipped when they're the
//	   same mint — Jupiter rejects equal in/out)
//	 JupLend payback(debt)              (repay the flash loan)
//	 tip]
//
// v1.5: the debt (repay reserve's liquidity) may be any asset with a wired
// JupLend flash market — USDC/USDT/wSOL. That mint is what JupLend
// flash-borrows, what the seized-collateral swap targets, and what the
// fixed payback repays.
//
// We wrap in JUPLEND's 0-bp flash loan (not Solend's own) for the same
// reason Kamino does: it's a different program, so it sidesteps Solend's
// flash-loan reentrancy guard (our liquidate repays into the very reserve a
// Solend flash loan would forbid touching between borrow/repay), and
// JupLend matches borrow↔payback via the instructions sysvar so no
// start/end wrapper is needed.
//
// Profit-or-revert with NO capital: the fixed-amount payback(debt) fails
// unless the swap produced at least the borrowed amount, so a landed tx is
// always net-positive (the ~liq_bonus% surplus stays in the wallet ATA) and
// an unprofitable one reverts for just the base fee.
package save

import (
	"fmt"
	"os"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/arb"
	"solana-arb-backtest-go/internal/flashloan"
	"solana-arb-backtest-go/internal/jupswap"
)

const FireCULimit uint32 = 1_400_000

// saturatingSub is Go's stand-in for Rust's u64::saturating_sub.
func saturatingSub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

// SaveALT is the dedicated ALT holding the fixed Solend + JupLend-flashloan
// accounts common to every Save liquidation (create with save_alt_print,
// analogous to Kamino's KaminoALT). Override via SAVE_ALT.
const SaveALT = "11111111111111111111111111111111" // placeholder until created on-chain

// FireCandidate is one Save liquidation opportunity, sized by the caller
// (via simulation).
type FireCandidate struct {
	Obligation solana.PublicKey
	// RepayReserve is the borrow reserve being repaid. Its LiquidityMint is
	// the debt asset (USDC/USDT/wSOL) — flash-borrowed, swapped into, and
	// repaid.
	RepayReserve *Reserve
	// WithdrawReserve is the collateral reserve being seized (its liquidity
	// mint is what we swap).
	WithdrawReserve *Reserve
	// CollateralTokenProgram is the token program owning the
	// collateral-underlying mint (redeem ATA).
	CollateralTokenProgram solana.PublicKey
	// DebtTokenProgram is the token program owning the debt mint
	// (USDC/USDT/wSOL are all classic SPL).
	DebtTokenProgram solana.PublicKey
	// RepayAmount is the debt to repay, in the debt asset's native units.
	RepayAmount uint64
	// SeizeUnderlying is the expected collateral-underlying out of
	// liquidate+redeem, to size the swap.
	SeizeUnderlying uint64
	// DepositReserves/BorrowReserves are the obligation's deposit + borrow
	// reserves, in obligation order, for refresh_obligation.
	DepositReserves []solana.PublicKey
	BorrowReserves  []solana.PublicKey
}

// FireTx is the built unsigned Save fire tx.
type FireTx struct {
	Tx *solana.Transaction
	// QuotedDebtOut is the debt-asset units the collateral→debt swap is
	// quoted to produce (native). In the same-mint case (collateral
	// underlying == debt) this is the seized amount itself, since there is
	// no swap.
	QuotedDebtOut uint64
	TxBytes       int
}

// BuildFireTx builds the unsigned Save fire tx. Quotes the collateral→debt
// swap live, so call only for a sim-confirmed candidate. blockhash = real
// recent hash for live submission, or default for replace-blockhash
// simulation.
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
	// Debt asset = the repay reserve's liquidity mint (USDC/USDT/wSOL).
	// It's the flash-borrow asset, the swap target, and the payback token.
	debtMint := c.RepayReserve.LiquidityMint
	if !flashloan.HasMarket(debtMint) {
		return nil, fmt.Errorf("no JupLend flash market for Save debt mint %s", debtMint)
	}
	underlying := c.WithdrawReserve.LiquidityMint

	// Same-mint case (seized underlying == debt): no swap — the redeemed
	// liquidity IS the debt asset. Jupiter rejects equal in/out mints, so
	// skip the swap leg (the fixed payback still guards profit).
	sameMint := underlying.Equals(debtMint)
	var swapIxs []solana.Instruction
	var quotedDebtOut uint64
	var swapAlts []solana.PublicKey
	if sameMint {
		quotedDebtOut = c.SeizeUnderlying
	} else {
		// ExactIn the redeemed collateral underlying → debt asset. Haircut
		// 0.05% to absorb redeem rounding (same as the marginfi/Kamino
		// paths).
		swapIn := saturatingSub(c.SeizeUnderlying, c.SeizeUnderlying/2000+1)
		quote, err := jupswap.Quote(underlying, debtMint, swapIn, slippageBps, maxSwapAccounts)
		if err != nil {
			return nil, err
		}
		plan, err := jupswap.SwapInstructions(quote, authority, false)
		if err != nil {
			return nil, err
		}
		swapIxs, quotedDebtOut, swapAlts = plan.Instructions, plan.QuotedOut, plan.AltAddresses
	}

	saveAltStr := os.Getenv("SAVE_ALT")
	if saveAltStr == "" {
		saveAltStr = SaveALT
	}
	saveAlt, err := solana.PublicKeyFromBase58(saveAltStr)
	if err != nil {
		return nil, fmt.Errorf("parse SAVE_ALT: %w", err)
	}
	altAddrs := append([]solana.PublicKey{}, swapAlts...)
	if !saveAlt.Equals(solana.MustPublicKeyFromBase58("11111111111111111111111111111111")) {
		altAddrs = append(altAddrs, saveAlt)
	}
	alts, err := jupswap.FetchAlts(rpcEndpoint, altAddrs)
	if err != nil {
		return nil, err
	}

	// Solend cTokens are always classic SPL (the program predates Token-2022
	// and its liquidate ix passes the classic token program for every
	// transfer).
	tp := pk(tokenProgram)
	debtAta := flashloan.AtaFor(authority, debtMint, c.DebtTokenProgram)
	underlyingAta := flashloan.AtaFor(authority, underlying, c.CollateralTokenProgram)
	ctokenAta := flashloan.AtaFor(authority, c.WithdrawReserve.CollateralMint, tp)

	borrowIx, ok := flashloan.Borrow(authority, debtMint, c.RepayAmount)
	if !ok {
		return nil, fmt.Errorf("no JupLend flash market for Save debt mint %s", debtMint)
	}
	paybackIx, ok := flashloan.Payback(authority, debtMint, c.RepayAmount)
	if !ok {
		return nil, fmt.Errorf("no JupLend flash market for Save debt mint %s", debtMint)
	}

	ixs := []solana.Instruction{
		arb.CuLimitIx(FireCULimit),
		arb.CuPriceIx(priorityMicroLamports),
		flashloan.CreateAtaIdempotentFor(authority, debtMint, c.DebtTokenProgram),
		flashloan.CreateAtaIdempotentFor(authority, underlying, c.CollateralTokenProgram),
		flashloan.CreateAtaIdempotentFor(authority, c.WithdrawReserve.CollateralMint, tp),
	}
	// Flash-borrow the debt asset we need to repay the liquidatee's debt.
	ixs = append(ixs, borrowIx)
	// Refresh Save state, then liquidate+redeem.
	ixs = append(ixs, RefreshReserve(c.RepayReserve))
	ixs = append(ixs, RefreshReserve(c.WithdrawReserve))
	ixs = append(ixs, RefreshObligation(c.Obligation, c.DepositReserves, c.BorrowReserves))
	ixs = append(ixs, LiquidateAndRedeem(
		c.RepayAmount,
		debtAta,       // source_liquidity (repay)
		ctokenAta,     // destination_collateral (transient cTokens)
		underlyingAta, // destination_liquidity (redeemed underlying)
		c.RepayReserve,
		c.WithdrawReserve,
		c.Obligation,
		c.RepayReserve.LendingMarket,
		authority, // user_transfer_authority (signer)
	))
	// Sell the seized underlying for the debt asset (unless it already is it).
	ixs = append(ixs, swapIxs...)
	// Fixed-amount payback = the profit-or-revert guard: reverts unless the
	// swap (or the same-mint redeem) covered the borrowed debt exactly.
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
	return &FireTx{Tx: tx, QuotedDebtOut: quotedDebtOut, TxBytes: len(txBytes)}, nil
}
