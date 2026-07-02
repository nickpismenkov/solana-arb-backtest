import 'dotenv/config';
import { createRequire } from 'node:module';
import bs58 from 'bs58';
import type { SubscribeRequest } from '@triton-one/yellowstone-grpc';

// Phase A (step 1): live cross-venue price decoder. Subscribes to the Orca
// Whirlpool + Raydium AMM v4 SOL/USDC pool accounts over gRPC and prints the
// real-time spread — Stage 1 at gRPC speed, on the actual data path a searcher
// would race on. Next step layers on timing vs. on-chain captures.

const require = createRequire(import.meta.url);
const ys = require('@triton-one/yellowstone-grpc');
const Client = ys.default ?? ys;
const CommitmentLevel = ys.CommitmentLevel;

const ENDPOINT = process.env.GRPC_ENDPOINT || 'https://solana-mainnet-grpc.gateway.tatum.io';
const X_TOKEN = process.env.GRPC_X_TOKEN;

const SOL = 'So11111111111111111111111111111111111111112';
const USDC = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v';
const DEC: Record<string, number> = { [SOL]: 9, [USDC]: 6 };

// Accounts to watch (resolved + verified in step 1).
const ORCA_POOL = 'Czfq3xZZDmsdGdUyrNLtRhGc47cXcZtLG4crryfu44zE';
const RAY_SOL_VAULT = 'DQyrAcCrDXQ7NeoqGgDCZwBvWDcYmFCjSb9JtteuvPpz';
const RAY_USDC_VAULT = 'HLmqeL62xR1QoZ1HKKbXRrdN1p3phKpxRMb2VVopvBBz';
const WATCH = [ORCA_POOL, RAY_SOL_VAULT, RAY_USDC_VAULT];
const KEY = Object.fromEntries(WATCH.map((a) => [a, Buffer.from(bs58.decode(a))]));

const u64le = (b: Buffer, o: number) => {
  let n = 0n;
  for (let i = 7; i >= 0; i--) n = (n << 8n) | BigInt(b[o + i]);
  return n;
};
const u128le = (b: Buffer, o: number) => {
  let n = 0n;
  for (let i = 15; i >= 0; i--) n = (n << 8n) | BigInt(b[o + i]);
  return n;
};
const mintAt = (b: Buffer, o: number) => bs58.encode(b.subarray(o, o + 32));

// Orca Whirlpool: sqrtPrice(u128)@65, tokenMintA@101, tokenMintB@181.
function orcaPrice(data: Buffer): number {
  const sqrt = Number(u128le(data, 65)) / 2 ** 64;
  const pAtomicBperA = sqrt * sqrt; // price of A in B, atomic units
  const mintA = mintAt(data, 101);
  const mintB = mintAt(data, 181);
  const uiBperA = pAtomicBperA * 10 ** (DEC[mintA] - DEC[mintB]);
  return mintA === SOL ? uiBperA : 1 / uiBperA; // want USDC per SOL
}
// SPL token account: amount(u64)@64.
const splUi = (data: Buffer, decimals: number) => Number(u64le(data, 64)) / 10 ** decimals;

async function main() {
  if (!X_TOKEN) throw new Error('Set GRPC_X_TOKEN in .env');
  const client = new Client(ENDPOINT, X_TOKEN, undefined);
  await client.connect();

  const request: SubscribeRequest = {
    accounts: { pools: { account: WATCH, owner: [], filters: [] } },
    slots: {},
    transactions: {},
    transactionsStatus: {},
    blocks: {},
    blocksMeta: {},
    entry: {},
    accountsDataSlice: [],
    commitment: CommitmentLevel.PROCESSED,
  };
  const stream = await client.subscribe(request);
  console.log(`\nWatching Orca vs Raydium SOL/USDC live (gRPC)…\n`);

  let orca = NaN;
  let raySol = NaN;
  let rayUsdc = NaN;
  let prints = 0;
  let lastSpread = NaN;
  const WANT = 40;

  const finish = (msg: string) => {
    console.log(msg);
    try {
      stream.end();
    } catch {
      /* noop */
    }
    process.exit(0);
  };
  const timer = setTimeout(() => finish('\n(30s elapsed — stopping)'), 30_000);

  stream.on('data', (u: { account?: { account?: { pubkey?: Uint8Array; data?: Uint8Array } }; slot?: unknown }) => {
    const acc = u.account?.account;
    if (!acc?.pubkey || !acc?.data) return;
    const pk = Buffer.from(acc.pubkey);
    const data = Buffer.from(acc.data);

    if (pk.equals(KEY[ORCA_POOL])) orca = orcaPrice(data);
    else if (pk.equals(KEY[RAY_SOL_VAULT])) raySol = splUi(data, 9);
    else if (pk.equals(KEY[RAY_USDC_VAULT])) rayUsdc = splUi(data, 6);
    else return;

    const ray = rayUsdc / raySol;
    if (!Number.isFinite(orca) || !Number.isFinite(ray)) return;

    const spreadBps = ((orca - ray) / Math.min(orca, ray)) * 10_000;
    // Only print when the spread actually moves (skip duplicate-instant bursts).
    if (Number.isFinite(lastSpread) && Math.abs(spreadBps - lastSpread) < 0.05) return;
    lastSpread = spreadBps;
    const cheap = orca < ray ? 'Orca' : 'Raydium';
    const t = new Date().toISOString().slice(11, 23);
    console.log(
      `${t}  Orca $${orca.toFixed(4)}  Raydium $${ray.toFixed(4)}  spread ${spreadBps >= 0 ? '+' : ''}${spreadBps.toFixed(1)} bps  (buy ${cheap})`,
    );
    if (++prints >= WANT) {
      clearTimeout(timer);
      finish(`\n✓ Live cross-venue decoder working (${prints} updates). Ready for the timing layer.`);
    }
  });
  stream.on('error', (e: unknown) => finish(`\n✗ stream error: ${e instanceof Error ? e.message : e}`));
}

main().catch((e) => {
  console.error('\nError:', e instanceof Error ? e.message : e);
  process.exit(1);
});
