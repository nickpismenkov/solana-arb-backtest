// Port of src/marginfi.rs
//
// marginfi v2 flash-loan instructions (alternative to Jupiter Lend, which
// Jito silently drops — see landProbe bisection). marginfi brackets the
// borrow/repay between startFlashloan and endFlashloan and enforces an
// account-health check at the end; a MarginfiAccount owned by the signer is
// required (one-time create, plain keypair account — NOT a PDA).
//
// Discriminators are VERIFIED (sha256("global:<name>")[..8], cross-checked vs
// the marginfi IDL). Group + USDC bank are the authoritative mainnet values.
// Vault/authority PDAs use the conventional seeds but are env-overridable
// (MARGINFI_USDC_VAULT / _VAULT_AUTH) in case the deployed build differs —
// marginfiProbe verifies the whole thing by mainnet simulation before trust.

import { PublicKey, TransactionInstruction, type AccountMeta } from '@solana/web3.js';

export const MARGINFI_PROGRAM = 'MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA';
export const MARGINFI_GROUP = '4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8';
export const USDC_BANK = '2s37akK2eyBbp8DZgCm7RtsaEz8eJP3Nxd4urLHQv7yB';
export const USDC_MINT = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v';
const TOKEN_PROGRAM = 'TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA';
const SYS_PROGRAM = '11111111111111111111111111111111';
const INSTRUCTIONS_SYSVAR = 'Sysvar1nstructions1111111111111111111111111';

// VERIFIED Anchor discriminators (sha256("global:<name>")[..8]).
const DISC_START_FLASHLOAN = Buffer.from([14, 131, 33, 220, 81, 186, 180, 107]);
const DISC_END_FLASHLOAN = Buffer.from([105, 124, 201, 106, 153, 2, 8, 156]);
const DISC_BORROW = Buffer.from([4, 126, 116, 53, 48, 5, 212, 31]);
// Closing a borrow requires `repay`, not `deposit` (deposit refuses a bank you
// owe to -> OperationDepositOnly 6019). repayAll=true clears the whole
// liability (principal + dust interest) and leaves any surplus USDC as profit.
const DISC_REPAY = Buffer.from([79, 209, 172, 177, 222, 51, 173, 151]);
const DISC_ACCOUNT_INIT = Buffer.from([43, 78, 61, 255, 148, 52, 249, 154]);
// Liquidation ixs. VERIFIED against 3 real mainnet liquidations: liquidate data
// is 18 bytes = disc(8) + asset_amount(u64) + liquidatee_accounts(u8) +
// liquidator_accounts(u8) — the deployed build is the 3-arg version.
const DISC_LIQUIDATE = Buffer.from([214, 169, 151, 213, 251, 167, 86, 219]);
const DISC_WITHDRAW = Buffer.from([36, 72, 74, 19, 210, 210, 192, 192]);

function pk(s: string): PublicKey {
  return new PublicKey(s);
}

function meta(pubkey: PublicKey, isSigner: boolean, isWritable: boolean): AccountMeta {
  return { pubkey, isSigner, isWritable };
}

/** Generic bank-vault PDAs (any bank, not just USDC). */
export function bankLiquidityVault(bank: PublicKey): PublicKey {
  return PublicKey.findProgramAddressSync([Buffer.from('liquidity_vault'), bank.toBuffer()], pk(MARGINFI_PROGRAM))[0];
}
export function bankLiquidityVaultAuth(bank: PublicKey): PublicKey {
  return PublicKey.findProgramAddressSync(
    [Buffer.from('liquidity_vault_auth'), bank.toBuffer()],
    pk(MARGINFI_PROGRAM),
  )[0];
}
export function bankInsuranceVault(bank: PublicKey): PublicKey {
  return PublicKey.findProgramAddressSync([Buffer.from('insurance_vault'), bank.toBuffer()], pk(MARGINFI_PROGRAM))[0];
}

/**
 * Bank liquidity vault + its authority. Conventional PDA seeds; overridable
 * via env if the deployed build diverged (verified by simulation).
 */
export function usdcVault(): PublicKey {
  const override = process.env.MARGINFI_USDC_VAULT;
  if (override) {
    try {
      return new PublicKey(override);
    } catch {
      /* fall through to derived PDA */
    }
  }
  return PublicKey.findProgramAddressSync([Buffer.from('liquidity_vault'), pk(USDC_BANK).toBuffer()], pk(MARGINFI_PROGRAM))[0];
}
export function usdcVaultAuthority(): PublicKey {
  const override = process.env.MARGINFI_USDC_VAULT_AUTH;
  if (override) {
    try {
      return new PublicKey(override);
    } catch {
      /* fall through to derived PDA */
    }
  }
  return PublicKey.findProgramAddressSync(
    [Buffer.from('liquidity_vault_auth'), pk(USDC_BANK).toBuffer()],
    pk(MARGINFI_PROGRAM),
  )[0];
}

