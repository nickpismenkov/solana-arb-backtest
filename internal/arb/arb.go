// Package arb assembles the guarded flash-loan arb transaction: resolves
// both pools' accounts, assembles [CU, create ATAs, flash-borrow USDC, leg1
// USDC→base exact-in, leg2 base→USDC EXACT-OUT=repay, flash-payback] in
// either direction, and compiles a v0 tx against the ALT so it fits 1232
// bytes. Leg 2 exact-out for exactly the repay amount is the profit-or-revert
// guard.
package arb

import (
	"encoding/binary"
	"fmt"
	"math/big"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/flashloan"
	"solana-arb-backtest-go/internal/pools"
	"solana-arb-backtest-go/internal/swap"
	"solana-arb-backtest-go/internal/ticks"
)

const computeBudget = "ComputeBudget111111111111111111111111111111"
const sysProgram = "11111111111111111111111111111111"

func pk(s string) solana.PublicKey { return solana.MustPublicKeyFromBase58(s) }

// PkAt reads a pubkey out of raw account bytes at offset o.
func PkAt(d []byte, o int) solana.PublicKey {
	return solana.PublicKeyFromBytes(d[o : o+32])
}

func CuLimitIx(units uint32) solana.Instruction {
	data := []byte{0x02}
	data = binary.LittleEndian.AppendUint32(data, units)
	return solana.NewInstruction(pk(computeBudget), solana.AccountMetaSlice{}, data)
}

func CuPriceIx(microLamports uint64) solana.Instruction {
	data := []byte{0x03}
	data = binary.LittleEndian.AppendUint64(data, microLamports)
	return solana.NewInstruction(pk(computeBudget), solana.AccountMetaSlice{}, data)
}

// TransferIx is a system-program transfer (Jito tip). Inside the arb tx so a
// revert pays no tip.
func TransferIx(from, to solana.PublicKey, lamports uint64) solana.Instruction {
	data := []byte{2, 0, 0, 0} // SystemInstruction::Transfer
	data = binary.LittleEndian.AppendUint64(data, lamports)
	metas := solana.AccountMetaSlice{
		solana.NewAccountMeta(from, true, true),
		solana.NewAccountMeta(to, true, false),
	}
	return solana.NewInstruction(pk(sysProgram), metas, data)
}

// PoolData holds both pools' raw account data, fetched once per build.
type PoolData struct {
	Orca []byte
	Ray  []byte
}

// TpOf is the token program owning mint: baseTp if it's the base mint, else quoteTp.
func TpOf(mint, base, baseTp, quoteTp solana.PublicKey) solana.PublicKey {
	if mint.Equals(base) {
		return baseTp
	}
	return quoteTp
}

// OrcaAccounts resolves the Orca swap accounts for our wallet at the current
// tick. aToB selects the tick-array traversal direction. ATAs are derived
// under each mint's owning token program (classic or Token-2022).
func OrcaAccounts(od []byte, orcaPk, signer solana.PublicKey, aToB bool, base, baseTp, quoteTp solana.PublicKey) (*swap.OrcaSwapAccounts, error) {
	ost, ok := ticks.DecodeOrcaState(od)
	if !ok {
		return nil, fmt.Errorf("orca state")
	}
	mintA := PkAt(od, 101)
	mintB := PkAt(od, 181)
	n := 88 * int32(ost.TickSpacing)
	start := ticks.OrcaStartIndex(ost.Tick, ost.TickSpacing)
	var starts [3]int32
	if aToB {
		starts = [3]int32{start, start - n, start - 2*n}
	} else {
		starts = [3]int32{start, start + n, start + 2*n}
	}
	return &swap.OrcaSwapAccounts{
		Whirlpool:      orcaPk,
		TokenAuthority: signer,
		TokenOwnerA:    flashloan.AtaFor(signer, mintA, TpOf(mintA, base, baseTp, quoteTp)),
		TokenVaultA:    PkAt(od, 133),
		TokenOwnerB:    flashloan.AtaFor(signer, mintB, TpOf(mintB, base, baseTp, quoteTp)),
		TokenVaultB:    PkAt(od, 213),
		TickArrays: [3]solana.PublicKey{
			ticks.OrcaTickArray(orcaPk, starts[0]),
			ticks.OrcaTickArray(orcaPk, starts[1]),
			ticks.OrcaTickArray(orcaPk, starts[2]),
		},
		Oracle: swap.OrcaOracle(orcaPk),
	}, nil
}

