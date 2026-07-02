import 'dotenv/config';
import { createRequire } from 'node:module';
import bs58 from 'bs58';
import type { SubscribeRequest } from '@triton-one/yellowstone-grpc';

// Leg 1 (detection primitive): "see the triggering swap the instant it lands."
// Subscribes to TRANSACTIONS touching the Orca + Raydium SOL/USDC pools and
// prints each with slot + arrival time. This is the trigger a backrun reacts to.
// Same logic will run on the NY box against a ShredStream-sourced feed for real
// (ahead-of-Turbine) latency; here we validate it works on the current feed.

const require = createRequire(import.meta.url);
const ys = require('@triton-one/yellowstone-grpc');
const Client = ys.default ?? ys;
const CommitmentLevel = ys.CommitmentLevel;

const ENDPOINT = process.env.GRPC_ENDPOINT || 'https://solana-mainnet-grpc.gateway.tatum.io';
const X_TOKEN = process.env.GRPC_X_TOKEN;

const ORCA_POOL = 'Czfq3xZZDmsdGdUyrNLtRhGc47cXcZtLG4crryfu44zE';
const RAY_POOL = '58oQChx4yWmvKdwLLZzBi4ChoCc2fqCUWBkwMihLYQo2';
const LABEL: Record<string, string> = { [ORCA_POOL]: 'Orca', [RAY_POOL]: 'Raydium' };

async function main() {
  if (!X_TOKEN) throw new Error('Set GRPC_X_TOKEN in .env');
  const client = new Client(ENDPOINT, X_TOKEN, undefined);
  await client.connect();

  const request: SubscribeRequest = {
    accounts: {},
    slots: {},
    transactions: {
      swaps: {
        accountInclude: [ORCA_POOL, RAY_POOL],
        accountExclude: [],
        accountRequired: [],
        vote: false,
        failed: false,
      },
    },
    transactionsStatus: {},
    blocks: {},
    blocksMeta: {},
    entry: {},
    accountsDataSlice: [],
    commitment: CommitmentLevel.PROCESSED,
  };
  const stream = await client.subscribe(request);
  console.log(`\nLeg 1 — watching swaps on Orca + Raydium SOL/USDC (transaction feed)…\n`);

  let n = 0;
  const WANT = 15;
  const finish = (msg: string) => {
    console.log(msg);
    try {
      stream.end();
    } catch {
      /* noop */
    }
    process.exit(0);
  };
  const timer = setTimeout(() => finish(`\n(30s elapsed — saw ${n} swaps)`), 30_000);

  stream.on('data', (u: {
    transaction?: { transaction?: { signature?: Uint8Array; transaction?: { message?: { accountKeys?: Uint8Array[] } } }; slot?: string };
  }) => {
    const tx = u.transaction;
    if (!tx?.transaction?.signature) return;
    const sig = bs58.encode(Buffer.from(tx.transaction.signature));
    const slot = u.transaction?.slot ?? '?';
    // which pool(s) it touched
    const keys = (tx.transaction.transaction?.message?.accountKeys ?? []).map((k) => bs58.encode(Buffer.from(k)));
    const venues = [ORCA_POOL, RAY_POOL].filter((p) => keys.includes(p)).map((p) => LABEL[p]);
    const t = new Date().toISOString().slice(11, 23);
    console.log(`${t}  slot ${slot}  ${venues.join('+') || '?'}  ${sig.slice(0, 12)}…`);
    if (++n >= WANT) {
      clearTimeout(timer);
      finish(`\n✓ Trigger feed works — saw ${n} live swaps on our pools. This is the backrun signal.`);
    }
  });
  stream.on('error', (e: unknown) => finish(`\n✗ stream error: ${e instanceof Error ? e.message : e}`));
}

main().catch((e) => {
  console.error('\nError:', e instanceof Error ? e.message : e);
  process.exit(1);
});
