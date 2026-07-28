// Package marginfi implements marginfi v2 flash-loan instructions
// (alternative to Jupiter Lend). marginfi brackets the borrow/repay between
// start_flashloan and end_flashloan and enforces an account-health check at
// the end; a MarginfiAccount owned by the signer is required (one-time
// create, plain keypair account — NOT a PDA).
package marginfi

import (
	"encoding/binary"
	"os"

	"github.com/gagliardetto/solana-go"
)

const (
	MarginfiProgram    = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA"
	MarginfiGroup      = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8"
	USDCBank           = "2s37akK2eyBbp8DZgCm7RtsaEz8eJP3Nxd4urLHQv7yB"
	USDCMint           = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	tokenProgram       = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	sysProgram         = "11111111111111111111111111111111"
	instructionsSysvar = "Sysvar1nstructions1111111111111111111111111"
)

// VERIFIED Anchor discriminators (sha256("global:<name>")[..8]).
var (
	discStartFlashloan = [8]byte{14, 131, 33, 220, 81, 186, 180, 107}
	discEndFlashloan   = [8]byte{105, 124, 201, 106, 153, 2, 8, 156}
	discBorrow         = [8]byte{4, 126, 116, 53, 48, 5, 212, 31}
	discRepay          = [8]byte{79, 209, 172, 177, 222, 51, 173, 151}
	discAccountInit    = [8]byte{43, 78, 61, 255, 148, 52, 249, 154}
	discLiquidate      = [8]byte{214, 169, 151, 213, 251, 167, 86, 219}
	discWithdraw       = [8]byte{36, 72, 74, 19, 210, 210, 192, 192}
)

func pk(s string) solana.PublicKey { return solana.MustPublicKeyFromBase58(s) }

// BankLiquidityVault derives the generic bank-vault PDA (any bank, not just USDC).
func BankLiquidityVault(bank solana.PublicKey) solana.PublicKey {
	addr, _, _ := solana.FindProgramAddress([][]byte{[]byte("liquidity_vault"), bank.Bytes()}, pk(MarginfiProgram))
	return addr
}
func BankLiquidityVaultAuth(bank solana.PublicKey) solana.PublicKey {
	addr, _, _ := solana.FindProgramAddress([][]byte{[]byte("liquidity_vault_auth"), bank.Bytes()}, pk(MarginfiProgram))
	return addr
}
func BankInsuranceVault(bank solana.PublicKey) solana.PublicKey {
	addr, _, _ := solana.FindProgramAddress([][]byte{[]byte("insurance_vault"), bank.Bytes()}, pk(MarginfiProgram))
	return addr
}

// USDCVault is the bank liquidity vault + its authority. Conventional PDA
// seeds; overridable via env if the deployed build diverged.
func USDCVault() solana.PublicKey {
	if v, ok := os.LookupEnv("MARGINFI_USDC_VAULT"); ok {
		if p, err := solana.PublicKeyFromBase58(v); err == nil {
			return p
		}
	}
	return BankLiquidityVault(pk(USDCBank))
}
func USDCVaultAuthority() solana.PublicKey {
	if v, ok := os.LookupEnv("MARGINFI_USDC_VAULT_AUTH"); ok {
		if p, err := solana.PublicKeyFromBase58(v); err == nil {
			return p
		}
	}
	return BankLiquidityVaultAuth(pk(USDCBank))
}

// AccountInitialize is a one-time: initialize a fresh MarginfiAccount (a
// plain keypair account, which must sign THIS ix only). feePayer usually ==
// authority.
func AccountInitialize(marginfiAccount, authority, feePayer solana.PublicKey) solana.Instruction {
	metas := solana.AccountMetaSlice{
		solana.NewAccountMeta(pk(MarginfiGroup), false, false),
		solana.NewAccountMeta(marginfiAccount, true, true),
		solana.NewAccountMeta(authority, false, true),
		solana.NewAccountMeta(feePayer, true, true),
		solana.NewAccountMeta(pk(sysProgram), false, false),
	}
	return solana.NewInstruction(pk(MarginfiProgram), metas, discAccountInit[:])
}

