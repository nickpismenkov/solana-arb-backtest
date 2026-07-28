// Package swap builds swap instructions for Orca Whirlpool and Raydium CLMM,
// built directly (no aggregator, no network hop) so they fit the shred-
// reaction budget.
package swap

import (
	"encoding/binary"
	"math/big"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/decode"
)

// Anchor "global:swap" sighash — shared by both programs (disambiguated by
// program id, matching the decode package).
var discSwap = [8]byte{0xf8, 0xc6, 0x9e, 0x91, 0xe1, 0x75, 0x87, 0xc8}

// "global:swap_v2" sighash — shared by Orca + Raydium CLMM. Handles
// Token-2022 mints (passes both token programs + mint accounts).
var discSwapV2 = [8]byte{0x2b, 0x04, 0xed, 0x0b, 0x1a, 0xc9, 0x1e, 0x62}

const (
	tokenProgram     = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	token2022Program = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"
	memoProgram      = "MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr"
)

func pk(s string) solana.PublicKey { return solana.MustPublicKeyFromBase58(s) }

func u128LEBytes(v *big.Int) []byte {
	b := v.Bytes() // big-endian
	out := make([]byte, 16)
	for i := 0; i < len(b) && i < 16; i++ {
		out[i] = b[len(b)-1-i]
	}
	return out
}

// OrcaSwapAccounts are the accounts the caller resolves once (from pool
// state + our ATAs) and reuses.
type OrcaSwapAccounts struct {
	Whirlpool      solana.PublicKey
	TokenAuthority solana.PublicKey // our wallet (signer)
	TokenOwnerA    solana.PublicKey // our ATA for mintA
	TokenVaultA    solana.PublicKey
	TokenOwnerB    solana.PublicKey // our ATA for mintB
	TokenVaultB    solana.PublicKey
	TickArrays     [3]solana.PublicKey
	Oracle         solana.PublicKey // PDA ["oracle", whirlpool]
}

// OrcaSwapIx builds Orca `swap`: data = disc + amount + other_amount_threshold
// + sqrt_price_limit + amount_specified_is_input + a_to_b. exactIn: true →
// amount is input, threshold is min-out; false → amount is desired output,
// threshold is max-in.
func OrcaSwapIx(a *OrcaSwapAccounts, amount, threshold uint64, sqrtPriceLimit *big.Int, exactIn, aToB bool) solana.Instruction {
	data := make([]byte, 0, 42)
	data = append(data, discSwap[:]...)
	data = binary.LittleEndian.AppendUint64(data, amount)
	data = binary.LittleEndian.AppendUint64(data, threshold)
	data = append(data, u128LEBytes(sqrtPriceLimit)...)
	data = append(data, boolByte(exactIn), boolByte(aToB))

	tok := pk(tokenProgram)
	metas := solana.AccountMetaSlice{
		solana.NewAccountMeta(tok, false, false),
		solana.NewAccountMeta(a.TokenAuthority, false, true),
		solana.NewAccountMeta(a.Whirlpool, true, false),
		solana.NewAccountMeta(a.TokenOwnerA, true, false),
		solana.NewAccountMeta(a.TokenVaultA, true, false),
		solana.NewAccountMeta(a.TokenOwnerB, true, false),
		solana.NewAccountMeta(a.TokenVaultB, true, false),
		solana.NewAccountMeta(a.TickArrays[0], true, false),
		solana.NewAccountMeta(a.TickArrays[1], true, false),
		solana.NewAccountMeta(a.TickArrays[2], true, false),
		solana.NewAccountMeta(a.Oracle, true, false),
	}
	return solana.NewInstruction(pk(decode.OrcaProgram), metas, data)
}

// OrcaSwapV2Ix builds Orca `swap_v2` (Token-2022-aware). Same args as `swap`
// plus a trailing remaining_accounts_info (we send None = 0x00). Accounts add
// token_program_a/b, memo, mint_a/b at the front/after-whirlpool; oracle is
// writable. tpA/tpB are the token programs owning mintA/mintB.
func OrcaSwapV2Ix(a *OrcaSwapAccounts, mintA, mintB, tpA, tpB solana.PublicKey, amount, threshold uint64, sqrtPriceLimit *big.Int, exactIn, aToB bool) solana.Instruction {
	data := make([]byte, 0, 43)
	data = append(data, discSwapV2[:]...)
	data = binary.LittleEndian.AppendUint64(data, amount)
	data = binary.LittleEndian.AppendUint64(data, threshold)
	data = append(data, u128LEBytes(sqrtPriceLimit)...)
	data = append(data, boolByte(exactIn), boolByte(aToB), 0) // remaining_accounts_info = None

	memo := pk(memoProgram)
	metas := solana.AccountMetaSlice{
		solana.NewAccountMeta(tpA, false, false),
		solana.NewAccountMeta(tpB, false, false),
		solana.NewAccountMeta(memo, false, false),
		solana.NewAccountMeta(a.TokenAuthority, false, true),
		solana.NewAccountMeta(a.Whirlpool, true, false),
		solana.NewAccountMeta(mintA, false, false),
		solana.NewAccountMeta(mintB, false, false),
		solana.NewAccountMeta(a.TokenOwnerA, true, false),
		solana.NewAccountMeta(a.TokenVaultA, true, false),
		solana.NewAccountMeta(a.TokenOwnerB, true, false),
		solana.NewAccountMeta(a.TokenVaultB, true, false),
		solana.NewAccountMeta(a.TickArrays[0], true, false),
		solana.NewAccountMeta(a.TickArrays[1], true, false),
		solana.NewAccountMeta(a.TickArrays[2], true, false),
		solana.NewAccountMeta(a.Oracle, true, false),
	}
	return solana.NewInstruction(pk(decode.OrcaProgram), metas, data)
}

