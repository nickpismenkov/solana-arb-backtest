// Package arb builds the guarded flash-loan arb transaction: resolves both
// pools' accounts, assembles [CU, create ATAs, flash-borrow USDC, leg1
// USDC->base exact-in, leg2 base->USDC EXACT-OUT=repay, flash-payback] in
// either direction, and compiles a v0 tx against the ALT so it fits 1232
// bytes. Leg 2 exact-out for exactly the repay amount is the
// profit-or-revert guard.
package arb

import (
	"encoding/binary"
	"fmt"

	"arbengine/internal/execute"
	"arbengine/internal/flashloan"
	"arbengine/internal/pools"
	"arbengine/internal/solana"
	"arbengine/internal/swap"
)

const computeBudgetProgram = "ComputeBudget111111111111111111111111111111"
const sysProgram = "11111111111111111111111111111111"

func pk(s string) solana.Pubkey { return solana.MustPubkeyFromBase58(s) }

// PkAt reads a 32-byte pubkey out of raw account data at offset o.
func PkAt(d []byte, o int) solana.Pubkey {
	pubkey, _ := solana.PubkeyFromBytes(d[o : o+32])
	return pubkey
}

func CuLimitIx(units uint32) solana.Instruction {
	data := make([]byte, 5)
	data[0] = 0x02
	binary.LittleEndian.PutUint32(data[1:], units)
	return solana.Instruction{ProgramID: pk(computeBudgetProgram), Data: data}
}

func CuPriceIx(microLamports uint64) solana.Instruction {
	data := make([]byte, 9)
	data[0] = 0x03
	binary.LittleEndian.PutUint64(data[1:], microLamports)
	return solana.Instruction{ProgramID: pk(computeBudgetProgram), Data: data}
}

// TransferIx is a system-program transfer (Jito tip). Inside the arb tx so a
// revert pays no tip.
func TransferIx(from, to solana.Pubkey, lamports uint64) solana.Instruction {
	data := make([]byte, 12)
	binary.LittleEndian.PutUint32(data[0:4], 2) // SystemInstruction::Transfer
	binary.LittleEndian.PutUint64(data[4:], lamports)
	return solana.Instruction{
		ProgramID: pk(sysProgram),
		Accounts: []solana.AccountMeta{
			solana.WritableSigner(from),
			solana.Writable(to),
		},
		Data: data,
	}
}

// PoolData holds both pools' raw account data, fetched once per build.
type PoolData struct {
	Orca []byte
	Ray  []byte
}

// TpOf is the token program owning `mint`: baseTp if it's the base mint,
// else quoteTp.
func TpOf(mint, base, baseTp, quoteTp solana.Pubkey) solana.Pubkey {
	if mint == base {
		return baseTp
	}
	return quoteTp
}

// OrcaAccounts resolves the Orca swap accounts for our wallet at the current
// tick. aToB selects the tick-array traversal direction. ATAs are derived
// under each mint's owning token program (classic or Token-2022).
func OrcaAccounts(od []byte, orcaPk, signer solana.Pubkey, aToB bool, base, baseTp, quoteTp solana.Pubkey) swap.OrcaSwapAccounts {
	ost, ok := execute.DecodeOrcaState(od)
	if !ok {
		panic("arb: orca state")
	}
	mintA := PkAt(od, 101)
	mintB := PkAt(od, 181)
	n := 88 * int32(ost.TickSpacing)
	start := execute.OrcaStartIndex(ost.Tick, ost.TickSpacing)
	var starts [3]int32
	if aToB {
		starts = [3]int32{start, start - n, start - 2*n}
	} else {
		starts = [3]int32{start, start + n, start + 2*n}
	}
	return swap.OrcaSwapAccounts{
		Whirlpool:      orcaPk,
		TokenAuthority: signer,
		TokenOwnerA:    flashloan.AtaFor(signer, mintA, TpOf(mintA, base, baseTp, quoteTp)),
		TokenVaultA:    PkAt(od, 133),
		TokenOwnerB:    flashloan.AtaFor(signer, mintB, TpOf(mintB, base, baseTp, quoteTp)),
		TokenVaultB:    PkAt(od, 213),
		TickArrays: [3]solana.Pubkey{
			execute.OrcaTickArray(orcaPk, starts[0]),
			execute.OrcaTickArray(orcaPk, starts[1]),
			execute.OrcaTickArray(orcaPk, starts[2]),
		},
		Oracle: swap.OrcaOracle(orcaPk),
	}
}

