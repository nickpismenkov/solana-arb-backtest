// Port of src/bin/tickarray_probe.rs
//
// Verify tick-array derivation against the chain (no wallet, no money). Reads
// each pool's live state, derives the current tick-array PDA, and checks that
// account actually exists and is owned by the DEX program. If both resolve to
// real program-owned accounts, the start-index math + PDA seeds are correct —
// the foundation the swap instructions stand on.
//
// Usage: RPC_ENDPOINT=<url> tsx src/bin/tickarrayProbe.ts

import 'dotenv/config';
import { PublicKey } from '@solana/web3.js';
import { ORCA_PROGRAM, RAY_CLMM_PROGRAM } from '../lib/decode.js';
import { decodeOrcaState, decodeRayState, orcaStartIndex, orcaTickArray, rayStartIndex, rayTickArray, type PoolState } from '../lib/execute.js';
import { pair } from '../lib/pools.js';

async function rpc(endpoint: string, method: string, params: unknown): Promise<any | undefined> {
  const body = { jsonrpc: '2.0', id: 1, method, params };
  try {
    const res = await fetch(endpoint, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body),
    });
    return await res.json();
  } catch {
    return undefined;
  }
}

async function account(endpoint: string, key: string): Promise<[Uint8Array, string] | undefined> {
  const r = await rpc(endpoint, 'getAccountInfo', [key, { encoding: 'base64' }]);
  const v = r?.result?.value;
  const b64 = v?.data?.[0];
  if (typeof b64 !== 'string') return undefined;
  const owner = v?.owner;
  if (typeof owner !== 'string') return undefined;
  return [Uint8Array.from(Buffer.from(b64, 'base64')), owner];
}

async function check(
  endpoint: string,
  label: string,
  program: string,
  state: PoolState,
  tickArray: PublicKey,
  start: number,
): Promise<void> {
  console.log(`\n${label}: tick=${state.tick} spacing=${state.tickSpacing} liquidity=${state.liquidity}`);
  console.log(`  current tick-array start index: ${start}`);
  console.log(`  derived tick-array PDA: ${tickArray.toBase58()}`);
  const found = await account(endpoint, tickArray.toBase58());
  if (found !== undefined) {
    const [data, owner] = found;
    const ok = owner === program;
    console.log(`  on-chain: owner=${owner} len=${data.length} → ${ok ? '✓ program-owned (derivation VALID)' : '✗ wrong owner'}`);
  } else {
    console.log('  on-chain: account not found ✗ (derivation wrong or empty array)');
  }
}

async function main(): Promise<void> {
  const endpoint = process.env.RPC_ENDPOINT ?? 'https://api.mainnet-beta.solana.com';

  const cfg = pair();
  const orcaPool = new PublicKey(cfg.orcaPool);
  const rayPool = new PublicKey(cfg.rayPool);

  const orcaAccount = await account(endpoint, cfg.orcaPool);
  if (orcaAccount !== undefined) {
    const [data] = orcaAccount;
    const st = decodeOrcaState(data);
    if (st !== undefined) {
      const start = orcaStartIndex(st.tick, st.tickSpacing);
      await check(endpoint, 'Orca', ORCA_PROGRAM, st, orcaTickArray(orcaPool, start), start);
    }
  }
  const rayAccount = await account(endpoint, cfg.rayPool);
  if (rayAccount !== undefined) {
    const [data] = rayAccount;
    const st = decodeRayState(data);
    if (st !== undefined) {
      const start = rayStartIndex(st.tick, st.tickSpacing);
      await check(endpoint, 'Raydium CLMM', RAY_CLMM_PROGRAM, st, rayTickArray(rayPool, start), start);
    }
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