// OrcaOracle derives the Orca oracle PDA: seeds ["oracle", whirlpool].
func OrcaOracle(whirlpool solana.PublicKey) solana.PublicKey {
	addr, _, err := solana.FindProgramAddress([][]byte{[]byte("oracle"), whirlpool.Bytes()}, pk(decode.OrcaProgram))
	if err != nil {
		panic(err)
	}
	return addr
}

type RaySwapAccounts struct {
	Payer              solana.PublicKey // our wallet (signer)
	AmmConfig          solana.PublicKey
	PoolState          solana.PublicKey
	InputTokenAccount  solana.PublicKey
	OutputTokenAccount solana.PublicKey
	InputVault         solana.PublicKey
	OutputVault        solana.PublicKey
	ObservationState   solana.PublicKey
	// Current tick array first, then the next two in traversal direction —
	// Raydium walks them as remaining accounts and errors with
	// NotEnoughTickArrayAccount (6023) if the walk runs past what's provided.
	TickArrays [3]solana.PublicKey
}

// RaySwapIx builds Raydium CLMM `swap`: data = disc + amount +
// other_amount_threshold + sqrt_price_limit_x64 + is_base_input.
// isBaseInput: true → amount is input (exact-in), threshold is min-out;
// false → amount is output (exact-out), threshold is max-in.
func RaySwapIx(a *RaySwapAccounts, amount, threshold uint64, sqrtPriceLimitX64 *big.Int, isBaseInput bool) solana.Instruction {
	data := make([]byte, 0, 41)
	data = append(data, discSwap[:]...)
	data = binary.LittleEndian.AppendUint64(data, amount)
	data = binary.LittleEndian.AppendUint64(data, threshold)
	data = append(data, u128LEBytes(sqrtPriceLimitX64)...)
	data = append(data, boolByte(isBaseInput))

	tok := pk(tokenProgram)
	metas := solana.AccountMetaSlice{
		solana.NewAccountMeta(a.Payer, false, true),
		solana.NewAccountMeta(a.AmmConfig, false, false),
		solana.NewAccountMeta(a.PoolState, true, false),
		solana.NewAccountMeta(a.InputTokenAccount, true, false),
		solana.NewAccountMeta(a.OutputTokenAccount, true, false),
		solana.NewAccountMeta(a.InputVault, true, false),
		solana.NewAccountMeta(a.OutputVault, true, false),
		solana.NewAccountMeta(a.ObservationState, true, false),
		solana.NewAccountMeta(tok, false, false),
		solana.NewAccountMeta(a.TickArrays[0], true, false),
		solana.NewAccountMeta(a.TickArrays[1], true, false),
		solana.NewAccountMeta(a.TickArrays[2], true, false),
	}
	return solana.NewInstruction(pk(decode.RayClmmProgram), metas, data)
}

// RaySwapV2Ix builds Raydium CLMM `swap_v2` (Token-2022-aware). Args
// identical to `swap`. Accounts add token_program_2022, memo, and
// input/output vault MINTS; there is NO named tick_array — all tick arrays
// are remaining accounts (writable). Both token programs (classic + 2022)
// are always passed. inputMint/outputMint must match input_vault/output_vault.
func RaySwapV2Ix(a *RaySwapAccounts, inputMint, outputMint solana.PublicKey, amount, threshold uint64, sqrtPriceLimitX64 *big.Int, isBaseInput bool) solana.Instruction {
	data := make([]byte, 0, 41)
	data = append(data, discSwapV2[:]...)
	data = binary.LittleEndian.AppendUint64(data, amount)
	data = binary.LittleEndian.AppendUint64(data, threshold)
	data = append(data, u128LEBytes(sqrtPriceLimitX64)...)
	data = append(data, boolByte(isBaseInput))

	tok := pk(tokenProgram)
	tok22 := pk(token2022Program)
	memo := pk(memoProgram)
	metas := solana.AccountMetaSlice{
		solana.NewAccountMeta(a.Payer, false, true),
		solana.NewAccountMeta(a.AmmConfig, false, false),
		solana.NewAccountMeta(a.PoolState, true, false),
		solana.NewAccountMeta(a.InputTokenAccount, true, false),
		solana.NewAccountMeta(a.OutputTokenAccount, true, false),
		solana.NewAccountMeta(a.InputVault, true, false),
		solana.NewAccountMeta(a.OutputVault, true, false),
		solana.NewAccountMeta(a.ObservationState, true, false),
		solana.NewAccountMeta(tok, false, false),
		solana.NewAccountMeta(tok22, false, false),
		solana.NewAccountMeta(memo, false, false),
		solana.NewAccountMeta(inputMint, false, false),
		solana.NewAccountMeta(outputMint, false, false),
		// Tick arrays as remaining accounts (writable).
		solana.NewAccountMeta(a.TickArrays[0], true, false),
		solana.NewAccountMeta(a.TickArrays[1], true, false),
		solana.NewAccountMeta(a.TickArrays[2], true, false),
	}
	return solana.NewInstruction(pk(decode.RayClmmProgram), metas, data)
}

// Price-limit sentinels: no slippage cap at the swap level (guarded on the
// flash-repay min-out instead). Orca uses Q64.64 bounds; Raydium Q64.64 too.
var (
	MinSqrtPrice    = big.NewInt(4295048016)
	MaxSqrtPrice, _ = new(big.Int).SetString("79226673515401279992447579055", 10)
)

// SqrtLimit: for an a_to_b (price-decreasing) swap the limit is the min; else the max.
func SqrtLimit(aToB bool) *big.Int {
	if aToB {
		return MinSqrtPrice
	}
	return MaxSqrtPrice
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}
