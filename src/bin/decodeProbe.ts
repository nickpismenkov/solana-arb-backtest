// Port of src/bin/decode_probe.rs
//
// Verify the shred swap decoder against real on-chain swaps. Pulls recent
// signatures for our pools, fetches each tx, resolves ALTs, and decodes the
// swaps — so we confirm direction/amount extraction (incl. ALT-referenced
// swaps) before wiring the decoder into the shred-time pricer.
//
// Usage: RPC_ENDPOINT=https://api.mainnet-beta.solana.com tsx src/bin/decodeProbe.ts

import 'dotenv/config';
import { MessageV0, PublicKey, VersionedTransaction } from '@solana/web3.js';
import { decodeSwaps, AltCache } from '../lib/decode.js';
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

async function recentSigs(endpoint: string, pool: string, limit: number): Promise<string[]> {
  const r = await rpc(endpoint, 'getSignaturesForAddress', [pool, { limit }]);
  const arr: any[] = Array.isArray(r?.result) ? r.result : [];
  return arr.map((s) => s?.signature).filter((s): s is string => typeof s === 'string');
}

async function fetchTx(endpoint: string, sig: string): Promise<VersionedTransaction | undefined> {
  const r = await rpc(endpoint, 'getTransaction', [sig, { encoding: 'base64', maxSupportedTransactionVersion: 0 }]);
  const b64 = r?.result?.transaction?.[0];
  if (typeof b64 !== 'string') return undefined;
  try {
    const bytes = Buffer.from(b64, 'base64');
    return VersionedTransaction.deserialize(bytes);
  } catch {
    return undefined;
  }
}

async function main(): Promise<void> {
  const endpoint = process.env.RPC_ENDPOINT ?? 'https://api.mainnet-beta.solana.com';
  const alt = new AltCache(endpoint);

  let nTx = 0;
  let nSwaps = 0;
  let nAlt = 0;
  const cfg = pair();
  for (const [label, pool] of [
    ['Orca', cfg.orcaPool],
    ['Raydium', cfg.rayPool],
  ] as const) {
    console.log(`\n=== ${label} pool ${pool} — recent swaps ===`);
    for (const sig of await recentSigs(endpoint, pool, 8)) {
      const txn = await fetchTx(endpoint, sig);
      if (txn === undefined) continue;
      nTx += 1;
      const usesAlt = txn.message.addressTableLookups.length > 0;
      if (usesAlt) nAlt += 1;
      const keys = alt.resolveKeys(txn.message);
      if (keys === undefined) {
        console.log(`  ${sig.slice(0, 8)}… ALT resolve failed`);
        continue;
      }
      const swaps = decodeSwaps(txn, keys);
      for (const s of swaps) {
        nSwaps += 1;
        console.log(
          `  ${sig.slice(0, 8)}… ${s.kind.padStart(7)} ${s.dir === 0 ? 'SellBase' : 'BuyBase'} amount=${s.amount} input=${s.amountIsInput} alt=${usesAlt}`,
        );
      }
      if (swaps.length === 0) {
        // Diagnose: is the pool present in resolved keys (ALT ok) and
        // what top-level programs are calling it (CPI/router)?
        const poolPk = new PublicKey(pool);
        const poolInKeys = keys.some((k) => k.equals(poolPk));
        const progs: string[] =
          txn.message instanceof MessageV0
            ? txn.message.compiledInstructions
                .map((ix) => keys[ix.programIdIndex])
                .filter((p): p is PublicKey => p !== undefined)
                .map((p) => p.toBase58().slice(0, 8))
            : [];
        console.log(`  ${sig.slice(0, 8)}… no swap; pool_in_resolved_keys=${poolInKeys} top_level_programs=[${progs.join(', ')}]`);
      }
    }
  }
  console.log(`\ntxs fetched=${nTx}  swaps decoded=${nSwaps}  txs using ALTs=${nAlt}`);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