func rayAccounts(rd []byte, rayPk, signer solana.PublicKey, base, usdc solana.PublicKey, baseIn bool, baseTp, quoteTp solana.PublicKey) (*swap.RaySwapAccounts, error) {
	rst, ok := ticks.DecodeRayState(rd)
	if !ok {
		return nil, fmt.Errorf("ray state")
	}
	mint0 := PkAt(rd, 73)
	baseIs0 := mint0.Equals(base)
	var baseVault, quoteVault solana.PublicKey
	if baseIs0 {
		baseVault, quoteVault = PkAt(rd, 137), PkAt(rd, 169)
	} else {
		baseVault, quoteVault = PkAt(rd, 169), PkAt(rd, 137)
	}
	// base_in = leg spends base (sell); else spends USDC (buy). ATAs under the
	// owning token program per mint.
	baseAta := flashloan.AtaFor(signer, base, baseTp)
	usdcAta := flashloan.AtaFor(signer, usdc, quoteTp)
	var inputVault, outputVault, inputAta, outputAta solana.PublicKey
	if baseIn {
		inputVault, outputVault, inputAta, outputAta = baseVault, quoteVault, baseAta, usdcAta
	} else {
		inputVault, outputVault, inputAta, outputAta = quoteVault, baseVault, usdcAta, baseAta
	}
	// Tick-array traversal: input mint == token0 → price/tick decreases
	// (zero-for-one) → arrays descend from the current one; else ascend.
	zeroForOne := baseIs0
	if !baseIn {
		zeroForOne = !baseIs0
	}
	n := 60 * int32(rst.TickSpacing)
	rstart := ticks.RayStartIndex(rst.Tick, rst.TickSpacing)
	var starts [3]int32
	if zeroForOne {
		starts = [3]int32{rstart, rstart - n, rstart - 2*n}
	} else {
		starts = [3]int32{rstart, rstart + n, rstart + 2*n}
	}
	return &swap.RaySwapAccounts{
		Payer:              signer,
		AmmConfig:          PkAt(rd, 9),
		PoolState:          rayPk,
		InputTokenAccount:  inputAta,
		OutputTokenAccount: outputAta,
		InputVault:         inputVault,
		OutputVault:        outputVault,
		ObservationState:   PkAt(rd, 201),
		TickArrays: [3]solana.PublicKey{
			ticks.RayTickArray(rayPk, starts[0]),
			ticks.RayTickArray(rayPk, starts[1]),
			ticks.RayTickArray(rayPk, starts[2]),
		},
	}, nil
}

// ALT mirrors an AddressLookupTableAccount: a table pubkey + its resolved addresses.
type ALT struct {
	Key       solana.PublicKey
	Addresses []solana.PublicKey
}

// LoadAlt loads an ALT account into the form v0 message compilation needs.
func LoadAlt(altAddr string, altAccountData []byte) *ALT {
	var addrs []solana.PublicKey
	for o := 56; o+32 <= len(altAccountData); o += 32 {
		addrs = append(addrs, solana.PublicKeyFromBytes(altAccountData[o:o+32]))
	}
	return &ALT{Key: pk(altAddr), Addresses: addrs}
}

