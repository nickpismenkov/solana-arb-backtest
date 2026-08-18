// Port of src/bin/mfi_stream_detect.rs
//
// Step 1 of the streaming migration: prove Dragon's Mouth OWNER-subscription
// streams the marginfi program's accounts and populates a LIVE in-memory loan
// book — the thing that removes the hot-path RPC poll. Read-only: decodes each
// account update into MarginfiAccount / Bank maps, counts the update rate, and
// reports freshness (slot lag vs an independent RPC tip). No firing.
//
// Usage: GRPC_ENDPOINT=<triton-url-with-token> GRPC_X_TOKEN=<tok> HELIUS_RPC=<url>
//        [SECS=30] npx tsx src/bin/mfiStreamDetect.ts
//
// NOTE ON THE gRPC WIRING: the Rust source subscribes directly via
// `yellowstone_grpc_client::GeyserGrpcClient` (Dragon's Mouth `subscribe`
// bidi-stream, Yellowstone Geyser proto). Those `.proto` definitions
// (yellowstone-grpc-proto) are not vendored anywhere in this repo and are
// unreachable offline in this sandbox — the SAME limitation already hit and
// documented by Chunk A's `ts-port/src/lib/grpc.ts` (`subscribeGeyserAccounts`
// throws "not implemented" there for the identical reason). This file mirrors
// that precedent: `subscribeMarginfiAccounts` below is a faithful shape of a
// `@grpc/grpc-js` client call (SubscribeRequest with an accounts filter,
// CommitmentLevel.PROCESSED) but throws at call time rather than silently
// no-opping, since we cannot compile the Yellowstone service/message
// descriptors without the .proto file. Everything AROUND the stream — the
// live loan book (accounts/banks maps), the update/decode counters, the
// independent RPC-tip freshness thread, the slot-lag histogram, and the final
// verdict printout — is ported byte-for-byte faithful to the Rust logic and
// will run correctly the moment a real stream feeds it `AccountUpdate`s.
import 'dotenv/config';
import { PublicKey } from '@solana/web3.js';
import { decodeBank, decodeMarginfiAccount, MA_SIZE, type Bank, type MarginfiAccount } from '../lib/liquidation.js';

const MARGINFI_PROGRAM = 'MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA';

async function rpcJson(endpoint: string, body: unknown): Promise<any> {
  const resp = await fetch(endpoint, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  });
  return resp.json();
}

interface RawAccountUpdate {
  slot: bigint;
  pubkey: Uint8Array;
  data: Uint8Array;
}

/**
 * Opens the Yellowstone Dragon's Mouth `subscribe` bidi-stream with an
 * accounts filter (specific pubkeys, CommitmentLevel PROCESSED) and yields
 * decoded account updates.
 *
 * NOT IMPLEMENTED here: requires the Yellowstone Geyser .proto service/message
 * definitions (yellowstone-grpc-proto in the Rust source) to build a
 * `@grpc/grpc-js` service client and `SubscribeRequest`/`SubscribeUpdate`
 * message types. Those .proto files are not vendored in this repo and
 * unreachable offline in this sandbox, so no working client stub can be
 * generated — same limitation as Chunk A's `lib/grpc.ts`. A real port would
 * load the proto with `@grpc/proto-loader`, build a `Client` bound to
 * `endpoint` with `credentials.combineChannelCredentials(credentials.createSsl(), ...)`
 * plus an `x-token` metadata call credential, then open the bidi stream and
 * `write({ accounts: { mfi: { account: subAccounts, owner: [], filters: [] } },
 * commitment: CommitmentLevel.PROCESSED })`.
 */
async function* subscribeMarginfiAccounts(
  _endpoint: string,
  _xToken: string,
  _subAccounts: string[],
): AsyncGenerator<RawAccountUpdate> {
  throw new Error(
    'not implemented: requires the Yellowstone Geyser .proto definitions (yellowstone-grpc-proto), unavailable in this sandbox',
  );
  // eslint-disable-next-line no-unreachable
  yield undefined as never;
}

