import 'dotenv/config';
import { createRequire } from 'node:module';
import bs58 from 'bs58';
import type { SubscribeRequest } from '@triton-one/yellowstone-grpc';

// Phase A (timing): detect REAL fee-adjusted arbs (mid gap > sum of both venue
// fees) and measure how long each lasts (slots + ms) = the reaction budget you'd
// need to win it. Answers "what we lack": if real arbs close within ~1 slot,
// you must be as fast as the co-located incumbents.

const require = createRequire(import.meta.url);
const ys = require('@triton-one/yellowstone-grpc');
const Client = ys.default ?? ys;
const CommitmentLevel = ys.CommitmentLevel;

const ENDPOINT = process.env.GRPC_ENDPOINT || 'https://solana-mainnet-grpc.gateway.tatum.io';
const X_TOKEN = process.env.GRPC_X_TOKEN;

const SOL = 'So11111111111111111111111111111111111111112';
const USDC = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v';
const DEC: Record<string, number> = { [SOL]: 9, [USDC]: 6 };

const ORCA_POOL = 'Czfq3xZZDmsdGdUyrNLtRhGc47cXcZtLG4crryfu44zE';
const RAY_SOL_VAULT = 'DQyrAcCrDXQ7NeoqGgDCZwBvWDcYmFCjSb9JtteuvPpz';
const RAY_USDC_VAULT = 'HLmqeL62xR1QoZ1HKKbXRrdN1p3phKpxRMb2VVopvBBz';
const WATCH = [ORCA_POOL, RAY_SOL_VAULT, RAY_USDC_VAULT];
const KEY = Object.fromEntries(WATCH.map((a) => [a, Buffer.from(bs58.decode(a))]));

const ORCA_FEE_BPS = 4;
const RAY_FEE_BPS = 25;
const ARB_BPS = ORCA_FEE_BPS + RAY_FEE_BPS; // 29 — mid gap must exceed this
const RUN_MS = Number(process.env.RUN_MS || 120_000);

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
function orcaPrice(data: Buffer): number {
  const sqrt = Number(u128le(data, 65)) / 2 ** 64;
  const mintA = mintAt(data, 101);
  const mintB = mintAt(data, 181);
  const uiBperA = sqrt * sqrt * 10 ** (DEC[mintA] - DEC[mintB]);
  return mintA === SOL ? uiBperA : 1 / uiBperA;
}
const splUi = (data: Buffer, decimals: number) => Number(u64le(data, 64)) / 10 ** decimals;
const median = (a: number[]) => (a.length ? [...a].sort((x, y) => x - y)[Math.floor(a.length / 2)] : 0);

async function main() {
  if (!X_TOKEN) throw new Error('Set GRPC_X_TOKEN in .env');
  const client = new Client(ENDPOINT, X_TOKEN, undefined);
  await client.connect();
  const request: SubscribeRequest = {
    accounts: { pools: { account: WATCH, owner: [], filters: [] } },
    slots: {}, transactions: {}, transactionsStatus: {}, blocks: {}, blocksMeta: {}, entry: {},
    accountsDataSlice: [], commitment: CommitmentLevel.PROCESSED,
  };
  const stream = await client.subscribe(request);
  console.log(`\nPhase A timing — real arbs = mid gap > ${ARB_BPS} bps (Orca ${ORCA_FEE_BPS} + Raydium ${RAY_FEE_BPS}).`);
  console.log(`Watching ${RUN_MS / 1000}s…\n`);

  let orca = NaN, raySol = NaN, rayUsdc = NaN;
  const gaps: number[] = [];
  let updates = 0;

  // event tracking
  let inArb = false, openSlot = 0, openT = 0, peakNet = 0, lastSlot = 0;
  const events: { slots: number; ms: number; peakNetBps: number; dir: string }[] = [];

  const summarize = () => {
    console.log(`\n──────── summary (${(RUN_MS / 1000).toFixed(0)}s, ${updates} updates) ────────`);
    console.log(`Baseline signed gap (Ray−Orca): min ${Math.min(...gaps).toFixed(1)}  median ${median(gaps).toFixed(1)}  max ${Math.max(...gaps).toFixed(1)} bps`);
    console.log(`(median ≈ fee differential ${RAY_FEE_BPS - ORCA_FEE_BPS} bps = parity, not opportunity)\n`);
    console.log(`REAL fee-adjusted arbs (net > 0 after both fees): ${events.length}`);
    if (events.length) {
      console.log(`  peak net edge: median ${median(events.map((e) => e.peakNetBps)).toFixed(1)} bps, max ${Math.max(...events.map((e) => e.peakNetBps)).toFixed(1)} bps`);
      console.log(`  LIFETIME (reaction budget): median ${median(events.map((e) => e.slots))} slots / ${median(events.map((e) => e.ms))} ms, max ${Math.max(...events.map((e) => e.slots))} slots`);
      const sameSlot = events.filter((e) => e.slots <= 1).length;
      console.log(`  closed within ≤1 slot: ${sameSlot}/${events.length} (${((sameSlot / events.length) * 100).toFixed(0)}%) → those need co-located/ShredStream speed to win`);
    } else {
      console.log(`  → none appeared: the pair is arbed to within fees. Real edge is rarer/faster than this window, or lives on thinner pairs.`);
    }
    console.log();
  };
  const finish = () => { summarize(); try { stream.end(); } catch { /* */ } process.exit(0); };
  setTimeout(finish, RUN_MS);

  stream.on('data', (u: { account?: { account?: { pubkey?: Uint8Array; data?: Uint8Array }; slot?: string } }) => {
    const acc = u.account?.account;
    if (!acc?.pubkey || !acc?.data) return;
    const pk = Buffer.from(acc.pubkey);
    const data = Buffer.from(acc.data);
    const slot = Number(u.account?.slot ?? 0);
    if (pk.equals(KEY[ORCA_POOL])) orca = orcaPrice(data);
    else if (pk.equals(KEY[RAY_SOL_VAULT])) raySol = splUi(data, 9);
    else if (pk.equals(KEY[RAY_USDC_VAULT])) rayUsdc = splUi(data, 6);
    else return;
    const ray = rayUsdc / raySol;
    if (!Number.isFinite(orca) || !Number.isFinite(ray)) return;
    updates++;
    if (slot) lastSlot = slot;

    const signedGap = ((ray - orca) / orca) * 10_000; // + means Raydium dearer
    gaps.push(signedGap);
    // net edge after both fees, whichever direction
    let net = 0, dir = '';
    if (signedGap > ARB_BPS) { net = signedGap - ARB_BPS; dir = 'buy Orca / sell Raydium'; }
    else if (signedGap < -ARB_BPS) { net = -signedGap - ARB_BPS; dir = 'buy Raydium / sell Orca'; }

    if (net > 0) {
      if (!inArb) {
        inArb = true; openSlot = slot || lastSlot; openT = Date.now(); peakNet = net;
        console.log(`⚡ ARB open  slot ${openSlot}  net ${net.toFixed(1)} bps  (${dir})`);
      } else if (net > peakNet) peakNet = net;
    } else if (inArb) {
      inArb = false;
      const ev = { slots: (slot || lastSlot) - openSlot, ms: Date.now() - openT, peakNetBps: peakNet, dir };
      events.push(ev);
      console.log(`   closed after ${ev.slots} slots / ${ev.ms} ms  (peak net ${peakNet.toFixed(1)} bps)`);
    }
  });
  stream.on('error', (e: unknown) => { console.log('stream error:', e instanceof Error ? e.message : e); finish(); });
}

main().catch((e) => { console.error('\nError:', e instanceof Error ? e.message : e); process.exit(1); });
