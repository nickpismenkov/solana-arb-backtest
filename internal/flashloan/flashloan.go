// Package flashloan implements Jupiter Lend (Fluid) flash-loan instructions —
// 0 bp. Ported from the exact output of the @jup-ag/lend SDK's
// getFlashloanIx: for the USDC "main" market only accounts 0 (signer) and 2
// (signer's USDC ATA) vary; the other twelve are market constants. The
// program matches borrow↔payback via the instructions sysvar, so we just
// place borrow before payback in the tx — no index math.
package flashloan

import (
	"encoding/binary"

	"github.com/gagliardetto/solana-go"
)

const (
	JupLendProgram     = "jupgfSgfuAXv4B6R2Uxu85Z1qdzgju79s6MfZekN6XS"
	USDCMint           = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	USDTMint           = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"
	WSOLMint           = "So11111111111111111111111111111111111111112"
	TokenProgram       = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	Token2022Program   = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"
	ataProgram         = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"
	sysProgram         = "11111111111111111111111111111111"
	instructionsSysvar = "Sysvar1nstructions1111111111111111111111111"
)

// Anchor discriminators (first 8 bytes of each ix's data).
var discBorrow = [8]byte{0x67, 0x13, 0x4e, 0x18, 0xf0, 0x09, 0x87, 0x3f}
var discPayback = [8]byte{0xd5, 0x2f, 0x99, 0x89, 0x54, 0xf3, 0x5e, 0xe8}

// Accounts shared by EVERY market (indices 1,8,9 in the ix). The lending
// admin (idx 1) has no per-mint field; idx 8 = PDA(["liquidity"],
// vault-program); idx 9 = the vault program id itself.
const (
	mLending      = "ALXWtv2P4GqH1B7Lq731joag52yRBRqmHV4naiXPTYWL" // idx 1  (W)  global
	mLiquidity    = "7s1da8DduuBFqGra5bJBjpnvL5E9mGzCuMk1Qkh4or2Z" // idx 8  (r)  global
	mVaultProgram = "jupeiUmn818Jg1ekPURTpr4mFo29p46vygyykFJ3wZC"  // idx 9  (r)  global
)

// flashMarket holds the four per-asset flash-market accounts (indices
// 4,5,6,7). Verified against the live USDC set.
type flashMarket struct {
	reserve   string // idx 4 (W)
	token     string // idx 5 (W)
	rateModel string // idx 6 (r)
	vault     string // idx 7 (W)
}

// marketFor looks up the flash-loan market for a debt mint. Only the three
// assets the liquidation fire path repays are wired; anything else returns
// false (caller must reject — a wrong account set would just revert).
func marketFor(mint solana.PublicKey) (flashMarket, string, bool) {
	m := mint.String()
	switch m {
	case USDCMint:
		return flashMarket{
			reserve:   "94vK29npVbyRHXH63rRcTiSr26SFhrQTzbpNJuhQEDu",
			token:     "J9dyC4pBTBPvzzPh7J9rhFhg8RvgerDNKkUH9kEwGMsj",
			rateModel: "5pjzT5dFTsXcwixoab1QDLvZQvpYJxJeBphkyfHGn688",
			vault:     "BmkUoKMFYBxNSzWXyUjyMJjMAaVz4d8ZnxwwmhDCUXFB",
		}, USDCMint, true
	case USDTMint:
		return flashMarket{
			reserve:   "Enao27EWUV2fv3rUqwknJ1eRaM5aAeN5ijeCrM9tayRX",
			token:     "FmFLvD6X1zHh6gXGCgiB3zRiV1zoEKPgHqfUpXL9EvKu",
			rateModel: "6sAbVeSvEfjQGRAGg9W4PAfhB5qNhYiGdx6Fh9uVEsEC",
			vault:     "4HTRHjdgy4VSVRcsumuzVFCgWywNhjGsD5oG3kqAt5vo",
		}, USDTMint, true
	case WSOLMint:
		return flashMarket{
			reserve:   "4Y66HtUEqbbbpZdENGtFdVhUMS3tnagffn3M4do59Nfy",
			token:     "BZZKgXxhxVkzx3NN8RfBPwU7ZmnQbDtp3ezcsXbiALL6",
			rateModel: "Acvyi9HBGmqh3Exe1N4PjBVyY8fokq2AdC6fSLqV6KSo",
			vault:     "5JP5zgYCb9W37QQLgAHRHuinFLrKt87akDY1CgZoTPzr",
		}, WSOLMint, true
	default:
		return flashMarket{}, "", false
	}
}

func pk(s string) solana.PublicKey { return solana.MustPublicKeyFromBase58(s) }

