// Jupiter Lend (Fluid) liquidate instruction builder + fire-path scaffold.
//
// VERIFIED (from a real mainnet liquidate tx and the IDL): the instruction
// discriminator + the exact borsh arg encoding, and the 26 named account
// ORDER.
//
// SOLVED: remaining_accounts_indices layout = [oracle_sources, branches,
// ticks, tick_has_debt]; col_per_unit_debt is a minimum-acceptable slippage
// floor (1e15), not the price — the program computes the actual price from
// the vault oracle itself.
//
// SEED-DERIVED: the per-vault Liquidity-program PDAs, new_branch, and the
// oracle sources are derived purely from seeds + on-chain vault/oracle
// state (see DeriveLiquidateAccounts).
package jupiterlend

import (
	"encoding/binary"
	"fmt"
	"os"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/arb"
	"solana-arb-backtest-go/internal/flashloan"
	"solana-arb-backtest-go/internal/jupswap"
	"solana-arb-backtest-go/internal/marginfi"
)

// LiquidateDisc is the liquidate discriminator (sha256("global:liquidate")[..8]).
var LiquidateDisc = [8]byte{223, 179, 226, 125, 48, 46, 39, 74}

const (
	systemProgramID = "11111111111111111111111111111111"
	ataProgramID    = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"
)

func pk(s string) solana.PublicKey { return solana.MustPublicKeyFromBase58(s) }

// LiquidateAccounts is the full account set a liquidate ix needs for one vault.
type LiquidateAccounts struct {
	// liquidator-side (we fill these)
	Signer             solana.PublicKey
	SignerTokenAccount solana.PublicKey // liquidator's borrow-token (debt) ATA
	To                 solana.PublicKey
	ToTokenAccount     solana.PublicKey // liquidator's supply-token (collateral) ATA
	// vault-derived (from VaultConfig)
	VaultConfig   solana.PublicKey
	VaultState    solana.PublicKey
	SupplyToken   solana.PublicKey
	BorrowToken   solana.PublicKey
	Oracle        solana.PublicKey
	OracleProgram solana.PublicKey
	// Liquidity-program per-vault accounts
	NewBranch                      solana.PublicKey
	SupplyTokenReservesLiquidity   solana.PublicKey
	BorrowTokenReservesLiquidity   solana.PublicKey
	VaultSupplyPositionOnLiquidity solana.PublicKey
	VaultBorrowPositionOnLiquidity solana.PublicKey
	SupplyRateModel                solana.PublicKey
	BorrowRateModel                solana.PublicKey
	SupplyTokenClaimAccount        solana.PublicKey
	Liquidity                      solana.PublicKey
	LiquidityProgram               solana.PublicKey
	VaultSupplyTokenAccount        solana.PublicKey
	VaultBorrowTokenAccount        solana.PublicKey
	SupplyTokenProgram             solana.PublicKey
	BorrowTokenProgram             solana.PublicKey
	// tick/branch accounts referenced by remaining_accounts_indices.
	Remaining []solana.PublicKey
}

// AcctOrder is the index positions of the vault's Liquidity-program accounts
// inside a captured liquidate ix's account list (VERIFIED order).
var AcctOrder = []string{
	"signer", "signer_token_account", "to", "to_token_account", "vault_config",
	"vault_state", "supply_token", "borrow_token", "oracle", "new_branch",
	"supply_token_reserves_liquidity", "borrow_token_reserves_liquidity",
	"vault_supply_position_on_liquidity", "vault_borrow_position_on_liquidity",
	"supply_rate_model", "borrow_rate_model", "supply_token_claim_account",
	"liquidity", "liquidity_program", "vault_supply_token_account",
	"vault_borrow_token_account", "supply_token_program", "borrow_token_program",
	"system_program", "associated_token_program", "oracle_program",
}

// BuildLiquidateData borsh-encodes the liquidate instruction data. VERIFIED:
// reproduces the arg bytes of the captured tx. transferType is the inner
// enum discriminant when non-nil (the real tx used Some(1)).
func BuildLiquidateData(debtAmt uint64, colPerUnitDebt *[2]uint64, absorb bool, transferType *uint8, remainingAccountsIndices []uint8) []byte {
	d := make([]byte, 0, 43+len(remainingAccountsIndices))
	d = append(d, LiquidateDisc[:]...)
	d = binary.LittleEndian.AppendUint64(d, debtAmt)
	d = append(d, u128LEBytes(colPerUnitDebt)...)
	d = append(d, boolByte(absorb))
	if transferType != nil {
		d = append(d, 1, *transferType)
	} else {
		d = append(d, 0)
	}
	d = binary.LittleEndian.AppendUint32(d, uint32(len(remainingAccountsIndices)))
	d = append(d, remainingAccountsIndices...)
	return d
}

