// Package marginfi builds marginfi v2 flash-loan instructions (alternative
// to Jupiter Lend, which Jito silently drops — see land_probe bisection).
// marginfi brackets the borrow/repay between start_flashloan and
// end_flashloan and enforces an account-health check at the end; a
// MarginfiAccount owned by the signer is required (one-time create, plain
// keypair account — NOT a PDA).
//
// Discriminators are VERIFIED (sha256("global:<name>")[..8], cross-checked
// vs the marginfi IDL). Group + USDC bank are the authoritative mainnet
// values. Vault/authority PDAs use the conventional seeds but are
// env-overridable (MARGINFI_USDC_VAULT / _VAULT_AUTH) in case the deployed
// build differs — marginfi_probe verifies the whole thing by mainnet
// simulation before trust.
package marginfi

import (
	"encoding/binary"
	"os"

	"arbengine/internal/solana"
)

const (
	MarginfiProgram = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA"
	MarginfiGroup   = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8"
	USDCBank        = "2s37akK2eyBbp8DZgCm7RtsaEz8eJP3Nxd4urLHQv7yB"
	USDCMint        = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
)

const (
	tokenProgram       = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	sysProgram         = "11111111111111111111111111111111"
	instructionsSysvar = "Sysvar1nstructions1111111111111111111111111"
)

// VERIFIED Anchor discriminators (sha256("global:<name>")[..8]).
var (
	discStartFlashloan = [8]byte{14, 131, 33, 220, 81, 186, 180, 107}
	discEndFlashloan   = [8]byte{105, 124, 201, 106, 153, 2, 8, 156}
	discBorrow         = [8]byte{4, 126, 116, 53, 48, 5, 212, 31}
	// Closing a borrow requires `repay`, not `deposit` (deposit refuses a
	// bank you owe to → OperationDepositOnly 6019). repay_all=Some(true)
	// clears the whole liability (principal + dust interest) and leaves any
	// surplus USDC as profit.
	discRepay       = [8]byte{79, 209, 172, 177, 222, 51, 173, 151}
	discAccountInit = [8]byte{43, 78, 61, 255, 148, 52, 249, 154}
	// Liquidation ixs. VERIFIED against 3 real mainnet liquidations:
	// liquidate data is 18 bytes = disc(8) + asset_amount(u64) +
	// liquidatee_accounts(u8) + liquidator_accounts(u8) — the deployed
	// build is the 3-arg version.
	discLiquidate = [8]byte{214, 169, 151, 213, 251, 167, 86, 219}
	discWithdraw  = [8]byte{36, 72, 74, 19, 210, 210, 192, 192}
)

func pk(s string) solana.Pubkey { return solana.MustPubkeyFromBase58(s) }

// BankLiquidityVault derives the generic bank-vault PDA (any bank, not just
// USDC).
func BankLiquidityVault(bank solana.Pubkey) solana.Pubkey {
	addr, _ := solana.FindProgramAddress([][]byte{[]byte("liquidity_vault"), bank.Bytes()}, pk(MarginfiProgram))
	return addr
}

// BankLiquidityVaultAuth derives the generic bank-vault-authority PDA (any
// bank, not just USDC).
func BankLiquidityVaultAuth(bank solana.Pubkey) solana.Pubkey {
	addr, _ := solana.FindProgramAddress([][]byte{[]byte("liquidity_vault_auth"), bank.Bytes()}, pk(MarginfiProgram))
	return addr
}

// BankInsuranceVault derives the generic bank-insurance-vault PDA (any
// bank, not just USDC).
func BankInsuranceVault(bank solana.Pubkey) solana.Pubkey {
	addr, _ := solana.FindProgramAddress([][]byte{[]byte("insurance_vault"), bank.Bytes()}, pk(MarginfiProgram))
	return addr
}

// USDCVault returns the USDC bank's liquidity vault + its authority.
// Conventional PDA seeds; overridable via env if the deployed build
// diverged (verified by simulation).
func USDCVault() solana.Pubkey {
	if s, ok := os.LookupEnv("MARGINFI_USDC_VAULT"); ok {
		if p, err := solana.PubkeyFromBase58(s); err == nil {
			return p
		}
	}
	return BankLiquidityVault(pk(USDCBank))
}

// USDCVaultAuthority returns the USDC bank's liquidity vault authority PDA.
// Conventional PDA seeds; overridable via env if the deployed build
// diverged (verified by simulation).
func USDCVaultAuthority() solana.Pubkey {
	if s, ok := os.LookupEnv("MARGINFI_USDC_VAULT_AUTH"); ok {
		if p, err := solana.PubkeyFromBase58(s); err == nil {
			return p
		}
	}
	return BankLiquidityVaultAuth(pk(USDCBank))
}

