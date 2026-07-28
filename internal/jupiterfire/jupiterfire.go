// Package jupiterfire is the Jupiter Lend (Fluid) liquidate instruction
// builder + fire-path scaffold.
//
// HONESTY BANNER — what is VERIFIED vs INFERRED (read before trusting this).
//
// VERIFIED (from a real mainnet liquidate tx 5nLVofDj... and the IDL):
//   - the instruction discriminator + the exact borsh arg encoding (debt_amt
//     u64, col_per_unit_debt u128, absorb bool, transfer_type Option<enum>,
//     remaining_accounts_indices Vec<u8>), unit-tested to reproduce the
//     captured tx's arg bytes;
//   - the 26 named account ORDER (matched account-for-account to that tx).
//
// SOLVED (see internal/jupitermath + jupiter_fire_probe, reversed from the
// on-chain program source and the published SDK, verified against 8 real
// txs):
//   - remaining_accounts_indices layout = [oracle_sources, branches, ticks,
//     tick_has_debt] and the tick/branch account SELECTION -> buildRemainingAccounts
//     derives the exact PDA set from live vault state;
//   - col_per_unit_debt -- reversed as a *minimum-acceptable slippage floor*
//     (1e15), NOT the price: the program computes the actual price from the
//     vault oracle itself. Real liquidators pass 0 (accept oracle price -
//     2/8 txs) or a computed floor. jupitermath.ComputeColPerDebt reproduces
//     the on-chain formula; the resolver revert (to=ADDRESS_DEAD) yields the
//     exact live ratio.
//
// SEED-DERIVED (the former tx-replay dependency is GONE -- see
// DeriveLiquidateAccounts): the per-vault Liquidity-program PDAs
// (reserves/positions/token accounts/rate models), new_branch, and the
// oracle sources are now derived PURELY from seeds + on-chain vault/oracle
// state. Seeds reversed from the on-chain Anchor source (audit repo
// code-423n4/2026-02-jupiter-lend) and validated against live accounts
// (jupiter_seed_probe PROOF A = 159/159 across 14 vaults) + the on-chain
// program's own #[account(seeds=...)] re-derivation at sim
// (jupiter_fire_probe STAGE 5: the fully seed-derived set gates at
// VaultInvalidLiquidation 6027 on a vault with NO recent liquidate tx). The
// liquidate sim is still the ground-truth gate before any fire.
package jupiterfire

import (
	"encoding/binary"
	"fmt"
	"os"

	"arbengine/internal/arb"
	"arbengine/internal/flashloan"
	"arbengine/internal/jup"
	"arbengine/internal/jupiter"
	"arbengine/internal/jupitermath"
	"arbengine/internal/marginfi"
	"arbengine/internal/solana"
)

// LiquidateDisc is the liquidate discriminator (sha256("global:liquidate")[..8])
// -- VERIFIED against the on-chain tx.
var LiquidateDisc = [8]byte{223, 179, 226, 125, 48, 46, 39, 74}

const (
	systemProgram = "11111111111111111111111111111111"
	ataProgram    = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"
)

