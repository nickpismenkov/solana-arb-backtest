// Package swap builds swap instructions for Orca Whirlpool and Raydium CLMM,
// built directly (no aggregator, no network hop). Account orders follow each
// program's on-chain layout.
package swap

import (
	"encoding/binary"

	"arbengine/internal/execute"
	"arbengine/internal/solana"
)

// Anchor "global:swap" sighash — shared by both programs (disambiguated by
// program id, matching the decode package).
var discSwap = [8]byte{0xf8, 0xc6, 0x9e, 0x91, 0xe1, 0x75, 0x87, 0xc8}

// "global:swap_v2" sighash — shared by Orca + Raydium CLMM. Handles
// Token-2022 mints (passes both token programs + mint accounts).
var discSwapV2 = [8]byte{0x2b, 0x04, 0xed, 0x0b, 0x1a, 0xc9, 0x1e, 0x62}

const (
	TokenProgram     = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	Token2022Program = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"
	MemoProgram      = "MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr"
)

// OrcaSwapAccounts are accounts the caller resolves once (from pool state +
// our ATAs) and reuses.
type OrcaSwapAccounts struct {
	Whirlpool      solana.Pubkey
	TokenAuthority solana.Pubkey // our wallet (signer)
	TokenOwnerA    solana.Pubkey // our ATA for mintA
	TokenVaultA    solana.Pubkey
	TokenOwnerB    solana.Pubkey // our ATA for mintB
	TokenVaultB    solana.Pubkey
	TickArrays     [3]solana.Pubkey
	Oracle         solana.Pubkey // PDA ["oracle", whirlpool]
}

func u64le(v uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, v)
	return b
}

func u128le(v [16]byte) []byte { return v[:] }

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

// OrcaSwapIx builds Orca `swap`: data = disc + amount + other_amount_threshold
// + sqrt_price_limit + amount_specified_is_input + a_to_b. exactIn: true ->
// amount is input, threshold is min-out; false -> amount is desired output,
// threshold is max-in.
func OrcaSwapIx(a OrcaSwapAccounts, amount, threshold uint64, sqrtPriceLimit Uint128, exactIn, aToB bool) solana.Instruction {
	data := make([]byte, 0, 8+8+8+16+1+1)
	data = append(data, discSwap[:]...)
	data = append(data, u64le(amount)...)
	data = append(data, u64le(threshold)...)
	data = append(data, u128le(sqrtPriceLimit.Bytes())...)
	data = append(data, boolByte(exactIn))
	data = append(data, boolByte(aToB))

	tok := solana.MustPubkeyFromBase58(TokenProgram)
	metas := []solana.AccountMeta{
		solana.ReadonlyMeta(tok),
		solana.SignerMeta(a.TokenAuthority),
		solana.Writable(a.Whirlpool),
		solana.Writable(a.TokenOwnerA),
		solana.Writable(a.TokenVaultA),
		solana.Writable(a.TokenOwnerB),
		solana.Writable(a.TokenVaultB),
		solana.Writable(a.TickArrays[0]),
		solana.Writable(a.TickArrays[1]),
		solana.Writable(a.TickArrays[2]),
		solana.Writable(a.Oracle),
	}
	return solana.Instruction{ProgramID: solana.MustPubkeyFromBase58(execute.OrcaProgram), Accounts: metas, Data: data}
}

// OrcaSwapV2Ix builds Orca `swap_v2` (Token-2022-aware). Same args as `swap`
// plus a trailing remaining_accounts_info: Option<..> (we send None = 0x00).
// Accounts add token_program_a/b, memo, mint_a/b at the front/after-whirlpool;
// oracle is writable. tpA/tpB are the token programs owning mintA/mintB.
func OrcaSwapV2Ix(a OrcaSwapAccounts, mintA, mintB, tpA, tpB solana.Pubkey, amount, threshold uint64, sqrtPriceLimit Uint128, exactIn, aToB bool) solana.Instruction {
	data := make([]byte, 0, 8+8+8+16+1+1+1)
	data = append(data, discSwapV2[:]...)
	data = append(data, u64le(amount)...)
	data = append(data, u64le(threshold)...)
	data = append(data, u128le(sqrtPriceLimit.Bytes())...)
	data = append(data, boolByte(exactIn))
	data = append(data, boolByte(aToB))
	data = append(data, 0) // remaining_accounts_info = None

	memo := solana.MustPubkeyFromBase58(MemoProgram)
	metas := []solana.AccountMeta{
		solana.ReadonlyMeta(tpA),
		solana.ReadonlyMeta(tpB),
		solana.ReadonlyMeta(memo),
		solana.SignerMeta(a.TokenAuthority),
		solana.Writable(a.Whirlpool),
		solana.ReadonlyMeta(mintA),
		solana.ReadonlyMeta(mintB),
		solana.Writable(a.TokenOwnerA),
		solana.Writable(a.TokenVaultA),
		solana.Writable(a.TokenOwnerB),
		solana.Writable(a.TokenVaultB),
		solana.Writable(a.TickArrays[0]),
		solana.Writable(a.TickArrays[1]),
		solana.Writable(a.TickArrays[2]),
		solana.Writable(a.Oracle),
	}
	return solana.Instruction{ProgramID: solana.MustPubkeyFromBase58(execute.OrcaProgram), Accounts: metas, Data: data}
}