// StartFlashloan sets ACCOUNT_IN_FLASHLOAN; endIndex = the tx-relative ix
// index of the matching end_flashloan (validated via the instructions sysvar).
func StartFlashloan(marginfiAccount, authority solana.PublicKey, endIndex uint64) solana.Instruction {
	data := make([]byte, 0, 16)
	data = append(data, discStartFlashloan[:]...)
	data = binary.LittleEndian.AppendUint64(data, endIndex)
	metas := solana.AccountMetaSlice{
		solana.NewAccountMeta(marginfiAccount, true, false),
		solana.NewAccountMeta(authority, false, true),
		solana.NewAccountMeta(pk(instructionsSysvar), false, false),
	}
	return solana.NewInstruction(pk(MarginfiProgram), metas, data)
}

// EndFlashloan unsets the flag then runs the real health check. Pass one
// [bank, oracle…] group per still-active balance as remaining. If the
// flashloan nets to zero balances, remaining can be empty.
func EndFlashloan(marginfiAccount, authority solana.PublicKey, remaining solana.AccountMetaSlice) solana.Instruction {
	metas := solana.AccountMetaSlice{
		solana.NewAccountMeta(marginfiAccount, true, false),
		solana.NewAccountMeta(authority, false, true),
	}
	metas = append(metas, remaining...)
	return solana.NewInstruction(pk(MarginfiProgram), metas, discEndFlashloan[:])
}

// BorrowUSDC flash-borrows amount USDC base units into destAta. Inside a
// flashloan the risk engine is skipped, so no oracle remaining-accounts here.
func BorrowUSDC(marginfiAccount, authority, destAta solana.PublicKey, amount uint64) solana.Instruction {
	data := make([]byte, 0, 16)
	data = append(data, discBorrow[:]...)
	data = binary.LittleEndian.AppendUint64(data, amount)
	metas := solana.AccountMetaSlice{
		solana.NewAccountMeta(pk(MarginfiGroup), false, false),
		solana.NewAccountMeta(marginfiAccount, true, false),
		solana.NewAccountMeta(authority, false, true),
		solana.NewAccountMeta(pk(USDCBank), true, false),
		solana.NewAccountMeta(destAta, true, false),
		solana.NewAccountMeta(USDCVaultAuthority(), false, false),
		solana.NewAccountMeta(USDCVault(), true, false),
		solana.NewAccountMeta(pk(tokenProgram), false, false),
	}
	return solana.NewInstruction(pk(MarginfiProgram), metas, data)
}

// PaybackUSDC repays the USDC borrow from sourceAta. repayAll=true clears the
// entire liability (principal + dust interest) regardless of amount and
// leaves any surplus USDC in the ATA as profit — the correct close for a
// flashloan.
func PaybackUSDC(marginfiAccount, authority, sourceAta solana.PublicKey, amount uint64, repayAll bool) solana.Instruction {
	return PaybackAsset(marginfiAccount, authority, pk(USDCBank), sourceAta, amount, repayAll)
}

// PaybackAsset is the generic lending_account_repay for ANY bank
// (USDC/USDT/SOL/…) — same ix layout as the USDC path, with the bank + its
// derived liquidity vault.
func PaybackAsset(marginfiAccount, authority, bank, sourceAta solana.PublicKey, amount uint64, repayAll bool) solana.Instruction {
	data := make([]byte, 0, 18)
	data = append(data, discRepay[:]...)
	data = binary.LittleEndian.AppendUint64(data, amount)
	data = append(data, 1) // Borsh Option<bool>::Some
	data = append(data, boolByte(repayAll))
	metas := solana.AccountMetaSlice{
		solana.NewAccountMeta(pk(MarginfiGroup), false, false),
		solana.NewAccountMeta(marginfiAccount, true, false),
		solana.NewAccountMeta(authority, false, true),
		solana.NewAccountMeta(bank, true, false),
		solana.NewAccountMeta(sourceAta, true, false),
		solana.NewAccountMeta(BankLiquidityVault(bank), true, false),
		solana.NewAccountMeta(pk(tokenProgram), false, false),
	}
	return solana.NewInstruction(pk(MarginfiProgram), metas, data)
}

