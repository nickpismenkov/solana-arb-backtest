// Package savefire builds the atomic Save (Solend) liquidation FIRE path —
// one flash-loan-wrapped v0 tx:
//
//	[cu_limit, cu_price, create ATAs,
//	 JupLend borrow(debt)               (capital, no inventory)
//	 save refresh_reserve(repay=debt) · refresh_reserve(withdraw=collateral)
//	   · refresh_obligation
//	 save liquidate_obligation_and_redeem  (repay debt, seize collateral,
//	   redeem cTokens -> underlying into our ATA)
//	 Jupiter swap collateral-underlying -> debt  (skipped when they're the
//	   same mint — Jupiter rejects equal in/out)
//	 JupLend payback(debt)              (repay the flash loan)
//	 tip]
//
// v1.5: the debt (repay reserve's liquidity) may be any asset with a wired
// JupLend flash market — USDC/USDT/wSOL. That mint is what JupLend
// flash-borrows, what the seized-collateral swap targets, and what the fixed
// payback repays.
//
// We wrap in JUPLEND's 0-bp flash loan (not Solend's own) for the same
// reason Kamino does: it's a different program, so it sidesteps Solend's
// flash-loan reentrancy guard (our liquidate repays into the very reserve a
// Solend flash loan would forbid touching between borrow/repay), and
// JupLend matches borrow<->payback via the instructions sysvar so no
// start/end wrapper is needed.
//
// Profit-or-revert with NO capital: the fixed-amount payback(debt) fails
// unless the swap produced at least the borrowed amount, so a landed tx is
// always net-positive (the ~liq_bonus% surplus stays in the wallet ATA) and
// an unprofitable one reverts for just the base fee.
package savefire

import (
	"fmt"

	"arbengine/internal/arb"
	"arbengine/internal/config"
	"arbengine/internal/flashloan"
	"arbengine/internal/jup"
	"arbengine/internal/save"
	"arbengine/internal/solana"
)

const FireCULimit uint32 = 1_400_000

// SaveALT is the dedicated ALT holding the fixed Solend + JupLend-flashloan
// accounts common to every Save liquidation (create with save_alt_print,
// analogous to LIQ_ALT). Override via SAVE_ALT.
const SaveALT = "11111111111111111111111111111111" // placeholder until created on-chain

// SaveFireCandidate is one Save liquidation opportunity, sized by the
// caller (via simulation).
type SaveFireCandidate struct {
	Obligation solana.Pubkey
	// RepayReserve is the borrow reserve being repaid. Its LiquidityMint is
	// the debt asset (USDC/USDT/wSOL) — flash-borrowed, swapped into, and
	// repaid.
	RepayReserve save.Reserve
	// WithdrawReserve is the collateral reserve being seized (its liquidity
	// mint is what we swap).
	WithdrawReserve save.Reserve
	// CollateralTokenProgram owns the collateral-underlying mint (redeem
	// ATA).
	CollateralTokenProgram solana.Pubkey
	// DebtTokenProgram owns the debt mint (USDC/USDT/wSOL are all classic
	// SPL).
	DebtTokenProgram solana.Pubkey
	// RepayAmount is the debt to repay, in the debt asset's native units.
	RepayAmount uint64
	// SeizeUnderlying is the expected collateral-underlying out of
	// liquidate+redeem, to size the swap.
	SeizeUnderlying uint64
	// DepositReserves and BorrowReserves are the obligation's deposit +
	// borrow reserves, in obligation order, for refresh_obligation.
	DepositReserves []solana.Pubkey
	BorrowReserves  []solana.Pubkey
}

// SaveFireTx is the built, unsigned Save fire transaction.
type SaveFireTx struct {
	Tx solana.VersionedTransaction
	// QuotedDebtOut is the debt-asset units the collateral->debt swap is
	// quoted to produce (native). In the same-mint case (collateral
	// underlying == debt) this is the seized amount itself, since there is
	// no swap.
	QuotedDebtOut uint64
	TxBytes       int
}

var systemProgram = solana.MustPubkeyFromBase58("11111111111111111111111111111111")

// classicTokenProgram: Solend cTokens are always classic SPL (the program
// predates Token-2022 and its liquidate ix passes the classic token program
// for every transfer).
var classicTokenProgram = solana.MustPubkeyFromBase58("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA")