func rayAccounts(rd []byte, rayPk, signer, base, usdc solana.Pubkey, baseIn bool, baseTp, quoteTp solana.Pubkey) swap.RaySwapAccounts {
	rst, ok := execute.DecodeRayState(rd)
	if !ok {
		panic("arb: ray state")
	}
	mint0 := PkAt(rd, 73)
	baseIs0 := mint0 == base
	var baseVault, quoteVault solana.Pubkey
	if baseIs0 {
		baseVault, quoteVault = PkAt(rd, 137), PkAt(rd, 169)
	} else {
		baseVault, quoteVault = PkAt(rd, 169), PkAt(rd, 137)
	}
	// baseIn = leg spends base (sell); else spends USDC (buy). ATAs under the
	// owning token program per mint.
	baseAta := flashloan.AtaFor(signer, base, baseTp)
	usdcAta := flashloan.AtaFor(signer, usdc, quoteTp)
	var inputVault, outputVault, inputAta, outputAta solana.Pubkey
	if baseIn {
		inputVault, outputVault, inputAta, outputAta = baseVault, quoteVault, baseAta, usdcAta
	} else {
		inputVault, outputVault, inputAta, outputAta = quoteVault, baseVault, usdcAta, baseAta
	}
	// Tick-array traversal: input mint == token0 -> price/tick decreases
	// (zero-for-one) -> arrays descend from the current one; else ascend.
	zeroForOne := baseIs0
	if !baseIn {
		zeroForOne = !baseIs0
	}
	n := 60 * int32(rst.TickSpacing)
	rstart := execute.RayStartIndex(rst.Tick, rst.TickSpacing)
	var starts [3]int32
	if zeroForOne {
		starts = [3]int32{rstart, rstart - n, rstart - 2*n}
	} else {
		starts = [3]int32{rstart, rstart + n, rstart + 2*n}
	}
	return swap.RaySwapAccounts{
		Payer:              signer,
		AmmConfig:          PkAt(rd, 9),
		PoolState:          rayPk,
		InputTokenAccount:  inputAta,
		OutputTokenAccount: outputAta,
		InputVault:         inputVault,
		OutputVault:        outputVault,
		ObservationState:   PkAt(rd, 201),
		TickArrays: [3]solana.Pubkey{
			execute.RayTickArray(rayPk, starts[0]),
			execute.RayTickArray(rayPk, starts[1]),
			execute.RayTickArray(rayPk, starts[2]),
		},
	}
}