// LendingAccountLiquidate is the lending_account_liquidate (3-arg, VERIFIED
// live). Seizes assetAmount of assetBank collateral from liquidatee into the
// liquidator's account and takes on the matching liability — a 2.5%
// liquidator bonus (+2.5% insurance). Wrap this in start/end_flashloan so the
// liquidator init-health check is skipped.
//
// liquidateeObs = the liquidatee's observation list: for each of its active
// balances, in balance order, [bank(ro), oracle(ro), …] (2 metas per normal
// Pyth/SB bank). The vaults are all derived from liabBank.
func LendingAccountLiquidate(
	assetBank, liabBank, liquidatorAccount, authority, liquidateeAccount, liabTokenProgram solana.PublicKey,
	assetAmount uint64,
	assetOracle, liabOracle solana.PublicKey,
	liquidateeObs solana.AccountMetaSlice,
) solana.Instruction {
	data := make([]byte, 0, 18)
	data = append(data, discLiquidate[:]...)
	data = binary.LittleEndian.AppendUint64(data, assetAmount)
	data = append(data, byte(len(liquidateeObs))) // liquidatee_accounts
	data = append(data, 0)                        // liquidator_accounts = 0 (init-health skipped in flashloan)

	metas := solana.AccountMetaSlice{
		solana.NewAccountMeta(pk(MarginfiGroup), false, false),
		solana.NewAccountMeta(assetBank, true, false),
		solana.NewAccountMeta(liabBank, true, false),
		solana.NewAccountMeta(liquidatorAccount, true, false),
		solana.NewAccountMeta(authority, false, true),
		solana.NewAccountMeta(liquidateeAccount, true, false),
		solana.NewAccountMeta(BankLiquidityVaultAuth(liabBank), false, false),
		solana.NewAccountMeta(BankLiquidityVault(liabBank), true, false),
		solana.NewAccountMeta(BankInsuranceVault(liabBank), true, false),
		solana.NewAccountMeta(liabTokenProgram, false, false),
		// remaining: front oracle block (asset then liab), then liquidatee obs.
		solana.NewAccountMeta(assetOracle, false, false),
		solana.NewAccountMeta(liabOracle, false, false),
	}
	metas = append(metas, liquidateeObs...)
	return solana.NewInstruction(pk(MarginfiProgram), metas, data)
}

// LendingAccountWithdraw. withdrawAll=true closes the balance and takes
// everything (amount ignored). Inside a flashloan no observation list is
// needed (init-health skipped).
func LendingAccountWithdraw(
	marginfiAccount, authority, bank, destAta, tokenProgramID solana.PublicKey,
	amount uint64, withdrawAll bool,
) solana.Instruction {
	data := make([]byte, 0, 18)
	data = append(data, discWithdraw[:]...)
	data = binary.LittleEndian.AppendUint64(data, amount)
	data = append(data, 1) // Option::Some
	data = append(data, boolByte(withdrawAll))
	metas := solana.AccountMetaSlice{
		solana.NewAccountMeta(pk(MarginfiGroup), false, false),
		solana.NewAccountMeta(marginfiAccount, true, false),
		solana.NewAccountMeta(authority, false, true),
		solana.NewAccountMeta(bank, true, false),
		solana.NewAccountMeta(destAta, true, false),
		solana.NewAccountMeta(BankLiquidityVaultAuth(bank), false, false),
		solana.NewAccountMeta(BankLiquidityVault(bank), true, false),
		solana.NewAccountMeta(tokenProgramID, false, false),
	}
	return solana.NewInstruction(pk(MarginfiProgram), metas, data)
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}