/**
 * One-time: initialize a fresh MarginfiAccount (a plain keypair account,
 * which must sign THIS ix only). feePayer usually === authority.
 * Accounts: [group, marginfiAccount (W+signer), authority (signer),
 *            feePayer (W+signer), systemProgram].
 */
export function accountInitialize(marginfiAccount: PublicKey, authority: PublicKey, feePayer: PublicKey): TransactionInstruction {
  return new TransactionInstruction({
    programId: pk(MARGINFI_PROGRAM),
    keys: [
      meta(pk(MARGINFI_GROUP), false, false),
      meta(marginfiAccount, true, true),
      meta(authority, true, false),
      meta(feePayer, true, true),
      meta(pk(SYS_PROGRAM), false, false),
    ],
    data: DISC_ACCOUNT_INIT,
  });
}

/**
 * startFlashloan: sets ACCOUNT_IN_FLASHLOAN; `endIndex` = the tx-relative ix
 * index of the matching endFlashloan (validated via the instructions sysvar).
 * Accounts: [marginfiAccount (W), authority (signer), ixsSysvar].
 */
export function startFlashloan(marginfiAccount: PublicKey, authority: PublicKey, endIndex: bigint): TransactionInstruction {
  const data = Buffer.alloc(16);
  DISC_START_FLASHLOAN.copy(data, 0);
  data.writeBigUInt64LE(endIndex, 8);
  return new TransactionInstruction({
    programId: pk(MARGINFI_PROGRAM),
    keys: [
      meta(marginfiAccount, false, true),
      meta(authority, true, false),
      meta(pk(INSTRUCTIONS_SYSVAR), false, false),
    ],
    data,
  });
}

/**
 * endFlashloan: unsets the flag then runs the real health check. Pass one
 * `[bank, oracle…]` group per still-active balance as `remaining`. If the
 * flashloan nets to zero balances, `remaining` can be empty.
 * Accounts: [marginfiAccount (W), authority (signer)] + remaining.
 */
export function endFlashloan(marginfiAccount: PublicKey, authority: PublicKey, remaining: AccountMeta[]): TransactionInstruction {
  return new TransactionInstruction({
    programId: pk(MARGINFI_PROGRAM),
    keys: [meta(marginfiAccount, false, true), meta(authority, true, false), ...remaining],
    data: DISC_END_FLASHLOAN,
  });
}

/**
 * Flash-borrow `amount` USDC base units into `destAta`. Inside a flashloan
 * the risk engine is skipped, so no oracle remaining-accounts here.
 * Accounts: [group, marginfiAccount (W), authority (signer), bank (W),
 *            destAta (W), vaultAuthority, liquidityVault (W), tokenProgram].
 */
export function borrowUsdc(marginfiAccount: PublicKey, authority: PublicKey, destAta: PublicKey, amount: bigint): TransactionInstruction {
  const data = Buffer.alloc(16);
  DISC_BORROW.copy(data, 0);
  data.writeBigUInt64LE(amount, 8);
  return new TransactionInstruction({
    programId: pk(MARGINFI_PROGRAM),
    keys: [
      meta(pk(MARGINFI_GROUP), false, false),
      meta(marginfiAccount, false, true),
      meta(authority, true, false),
      meta(pk(USDC_BANK), false, true),
      meta(destAta, false, true),
      meta(usdcVaultAuthority(), false, false),
      meta(usdcVault(), false, true),
      meta(pk(TOKEN_PROGRAM), false, false),
    ],
    data,
  });
}

/**
 * Repay the USDC borrow from `sourceAta`. `repayAll=true` clears the entire
 * liability (principal + dust interest) regardless of `amount` and leaves any
 * surplus USDC in the ATA as profit — the correct close for a flashloan.
 */
export function paybackUsdc(marginfiAccount: PublicKey, authority: PublicKey, sourceAta: PublicKey, amount: bigint, repayAll: boolean): TransactionInstruction {
  return paybackAsset(marginfiAccount, authority, pk(USDC_BANK), sourceAta, amount, repayAll);
}

/**
 * Generic `lending_account_repay` for ANY bank (USDC/USDT/SOL/…) — same ix
 * layout as the USDC path, with the bank + its derived liquidity vault. Used
 * to close a non-USDC liability the liquidator absorbed. Verified: identical
 * account order to the USDC repay, which is sim-confirmed.
 * Accounts: [group, marginfiAccount (W), authority (signer), bank (W),
 *            sourceAta (W), liquidityVault (W), tokenProgram].
 */
