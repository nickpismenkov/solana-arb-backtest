// Package flashloan builds Jupiter Lend (Fluid) flash-loan instructions —
// 0 bp. Ported from the exact output of the @jup-ag/lend SDK's
// getFlashloanIx: for the USDC "main" market only the signer and the
// signer's USDC ATA vary; the other twelve accounts are market constants.
// The program matches borrow<->payback via the instructions sysvar, so we
// just place borrow before payback in the tx — no index math.
package flashloan

import (
	"encoding/binary"

	"arbengine/internal/solana"
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
// 4,5,6,7). idx4 reserve = PDA(["reserve", mint], vault-program); idx6
// rate_model = PDA(["rate_model", mint], vault-program); idx7 vault =
// ATA(mLiquidity, mint, classic token program); idx5 "token" is the 120B
// account linking mLending<->mint (looked up live; not a clean [seed,mint]
// PDA, so pinned as a constant).
type flashMarket struct {
	reserve   string // idx 4 (W)
	token     string // idx 5 (W)
	rateModel string // idx 6 (r)
	vault     string // idx 7 (W)
}

// marketFor looks up the flash-loan market for a debt mint. Only the three
// assets the liquidation fire path repays are wired; anything else returns
// false (caller must reject — a wrong account set would just revert).
func marketFor(mint solana.Pubkey) (flashMarket, string, bool) {
	switch mint.String() {
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

func pk(s string) solana.Pubkey { return solana.MustPubkeyFromBase58(s) }

// AtaFor derives the ATA for a mint under a specific token program (classic
// or Token-2022). The token program id is part of the ATA derivation seeds,
// so a Token-2022 mint's ATA differs from a classic one.
func AtaFor(owner, mint, tokenProgram solana.Pubkey) solana.Pubkey {
	addr, _ := solana.FindProgramAddress([][]byte{owner.Bytes(), tokenProgram.Bytes(), mint.Bytes()}, pk(ataProgram))
	return addr
}

// Ata is the signer's ATA for a mint (classic SPL Token program — the common case).
func Ata(owner, mint solana.Pubkey) solana.Pubkey {
	return AtaFor(owner, mint, pk(TokenProgram))
}

// CreateAtaIdempotentFor create-idempotents the signer's ATA under a given token program.
func CreateAtaIdempotentFor(signer, mint, tokenProgram solana.Pubkey) solana.Instruction {
	ata := AtaFor(signer, mint, tokenProgram)
	return solana.Instruction{
		ProgramID: pk(ataProgram),
		Accounts: []solana.AccountMeta{
			solana.WritableSigner(signer),
			solana.Writable(ata),
			solana.ReadonlyMeta(signer),
			solana.ReadonlyMeta(mint),
			solana.ReadonlyMeta(pk(sysProgram)),
			solana.ReadonlyMeta(tokenProgram),
		},
		Data: []byte{1}, // createIdempotent
	}
}

// CreateAtaIdempotent create-idempotents the signer's ATA (classic SPL Token program).
func CreateAtaIdempotent(signer, mint solana.Pubkey) solana.Instruction {
	return CreateAtaIdempotentFor(signer, mint, pk(TokenProgram))
}

func marketMetas(signer, mint solana.Pubkey) ([]solana.AccountMeta, bool) {
	m, mintStr, ok := marketFor(mint)
	if !ok {
		return nil, false
	}
	ata := Ata(signer, mint)
	return []solana.AccountMeta{
		solana.WritableSigner(signer),               // 0 signer
		solana.Writable(pk(mLending)),               // 1 lending admin (global)
		solana.Writable(ata),                        // 2 signer's debt ATA
		solana.ReadonlyMeta(pk(mintStr)),            // 3 mint
		solana.Writable(pk(m.reserve)),              // 4
		solana.Writable(pk(m.token)),                // 5
		solana.ReadonlyMeta(pk(m.rateModel)),        // 6
		solana.Writable(pk(m.vault)),                // 7
		solana.ReadonlyMeta(pk(mLiquidity)),         // 8 (global)
		solana.ReadonlyMeta(pk(mVaultProgram)),      // 9 (global)
		solana.ReadonlyMeta(pk(TokenProgram)),       // 10
		solana.ReadonlyMeta(pk(ataProgram)),         // 11
		solana.ReadonlyMeta(pk(sysProgram)),         // 12
		solana.ReadonlyMeta(pk(instructionsSysvar)), // 13
	}, true
}

func buildIx(signer, mint solana.Pubkey, disc [8]byte, amount uint64) (solana.Instruction, bool) {
	metas, ok := marketMetas(signer, mint)
	if !ok {
		return solana.Instruction{}, false
	}
	data := make([]byte, 0, 16)
	data = append(data, disc[:]...)
	amtBuf := make([]byte, 8)
	binary.LittleEndian.PutUint64(amtBuf, amount)
	data = append(data, amtBuf...)
	return solana.Instruction{ProgramID: pk(JupLendProgram), Accounts: metas, Data: data}, true
}

// HasMarket reports whether the JupLend flash-loan market for mint is wired
// (USDC/USDT/wSOL).
func HasMarket(mint solana.Pubkey) bool {
	_, _, ok := marketFor(mint)
	return ok
}

// Borrow flash-borrows `amount` base units of `mint` (USDC/USDT/wSOL) to
// signer. ok=false if the mint has no wired flash market.
func Borrow(signer, mint solana.Pubkey, amount uint64) (solana.Instruction, bool) {
	return buildIx(signer, mint, discBorrow, amount)
}

// Payback repays `amount` base units of `mint` (must equal the borrow — 0 bp fee).
func Payback(signer, mint solana.Pubkey, amount uint64) (solana.Instruction, bool) {
	return buildIx(signer, mint, discPayback, amount)
}

// BorrowUSDC flash-borrows `amount` base units of USDC to signer (back-compat wrapper).
func BorrowUSDC(signer solana.Pubkey, amount uint64) solana.Instruction {
	ix, ok := Borrow(signer, pk(USDCMint), amount)
	if !ok {
		panic("flashloan: USDC market is always wired")
	}
	return ix
}

// PaybackUSDC repays `amount` base units of USDC (must equal the borrow — 0 bp fee).
func PaybackUSDC(signer solana.Pubkey, amount uint64) solana.Instruction {
	ix, ok := Payback(signer, pk(USDCMint), amount)
	if !ok {
		panic("flashloan: USDC market is always wired")
	}
	return ix
}