// LiquidateAccounts is the full account set a liquidate ix needs for one
// vault. The vault-derived fields come from VaultConfig; the
// Liquidity-program PDAs are captured from a recent on-chain tx via
// AccountsFromCaptured (see the honesty banner).
type LiquidateAccounts struct {
	// liquidator-side (we fill these)
	Signer             solana.Pubkey
	SignerTokenAccount solana.Pubkey // liquidator's borrow-token (debt) ATA
	To                 solana.Pubkey
	ToTokenAccount     solana.Pubkey // liquidator's supply-token (collateral) ATA
	// vault-derived (from VaultConfig)
	VaultConfig   solana.Pubkey
	VaultState    solana.Pubkey
	SupplyToken   solana.Pubkey
	BorrowToken   solana.Pubkey
	Oracle        solana.Pubkey
	OracleProgram solana.Pubkey
	// Liquidity-program per-vault accounts (captured from a real tx)
	NewBranch                      solana.Pubkey
	SupplyTokenReservesLiquidity   solana.Pubkey
	BorrowTokenReservesLiquidity   solana.Pubkey
	VaultSupplyPositionOnLiquidity solana.Pubkey
	VaultBorrowPositionOnLiquidity solana.Pubkey
	SupplyRateModel                solana.Pubkey
	BorrowRateModel                solana.Pubkey
	SupplyTokenClaimAccount        solana.Pubkey
	Liquidity                      solana.Pubkey
	LiquidityProgram               solana.Pubkey
	VaultSupplyTokenAccount        solana.Pubkey
	VaultBorrowTokenAccount        solana.Pubkey
	SupplyTokenProgram             solana.Pubkey
	BorrowTokenProgram             solana.Pubkey
	// Remaining is the tick/branch accounts referenced by
	// remaining_accounts_indices.
	Remaining []solana.Pubkey
}

// AcctOrder is the index positions of the vault's Liquidity-program accounts
// inside a captured liquidate ix's account list (VERIFIED order from tx
// 5nLVofDj...). Lets us lift the hard-to-derive PDAs straight from a real tx
// for the same vault.
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
// reproduces the arg bytes of the captured tx (see tests). transferType is
// the inner enum discriminant when set (the real tx used Some(1)).
func BuildLiquidateData(debtAmt uint64, colPerUnitDebt [16]byte, absorb bool, transferType *uint8, remainingAccountsIndices []byte) []byte {
	d := make([]byte, 0, 43+len(remainingAccountsIndices))
	d = append(d, LiquidateDisc[:]...)
	var amtBuf [8]byte
	binary.LittleEndian.PutUint64(amtBuf[:], debtAmt)
	d = append(d, amtBuf[:]...)
	d = append(d, colPerUnitDebt[:]...) // already little-endian raw bytes
	if absorb {
		d = append(d, 1)
	} else {
		d = append(d, 0)
	}
	if transferType != nil {
		d = append(d, 1, *transferType)
	} else {
		d = append(d, 0)
	}
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(remainingAccountsIndices)))
	d = append(d, lenBuf[:]...)
	d = append(d, remainingAccountsIndices...)
	return d
}

// u128LEBytes encodes a u128 value (given as *big-endian-free* uint64 hi/lo
// is avoided; callers pass the raw little-endian bytes directly via
// ColPerUnitDebtBytes helpers). This helper turns a uint64 into the raw
// 16-byte little-endian u128 representation (high 8 bytes zero).
func u128LEBytesFromU64(v uint64) [16]byte {
	var out [16]byte
	binary.LittleEndian.PutUint64(out[0:8], v)
	return out
}