async function main(): Promise<void> {
  const endpoint = process.env.GRPC_ENDPOINT;
  if (!endpoint) throw new Error('GRPC_ENDPOINT');
  const xToken = process.env.GRPC_X_TOKEN;
  if (!xToken) throw new Error('GRPC_X_TOKEN');
  const rpc = process.env.HELIUS_RPC;
  if (!rpc) throw new Error('HELIUS_RPC');
  const secs = Number.parseInt(process.env.SECS ?? '', 10) || 30;

  // Independent tip-slot reference (bg loop) → absolute freshness of the stream.
  let tip = 0n;
  const tipTimer = setInterval(() => {
    void (async () => {
      try {
        const v = await rpcJson(rpc, { jsonrpc: '2.0', id: 1, method: 'getSlot', params: [{ commitment: 'processed' }] });
        if (typeof v?.result === 'number') tip = BigInt(v.result);
      } catch {
        /* ignore, retry next tick */
      }
    })();
  }, 400);

  // The LIVE loan book — populated purely from the stream. This is what the
  // fire loop will read instead of polling+re-fetching on the hot path.
  const accounts = new Map<string, MarginfiAccount>();
  const banks = new Map<string, Bank>();

  // Triton tier3 rejects owner (program-wide) subscriptions (0 updates), but
  // supports thousands of SPECIFIC accounts on one connection. For liquidations
  // we know our set: scan the marginfi accounts (pubkeys only) and subscribe to
  // a capped batch — the real migration pattern (subscribe the watch-set).
  const maxSub = Number.parseInt(process.env.MAX_SUB ?? '', 10) || 2000;
  console.error('[stream] scanning marginfi account pubkeys to subscribe …');
  const scan = await rpcJson(rpc, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getProgramAccounts',
    params: [
      MARGINFI_PROGRAM,
      { encoding: 'base64', dataSlice: { offset: 0, length: 0 }, filters: [{ dataSize: MA_SIZE }] },
    ],
  });
  const scanResult: any[] = Array.isArray(scan?.result) ? scan.result : [];
  const subAccounts: string[] = scanResult
    .map((e) => e?.pubkey)
    .filter((s): s is string => typeof s === 'string')
    .slice(0, maxSub);
  // Prepend the 3 known high-activity banks (update every slot-ish) so we can
  // tell a subscription-size limit (no bank updates either) from quiet
  // borrowers (bank updates flow, borrowers just idle).
  for (const b of [
    '2s37akK2eyBbp8DZgCm7RtsaEz8eJP3Nxd4urLHQv7yB',
    'DeyH7QxWvnbbaVB4zFrf4hoq7Q8z1ZT14co42BGwGtfM',
    'CCKtUs6Cgwo4aaQUmBPmyoApH2gUDErxNZCAntD6LYGh',
  ].reverse()) {
    subAccounts.unshift(b);
  }
  console.error(`[stream] subscribing to ${subAccounts.length} accounts (3 active banks + ${subAccounts.length - 3} borrowers)`);

  console.error("[stream] connecting to Dragon's Mouth …");

  let updates = 0n;
  let acctDecodes = 0n;
  let bankDecodes = 0n;
  const lags: bigint[] = [];
  const deadline = Date.now() + secs * 1000;
  let lastLog = Date.now();

  try {
    for await (const acc of subscribeMarginfiAccounts(endpoint, xToken, subAccounts)) {
      if (Date.now() >= deadline) break;
      updates += 1n;
      const t = tip;
      if (t > 0n) lags.push(t - acc.slot);
      let pk: PublicKey | null = null;
      try {
        pk = new PublicKey(acc.pubkey);
      } catch {
        pk = null;
      }
      if (pk) {
        const data = Buffer.from(acc.data);
        if (data.length === MA_SIZE) {
          const a = decodeMarginfiAccount(data);
          if (a) {
            accounts.set(pk.toBase58(), a);
            acctDecodes += 1n;
          }
        } else {
          const b = decodeBank(data);
          if (b) {
            banks.set(pk.toBase58(), b);
            bankDecodes += 1n;
          }
        }
      }
      if (Date.now() - lastLog >= 5000) {
        console.error(`[stream] ${updates} updates | live loan book: ${accounts.size} accounts, ${banks.size} banks`);
        lastLog = Date.now();
      }
    }
  } catch (e) {
    console.error(`[stream] error: ${e instanceof Error ? e.message : String(e)}`);
  } finally {
    clearInterval(tipTimer);
  }

  lags.sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));
  const n = lags.length;
  const med = n > 0 ? lags[Math.floor(n / 2)] : 0n;
  console.log(`\n═══ marginfi streaming detector (Dragon's Mouth, ${secs}s) ═══`);
  console.log(`  account updates received: ${updates}  (${(Number(updates) / secs).toFixed(0)}/s)`);
  console.log(`  decoded into live map:    ${acctDecodes} account-writes, ${bankDecodes} bank-writes`);
  console.log(`  live loan book now holds: ${accounts.size} accounts, ${banks.size} banks`);
  console.log(`  stream freshness (slot lag vs RPC tip): median ${med}  [≤1 or negative = leads block production]`);
  if (updates > 0n && med <= 1n) {
    console.log('  VERDICT: ✅ live loan book populates from the stream, fresh. Hot-path RPC poll is replaceable.');
  } else if (updates === 0n) {
    console.log('  VERDICT: ❌ no updates — owner subscription not delivering (check tier/endpoint).');
  } else {
    console.log(`  VERDICT: ⚠ updates flowing but stale (median ${med} slots) — investigate.`);
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