// AtaFor is the ATA for a mint under a specific token program (classic or
// Token-2022). The token program id is part of the ATA derivation seeds, so
// a Token-2022 mint's ATA differs from a classic one.
func AtaFor(owner, mint, tokenProgram solana.PublicKey) solana.PublicKey {
	addr, _, err := solana.FindProgramAddress(
		[][]byte{owner.Bytes(), tokenProgram.Bytes(), mint.Bytes()},
		pk(ataProgram),
	)
	if err != nil {
		panic(err)
	}
	return addr
}

// Ata is the signer's ATA for a mint (classic SPL Token program — the common case).
func Ata(owner, mint solana.PublicKey) solana.PublicKey {
	return AtaFor(owner, mint, pk(TokenProgram))
}

// CreateAtaIdempotentFor creates-idempotent the signer's ATA under a given token program.
func CreateAtaIdempotentFor(signer, mint, tokenProgram solana.PublicKey) solana.Instruction {
	ata := AtaFor(signer, mint, tokenProgram)
	metas := solana.AccountMetaSlice{
		solana.NewAccountMeta(signer, true, true),
		solana.NewAccountMeta(ata, true, false),
		solana.NewAccountMeta(signer, false, false),
		solana.NewAccountMeta(mint, false, false),
		solana.NewAccountMeta(pk(sysProgram), false, false),
		solana.NewAccountMeta(tokenProgram, false, false),
	}
	return solana.NewInstruction(pk(ataProgram), metas, []byte{1}) // createIdempotent
}

// CreateAtaIdempotent creates-idempotent the signer's ATA (classic SPL Token program).
func CreateAtaIdempotent(signer, mint solana.PublicKey) solana.Instruction {
	return CreateAtaIdempotentFor(signer, mint, pk(TokenProgram))
}

func marketMetas(signer, mint solana.PublicKey) (solana.AccountMetaSlice, bool) {
	m, mintStr, ok := marketFor(mint)
	if !ok {
		return nil, false
	}
	ata := Ata(signer, mint)
	return solana.AccountMetaSlice{
		solana.NewAccountMeta(signer, true, true),                   // 0 signer
		solana.NewAccountMeta(pk(mLending), true, false),            // 1 lending admin (global)
		solana.NewAccountMeta(ata, true, false),                     // 2 signer's debt ATA
		solana.NewAccountMeta(pk(mintStr), false, false),            // 3 mint
		solana.NewAccountMeta(pk(m.reserve), true, false),           // 4
		solana.NewAccountMeta(pk(m.token), true, false),             // 5
		solana.NewAccountMeta(pk(m.rateModel), false, false),        // 6
		solana.NewAccountMeta(pk(m.vault), true, false),             // 7
		solana.NewAccountMeta(pk(mLiquidity), false, false),         // 8 (global)
		solana.NewAccountMeta(pk(mVaultProgram), false, false),      // 9 (global)
		solana.NewAccountMeta(pk(TokenProgram), false, false),       // 10
		solana.NewAccountMeta(pk(ataProgram), false, false),         // 11
		solana.NewAccountMeta(pk(sysProgram), false, false),         // 12
		solana.NewAccountMeta(pk(instructionsSysvar), false, false), // 13
	}, true
}

func buildIx(signer, mint solana.PublicKey, disc [8]byte, amount uint64) (solana.Instruction, bool) {
	metas, ok := marketMetas(signer, mint)
	if !ok {
		return nil, false
	}
	data := make([]byte, 0, 16)
	data = append(data, disc[:]...)
	data = binary.LittleEndian.AppendUint64(data, amount)
	return solana.NewInstruction(pk(JupLendProgram), metas, data), true
}

// HasMarket is true if the JupLend flash-loan market for mint is wired (USDC/USDT/wSOL).
func HasMarket(mint solana.PublicKey) bool {
	_, _, ok := marketFor(mint)
	return ok
}

// Borrow flash-borrows amount base units of mint (USDC/USDT/wSOL) to signer.
// ok=false if the mint has no wired flash market.
func Borrow(signer, mint solana.PublicKey, amount uint64) (solana.Instruction, bool) {
	return buildIx(signer, mint, discBorrow, amount)
}

// Payback repays amount base units of mint (must equal the borrow — 0 bp fee).
func Payback(signer, mint solana.PublicKey, amount uint64) (solana.Instruction, bool) {
	return buildIx(signer, mint, discPayback, amount)
}

// BorrowUSDC flash-borrows amount base units of USDC to signer (back-compat wrapper).
func BorrowUSDC(signer solana.PublicKey, amount uint64) solana.Instruction {
	ix, ok := Borrow(signer, pk(USDCMint), amount)
	if !ok {
		panic("USDC market is always wired")
	}
	return ix
}

// PaybackUSDC repays amount base units of USDC (must equal the borrow — 0 bp fee).
func PaybackUSDC(signer solana.PublicKey, amount uint64) solana.Instruction {
	ix, ok := Payback(signer, pk(USDCMint), amount)
	if !ok {
		panic("USDC market is always wired")
	}
	return ix
}