// BuildLiquidateIx assembles the liquidate instruction. Account order is
// VERIFIED; the caller is responsible for supplying correct Remaining
// (tick/branch) accounts + indices (see honesty banner -- currently
// INFERRED).
func BuildLiquidateIx(a *LiquidateAccounts, debtAmt uint64, colPerUnitDebt [16]byte, absorb bool, transferType *uint8, remainingAccountsIndices []byte) solana.Instruction {
	accounts := []solana.AccountMeta{
		solana.NewAccountMeta(a.Signer, true, true),
		solana.Writable(a.SignerTokenAccount),
		solana.ReadonlyMeta(a.To),
		solana.Writable(a.ToTokenAccount),
		solana.ReadonlyMeta(a.VaultConfig),
		solana.Writable(a.VaultState),
		solana.ReadonlyMeta(a.SupplyToken),
		solana.ReadonlyMeta(a.BorrowToken),
		solana.ReadonlyMeta(a.Oracle),
		solana.Writable(a.NewBranch),
		solana.Writable(a.SupplyTokenReservesLiquidity),
		solana.Writable(a.BorrowTokenReservesLiquidity),
		solana.Writable(a.VaultSupplyPositionOnLiquidity),
		solana.Writable(a.VaultBorrowPositionOnLiquidity),
		solana.ReadonlyMeta(a.SupplyRateModel),
		solana.ReadonlyMeta(a.BorrowRateModel),
		solana.Writable(a.SupplyTokenClaimAccount),
		solana.ReadonlyMeta(a.Liquidity),
		solana.ReadonlyMeta(a.LiquidityProgram),
		solana.Writable(a.VaultSupplyTokenAccount),
		solana.Writable(a.VaultBorrowTokenAccount),
		solana.ReadonlyMeta(a.SupplyTokenProgram),
		solana.ReadonlyMeta(a.BorrowTokenProgram),
		solana.ReadonlyMeta(solana.MustPubkeyFromBase58(systemProgram)),
		solana.ReadonlyMeta(solana.MustPubkeyFromBase58(ataProgram)),
		solana.ReadonlyMeta(a.OracleProgram),
	}
	// Fluid tick/branch remaining accounts (writable -- they mutate on liquidation).
	for _, r := range a.Remaining {
		accounts = append(accounts, solana.Writable(r))
	}
	return solana.Instruction{
		ProgramID: solana.MustPubkeyFromBase58(jupiter.VaultsProgram),
		Accounts:  accounts,
		Data:      BuildLiquidateData(debtAmt, colPerUnitDebt, absorb, transferType, remainingAccountsIndices),
	}
}

// AccountsFromCaptured lifts the per-vault Liquidity-program accounts out of
// a captured liquidate ix account list (the AcctOrder positions), so a fresh
// liquidate for the same vault reuses the exact PDAs it used before.
// txAccounts = the ordered account pubkeys of a real liquidate ix for this
// vault; the accounts past index 26 are treated as Remaining. Returns the
// vault-fixed accounts (signer/token-accounts left to the caller). This is
// the derive-from-truth account resolver.
func AccountsFromCaptured(v *jupiter.Vault, txAccounts []solana.Pubkey) (LiquidateAccounts, bool) {
	if len(txAccounts) < 26 {
		return LiquidateAccounts{}, false
	}
	g := func(i int) solana.Pubkey { return txAccounts[i] }
	var remaining []solana.Pubkey
	if len(txAccounts) > 26 {
		remaining = append(remaining, txAccounts[26:]...)
	}
	return LiquidateAccounts{
		Signer:                         g(0),
		SignerTokenAccount:             g(1),
		To:                             g(2),
		ToTokenAccount:                 g(3),
		VaultConfig:                    v.ConfigPubkey,
		VaultState:                     v.StatePubkey,
		SupplyToken:                    v.Config.SupplyToken,
		BorrowToken:                    v.Config.BorrowToken,
		Oracle:                         v.Config.Oracle,
		OracleProgram:                  v.Config.OracleProgram,
		NewBranch:                      g(9),
		SupplyTokenReservesLiquidity:   g(10),
		BorrowTokenReservesLiquidity:   g(11),
		VaultSupplyPositionOnLiquidity: g(12),
		VaultBorrowPositionOnLiquidity: g(13),
		SupplyRateModel:                g(14),
		BorrowRateModel:                g(15),
		SupplyTokenClaimAccount:        g(16),
		Liquidity:                      g(17),
		LiquidityProgram:               g(18),
		VaultSupplyTokenAccount:        g(19),
		VaultBorrowTokenAccount:        g(20),
		SupplyTokenProgram:             g(21),
		BorrowTokenProgram:             g(22),
		Remaining:                      remaining,
	}, true
}

