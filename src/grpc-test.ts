import 'dotenv/config';
import { createRequire } from 'node:module';
import type { SubscribeRequest } from '@triton-one/yellowstone-grpc';

// CJS module with non-static named exports → require at runtime.
const require = createRequire(import.meta.url);
const ys = require('@triton-one/yellowstone-grpc');
const Client = ys.default ?? ys;
const CommitmentLevel = ys.CommitmentLevel;

// Confirms a Yellowstone-compatible gRPC endpoint streams live data: subscribes
// to slot updates and prints the first several with inter-arrival timing.

// Provider-agnostic (currently Tatum): endpoint + API key (sent as x-token).
const ENDPOINT = process.env.GRPC_ENDPOINT || 'https://solana-mainnet-grpc.gateway.tatum.io';
const X_TOKEN = process.env.GRPC_X_TOKEN;

async function main() {
  if (!X_TOKEN) throw new Error('Set GRPC_X_TOKEN in .env (your gRPC provider API key → x-token header).');
  console.log(`\nConnecting to ${ENDPOINT} …`);
  const client = new Client(ENDPOINT, X_TOKEN, undefined);
  await client.connect();

  try {
    const slot = await client.getSlot();
    console.log(`✓ Handshake OK — getSlot returned (auth accepted)\n`, slot);
  } catch (e) {
    console.log(`(getSlot failed, continuing to subscribe: ${e instanceof Error ? e.message : e})\n`);
  }

  const request: SubscribeRequest = {
    accounts: {},
    slots: { all: {} },
    transactions: {},
    transactionsStatus: {},
    blocks: {},
    blocksMeta: {},
    entry: {},
    accountsDataSlice: [],
    commitment: CommitmentLevel.PROCESSED,
  };

  // v5: pass the initial request INTO subscribe() (not write-after).
  let stream;
  try {
    stream = await client.subscribe(request);
  } catch (e) {
    const err = e as { code?: number | string; details?: string; message?: string };
    console.log('\n✗ subscribe() failed to open:');
    console.log('   code   :', err.code);
    console.log('   details:', err.details);
    console.log('   message:', err.message);
    process.exit(1);
  }
  console.log('Subscribed; waiting for slot updates…\n');

  const WANT = 8;
  let count = 0;
  let lastT = Date.now();
  const deltas: number[] = [];
  const finish = (ok: boolean, msg?: string) => {
    if (msg) console.log(msg);
    try {
      stream.end();
    } catch {
      /* noop */
    }
    process.exit(ok ? 0 : 1);
  };
  const timer = setTimeout(() => finish(false, '\n✗ Timed out waiting for slot updates (30s).'), 30_000);

  stream.on('data', (u: { slot?: { slot?: string } }) => {
    if (!u.slot?.slot) return;
    const now = Date.now();
    const d = now - lastT;
    lastT = now;
    count++;
    if (count > 1) deltas.push(d);
    console.log(`  slot ${u.slot.slot}  (+${d} ms)`);
    if (count >= WANT) {
      clearTimeout(timer);
      const avg = deltas.reduce((s, x) => s + x, 0) / (deltas.length || 1);
      console.log(`\n✓ LIVE STREAM OK — ${count} slots, mean inter-arrival ${avg.toFixed(0)} ms (~400ms healthy).\n`);
      finish(true);
    }
  });
  stream.on('error', (e: unknown) => {
    clearTimeout(timer);
    const err = e as { code?: number | string; details?: string; message?: string };
    finish(false, `\n✗ Stream error — code:${err.code} details:${err.details} message:${err.message}`);
  });
}

main().catch((e) => {
  console.error('\nError:', e instanceof Error ? e.message : e);
  process.exit(1);
});
