// Port of src/bin/mfi_oracle_census.rs
//
// Census of marginfi bank oracles — groups the group's banks by oracle_setup
// and inspects each oracle account (owner program, size, disc) so we know
// exactly which decoders to build for full pricing coverage. Read-only.
//
// Usage: HELIUS_RPC=<url> npx tsx src/bin/mfiOracleCensus.ts

import 'dotenv/config';
import { PublicKey } from '@solana/web3.js';
import { decodeBank, type Bank } from '../lib/liquidation.js';

const MARGINFI_PROGRAM = 'MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA';
const MARGINFI_GROUP = '4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8';
const BANK_SIZE = 1864;

async function rpc(endpoint: string, body: unknown): Promise<any | null> {
  for (let attempt = 0; attempt < 4; attempt++) {
    try {
      const resp = await fetch(endpoint, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(body),
      });
      const v = await resp.json();
      return v;
    } catch {
      /* retry */
    }
    await new Promise((r) => setTimeout(r, 400 << attempt));
  }
  return null;
}

function b64(d: any): Buffer | null {
  const s = d?.[0];
  if (typeof s !== 'string') return null;
  try {
    return Buffer.from(s, 'base64');
  } catch {
    return null;
  }
}

async function main(): Promise<void> {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (!endpoint) throw new Error('HELIUS_RPC');

  // Banks of the main group (Bank.group at offset 41).
  const resp = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getProgramAccounts',
    params: [
      MARGINFI_PROGRAM,
      {
        encoding: 'base64',
        filters: [{ dataSize: BANK_SIZE }, { memcmp: { offset: 41, bytes: MARGINFI_GROUP } }],
      },
    ],
  });
  const entries: any[] = Array.isArray(resp?.result) ? resp.result : [];
  console.log(`${entries.length} banks in group`);

  const bySetup = new Map<number, Array<[PublicKey, Bank]>>();
  for (const e of entries) {
    const pkStr = e?.pubkey;
    if (typeof pkStr !== 'string') continue;
    let pk: PublicKey;
    try {
      pk = new PublicKey(pkStr);
    } catch {
      continue;
    }
    const raw = b64(e?.account?.data);
    if (!raw) continue;
    const bank = decodeBank(raw);
    if (!bank) continue;
    const arr = bySetup.get(bank.oracleSetup) ?? [];
    arr.push([pk, bank]);
    bySetup.set(bank.oracleSetup, arr);
  }

  const setups = [...bySetup.keys()].sort((a, b) => a - b);
  for (const setup of setups) {
    const banks = bySetup.get(setup)!;
    console.log(`\n──── oracle_setup=${setup} (${banks.length} banks)`);
    for (const [pk, bank] of banks.slice(0, 6)) {
      const info = await rpc(endpoint, {
        jsonrpc: '2.0',
        id: 1,
        method: 'getAccountInfo',
        params: [bank.oracleKey.toBase58(), { encoding: 'base64' }],
      });
      let owner = '?';
      let len = 0;
      let disc = '';
      if (info !== null) {
        const val = info?.result?.value;
        owner = typeof val?.owner === 'string' ? val.owner : 'MISSING';
        const data = b64(val?.data) ?? Buffer.alloc(0);
        len = data.length;
        disc = [...data.subarray(0, 8)].map((b) => b.toString(16).padStart(2, '0')).join('');
      }
      console.log(
        `  bank ${pk.toBase58().slice(0, 8)}…  mint ${bank.mint.toBase58().slice(0, 8)}…  oracle ${bank.oracleKey.toBase58()}  owner ${owner}  len ${len} disc ${disc}`,
      );
    }
    if (banks.length > 6) console.log(`  … +${banks.length - 6} more`);
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