// DeriveLiquidateAccounts builds the full vault-fixed + Liquidity-program
// account set for a liquidate PURELY FROM SEEDS + on-chain vault state -- no
// captured liquidate tx required. This is the standalone resolver that
// unblocks arming for vaults with no recent liquidate (the previous
// AccountsFromCaptured path). Every Liquidity PDA is re-derived by the
// liquidity program's own #[account(seeds=...)] on the CPI, so a wrong seed
// reverts at sim (ConstraintSeeds) -- the program is the check.
//
// Signer-side accounts (signer + our debt/collateral ATAs, To) are left
// default; set them via SetLiquidatorSide. Remaining is filled by
// BuildRemainingAccounts. supplyTokenProgram/borrowTokenProgram are the
// mints' owning token programs (SPL-Token or Token-2022) -- resolve from
// getAccountInfo(mint).owner.
func DeriveLiquidateAccounts(v *jupiter.Vault, supplyTokenProgram, borrowTokenProgram solana.Pubkey) LiquidateAccounts {
	vc := v.ConfigPubkey
	supply := v.Config.SupplyToken
	borrow := v.Config.BorrowToken
	liq := jupitermath.LiquidityPDA()
	nbID := jupitermath.NewBranchID(v.State.BranchLiquidated, v.State.CurrentBranchID, v.State.TotalBranchID)
	return LiquidateAccounts{
		Signer:                         solana.ZeroPubkey,
		SignerTokenAccount:             solana.ZeroPubkey,
		To:                             solana.ZeroPubkey,
		ToTokenAccount:                 solana.ZeroPubkey,
		VaultConfig:                    vc,
		VaultState:                     v.StatePubkey,
		SupplyToken:                    supply,
		BorrowToken:                    borrow,
		Oracle:                         v.Config.Oracle,
		OracleProgram:                  v.Config.OracleProgram,
		NewBranch:                      jupitermath.BranchPDA(v.Config.VaultID, nbID),
		SupplyTokenReservesLiquidity:   jupitermath.ReservePDA(supply),
		BorrowTokenReservesLiquidity:   jupitermath.ReservePDA(borrow),
		VaultSupplyPositionOnLiquidity: jupitermath.UserSupplyPositionPDA(supply, vc),
		VaultBorrowPositionOnLiquidity: jupitermath.UserBorrowPositionPDA(borrow, vc),
		SupplyRateModel:                jupitermath.RateModelPDA(supply),
		BorrowRateModel:                jupitermath.RateModelPDA(borrow),
		// SupplyTokenClaimAccount is Option<UncheckedAccount> on Liquidate and
		// is ONLY consumed for claim-type withdraws (is_claim_type), which a
		// liquidation is NOT -- so the real liquidator passes None. Anchor
		// encodes a None optional account as the invoked program's own id
		// (the VAULTS program), which is what we pass. (The would-be
		// UserClaim PDA is jupitermath.UserClaimPDA(vc, supply); it isn't
		// created for a vault, confirming None -- see jupitermath.)
		SupplyTokenClaimAccount: solana.MustPubkeyFromBase58(jupiter.VaultsProgram),
		Liquidity:               liq,
		LiquidityProgram:        solana.MustPubkeyFromBase58(jupiter.LiquidityProgram),
		VaultSupplyTokenAccount: flashloan.AtaFor(liq, supply, supplyTokenProgram),
		VaultBorrowTokenAccount: flashloan.AtaFor(liq, borrow, borrowTokenProgram),
		SupplyTokenProgram:      supplyTokenProgram,
		BorrowTokenProgram:      borrowTokenProgram,
		Remaining:               nil,
	}
}

// ADDRESSDEAD is the all-zero pubkey. Passing it as To triggers the
// program's built-in resolver: it runs the full liquidation math and
// REVERTS with VaultLiquidationResult: [actual_col, actual_debt,
// topmost_tick] -- exact ground truth for pricing/sizing, computed by the
// program itself.
var ADDRESSDEAD = solana.ZeroPubkey

// SetLiquidatorSide overwrites the liquidator-side accounts (signer + our
// debt/collateral ATAs) on a captured account set, so a fresh liquidate
// seizes to OUR wallet.
func SetLiquidatorSide(a *LiquidateAccounts, signer, signerTokenAccount, to, toTokenAccount solana.Pubkey) {
	a.Signer = signer
	a.SignerTokenAccount = signerTokenAccount
	a.To = to
	a.ToTokenAccount = toTokenAccount
}