// AccountInitialize is one-time: initialize a fresh MarginfiAccount (a
// plain keypair account, which must sign THIS ix only). feePayer usually
// == authority.
// Accounts: [group, marginfi_account (W+signer), authority (signer),
//
//	fee_payer (W+signer), system_program].
func AccountInitialize(marginfiAccount, authority, feePayer solana.Pubkey) solana.Instruction {
	return solana.Instruction{
		ProgramID: pk(MarginfiProgram),
		Accounts: []solana.AccountMeta{
			solana.ReadonlyMeta(pk(MarginfiGroup)),
			solana.WritableSigner(marginfiAccount),
			solana.SignerMeta(authority),
			solana.WritableSigner(feePayer),
			solana.ReadonlyMeta(pk(sysProgram)),
		},
		Data: discAccountInit[:],
	}
}

// StartFlashloan sets ACCOUNT_IN_FLASHLOAN; endIndex = the tx-relative ix
// index of the matching end_flashloan (validated via the instructions
// sysvar).
// Accounts: [marginfi_account (W), authority (signer), ixs_sysvar].
func StartFlashloan(marginfiAccount, authority solana.Pubkey, endIndex uint64) solana.Instruction {
	data := make([]byte, 0, 16)
	data = append(data, discStartFlashloan[:]...)
	var idx [8]byte
	binary.LittleEndian.PutUint64(idx[:], endIndex)
	data = append(data, idx[:]...)
	return solana.Instruction{
		ProgramID: pk(MarginfiProgram),
		Accounts: []solana.AccountMeta{
			solana.Writable(marginfiAccount),
			solana.SignerMeta(authority),
			solana.ReadonlyMeta(pk(instructionsSysvar)),
		},
		Data: data,
	}
}

// EndFlashloan unsets the flag then runs the real health check. Pass one
// [bank, oracle…] group per still-active balance as remaining. If the
// flashloan nets to zero balances, remaining can be empty.
// Accounts: [marginfi_account (W), authority (signer)] + remaining.
func EndFlashloan(marginfiAccount, authority solana.Pubkey, remaining []solana.AccountMeta) solana.Instruction {
	accounts := make([]solana.AccountMeta, 0, 2+len(remaining))
	accounts = append(accounts,
		solana.Writable(marginfiAccount),
		solana.SignerMeta(authority),
	)
	accounts = append(accounts, remaining...)
	return solana.Instruction{
		ProgramID: pk(MarginfiProgram),
		Accounts:  accounts,
		Data:      discEndFlashloan[:],
	}
}

// BorrowUSDC flash-borrows amount USDC base units into destAta. Inside a
// flashloan the risk engine is skipped, so no oracle remaining-accounts
// here.
// Accounts: [group, marginfi_account (W), authority (signer), bank (W),
//
//	dest_ata (W), vault_authority, liquidity_vault (W), token_program].
func BorrowUSDC(marginfiAccount, authority, destAta solana.Pubkey, amount uint64) solana.Instruction {
	data := make([]byte, 0, 16)
	data = append(data, discBorrow[:]...)
	var amt [8]byte
	binary.LittleEndian.PutUint64(amt[:], amount)
	data = append(data, amt[:]...)
	return solana.Instruction{
		ProgramID: pk(MarginfiProgram),
		Accounts: []solana.AccountMeta{
			solana.ReadonlyMeta(pk(MarginfiGroup)),
			solana.Writable(marginfiAccount),
			solana.SignerMeta(authority),
			solana.Writable(pk(USDCBank)),
			solana.Writable(destAta),
			solana.ReadonlyMeta(USDCVaultAuthority()),
			solana.Writable(USDCVault()),
			solana.ReadonlyMeta(pk(tokenProgram)),
		},
		Data: data,
	}
}

// PaybackUSDC repays the USDC borrow from sourceAta. repayAll=true clears
// the entire liability (principal + dust interest) regardless of amount
// and leaves any surplus USDC in the ATA as profit — the correct close for
// a flashloan.
// Accounts: [group, marginfi_account (W), authority (signer), bank (W),
//
//	source_ata (W), liquidity_vault (W), token_program].
func PaybackUSDC(marginfiAccount, authority, sourceAta solana.Pubkey, amount uint64, repayAll bool) solana.Instruction {
	return PaybackAsset(marginfiAccount, authority, pk(USDCBank), sourceAta, amount, repayAll)
}

// PaybackAsset is the generic lending_account_repay for ANY bank
// (USDC/USDT/SOL/…) — same ix layout as the USDC path, with the bank + its
// derived liquidity vault. Used to close a non-USDC liability the
// liquidator absorbed. Verified: identical account order to the USDC
// repay, which is sim-confirmed.
func PaybackAsset(marginfiAccount, authority, bank, sourceAta solana.Pubkey, amount uint64, repayAll bool) solana.Instruction {
	data := make([]byte, 0, 18)
	data = append(data, discRepay[:]...)
	var amt [8]byte
	binary.LittleEndian.PutUint64(amt[:], amount)
	data = append(data, amt[:]...)
	data = append(data, 1) // Borsh Option<bool>::Some
	if repayAll {
		data = append(data, 1)
	} else {
		data = append(data, 0)
	}
	return solana.Instruction{
		ProgramID: pk(MarginfiProgram),
		Accounts: []solana.AccountMeta{
			solana.ReadonlyMeta(pk(MarginfiGroup)),
			solana.Writable(marginfiAccount),
			solana.SignerMeta(authority),
			solana.Writable(bank),
			solana.Writable(sourceAta),
			solana.Writable(BankLiquidityVault(bank)),
			solana.ReadonlyMeta(pk(tokenProgram)),
		},
		Data: data,
	}
}