export function paybackAsset(
  marginfiAccount: PublicKey,
  authority: PublicKey,
  bank: PublicKey,
  sourceAta: PublicKey,
  amount: bigint,
  repayAll: boolean,
): TransactionInstruction {
  const data = Buffer.alloc(18);
  DISC_REPAY.copy(data, 0);
  data.writeBigUInt64LE(amount, 8);
  data.writeUInt8(1, 16); // Borsh Option<bool>::Some
  data.writeUInt8(repayAll ? 1 : 0, 17);
  return new TransactionInstruction({
    programId: pk(MARGINFI_PROGRAM),
    keys: [
      meta(pk(MARGINFI_GROUP), false, false),
      meta(marginfiAccount, false, true),
      meta(authority, true, false),
      meta(bank, false, true),
      meta(sourceAta, false, true),
      meta(bankLiquidityVault(bank), false, true),
      meta(pk(TOKEN_PROGRAM), false, false),
    ],
    data,
  });
}

/**
 * lending_account_liquidate (3-arg, VERIFIED live). Seizes `assetAmount` of
 * `assetBank` collateral from `liquidatee` into the liquidator's account and
 * takes on the matching liability — a 2.5% liquidator bonus (+2.5% insurance).
 * Wrap this in start/endFlashloan so the liquidator init-health check is
 * skipped (that's why real liquidators pass liquidatorAccounts=0).
 *
 * `liquidateeObs` = the liquidatee's observation list: for each of its active
 * balances, in balance order, `[bank(ro), oracle(ro), …]` (2 metas per normal
 * Pyth/SB bank). The vaults are all derived from `liabBank`.
 * Fixed accounts: [group, assetBank(W), liabBank(W), liquidatorMa(W),
 *   authority(signer), liquidateeMa(W), liabVaultAuth, liabVault(W),
 *   liabInsuranceVault(W), tokenProgram] then remaining
 *   [assetOracle, liabOracle, liquidateeObs…].
 */
export function lendingAccountLiquidate(
  assetBank: PublicKey,
  liabBank: PublicKey,
  liquidatorAccount: PublicKey,
  authority: PublicKey,
  liquidateeAccount: PublicKey,
  liabTokenProgram: PublicKey,
  assetAmount: bigint,
  assetOracle: PublicKey,
  liabOracle: PublicKey,
  liquidateeObs: AccountMeta[],
): TransactionInstruction {
  const data = Buffer.alloc(18);
  DISC_LIQUIDATE.copy(data, 0);
  data.writeBigUInt64LE(assetAmount, 8);
  data.writeUInt8(liquidateeObs.length, 16); // liquidatee_accounts
  data.writeUInt8(0, 17); // liquidator_accounts = 0 (init-health skipped in flashloan)

  const keys: AccountMeta[] = [
    meta(pk(MARGINFI_GROUP), false, false),
    meta(assetBank, false, true),
    meta(liabBank, false, true),
    meta(liquidatorAccount, false, true),
    meta(authority, true, false),
    meta(liquidateeAccount, false, true),
    meta(bankLiquidityVaultAuth(liabBank), false, false),
    meta(bankLiquidityVault(liabBank), false, true),
    meta(bankInsuranceVault(liabBank), false, true),
    meta(liabTokenProgram, false, false),
    // remaining: front oracle block (asset then liab), then liquidatee obs.
    meta(assetOracle, false, false),
    meta(liabOracle, false, false),
    ...liquidateeObs,
  ];
  return new TransactionInstruction({ programId: pk(MARGINFI_PROGRAM), keys, data });
}

/**
 * lending_account_withdraw. `withdrawAll=true` closes the balance and takes
 * everything (amount ignored). Inside a flashloan no observation list is
 * needed (init-health skipped). Accounts: [group, marginfiAccount(W),
 *   authority(signer), bank(W), destAta(W), vaultAuth, vault(W), tokenProgram].
 */
export function lendingAccountWithdraw(
  marginfiAccount: PublicKey,
  authority: PublicKey,
  bank: PublicKey,
  destAta: PublicKey,
  tokenProgram: PublicKey,
  amount: bigint,
  withdrawAll: boolean,
): TransactionInstruction {
  const data = Buffer.alloc(18);
  DISC_WITHDRAW.copy(data, 0);
  data.writeBigUInt64LE(amount, 8);
  data.writeUInt8(1, 16); // Option::Some
  data.writeUInt8(withdrawAll ? 1 : 0, 17);
  return new TransactionInstruction({
    programId: pk(MARGINFI_PROGRAM),
    keys: [
      meta(pk(MARGINFI_GROUP), false, false),
      meta(marginfiAccount, false, true),
      meta(authority, true, false),
      meta(bank, false, true),
      meta(destAta, false, true),
      meta(bankLiquidityVaultAuth(bank), false, false),
      meta(bankLiquidityVault(bank), false, true),
      meta(tokenProgram, false, false),
    ],
    data,
  });
}