// BuildRemainingAccounts builds the remaining_accounts +
// remaining_accounts_indices for a liquidate, derived from CURRENT on-chain
// state -- the layout is [oracle_sources, branches, ticks, tick_has_debt]
// (verified against 8 real mainnet txs; see jupiter_fire_probe). Ported from
// the SDK getRemainingAccountsLiquidate.
//
// oracleSources are lifted from a recent tx (deterministic per vault
// oracle). fetch reads raw account bytes (ok=false if the account does not
// exist / not owned by the program). liquidationTick is jupitermath's value
// (bit-identical to what the program computes) -- pass what you compute
// from the live oracle price via jupitermath.LiquidationTickFromPrice1e8, or
// a low bound to include more ticks.
func BuildRemainingAccounts(
	vaultID uint16,
	topmostTick int32,
	currentBranchID uint32,
	liquidationTick int32,
	oracleSources []solana.Pubkey,
	fetch func(solana.Pubkey) ([]byte, bool),
) ([]solana.Pubkey, [4]byte) {
	// -- branches: current branch, then walk connected_branch_id, always incl 0 --
	var branchIDs []uint32
	var connected uint32
	if currentBranchID > 0 {
		if raw, ok := fetch(jupitermath.BranchPDA(vaultID, currentBranchID)); ok {
			if b, ok := jupitermath.DecodeBranchLite(raw); ok {
				branchIDs = append(branchIDs, currentBranchID)
				connected = b.ConnectedBranchID
			}
		}
	}
	for connected > 0 && !containsU32(branchIDs, connected) {
		pda := jupitermath.BranchPDA(vaultID, connected)
		raw, ok := fetch(pda)
		if !ok {
			break
		}
		b, ok := jupitermath.DecodeBranchLite(raw)
		if !ok {
			break
		}
		branchIDs = append(branchIDs, connected)
		connected = b.ConnectedBranchID
	}
	if !containsU32(branchIDs, 0) {
		branchIDs = append(branchIDs, 0)
	}

	// -- ticks: topmost (if a real perfect tick exists) then walk down to liq_tick --
	arrayFetch := func(idx uint8) ([]byte, bool) { return fetch(jupitermath.TickHasDebtPDA(vaultID, idx)) }
	var ticks []int32
	if topmostTick > liquidationTick {
		if _, ok := fetch(jupitermath.TickPDA(vaultID, topmostTick)); ok {
			ticks = append(ticks, topmostTick)
		}
	}
	nextTick := jupitermath.FindNextTickWithDebt(topmostTick, arrayFetch)
	for nextTick > liquidationTick && !containsI32(ticks, nextTick) {
		if _, ok := fetch(jupitermath.TickPDA(vaultID, nextTick)); ok {
			ticks = append(ticks, nextTick)
		}
		n := jupitermath.FindNextTickWithDebt(nextTick, arrayFetch)
		if n == nextTick {
			break
		}
		nextTick = n
	}

	// -- tick_has_debt arrays: index(topmost) down to index(next_tick) --
	topIdx := jupitermath.IndexForTick(topmostTick)
	nextIdx := jupitermath.IndexForTick(nextTick)
	hi, lo := topIdx, nextIdx
	if nextIdx > topIdx {
		hi, lo = nextIdx, topIdx
	}
	var thdIndices []uint8
	for i := hi; ; i-- {
		thdIndices = append(thdIndices, i)
		if i == lo {
			break
		}
	}

	// -- assemble in the exact program order --
	var remaining []solana.Pubkey
	remaining = append(remaining, oracleSources...)
	for _, b := range branchIDs {
		remaining = append(remaining, jupitermath.BranchPDA(vaultID, b))
	}
	for _, t := range ticks {
		remaining = append(remaining, jupitermath.TickPDA(vaultID, t))
	}
	for _, i := range thdIndices {
		remaining = append(remaining, jupitermath.TickHasDebtPDA(vaultID, i))
	}

	indices := [4]byte{
		byte(len(oracleSources)),
		byte(len(branchIDs)),
		byte(len(ticks)),
		byte(len(thdIndices)),
	}
	return remaining, indices
}

