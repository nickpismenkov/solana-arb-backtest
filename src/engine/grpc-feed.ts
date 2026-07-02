// gRPC account-subscription feed → price Ticks for the Detector. This is the
// swappable part: a ShredStream feed would implement the same (tick)=>void
// contract and the Detector/report stay identical.

import { createRequire } from 'node:module';
import bs58 from 'bs58';
import type { SubscribeRequest } from '@triton-one/yellowstone-grpc';
import type { Tick } from './detector.js';

const require = createRequire(import.meta.url);
const ys = require('@triton-one/yellowstone-grpc');
const Client = ys.default ?? ys;
const CommitmentLevel = ys.CommitmentLevel;

const SOL = 'So11111111111111111111111111111111111111112';
const USDC = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v';
const DEC: Record<string, number> = { [SOL]: 9, [USDC]: 6 };

export const ORCA_POOL = 'Czfq3xZZDmsdGdUyrNLtRhGc47cXcZtLG4crryfu44zE';
// Raydium CLMM SOL/USDC — the ACTIVE 4bp pool ($5.8M TVL), not the stale legacy
// AMM v4 (25bp) we used before. One account holds sqrtPrice, so it updates on
// every swap (no vault-ratio lag), and its fee matches Orca → 8bp threshold.
export const RAY_CLMM_POOL = '3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv';
const WATCH = [ORCA_POOL, RAY_CLMM_POOL];
const KEY = Object.fromEntries(WATCH.map((a) => [a, Buffer.from(bs58.decode(a))]));

const u128le = (b: Buffer, o: number) => {
  let n = 0n;
  for (let i = 15; i >= 0; i--) n = (n << 8n) | BigInt(b[o + i]);
  return n;
};
const mintAt = (b: Buffer, o: number) => bs58.encode(b.subarray(o, o + 32));
function orcaPrice(data: Buffer): number {
  const sqrt = Number(u128le(data, 65)) / 2 ** 64;
  const mintA = mintAt(data, 101);
  const mintB = mintAt(data, 181);
  const uiBperA = sqrt * sqrt * 10 ** (DEC[mintA] - DEC[mintB]);
  return mintA === SOL ? uiBperA : 1 / uiBperA;
}
// Raydium CLMM PoolState (verified on-chain): sqrtPriceX64 @253, mint0 @73,
// mint1 @105, decimals @233/234. Same Q64.64 → price math as Orca.
function rayClmmPrice(data: Buffer): number {
  const sqrt = Number(u128le(data, 253)) / 2 ** 64;
  const mint0 = mintAt(data, 73);
  const mint1 = mintAt(data, 105);
  const uiT1perT0 = sqrt * sqrt * 10 ** (data[233] - data[234]);
  return mint0 === SOL ? uiT1perT0 : 1 / uiT1perT0;
}

// Connects and calls onTick for every Orca/Raydium price change. Returns a stop fn.
export async function startGrpcFeed(onTick: (t: Tick) => void): Promise<() => void> {
  const endpoint = process.env.GRPC_ENDPOINT || 'https://solana-mainnet-grpc.gateway.tatum.io';
  const xToken = process.env.GRPC_X_TOKEN;
  if (!xToken) throw new Error('Set GRPC_X_TOKEN in .env');
  const client = new Client(endpoint, xToken, undefined);
  await client.connect();
  const request: SubscribeRequest = {
    accounts: { pools: { account: WATCH, owner: [], filters: [] } },
    slots: {}, transactions: {}, transactionsStatus: {}, blocks: {}, blocksMeta: {}, entry: {},
    accountsDataSlice: [], commitment: CommitmentLevel.PROCESSED,
  };
  const stream = await client.subscribe(request);

  stream.on('data', (u: { account?: { account?: { pubkey?: Uint8Array; data?: Uint8Array }; slot?: string } }) => {
    const acc = u.account?.account;
    if (!acc?.pubkey || !acc?.data) return;
    const pk = Buffer.from(acc.pubkey);
    const data = Buffer.from(acc.data);
    const slot = Number(u.account?.slot ?? 0);
    const tsMs = Date.now();
    if (pk.equals(KEY[ORCA_POOL])) {
      onTick({ venue: 'Orca', price: orcaPrice(data), slot, tsMs });
    } else if (pk.equals(KEY[RAY_CLMM_POOL])) {
      onTick({ venue: 'Raydium', price: rayClmmPrice(data), slot, tsMs });
    }
  });
  return () => {
    try {
      stream.end();
    } catch {
      /* noop */
    }
  };
}