// LendingAccountLiquidate builds lending_account_liquidate (3-arg,
// VERIFIED live). Seizes assetAmount of assetBank collateral from
// liquidatee into the liquidator's account and takes on the matching
// liability — a 2.5% liquidator bonus (+2.5% insurance). Wrap this in
// start/end_flashloan so the liquidator init-health check is skipped
// (that's why real liquidators pass liquidator_accounts=0).
//
// liquidateeObs is the liquidatee's observation list: for each of its
// active balances, in balance order, [bank(ro), oracle(ro), …] (2 metas
// per normal Pyth/SB bank). The vaults are all derived from liabBank.
// Fixed accounts: [group, asset_bank(W), liab_bank(W), liquidator_ma(W),
//
//	authority(signer), liquidatee_ma(W), liab_vault_auth, liab_vault(W),
//	liab_insurance_vault(W), token_program] then remaining
//	[asset_oracle, liab_oracle, liquidatee_obs…].
func LendingAccountLiquidate(
	assetBank solana.Pubkey,
	liabBank solana.Pubkey,
	liquidatorAccount solana.Pubkey,
	authority solana.Pubkey,
	liquidateeAccount solana.Pubkey,
	liabTokenProgram solana.Pubkey,
	assetAmount uint64,
	assetOracle solana.Pubkey,
	liabOracle solana.Pubkey,
	liquidateeObs []solana.AccountMeta,
) solana.Instruction {
	data := make([]byte, 0, 18)
	data = append(data, discLiquidate[:]...)
	var amt [8]byte
	binary.LittleEndian.PutUint64(amt[:], assetAmount)
	data = append(data, amt[:]...)
	data = append(data, byte(len(liquidateeObs))) // liquidatee_accounts
	data = append(data, 0)                        // liquidator_accounts = 0 (init-health skipped in flashloan)

	accounts := []solana.AccountMeta{
		solana.ReadonlyMeta(pk(MarginfiGroup)),
		solana.Writable(assetBank),
		solana.Writable(liabBank),
		solana.Writable(liquidatorAccount),
		solana.SignerMeta(authority),
		solana.Writable(liquidateeAccount),
		solana.ReadonlyMeta(BankLiquidityVaultAuth(liabBank)),
		solana.Writable(BankLiquidityVault(liabBank)),
		solana.Writable(BankInsuranceVault(liabBank)),
		solana.ReadonlyMeta(liabTokenProgram),
		// remaining: front oracle block (asset then liab), then liquidatee obs.
		solana.ReadonlyMeta(assetOracle),
		solana.ReadonlyMeta(liabOracle),
	}
	accounts = append(accounts, liquidateeObs...)
	return solana.Instruction{ProgramID: pk(MarginfiProgram), Accounts: accounts, Data: data}
}

// LendingAccountWithdraw builds lending_account_withdraw. withdrawAll=true
// closes the balance and takes everything (amount ignored). Inside a
// flashloan no observation list is needed (init-health skipped).
// Accounts: [group, marginfi_account(W),
//
//	authority(signer), bank(W), dest_ata(W), vault_auth, vault(W), token_program].
func LendingAccountWithdraw(
	marginfiAccount solana.Pubkey,
	authority solana.Pubkey,
	bank solana.Pubkey,
	destAta solana.Pubkey,
	tokenProgramID solana.Pubkey,
	amount uint64,
	withdrawAll bool,
) solana.Instruction {
	data := make([]byte, 0, 18)
	data = append(data, discWithdraw[:]...)
	var amt [8]byte
	binary.LittleEndian.PutUint64(amt[:], amount)
	data = append(data, amt[:]...)
	data = append(data, 1) // Option::Some
	if withdrawAll {
		data = append(data, 1)
	} else {
		data = append(data, 0)
	}
	return solana.Instruction{
		ProgramID: pk(MarginfiProgram),
		Accounts: []solana.AccountMeta{
			solana.ReadonlyMeta(pk(MarginfiGroup)),
			solana.Writable(marginfiAccount),
			solana.SignerMeta(authority),
			solana.Writable(bank),
			solana.Writable(destAta),
			solana.ReadonlyMeta(BankLiquidityVaultAuth(bank)),
			solana.Writable(BankLiquidityVault(bank)),
			solana.ReadonlyMeta(tokenProgramID),
		},
		Data: data,
	}
}
