// Port of src/bin/liq_alt_print.rs
//
// Print the constant accounts of every marginfi-USDC liquidation fire tx —
// the address set for the dedicated liquidation ALT (LIQ_ALT). Candidate-
// specific accounts (liquidatee, asset bank/mint/oracle/ATA, Jupiter route)
// stay static or come via Jupiter's own ALTs.
//
// Setup (one-time, ~0.002 SOL reclaimable rent):
//   solana address-lookup-table create --keypair ~/arb-keypair.json -u <rpc>
//   solana address-lookup-table extend <TABLE> --addresses "$(liqAltPrint | paste -sd, -)" …
//
// Usage: [HELIUS_RPC=<url>] tsx src/bin/liqAltPrint.ts

import 'dotenv/config';
import { PublicKey } from '@solana/web3.js';
import { ataFor } from '../lib/flashloan.js';
import * as marginfi from '../lib/marginfi.js';
import { decodeBank } from '../lib/liquidation.js';

const DEFAULT_AUTHORITY = 'DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak';
const DEFAULT_LIQUIDATOR_MA = 'B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD';
// USDC bank oracle (Pyth PriceUpdateV2) — cross-checked against the live bank
// when HELIUS_RPC is set.
const USDC_ORACLE = 'Dpw1EAVrSB1ibxiDQyTAW6Zip3J4Btk2x4SgApQCeFbX';
const JUPITER_PROGRAM = 'JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4';

async function liveUsdcOracle(endpoint: string): Promise<PublicKey | undefined> {
  try {
    const res = await fetch(endpoint, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({
        jsonrpc: '2.0',
        id: 1,
        method: 'getAccountInfo',
        params: [marginfi.USDC_BANK, { encoding: 'base64' }],
      }),
    });
    const v = (await res.json()) as { result?: { value?: { data?: [string, string] } } };
    const b64 = v.result?.value?.data?.[0];
    if (!b64) return undefined;
    const raw = Buffer.from(b64, 'base64');
    const bank = decodeBank(raw);
    return bank?.oracleKey;
  } catch {
    return undefined;
  }
}

async function main(): Promise<void> {
  const authority = new PublicKey(process.env.AUTHORITY ?? DEFAULT_AUTHORITY);
  const liquidatorMa = new PublicKey(process.env.LIQUIDATOR_MA ?? DEFAULT_LIQUIDATOR_MA);
  const usdc = new PublicKey(marginfi.USDC_MINT);
  const usdcBank = new PublicKey(marginfi.USDC_BANK);
  const tokenProgram = new PublicKey('TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA');

  let usdcOracle: string;
  const heliusRpc = process.env.HELIUS_RPC;
  const live = heliusRpc ? await liveUsdcOracle(heliusRpc) : undefined;
  if (live) {
    if (live.toBase58() !== USDC_ORACLE) {
      console.error(`⚠ live USDC oracle ${live.toBase58()} differs from constant ${USDC_ORACLE} — using live`);
    }
    usdcOracle = live.toBase58();
  } else {
    usdcOracle = USDC_ORACLE;
  }

  const addrs = [
    marginfi.MARGINFI_PROGRAM,
    marginfi.MARGINFI_GROUP,
    marginfi.USDC_BANK,
    marginfi.USDC_MINT,
    marginfi.bankLiquidityVault(usdcBank).toBase58(),
    marginfi.bankLiquidityVaultAuth(usdcBank).toBase58(),
    marginfi.bankInsuranceVault(usdcBank).toBase58(),
    usdcOracle,
    authority.toBase58(),
    liquidatorMa.toBase58(),
    ataFor(authority, usdc, tokenProgram).toBase58(),
    tokenProgram.toBase58(),
    'TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb', // Token-2022
    'ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL', // ATA program
    '11111111111111111111111111111111', // system
    'ComputeBudget111111111111111111111111111111',
    'Sysvar1nstructions1111111111111111111111111',
    JUPITER_PROGRAM,
  ];
  for (const a of addrs) console.log(a);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