func u128LEBytes(v *[2]uint64) []byte {
	b := make([]byte, 16)
	binary.LittleEndian.PutUint64(b[0:8], v[0])
	binary.LittleEndian.PutUint64(b[8:16], v[1])
	return b
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

// BuildLiquidateIx assembles the liquidate instruction. Account order is
// VERIFIED; the caller is responsible for supplying correct remaining
// (tick/branch) accounts + indices.
func BuildLiquidateIx(a *LiquidateAccounts, debtAmt uint64, colPerUnitDebt *[2]uint64, absorb bool, transferType *uint8, remainingAccountsIndices []uint8) solana.Instruction {
	metas := solana.AccountMetaSlice{
		solana.NewAccountMeta(a.Signer, true, true),
		solana.NewAccountMeta(a.SignerTokenAccount, true, false),
		solana.NewAccountMeta(a.To, false, false),
		solana.NewAccountMeta(a.ToTokenAccount, true, false),
		solana.NewAccountMeta(a.VaultConfig, false, false),
		solana.NewAccountMeta(a.VaultState, true, false),
		solana.NewAccountMeta(a.SupplyToken, false, false),
		solana.NewAccountMeta(a.BorrowToken, false, false),
		solana.NewAccountMeta(a.Oracle, false, false),
		solana.NewAccountMeta(a.NewBranch, true, false),
		solana.NewAccountMeta(a.SupplyTokenReservesLiquidity, true, false),
		solana.NewAccountMeta(a.BorrowTokenReservesLiquidity, true, false),
		solana.NewAccountMeta(a.VaultSupplyPositionOnLiquidity, true, false),
		solana.NewAccountMeta(a.VaultBorrowPositionOnLiquidity, true, false),
		solana.NewAccountMeta(a.SupplyRateModel, false, false),
		solana.NewAccountMeta(a.BorrowRateModel, false, false),
		solana.NewAccountMeta(a.SupplyTokenClaimAccount, true, false),
		solana.NewAccountMeta(a.Liquidity, false, false),
		solana.NewAccountMeta(a.LiquidityProgram, false, false),
		solana.NewAccountMeta(a.VaultSupplyTokenAccount, true, false),
		solana.NewAccountMeta(a.VaultBorrowTokenAccount, true, false),
		solana.NewAccountMeta(a.SupplyTokenProgram, false, false),
		solana.NewAccountMeta(a.BorrowTokenProgram, false, false),
		solana.NewAccountMeta(pk(systemProgramID), false, false),
		solana.NewAccountMeta(pk(ataProgramID), false, false),
		solana.NewAccountMeta(a.OracleProgram, false, false),
	}
	// Fluid tick/branch remaining accounts (writable — they mutate on liquidation).
	for _, r := range a.Remaining {
		metas = append(metas, solana.NewAccountMeta(r, true, false))
	}
	return solana.NewInstruction(pk(VaultsProgram), metas,
		BuildLiquidateData(debtAmt, colPerUnitDebt, absorb, transferType, remainingAccountsIndices))
}

// AccountsFromCaptured lifts the per-vault Liquidity-program accounts out of
// a captured liquidate ix account list (the AcctOrder positions), so a
// fresh liquidate for the same vault reuses the exact PDAs it used before.
func AccountsFromCaptured(v *Vault, txAccounts []solana.PublicKey) (*LiquidateAccounts, bool) {
	if len(txAccounts) < 26 {
		return nil, false
	}
	g := func(i int) solana.PublicKey { return txAccounts[i] }
	var remaining []solana.PublicKey
	if len(txAccounts) > 26 {
		remaining = append(remaining, txAccounts[26:]...)
	}
	return &LiquidateAccounts{
		Signer: g(0), SignerTokenAccount: g(1), To: g(2), ToTokenAccount: g(3),
		VaultConfig: v.ConfigPubkey, VaultState: v.StatePubkey,
		SupplyToken: v.Config.SupplyToken, BorrowToken: v.Config.BorrowToken,
		Oracle: v.Config.Oracle, OracleProgram: v.Config.OracleProgram,
		NewBranch:                    g(9),
		SupplyTokenReservesLiquidity: g(10), BorrowTokenReservesLiquidity: g(11),
		VaultSupplyPositionOnLiquidity: g(12), VaultBorrowPositionOnLiquidity: g(13),
		SupplyRateModel: g(14), BorrowRateModel: g(15), SupplyTokenClaimAccount: g(16),
		Liquidity: g(17), LiquidityProgram: g(18),
		VaultSupplyTokenAccount: g(19), VaultBorrowTokenAccount: g(20),
		SupplyTokenProgram: g(21), BorrowTokenProgram: g(22),
		Remaining: remaining,
	}, true
}

// DeriveLiquidateAccounts builds the full vault-fixed + Liquidity-program
// account set for a liquidate PURELY FROM SEEDS + on-chain vault state — no
// captured liquidate tx required.
func DeriveLiquidateAccounts(v *Vault, supplyTokenProgram, borrowTokenProgram solana.PublicKey) *LiquidateAccounts {
	vc := v.ConfigPubkey
	supply := v.Config.SupplyToken
	borrow := v.Config.BorrowToken
	liq := LiquidityPDA()
	nbID := NewBranchID(v.State.BranchLiquidated, v.State.CurrentBranchID, v.State.TotalBranchID)
	return &LiquidateAccounts{
		VaultConfig:                    vc,
		VaultState:                     v.StatePubkey,
		SupplyToken:                    supply,
		BorrowToken:                    borrow,
		Oracle:                         v.Config.Oracle,
		OracleProgram:                  v.Config.OracleProgram,
		NewBranch:                      BranchPDA(v.Config.VaultID, nbID),
		SupplyTokenReservesLiquidity:   ReservePDA(supply),
		BorrowTokenReservesLiquidity:   ReservePDA(borrow),
		VaultSupplyPositionOnLiquidity: UserSupplyPositionPDA(supply, vc),
		VaultBorrowPositionOnLiquidity: UserBorrowPositionPDA(borrow, vc),
		SupplyRateModel:                RateModelPDA(supply),
		BorrowRateModel:                RateModelPDA(borrow),
		// supply_token_claim_account is Option<UncheckedAccount> and is only
		// consumed for claim-type withdraws, which a liquidation is NOT — so
		// the real liquidator passes None. Anchor encodes a None optional
		// account as the invoked program's own id (the VAULTS program).
		SupplyTokenClaimAccount: pk(VaultsProgram),
		Liquidity:               liq,
		LiquidityProgram:        pk(LiquidityProgram),
		VaultSupplyTokenAccount: flashloan.AtaFor(liq, supply, supplyTokenProgram),
		VaultBorrowTokenAccount: flashloan.AtaFor(liq, borrow, borrowTokenProgram),
		SupplyTokenProgram:      supplyTokenProgram,
		BorrowTokenProgram:      borrowTokenProgram,
	}
}

// AddressDead is the all-zero pubkey. Passing it as `to` triggers the
// program's built-in resolver: it runs the full liquidation math and
// REVERTS with VaultLiquidationResult:[actual_col, actual_debt,
// topmost_tick] — exact ground truth for pricing/sizing.
var AddressDead = solana.PublicKey{}

// SetLiquidatorSide overwrites the liquidator-side accounts on a captured
// account set, so a fresh liquidate seizes to OUR wallet.
func SetLiquidatorSide(a *LiquidateAccounts, signer, signerTokenAccount, to, toTokenAccount solana.PublicKey) {
	a.Signer = signer
	a.SignerTokenAccount = signerTokenAccount
	a.To = to
	a.ToTokenAccount = toTokenAccount
}

// BuildRemainingAccounts builds the remaining_accounts +
// remaining_accounts_indices for a liquidate, derived from CURRENT on-chain
// state — the layout is [oracle_sources, branches, ticks, tick_has_debt].
// oracleSources are lifted from a recent tx (deterministic per vault
// oracle). fetch reads raw account bytes (nil,false if the account does not
// exist / not owned by the program). liquidationTick is jupiterlend's
// value (bit-identical to what the program computes).
func BuildRemainingAccounts(
	vaultID uint16, topmostTick int32, currentBranchID uint32, liquidationTick int32,
	oracleSources []solana.PublicKey, fetch func(solana.PublicKey) ([]byte, bool),
) ([]solana.PublicKey, [4]uint8) {
	// ── branches: current branch, then walk connected_branch_id, always incl 0 ──
	var branchIDs []uint32
	connected := uint32(0)
	if currentBranchID > 0 {
		if raw, ok := fetch(BranchPDA(vaultID, currentBranchID)); ok {
			if b, ok := DecodeBranchLite(raw); ok {
				branchIDs = append(branchIDs, currentBranchID)
				connected = b.ConnectedBranchID
			}
		}
	}
	for connected > 0 && !containsU32(branchIDs, connected) {
		pda := BranchPDA(vaultID, connected)
		raw, ok := fetch(pda)
		if !ok {
			break
		}
		b, ok := DecodeBranchLite(raw)
		if !ok {
			break
		}
		branchIDs = append(branchIDs, connected)
		connected = b.ConnectedBranchID
	}
	if !containsU32(branchIDs, 0) {
		branchIDs = append(branchIDs, 0)
	}

	// ── ticks: topmost (if a real perfect tick exists) then walk down to liq_tick ─
	arrayFetch := func(idx uint8) ([]byte, bool) { return fetch(TickHasDebtPDA(vaultID, idx)) }
	var ticksList []int32
	if topmostTick > liquidationTick {
		if _, ok := fetch(TickPDA(vaultID, topmostTick)); ok {
			ticksList = append(ticksList, topmostTick)
		}
	}
	nextTick := FindNextTickWithDebt(topmostTick, arrayFetch)
	for nextTick > liquidationTick && !containsI32(ticksList, nextTick) {
		if _, ok := fetch(TickPDA(vaultID, nextTick)); ok {
			ticksList = append(ticksList, nextTick)
		}
		n := FindNextTickWithDebt(nextTick, arrayFetch)
		if n == nextTick {
			break
		}
		nextTick = n
	}

	// ── tick_has_debt arrays: index(topmost) down to index(next_tick) ──
	topIdx := IndexForTick(topmostTick)
	nextIdx := IndexForTick(nextTick)
	hi, lo := topIdx, nextIdx
	if nextIdx > topIdx {
		hi, lo = nextIdx, topIdx
	}
	var thdIndices []uint8
	for i := int(hi); i >= int(lo); i-- {
		thdIndices = append(thdIndices, uint8(i))
	}

	// ── assemble in the exact program order ──
	var remaining []solana.PublicKey
	remaining = append(remaining, oracleSources...)
	for _, b := range branchIDs {
		remaining = append(remaining, BranchPDA(vaultID, b))
	}
	for _, t := range ticksList {
		remaining = append(remaining, TickPDA(vaultID, t))
	}
	for _, i := range thdIndices {
		remaining = append(remaining, TickHasDebtPDA(vaultID, i))
	}

	indices := [4]uint8{
		uint8(len(oracleSources)), uint8(len(branchIDs)), uint8(len(ticksList)), uint8(len(thdIndices)),
	}
	return remaining, indices
}

func containsU32(v []uint32, x uint32) bool {
	for _, e := range v {
		if e == x {
			return true
		}
	}
	return false
}
func containsI32(v []int32, x int32) bool {
	for _, e := range v {
		if e == x {
			return true
		}
	}
	return false
}

// ── flash-loan-wrapped fire tx (USDC-debt vaults; mirrors the Save fire path) ──
const FireCULimit uint32 = 1_400_000

// AltPlaceholder is the all-1s system-program pubkey = the "JUP_ALT not
// deployed yet" sentinel; skip it rather than fetch a table that doesn't exist.
const AltPlaceholder = "11111111111111111111111111111111"

// FireCandidate is one sized Jupiter-Lend liquidation opportunity (USDC debt).
type FireCandidate struct {
	// Fully-resolved liquidate accounts (Liquidity PDAs lifted from a real
	// tx; liquidator-side + remaining set to OURS via the builder).
	Accts *LiquidateAccounts
	// Debt (USDC, 6dp) we repay.
	DebtAmt uint64
	// Slippage floor (1e15). 0 = accept the oracle price.
	ColPerUnitDebt *[2]uint64
	// remaining accounts + indices from BuildRemainingAccounts.
	Remaining        []solana.PublicKey
	RemainingIndices [4]uint8
	// Collateral (supply_token) underlying we expect to seize, to size the swap.
	SeizeUnderlying uint64
	// Collateral mint + its token program (for the swap-back ATA).
	CollateralMint         solana.PublicKey
	CollateralTokenProgram solana.PublicKey
}

type FireTx struct {
	Tx            *solana.Transaction
	QuotedUSDCOut uint64
	TxBytes       int
}

var transferTypeOne = uint8(1)

// BuildFireTx builds the unsigned flash-loan-wrapped liquidate tx:
//
//	[cu, ATAs, marginfi start_flashloan → borrow USDC,
//	 jupiter LIQUIDATE (repay USDC, seize collateral),
//	 Jupiter swap collateral→USDC, marginfi payback + end_flashloan, tip]
func BuildFireTx(
	rpcEndpoint string,
	c *FireCandidate,
	liquidatorMA, authority solana.PublicKey,
	tipAccount *solana.PublicKey,
	tipLamports, priorityMicroLamports uint64,
	slippageBps uint32,
	maxSwapAccounts int,
	blockhash solana.Hash,
) (*FireTx, error) {
	usdc := pk(flashloan.USDCMint)
	tokenProgram := pk(flashloan.TokenProgram)
	if !c.Accts.BorrowToken.Equals(usdc) {
		return nil, fmt.Errorf("jupiter fire path currently wraps only USDC debt, got %s", c.Accts.BorrowToken)
	}

	// Swap leg: sell the seized collateral underlying → USDC (0.05% haircut
	// for seize rounding).
	swapIn := c.SeizeUnderlying - (c.SeizeUnderlying/2000 + 1)
	quote, err := jupswap.Quote(c.CollateralMint, usdc, swapIn, slippageBps, maxSwapAccounts)
	if err != nil {
		return nil, err
	}
	plan, err := jupswap.SwapInstructions(quote, authority, false)
	if err != nil {
		return nil, err
	}
	altAddrs := append([]solana.PublicKey{}, plan.AltAddresses...)
	// JUP_ALT holds the fixed liquidate accounts; LIQ_ALT is accepted too for
	// fleet parity. An unset var — or the all-1s placeholder — is skipped.
	for _, v := range []string{"JUP_ALT", "LIQ_ALT"} {
		if alt := os.Getenv(v); alt != "" && alt != AltPlaceholder {
			if p, err := solana.PublicKeyFromBase58(alt); err == nil {
				altAddrs = append(altAddrs, p)
			}
		}
	}
	alts, err := jupswap.FetchAlts(rpcEndpoint, altAddrs)
	if err != nil {
		return nil, err
	}

	usdcAta := flashloan.AtaFor(authority, usdc, tokenProgram)
	collatAta := flashloan.AtaFor(authority, c.CollateralMint, c.CollateralTokenProgram)

	a := *c.Accts
	SetLiquidatorSide(&a, authority, usdcAta, authority, collatAta)
	a.Remaining = c.Remaining

	ixs := []solana.Instruction{
		arb.CuLimitIx(FireCULimit),
		arb.CuPriceIx(priorityMicroLamports),
		flashloan.CreateAtaIdempotentFor(authority, usdc, tokenProgram),
		flashloan.CreateAtaIdempotentFor(authority, c.CollateralMint, c.CollateralTokenProgram),
	}
	startIdx := len(ixs)
	ixs = append(ixs, marginfi.StartFlashloan(liquidatorMA, authority, 0)) // end_index patched below
	ixs = append(ixs, marginfi.BorrowUSDC(liquidatorMA, authority, usdcAta, c.DebtAmt))
	// The reversed liquidate ix — correctly priced + tick/branch accounts.
	ixs = append(ixs, BuildLiquidateIx(&a, c.DebtAmt, c.ColPerUnitDebt, false, &transferTypeOne, c.RemainingIndices[:]))
	ixs = append(ixs, plan.Instructions...)
	ixs = append(ixs, marginfi.PaybackUSDC(liquidatorMA, authority, usdcAta, c.DebtAmt, true))
	endIndex := uint64(len(ixs))
	ixs[startIdx] = marginfi.StartFlashloan(liquidatorMA, authority, endIndex)
	ixs = append(ixs, marginfi.EndFlashloan(liquidatorMA, authority, nil))
	if tipAccount != nil && tipLamports > 0 {
		ixs = append(ixs, arb.TransferIx(authority, *tipAccount, tipLamports))
	}

	tx, err := arb.CompileV0(authority, ixs, alts, blockhash)
	if err != nil {
		return nil, fmt.Errorf("compile jupiter fire v0: %w", err)
	}
	txBytes, err := tx.MarshalBinary()
	if err != nil {
		return nil, err
	}
	return &FireTx{Tx: tx, QuotedUSDCOut: plan.QuotedOut, TxBytes: len(txBytes)}, nil
}
