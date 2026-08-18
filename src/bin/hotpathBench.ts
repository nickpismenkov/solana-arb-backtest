// Port of src/bin/hotpath_bench.rs
//
// Measure the real hot-path compute reaction (decode → predict → optimal_arb
// → build_arb_tx → sign → serialize+base64) with live pool data + the real
// keypair, and separately time a COLD vs WARM Jito connection to show the
// keep-alive Agent effect. Numbers, not estimates.
//
// Usage: RPC_ENDPOINT=<url> ALT_ADDRESS=<alt> KEYPAIR_PATH=<path> tsx src/bin/hotpathBench.ts

import 'dotenv/config';
import { readFileSync } from 'node:fs';
import { Keypair, PublicKey } from '@solana/web3.js';
import { buildArbTx, loadAlt, pkAt, type PoolData } from '../lib/arb.js';
import { afterBaseSwap, clmmFromOrca, clmmFromRay, optimalArb, wsol } from '../lib/clmm.js';
import { defaultBlockEngine, getTipAccounts } from '../lib/jito.js';
import { pair } from '../lib/pools.js';

async function rpc(endpoint: string, body: unknown): Promise<any> {
  const res = await fetch(endpoint, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  });
  return res.json();
}

async function accountData(endpoint: string, addr: string): Promise<Uint8Array> {
  const v = await rpc(endpoint, { jsonrpc: '2.0', id: 1, method: 'getAccountInfo', params: [addr, { encoding: 'base64' }] });
  const b64 = v?.result?.value?.data?.[0];
  return new Uint8Array(Buffer.from(b64, 'base64'));
}

async function main(): Promise<void> {
  const endpoint = process.env.RPC_ENDPOINT;
  if (endpoint === undefined) throw new Error('RPC_ENDPOINT');
  const altAddr = process.env.ALT_ADDRESS;
  if (altAddr === undefined) throw new Error('ALT_ADDRESS');
  const kpPath = process.env.KEYPAIR_PATH;
  if (kpPath === undefined) throw new Error('KEYPAIR_PATH');
  const cfg = pair();
  const base = wsol();

  const bytes: number[] = JSON.parse(readFileSync(kpPath, 'utf8'));
  const kp = Keypair.fromSecretKey(new Uint8Array(bytes));
  const signer = kp.publicKey;
  const alt = loadAlt(altAddr, await accountData(endpoint, altAddr));
  const pd: PoolData = { orca: await accountData(endpoint, cfg.orcaPool), ray: await accountData(endpoint, cfg.rayPool) };
  const bhV = await rpc(endpoint, { jsonrpc: '2.0', id: 1, method: 'getLatestBlockhash', params: [{ commitment: 'confirmed' }] });
  const bh: string = bhV?.result?.value?.blockhash;

  let orcaMintA: PublicKey | undefined;
  try {
    orcaMintA = pkAt(pd.orca, 101);
  } catch {
    orcaMintA = undefined;
  }
  const [oda, odb] = orcaMintA !== undefined && orcaMintA.equals(base) ? [cfg.baseDec, cfg.quoteDec] : [cfg.quoteDec, cfg.baseDec];

  // Warm up.
  buildArbTx(pd, signer, alt, 500_000_000n, true, undefined, 0n, 10_000n, bh, 0n);

  const n = 2000;
  const t0 = performance.now();
  let sink = 0n;
  for (let i = 0; i < n; i++) {
    // decode + predict
    const orca0 = clmmFromOrca(pd.orca, oda, odb, cfg.orcaFeeBps);
    if (orca0 === undefined) throw new Error('orca decode');
    const ray0 = clmmFromRay(pd.ray, cfg.rayFeeBps);
    if (ray0 === undefined) throw new Error('ray decode');
    const orcaP = afterBaseSwap(orca0, base, i % 2 === 0, 3_000_000_000.0);
    const [sizeRaw, , buyOrca] = optimalArb(orcaP, ray0, base, 500.0 * 1e6);
    // build + sign + serialize
    const tx = buildArbTx(pd, signer, alt, BigInt(Math.max(Math.trunc(sizeRaw), 1_000_000)), buyOrca, undefined, 0n, 10_000n, bh, 0n);
    tx.sign([kp]);
    const b64 = Buffer.from(tx.serialize()).toString('base64');
    sink += BigInt(b64.length);
  }
  const per = ((performance.now() - t0) * 1000) / n;
  console.log(`compute reaction (decode+predict+optimal_arb+build+sign+serialize): ${per.toFixed(1)} µs/iter  (n=${n}, sink=${sink})`);

  // Cold vs warm Jito connection.
  const be = defaultBlockEngine();
  const c0 = performance.now();
  await getTipAccounts(be).catch(() => undefined);
  const cold = performance.now() - c0;
  const w0 = performance.now();
  await getTipAccounts(be).catch(() => undefined);
  const warm = performance.now() - w0;
  const w2 = performance.now();
  await getTipAccounts(be).catch(() => undefined);
  const warm2 = performance.now() - w2;
  console.log(`Jito round trip: cold(handshake)=${cold.toFixed(1)} ms  warm=${warm.toFixed(1)} ms  warm2=${warm2.toFixed(1)} ms`);
  console.log('(note: from laptop, not the co-located box — box RTT to Amsterdam is ~0.8ms)');
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