func containsU32(s []uint32, v uint32) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func containsI32(s []int32, v int32) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// ── flash-loan-wrapped fire tx (USDC-debt vaults; mirrors save_fire.go) ─────

// FireCULimit is the compute-unit limit for the fire tx.
const FireCULimit uint32 = 1_400_000

const usdcMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
const tokenProgram = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"

// altPlaceholder is the all-1s system-program pubkey = the "JUP_ALT not
// deployed yet" sentinel; skip it rather than fetch a table that doesn't
// exist (mirrors save_fire's SAVE_ALT).
const altPlaceholder = "11111111111111111111111111111111"

// FireCandidate is one sized Jupiter-Lend liquidation opportunity (USDC debt).
type FireCandidate struct {
	// Accts is the fully-resolved liquidate accounts (Liquidity PDAs lifted
	// from a real tx; liquidator-side + Remaining set to OURS via the
	// builder).
	Accts LiquidateAccounts
	// DebtAmt is the debt (USDC, 6dp) we repay.
	DebtAmt uint64
	// ColPerUnitDebt is the slippage floor (1e15), raw little-endian u128
	// bytes. Zero = accept the oracle price (proven safe by real txs); or a
	// jupitermath.ComputeColPerDebt / resolver-derived value.
	ColPerUnitDebt [16]byte
	// Remaining + RemainingIndices come from BuildRemainingAccounts.
	Remaining        []solana.Pubkey
	RemainingIndices [4]byte
	// SeizeUnderlying is the collateral (supply_token) underlying we expect
	// to seize, to size the swap.
	SeizeUnderlying uint64
	// CollateralMint + CollateralTokenProgram are the collateral mint + its
	// token program (for the swap-back ATA).
	CollateralMint         solana.Pubkey
	CollateralTokenProgram solana.Pubkey
}

// FireTx is the built unsigned fire transaction plus sizing metadata.
type FireTx struct {
	Tx            solana.VersionedTransaction
	QuotedUSDCOut uint64
	TxBytes       int
}