// OrcaOracle derives the Orca oracle PDA: seeds ["oracle", whirlpool].
func OrcaOracle(whirlpool solana.Pubkey) solana.Pubkey {
	pk, _ := solana.FindProgramAddress([][]byte{[]byte("oracle"), whirlpool.Bytes()}, solana.MustPubkeyFromBase58(execute.OrcaProgram))
	return pk
}

// RaySwapAccounts are the Raydium CLMM swap accounts.
type RaySwapAccounts struct {
	Payer              solana.Pubkey // our wallet (signer)
	AmmConfig          solana.Pubkey
	PoolState          solana.Pubkey
	InputTokenAccount  solana.Pubkey
	OutputTokenAccount solana.Pubkey
	InputVault         solana.Pubkey
	OutputVault        solana.Pubkey
	ObservationState   solana.Pubkey
	// Current tick array first, then the next two in traversal direction —
	// Raydium walks them as remaining accounts and errors with
	// NotEnoughTickArrayAccount (6023) if the walk runs past what's provided.
	TickArrays [3]solana.Pubkey
}

// RaySwapIx builds Raydium CLMM `swap`: data = disc + amount +
// other_amount_threshold + sqrt_price_limit_x64 + is_base_input.
// isBaseInput: true -> amount is input (exact-in), threshold is min-out;
// false -> amount is output (exact-out), threshold is max-in.
func RaySwapIx(a RaySwapAccounts, amount, threshold uint64, sqrtPriceLimitX64 Uint128, isBaseInput bool) solana.Instruction {
	data := make([]byte, 0, 8+8+8+16+1)
	data = append(data, discSwap[:]...)
	data = append(data, u64le(amount)...)
	data = append(data, u64le(threshold)...)
	data = append(data, u128le(sqrtPriceLimitX64.Bytes())...)
	data = append(data, boolByte(isBaseInput))

	tok := solana.MustPubkeyFromBase58(TokenProgram)
	metas := []solana.AccountMeta{
		solana.SignerMeta(a.Payer),
		solana.ReadonlyMeta(a.AmmConfig),
		solana.Writable(a.PoolState),
		solana.Writable(a.InputTokenAccount),
		solana.Writable(a.OutputTokenAccount),
		solana.Writable(a.InputVault),
		solana.Writable(a.OutputVault),
		solana.Writable(a.ObservationState),
		solana.ReadonlyMeta(tok),
		solana.Writable(a.TickArrays[0]),
		solana.Writable(a.TickArrays[1]),
		solana.Writable(a.TickArrays[2]),
	}
	return solana.Instruction{ProgramID: solana.MustPubkeyFromBase58(execute.RayClmmProgram), Accounts: metas, Data: data}
}

// RaySwapV2Ix builds Raydium CLMM `swap_v2` (Token-2022-aware). Args
// identical to `swap`. Accounts add token_program_2022, memo, and
// input/output vault MINTS; there is NO named tick_array — all tick arrays
// are remaining accounts (writable). Both token programs (classic + 2022)
// are always passed. inputMint/outputMint must match input_vault/output_vault.
func RaySwapV2Ix(a RaySwapAccounts, inputMint, outputMint solana.Pubkey, amount, threshold uint64, sqrtPriceLimitX64 Uint128, isBaseInput bool) solana.Instruction {
	data := make([]byte, 0, 8+8+8+16+1)
	data = append(data, discSwapV2[:]...)
	data = append(data, u64le(amount)...)
	data = append(data, u64le(threshold)...)
	data = append(data, u128le(sqrtPriceLimitX64.Bytes())...)
	data = append(data, boolByte(isBaseInput))

	tok := solana.MustPubkeyFromBase58(TokenProgram)
	tok22 := solana.MustPubkeyFromBase58(Token2022Program)
	memo := solana.MustPubkeyFromBase58(MemoProgram)
	metas := []solana.AccountMeta{
		solana.SignerMeta(a.Payer),
		solana.ReadonlyMeta(a.AmmConfig),
		solana.Writable(a.PoolState),
		solana.Writable(a.InputTokenAccount),
		solana.Writable(a.OutputTokenAccount),
		solana.Writable(a.InputVault),
		solana.Writable(a.OutputVault),
		solana.Writable(a.ObservationState),
		solana.ReadonlyMeta(tok),
		solana.ReadonlyMeta(tok22),
		solana.ReadonlyMeta(memo),
		solana.ReadonlyMeta(inputMint),
		solana.ReadonlyMeta(outputMint),
		// Tick arrays as remaining accounts (writable).
		solana.Writable(a.TickArrays[0]),
		solana.Writable(a.TickArrays[1]),
		solana.Writable(a.TickArrays[2]),
	}
	return solana.Instruction{ProgramID: solana.MustPubkeyFromBase58(execute.RayClmmProgram), Accounts: metas, Data: data}
}

// Price-limit sentinels: no slippage cap at the swap level (we guard on the
// flash-repay min-out instead). Orca uses Q64.64 bounds; Raydium Q64.64 too.
var (
	MinSqrtPrice = Uint128FromString("4295048016")
	MaxSqrtPrice = Uint128FromString("79226673515401279992447579055")
)

// SqrtLimit returns the price-limit sentinel for a swap direction: for an
// a_to_b (price-decreasing) swap the limit is the min; else the max.
func SqrtLimit(aToB bool) Uint128 {
	if aToB {
		return MinSqrtPrice
	}
	return MaxSqrtPrice
}
