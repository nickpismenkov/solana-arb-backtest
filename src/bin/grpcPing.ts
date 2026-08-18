// Port of src/bin/grpc_ping.rs
//
// Definitive Triton liveness test: subscribe to SLOTS + a few specific accounts
// on one connection and count each separately. Slots tick ~2.5×/s unconditionally
// — so this discriminates:
//   • slots > 0, accounts = 0  → connection & stream fine; account sub is the issue
//   • slots = 0, accounts = 0  → whole stream throttled/banned (transport is up but
//                                 no data flows) → it's the rate-limit penalty box
// Usage: GRPC_ENDPOINT=<url> GRPC_X_TOKEN=<tok> [SECS=15] tsx src/bin/grpcPing.ts

type UpdateKind = 'slot' | 'account' | 'ping' | 'other';

interface SubscribeUpdate {
  kind: UpdateKind;
}

/**
 * Opens the Yellowstone Geyser `subscribe` stream with SLOTS + ACCOUNTS
 * filters and yields each update's kind.
 *
 * NOT IMPLEMENTED: requires the Yellowstone Geyser .proto definitions
 * (yellowstone-grpc-client / yellowstone-grpc-proto in the Rust source),
 * which are not vendored in this repo and unreachable offline in this
 * sandbox, so no working gRPC stub can be generated.
 */
async function* subscribePing(
  _endpoint: string,
  _xToken: string,
  _acctList: string[],
  _commitment: 'processed' | 'confirmed' | 'finalized',
): AsyncGenerator<SubscribeUpdate> {
  throw new Error(
    'not implemented: requires the Yellowstone Geyser .proto definitions, unavailable in this sandbox',
  );
}

async function main(): Promise<void> {
  const endpoint = process.env.GRPC_ENDPOINT;
  if (endpoint === undefined) throw new Error('GRPC_ENDPOINT');
  const xToken = process.env.GRPC_X_TOKEN;
  if (xToken === undefined) throw new Error('GRPC_X_TOKEN');
  const secs = Number.parseInt(process.env.SECS ?? '', 10) || 15;

  console.error('[ping] connecting …');
  const t = Date.now();
  // Default includes the Clock sysvar — it updates EVERY slot, so if even Clock
  // yields 0 account updates, account subscriptions are broadly not delivering.
  const acctList: string[] = process.env.ACCOUNTS
    ? process.env.ACCOUNTS.split(',').map((x) => x.trim())
    : [
        'SysvarC1ock11111111111111111111111111111111', // Clock — ticks every slot
        '2s37akK2eyBbp8DZgCm7RtsaEz8eJP3Nxd4urLHQv7yB', // marginfi USDC bank
        'DeyH7QxWvnbbaVB4zFrf4hoq7Q8z1ZT14co42BGwGtfM', // marginfi BONK bank
        'CCKtUs6Cgwo4aaQUmBPmyoApH2gUDErxNZCAntD6LYGh', // marginfi wSOL bank
      ];
  const commitment: 'processed' | 'confirmed' | 'finalized' =
    process.env.COMMITMENT === 'confirmed' ? 'confirmed' : process.env.COMMITMENT === 'finalized' ? 'finalized' : 'processed';
  console.error(`[ping] ${acctList.length} accounts, commitment=${commitment}`);

  let nSlot = 0n;
  let nAcct = 0n;
  let nOther = 0n;
  const deadline = Date.now() + secs * 1000;

  try {
    const stream = subscribePing(endpoint, xToken, acctList, commitment);
    console.error(`[ping] connected in ${Date.now() - t}ms`);
    console.error(`[ping] subscribed (slots + ${acctList.length} accounts, ${commitment}). listening ${secs}s …\n`);
    while (Date.now() < deadline) {
      const { value, done } = await stream.next();
      if (done) {
        console.error('[ping] stream closed by server');
        break;
      }
      switch (value.kind) {
        case 'slot':
          nSlot += 1n;
          break;
        case 'account':
          nAcct += 1n;
          break;
        case 'ping':
          break;
        case 'other':
          nOther += 1n;
          break;
      }
    }
  } catch (e) {
    console.error(`[ping] stream error: ${e}`);
  }

  console.log(`\n═══ Triton liveness (${secs}s) ═══`);
  console.log(`  SLOT updates:    ${nSlot}  (${(Number(nSlot) / secs).toFixed(1)}/s)`);
  console.log(`  ACCOUNT updates: ${nAcct}`);
  console.log(`  other:           ${nOther}`);
  if (nSlot > 0n && nAcct > 0n) {
    console.log('  VERDICT: ✅ FULLY LIVE — stream + account subscription both delivering.');
  } else if (nSlot > 0n) {
    console.log('  VERDICT: ⚠ stream is LIVE (slots flow) but ACCOUNT updates = 0 → subscription/filter issue, NOT a ban.');
  } else {
    console.log('  VERDICT: ❌ stream SILENT (0 slots too) — connection up but no data → rate-limit penalty box still active.');
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