// BuildArbTx builds the unsigned guarded arb v0 tx. orcaFirst=true buys base
// on Orca (leg1) and sells on Raydium (leg2); false is the reverse.
// blockhash should be a real recent hash for live submission, or the zero
// hash for replace-blockhash simulation. Returns the unsigned tx (sign
// before sending).
func BuildArbTx(
	poolData PoolData,
	signer solana.Pubkey,
	alt solana.AddressLookupTableAccount,
	borrowAmount uint64,
	orcaFirst bool,
	tipAccount *solana.Pubkey,
	tipLamports uint64,
	priorityMicroLamports uint64,
	blockhash solana.Hash,
	repayBuffer uint64,
) (solana.VersionedTransaction, error) {
	cfg := pools.Pair()
	usdc := pk(flashloan.USDCMint)
	base := pk(cfg.BaseMint)
	orcaPk := pk(cfg.OrcaPool)
	rayPk := pk(cfg.RayPool)
	baseTp := pk(cfg.BaseTokenProgram)
	quoteTp := pk(cfg.QuoteTokenProgram)
	v2 := cfg.NeedsSwapV2() // Token-2022 leg -> swapV2
	mintA := PkAt(poolData.Orca, 101)
	mintB := PkAt(poolData.Orca, 181)
	baseIsAOrca := mintA == base
	tpA := TpOf(mintA, base, baseTp, quoteTp)
	tpB := TpOf(mintB, base, baseTp, quoteTp)

	// Orca leg helper: swapV2 when the pair has a Token-2022 side, else classic.
	orcaLeg := func(aToB bool, amount, threshold uint64, exactIn bool) solana.Instruction {
		oa := OrcaAccounts(poolData.Orca, orcaPk, signer, aToB, base, baseTp, quoteTp)
		if v2 {
			return swap.OrcaSwapV2Ix(oa, mintA, mintB, tpA, tpB, amount, threshold, swap.SqrtLimit(aToB), exactIn, aToB)
		}
		return swap.OrcaSwapIx(oa, amount, threshold, swap.SqrtLimit(aToB), exactIn, aToB)
	}
	// Ray leg helper: baseIn = the leg spends base (sell). input/output mints
	// follow the direction so swapV2 gets the right vault mints.
	rayLeg := func(baseIn bool, amount, threshold uint64, isBaseInput bool) solana.Instruction {
		ra := rayAccounts(poolData.Ray, rayPk, signer, base, usdc, baseIn, baseTp, quoteTp)
		inMint, outMint := usdc, base
		if baseIn {
			inMint, outMint = base, usdc
		}
		if v2 {
			return swap.RaySwapV2Ix(ra, inMint, outMint, amount, threshold, swap.Uint128FromU64(0), isBaseInput)
		}
		return swap.RaySwapIx(ra, amount, threshold, swap.Uint128FromU64(0), isBaseInput)
	}

	// leg1 = buy base with USDC (exact-in borrow_amount); leg2 = sell base for
	// USDC (exact-out = borrow + repay_buffer) — the guard. The buffer forces
	// the tx to produce enough surplus USDC to cover the tip + fees, so a
	// landed trade is always net-positive; if the gap is too small, leg2
	// can't produce borrow+buffer and reverts -> bundle fails for free.
	leg2Out := borrowAmount + repayBuffer
	var leg1, leg2 solana.Instruction
	if orcaFirst {
		// Orca buy (input USDC -> a_to_b = !base_is_a); Ray sell base exact-out.
		leg1 = orcaLeg(!baseIsAOrca, borrowAmount, 0, true)
		leg2 = rayLeg(true, leg2Out, ^uint64(0), false)
	} else {
		// Ray buy base exact-in; Orca sell base exact-out (a_to_b = base_is_a).
		leg1 = rayLeg(false, borrowAmount, 0, true)
		leg2 = orcaLeg(baseIsAOrca, leg2Out, ^uint64(0), false)
	}

	ixs := []solana.Instruction{
		CuLimitIx(600_000),
		CuPriceIx(priorityMicroLamports),
		flashloan.CreateAtaIdempotentFor(signer, usdc, quoteTp),
		flashloan.CreateAtaIdempotentFor(signer, base, baseTp),
		flashloan.BorrowUSDC(signer, borrowAmount),
		leg1,
		leg2,
		flashloan.PaybackUSDC(signer, borrowAmount),
	}
	// Tip transfer to a Jito tip account, inside the tx -> only pays if it lands.
	if tipAccount != nil && tipLamports > 0 {
		ixs = append(ixs, TransferIx(signer, *tipAccount, tipLamports))
	}

	msg, err := solana.CompileV0(signer, ixs, []solana.AddressLookupTableAccount{alt}, blockhash)
	if err != nil {
		return solana.VersionedTransaction{}, fmt.Errorf("compile v0: %w", err)
	}
	return solana.NewUnsignedVersionedTransaction(msg), nil
}

// LoadALT loads an ALT account into the form v0 message compilation needs.
func LoadALT(altAddr string, altAccountData []byte) solana.AddressLookupTableAccount {
	var addresses []solana.Pubkey
	for o := 56; o+32 <= len(altAccountData); o += 32 {
		addresses = append(addresses, PkAt(altAccountData, o))
	}
	return solana.AddressLookupTableAccount{Key: pk(altAddr), Addresses: addresses}
}
