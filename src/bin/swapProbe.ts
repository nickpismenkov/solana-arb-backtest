// Port of src/bin/swap_probe.rs
//
// Verifies the swap.rs instruction builders against mainnet by assembling a
// real swap and simulating it. We don't need funds: Anchor validates the
// account context (PDAs, owners, tick arrays, oracle) BEFORE the handler
// runs, so a correct build fails late at the token transfer (unfunded ATA),
// while a wrong meta fails early with a constraint/seeds/owner error. The
// probe prints the error class + logs so we can tell which happened.
//
// Usage: RPC_ENDPOINT=<url> tsx src/bin/swapProbe.ts

import 'dotenv/config';
import { PublicKey, TransactionInstruction, TransactionMessage, VersionedTransaction } from '@solana/web3.js';
import { decodeOrcaState, decodeRayState, orcaStartIndex, orcaTickArray, rayStartIndex, rayTickArray } from '../lib/execute.js';
import { pair } from '../lib/pools.js';
import { orcaOracle, orcaSwapIx, raySwapIx, sqrtLimit, type OrcaSwapAccounts, type RaySwapAccounts } from '../lib/swap.js';

const ATA_PROGRAM = 'ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL';
const TOKEN_PROGRAM = 'TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA';

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function rpc(endpoint: string, body: unknown): Promise<any | undefined> {
  for (let attempt = 0; attempt < 4; attempt++) {
    try {
      const res = await fetch(endpoint, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(body),
      });
      return await res.json();
    } catch {
      // fall through to retry
    }
    await sleep(400 << attempt);
  }
  return undefined;
}

async function accountData(endpoint: string, addr: string): Promise<Uint8Array | undefined> {
  const v = await rpc(endpoint, { jsonrpc: '2.0', id: 1, method: 'getAccountInfo', params: [addr, { encoding: 'base64' }] });
  const b64 = v?.result?.value?.data?.[0];
  if (typeof b64 !== 'string') return undefined;
  return Uint8Array.from(Buffer.from(b64, 'base64'));
}

function pkAt(d: Uint8Array, o: number): PublicKey {
  return new PublicKey(d.subarray(o, o + 32));
}

function ata(owner: PublicKey, mint: PublicKey): PublicKey {
  return PublicKey.findProgramAddressSync([owner.toBuffer(), new PublicKey(TOKEN_PROGRAM).toBuffer(), mint.toBuffer()], new PublicKey(ATA_PROGRAM))[0];
}

async function simulate(endpoint: string, ix: TransactionInstruction, authority: PublicKey): Promise<[string | undefined, string[]]> {
  const msg = new TransactionMessage({
    payerKey: authority,
    recentBlockhash: '11111111111111111111111111111111',
    instructions: [ix],
  }).compileToLegacyMessage();
  const tx = new VersionedTransaction(msg);
  const b64 = Buffer.from(tx.serialize()).toString('base64');
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'simulateTransaction',
    params: [b64, { encoding: 'base64', sigVerify: false, replaceRecentBlockhash: true }],
  });
  const val = v?.result?.value ?? {};
  const err = val?.err === null || val?.err === undefined ? undefined : JSON.stringify(val.err);
  const logs: string[] = Array.isArray(val?.logs) ? val.logs.filter((l: unknown): l is string => typeof l === 'string') : [];
  return [err, logs];
}

function classify(err: string | undefined, logs: string[]): string {
  if (err === undefined) {
    return '✅ SIMULATED OK — metas correct, swap executed';
  }
  const joined = logs.join('\n').toLowerCase();
  // Failing on OUR token accounts (unfunded/uninitialized ATAs) means
  // every pool-side meta already validated — the builder is correct.
  if (
    joined.includes('insufficient') ||
    joined.includes('input_token_account') ||
    joined.includes('output_token_account') ||
    joined.includes('token_owner_account') ||
    joined.includes('3012') ||
    joined.includes('not be already initialized') ||
    joined.includes('could not create program address') ||
    joined.includes('account not found') ||
    joined.includes('uninitialized')
  ) {
    return '✅ METAS OK — reached swap handler; only our unfunded ATA failed';
  } else if (joined.includes('seeds') || joined.includes('constraint') || joined.includes('owned by') || joined.includes('declared program')) {
    return '❌ METAS WRONG — account-context validation failed';
  } else {
    return '⚠️  INCONCLUSIVE — inspect logs below';
  }
}