// BuildSaveFireTx builds the unsigned Save fire tx. Quotes the
// collateral->debt swap live, so call only for a sim-confirmed candidate.
// blockhash = real recent hash for live submission, or default for
// replace-blockhash simulation.
func BuildSaveFireTx(
	rpcEndpoint string,
	c *SaveFireCandidate,
	authority solana.Pubkey,
	tipAccount *solana.Pubkey,
	tipLamports uint64,
	priorityMicroLamports uint64,
	slippageBps uint32,
	maxSwapAccounts int,
	blockhash solana.Hash,
) (SaveFireTx, error) {
	// Debt asset = the repay reserve's liquidity mint (USDC/USDT/wSOL). It's
	// the flash-borrow asset, the swap target, and the payback token.
	debtMint := c.RepayReserve.LiquidityMint
	if !flashloan.HasMarket(debtMint) {
		return SaveFireTx{}, fmt.Errorf("no JupLend flash market for Save debt mint %s", debtMint.String())
	}
	underlying := c.WithdrawReserve.LiquidityMint

	// Same-mint case (seized underlying == debt): no swap — the redeemed
	// liquidity IS the debt asset. Jupiter rejects equal in/out mints, so
	// skip the swap leg (the fixed payback still guards profit).
	sameMint := underlying == debtMint
	var swapIxs []solana.Instruction
	var quotedDebtOut uint64
	var swapALTs []solana.Pubkey
	if sameMint {
		quotedDebtOut = c.SeizeUnderlying
	} else {
		// ExactIn the redeemed collateral underlying -> debt asset. Haircut
		// 0.05% to absorb redeem rounding (same as the marginfi/Kamino
		// paths).
		swapIn := c.SeizeUnderlying - min(c.SeizeUnderlying, c.SeizeUnderlying/2000+1)
		quote, err := jup.Quote(underlying, debtMint, swapIn, slippageBps, maxSwapAccounts)
		if err != nil {
			return SaveFireTx{}, err
		}
		plan, err := jup.SwapInstructions(quote, authority, false)
		if err != nil {
			return SaveFireTx{}, err
		}
		swapIxs = plan.Instructions
		quotedDebtOut = plan.QuotedOut
		swapALTs = plan.ALTAddresses
	}

	saveALT := solana.MustPubkeyFromBase58(config.EnvOr("SAVE_ALT", SaveALT))
	altAddrs := append([]solana.Pubkey{}, swapALTs...)
	if saveALT != systemProgram {
		altAddrs = append(altAddrs, saveALT)
	}
	alts, err := jup.FetchALTs(rpcEndpoint, altAddrs)
	if err != nil {
		return SaveFireTx{}, err
	}

	debtATA := flashloan.AtaFor(authority, debtMint, c.DebtTokenProgram)
	underlyingATA := flashloan.AtaFor(authority, underlying, c.CollateralTokenProgram)
	ctokenATA := flashloan.AtaFor(authority, c.WithdrawReserve.CollateralMint, classicTokenProgram)

	borrowIx, ok := flashloan.Borrow(authority, debtMint, c.RepayAmount)
	if !ok {
		return SaveFireTx{}, fmt.Errorf("no JupLend flash market for Save debt mint %s", debtMint.String())
	}
	paybackIx, ok := flashloan.Payback(authority, debtMint, c.RepayAmount)
	if !ok {
		return SaveFireTx{}, fmt.Errorf("no JupLend flash market for Save debt mint %s", debtMint.String())
	}

	ixs := []solana.Instruction{
		arb.CuLimitIx(FireCULimit),
		arb.CuPriceIx(priorityMicroLamports),
		flashloan.CreateAtaIdempotentFor(authority, debtMint, c.DebtTokenProgram),
		flashloan.CreateAtaIdempotentFor(authority, underlying, c.CollateralTokenProgram),
		flashloan.CreateAtaIdempotentFor(authority, c.WithdrawReserve.CollateralMint, classicTokenProgram),
	}
	// Flash-borrow the debt asset we need to repay the liquidatee's debt.
	ixs = append(ixs, borrowIx)
	// Refresh Save state, then liquidate+redeem.
	ixs = append(ixs, save.RefreshReserve(&c.RepayReserve))
	ixs = append(ixs, save.RefreshReserve(&c.WithdrawReserve))
	ixs = append(ixs, save.RefreshObligation(c.Obligation, c.DepositReserves, c.BorrowReserves))
	ixs = append(ixs, save.LiquidateAndRedeem(
		c.RepayAmount,
		debtATA,       // source_liquidity (repay)
		ctokenATA,     // destination_collateral (transient cTokens)
		underlyingATA, // destination_liquidity (redeemed underlying)
		&c.RepayReserve,
		&c.WithdrawReserve,
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

	msg, err := solana.CompileV0(authority, ixs, alts, blockhash)
	if err != nil {
		return SaveFireTx{}, fmt.Errorf("compile save fire v0: %w", err)
	}
	tx := solana.NewUnsignedVersionedTransaction(msg)
	txBytes, err := tx.MarshalBinary()
	if err != nil {
		return SaveFireTx{}, err
	}
	return SaveFireTx{Tx: tx, QuotedDebtOut: quotedDebtOut, TxBytes: len(txBytes)}, nil
}