// BuildArbTx builds the unsigned guarded arb v0 tx. orcaFirst = true buys
// base on Orca (leg1) and sells on Raydium (leg2); false is the reverse.
// blockhash should be a real recent hash for live submission, or a zero hash
// for replace-blockhash simulation. Returns the unsigned tx (sign before sending).
func BuildArbTx(
	poolData *PoolData,
	signer solana.PublicKey,
	alt *ALT,
	borrowAmount uint64,
	orcaFirst bool,
	tipAccount *solana.PublicKey,
	tipLamports uint64,
	priorityMicroLamports uint64,
	blockhash solana.Hash,
	repayBuffer uint64,
) (*solana.Transaction, error) {
	cfg := pools.Pair()
	usdc := pk(flashloan.USDCMint)
	base := pk(cfg.BaseMint)
	orcaPk := pk(cfg.OrcaPool)
	rayPk := pk(cfg.RayPool)
	baseTp := pk(cfg.BaseTokenProgram)
	quoteTp := pk(cfg.QuoteTokenProgram)
	v2 := cfg.NeedsSwapV2() // Token-2022 leg → swapV2
	mintA := PkAt(poolData.Orca, 101)
	mintB := PkAt(poolData.Orca, 181)
	baseIsAOrca := mintA.Equals(base)
	tpA := TpOf(mintA, base, baseTp, quoteTp)
	tpB := TpOf(mintB, base, baseTp, quoteTp)

	// Orca leg helper: swapV2 when the pair has a Token-2022 side, else classic.
	orcaLeg := func(aToB bool, amount, threshold uint64, exactIn bool) (solana.Instruction, error) {
		oa, err := OrcaAccounts(poolData.Orca, orcaPk, signer, aToB, base, baseTp, quoteTp)
		if err != nil {
			return nil, err
		}
		if v2 {
			return swap.OrcaSwapV2Ix(oa, mintA, mintB, tpA, tpB, amount, threshold, swap.SqrtLimit(aToB), exactIn, aToB), nil
		}
		return swap.OrcaSwapIx(oa, amount, threshold, swap.SqrtLimit(aToB), exactIn, aToB), nil
	}
	// Ray leg helper: base_in = the leg spends base (sell). input/output mints
	// follow the direction so swapV2 gets the right vault mints.
	rayLeg := func(baseIn bool, amount, threshold uint64, isBaseInput bool) (solana.Instruction, error) {
		ra, err := rayAccounts(poolData.Ray, rayPk, signer, base, usdc, baseIn, baseTp, quoteTp)
		if err != nil {
			return nil, err
		}
		inMint, outMint := usdc, base
		if baseIn {
			inMint, outMint = base, usdc
		}
		if v2 {
			return swap.RaySwapV2Ix(ra, inMint, outMint, amount, threshold, big.NewInt(0), isBaseInput), nil
		}
		return swap.RaySwapIx(ra, amount, threshold, big.NewInt(0), isBaseInput), nil
	}

	// leg1 = buy base with USDC (exact-in borrow_amount); leg2 = sell base for
	// USDC (exact-out = borrow + repay_buffer) — the guard.
	leg2Out := borrowAmount + repayBuffer
	var leg1, leg2 solana.Instruction
	var err error
	if orcaFirst {
		leg1, err = orcaLeg(!baseIsAOrca, borrowAmount, 0, true)
		if err != nil {
			return nil, err
		}
		leg2, err = rayLeg(true, leg2Out, ^uint64(0), false)
		if err != nil {
			return nil, err
		}
	} else {
		leg1, err = rayLeg(false, borrowAmount, 0, true)
		if err != nil {
			return nil, err
		}
		leg2, err = orcaLeg(baseIsAOrca, leg2Out, ^uint64(0), false)
		if err != nil {
			return nil, err
		}
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
	// Tip transfer to a Jito tip account, inside the tx → only pays if it lands.
	if tipAccount != nil && tipLamports > 0 {
		ixs = append(ixs, TransferIx(signer, *tipAccount, tipLamports))
	}

	opts := []solana.TransactionOption{solana.TransactionPayer(signer)}
	if alt != nil {
		opts = append(opts, solana.TransactionAddressTables(map[solana.PublicKey]solana.PublicKeySlice{
			alt.Key: alt.Addresses,
		}))
	}
	tx, err := solana.NewTransaction(ixs, blockhash, opts...)
	if err != nil {
		return nil, fmt.Errorf("compile v0: %w", err)
	}
	return tx, nil
}

// CompileV0 compiles a v0 transaction from instructions + zero or more ALTs
// — the shared "fire tx" assembly step used by the kamino/save/jupiterlend
// flashloan-wrapped liquidation builders (mirrors solana_message::v0::Message
// ::try_compile in the original Rust).
func CompileV0(payer solana.PublicKey, ixs []solana.Instruction, alts []*ALT, blockhash solana.Hash) (*solana.Transaction, error) {
	opts := []solana.TransactionOption{solana.TransactionPayer(payer)}
	if len(alts) > 0 {
		tables := make(map[solana.PublicKey]solana.PublicKeySlice, len(alts))
		for _, a := range alts {
			tables[a.Key] = a.Addresses
		}
		opts = append(opts, solana.TransactionAddressTables(tables))
	}
	tx, err := solana.NewTransaction(ixs, blockhash, opts...)
	if err != nil {
		return nil, fmt.Errorf("compile v0: %w", err)
	}
	return tx, nil
}