// BuildJupiterFireTx builds the unsigned flash-loan-wrapped liquidate tx:
//
//	[cu, ATAs, marginfi start_flashloan -> borrow USDC,
//	 jupiter LIQUIDATE (repay USDC, seize collateral),
//	 Jupiter swap collateral->USDC, marginfi payback + end_flashloan, tip]
//
// Same profit-or-revert shape as the Save path. blockhash default for
// replace-blockhash simulation.
func BuildJupiterFireTx(
	rpcEndpoint string,
	c *FireCandidate,
	liquidatorMA *solana.Pubkey,
	authority *solana.Pubkey,
	tipAccount *solana.Pubkey,
	tipLamports uint64,
	priorityMicroLamports uint64,
	slippageBps uint32,
	maxSwapAccounts int,
	blockhash solana.Hash,
) (FireTx, error) {
	usdc := solana.MustPubkeyFromBase58(usdcMint)
	tp := solana.MustPubkeyFromBase58(tokenProgram)
	if c.Accts.BorrowToken != usdc {
		return FireTx{}, fmt.Errorf("jupiter fire path currently wraps only USDC debt, got %s", c.Accts.BorrowToken.String())
	}

	// Swap leg: sell the seized collateral underlying -> USDC (0.05%
	// haircut for seize rounding, as in save_fire).
	swapIn := c.SeizeUnderlying - (c.SeizeUnderlying/2000 + 1)
	if c.SeizeUnderlying < c.SeizeUnderlying/2000+1 {
		swapIn = 0
	}
	quote, err := jup.Quote(c.CollateralMint, usdc, swapIn, slippageBps, maxSwapAccounts)
	if err != nil {
		return FireTx{}, err
	}
	plan, err := jup.SwapInstructions(quote, *authority, false)
	if err != nil {
		return FireTx{}, err
	}
	altAddrs := append([]solana.Pubkey{}, plan.ALTAddresses...)
	// JUP_ALT holds the fixed liquidate accounts (see jup_alt_print);
	// LIQ_ALT is accepted too for fleet parity. Both are appended to
	// Jupiter's own swap ALTs, exactly like save_fire folds in SAVE_ALT. An
	// unset var -- or the all-1s placeholder (= "not deployed") -- is
	// skipped so we never try to fetch a non-existent table.
	for _, v := range []string{"JUP_ALT", "LIQ_ALT"} {
		alt, ok := os.LookupEnv(v)
		if !ok {
			continue
		}
		if alt == altPlaceholder {
			continue
		}
		pk, err := solana.PubkeyFromBase58(alt)
		if err == nil {
			altAddrs = append(altAddrs, pk)
		}
	}
	alts, err := jup.FetchALTs(rpcEndpoint, altAddrs)
	if err != nil {
		return FireTx{}, err
	}

	usdcAta := flashloan.AtaFor(*authority, usdc, tp)
	collatAta := flashloan.AtaFor(*authority, c.CollateralMint, c.CollateralTokenProgram)

	a := c.Accts
	SetLiquidatorSide(&a, *authority, usdcAta, *authority, collatAta)
	a.Remaining = append([]solana.Pubkey{}, c.Remaining...)

	ixs := []solana.Instruction{
		arb.CuLimitIx(FireCULimit),
		arb.CuPriceIx(priorityMicroLamports),
		flashloan.CreateAtaIdempotentFor(*authority, usdc, tp),
		flashloan.CreateAtaIdempotentFor(*authority, c.CollateralMint, c.CollateralTokenProgram),
	}
	startIdx := len(ixs)
	ixs = append(ixs, marginfi.StartFlashloan(*liquidatorMA, *authority, 0)) // end_index patched below
	ixs = append(ixs, marginfi.BorrowUSDC(*liquidatorMA, *authority, usdcAta, c.DebtAmt))
	// The reversed liquidate ix -- correctly priced + tick/branch accounts.
	transferType := uint8(1)
	ixs = append(ixs, BuildLiquidateIx(&a, c.DebtAmt, c.ColPerUnitDebt, false, &transferType, c.RemainingIndices[:]))
	ixs = append(ixs, plan.Instructions...)
	ixs = append(ixs, marginfi.PaybackUSDC(*liquidatorMA, *authority, usdcAta, c.DebtAmt, true))
	endIndex := uint64(len(ixs))
	ixs[startIdx] = marginfi.StartFlashloan(*liquidatorMA, *authority, endIndex)
	ixs = append(ixs, marginfi.EndFlashloan(*liquidatorMA, *authority, nil))
	if tipAccount != nil && tipLamports > 0 {
		ixs = append(ixs, arb.TransferIx(*authority, *tipAccount, tipLamports))
	}

	msg, err := solana.CompileV0(*authority, ixs, alts, blockhash)
	if err != nil {
		return FireTx{}, fmt.Errorf("compile jupiter fire v0: %w", err)
	}
	tx := solana.NewUnsignedVersionedTransaction(msg)
	txBytes, err := tx.MarshalBinary()
	if err != nil {
		return FireTx{}, err
	}
	return FireTx{Tx: tx, QuotedUSDCOut: plan.QuotedOut, TxBytes: len(txBytes)}, nil
}

// ColPerUnitDebtFromU64 packs a uint64 minimum-acceptable slippage floor
// into the raw little-endian u128 wire representation expected by
// BuildLiquidateData / FireCandidate.ColPerUnitDebt.
func ColPerUnitDebtFromU64(v uint64) [16]byte {
	return u128LEBytesFromU64(v)
}