async function main(): Promise<void> {
  const endpoint = process.env.RPC_ENDPOINT ?? 'https://api.mainnet-beta.solana.com';
  const cfg = pair();
  // Any wallet works — we're checking account structure, not balances.
  const authority = new PublicKey('Anu6Awu4kxaEDrg1nkpcikx6tJ2xhfVci5TvDrZBsZEB');

  // ── Orca ──
  console.log(`=== Orca ${cfg.label} pool ${cfg.orcaPool} ===`);
  const od = await accountData(endpoint, cfg.orcaPool);
  if (od === undefined) throw new Error('orca pool');
  const ost = decodeOrcaState(od);
  if (ost === undefined) throw new Error('orca state');
  const orcaPk = new PublicKey(cfg.orcaPool);
  const mintA = pkAt(od, 101);
  const mintB = pkAt(od, 181);
  const start = orcaStartIndex(ost.tick, ost.tickSpacing);
  // Three consecutive tick arrays in the swap direction (a_to_b = price down).
  const n = 88 * ost.tickSpacing;
  const baseIsA = mintA.equals(new PublicKey(cfg.baseMint));
  const aToB = baseIsA; // sell base = A→B when base is mintA
  const starts = aToB ? [start, start - n, start - 2 * n] : [start, start + n, start + 2 * n];
  const oa: OrcaSwapAccounts = {
    whirlpool: orcaPk,
    tokenAuthority: authority,
    tokenOwnerA: ata(authority, mintA),
    tokenVaultA: pkAt(od, 133),
    tokenOwnerB: ata(authority, mintB),
    tokenVaultB: pkAt(od, 213),
    tickArrays: [orcaTickArray(orcaPk, starts[0]!), orcaTickArray(orcaPk, starts[1]!), orcaTickArray(orcaPk, starts[2]!)],
    oracle: orcaOracle(orcaPk),
  };
  const ix = orcaSwapIx(oa, 100_000n, 0n, sqrtLimit(aToB), true, aToB);
  const [err, logs] = await simulate(endpoint, ix, authority);
  console.log(`  a_to_b=${aToB} err=${err}`);
  console.log(`  ${classify(err, logs)}`);
  for (const l of logs.slice(0, 14)) {
    console.log(`    ${l}`);
  }

  // ── Raydium CLMM ──
  console.log(`\n=== Raydium CLMM ${cfg.label} pool ${cfg.rayPool} ===`);
  const rd = await accountData(endpoint, cfg.rayPool);
  if (rd === undefined) throw new Error('ray pool');
  const rst = decodeRayState(rd);
  if (rst === undefined) throw new Error('ray state');
  const rayPk = new PublicKey(cfg.rayPool);
  const ammConfig = pkAt(rd, 9);
  const mint0 = pkAt(rd, 73);
  const mint1 = pkAt(rd, 105);
  const vault0 = pkAt(rd, 137);
  const vault1 = pkAt(rd, 169);
  const observation = pkAt(rd, 201);
  const baseIs0 = mint0.equals(new PublicKey(cfg.baseMint));
  // Sell base: input is base. If base is mint0, input vault = vault0.
  const [inputMint, inputVault, outputVault] = baseIs0 ? [mint0, vault0, vault1] : [mint1, vault1, vault0];
  const outputMint = baseIs0 ? mint1 : mint0;
  // Selling base: input == base. zero_for_one when input is mint0 → arrays descend.
  const zeroForOne = baseIs0;
  const rn = 60 * rst.tickSpacing;
  const rstart = rayStartIndex(rst.tick, rst.tickSpacing);
  const rstarts = zeroForOne ? [rstart, rstart - rn, rstart - 2 * rn] : [rstart, rstart + rn, rstart + 2 * rn];
  const ra: RaySwapAccounts = {
    payer: authority,
    ammConfig,
    poolState: rayPk,
    inputTokenAccount: ata(authority, inputMint),
    outputTokenAccount: ata(authority, outputMint),
    inputVault,
    outputVault,
    observationState: observation,
    tickArrays: [rayTickArray(rayPk, rstarts[0]!), rayTickArray(rayPk, rstarts[1]!), rayTickArray(rayPk, rstarts[2]!)],
  };
  const isBaseInput = true;
  const rix = raySwapIx(ra, 100_000n, 0n, sqrtLimit(baseIs0), isBaseInput);
  const [rerr, rlogs] = await simulate(endpoint, rix, authority);
  console.log(`  is_base_input=${isBaseInput} err=${rerr}`);
  console.log(`  ${classify(rerr, rlogs)}`);
  for (const l of rlogs.slice(0, 14)) {
    console.log(`    ${l}`);
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
