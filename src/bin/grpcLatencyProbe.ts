// Port of src/bin/grpc_latency_probe.rs
//
// Measure the REAL freshness of the Yellowstone gRPC account stream — the thing
// that decides whether streaming can replace hot-path RPC polling for the
// liquidation fire loop. Subscribes to marginfi program account updates + slot
// updates, and for each account update at slot S computes the lag against the
// latest tip slot we've seen (lag 0-1 = we get updates as blocks are produced;
// lag 3+ ≈ >1s behind = too slow to fire competitively).
//
// Usage: GRPC_ENDPOINT=<url> GRPC_X_TOKEN=<tok> [SECS=30] tsx src/bin/grpcLatencyProbe.ts

const MARGINFI_PROGRAM = 'MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA';
void MARGINFI_PROGRAM;

interface AccountUpdate {
  slot: number;
}

/**
 * Opens the Yellowstone Geyser `subscribe` stream with an ACCOUNTS filter on
 * `watch` and yields each account update's slot.
 *
 * NOT IMPLEMENTED: requires the Yellowstone Geyser .proto definitions
 * (yellowstone-grpc-client / yellowstone-grpc-proto in the Rust source),
 * which are not vendored in this repo and unreachable offline in this
 * sandbox, so no working gRPC stub can be generated.
 */
async function* subscribeAccountUpdates(
  _endpoint: string,
  _xToken: string,
  _watch: string[],
): AsyncGenerator<AccountUpdate> {
  throw new Error(
    'not implemented: requires the Yellowstone Geyser .proto definitions, unavailable in this sandbox',
  );
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function main(): Promise<void> {
  const endpoint = process.env.GRPC_ENDPOINT;
  if (endpoint === undefined) throw new Error('GRPC_ENDPOINT');
  const xToken = process.env.GRPC_X_TOKEN;
  if (xToken === undefined) throw new Error('GRPC_X_TOKEN');
  const rpc = process.env.HELIUS_RPC;
  if (rpc === undefined) throw new Error('HELIUS_RPC');
  const secs = Number.parseInt(process.env.SECS ?? '', 10) || 30;

  // Independent tip-slot reference: poll getSlot(processed) via RPC on a
  // background loop so lag = rpc_tip − gRPC_account_slot is an ABSOLUTE
  // latency (not a self-referential max-seen proxy). A stream FRESHER than
  // RPC yields lag ≤ 0.
  let tip = 0;
  let tipPolling = true;
  const tipLoop = (async (): Promise<void> => {
    while (tipPolling) {
      try {
        const res = await fetch(rpc, {
          method: 'POST',
          headers: { 'content-type': 'application/json' },
          body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'getSlot', params: [{ commitment: 'processed' }] }),
        });
        const v: any = await res.json();
        if (typeof v?.result === 'number') tip = v.result;
      } catch {
        // ignore
      }
      await sleep(400);
    }
  })();

  let endpointHost = '?';
  const parts = endpoint.split('/');
  if (parts.length > 2) endpointHost = parts[2];
  console.error(`[grpc] connecting to ${endpointHost} …`);
  const tConnect = Date.now();

  // Tatum's gateway tier appears to reject owner (program-wide) subscriptions,
  // so subscribe to specific high-activity accounts — marginfi USDC + BONK
  // banks (update on every deposit/borrow/interest tick). ACCOUNTS env
  // overrides with a comma-separated list.
  const watch: string[] = process.env.ACCOUNTS
    ? process.env.ACCOUNTS.split(',').map((x) => x.trim())
    : [
        '2s37akK2eyBbp8DZgCm7RtsaEz8eJP3Nxd4urLHQv7yB', // marginfi USDC bank
        'DeyH7QxWvnbbaVB4zFrf4hoq7Q8z1ZT14co42BGwGtfM', // marginfi BONK bank
        'CCKtUs6Cgwo4aaQUmBPmyoApH2gUDErxNZCAntD6LYGh', // marginfi wSOL bank
      ];
  console.error(`[grpc] watching ${watch.length} specific accounts`);

  let acctUpdates = 0;
  const lags: number[] = [];
  const deadline = Date.now() + secs * 1000;

  try {
    const stream = subscribeAccountUpdates(endpoint, xToken, watch);
    console.error(`[grpc] connected in ${Date.now() - tConnect}ms`);
    console.error(`[grpc] subscribed (marginfi accounts, processed). measuring ${secs}s …\n`);
    while (Date.now() < deadline) {
      const { value, done } = await stream.next();
      if (done) break;
      acctUpdates += 1;
      const t = tip;
      if (t > 0) lags.push(t - value.slot);
    }
  } catch (e) {
    console.error(`[grpc] stream error: ${e}`);
  } finally {
    tipPolling = false;
  }
  await tipLoop;

  lags.sort((a, b) => a - b);
  const n = lags.length;
  const med = n > 0 ? lags[Math.floor(n / 2)] : 0;
  const p90 = n > 0 ? lags[Math.min(Math.floor((n * 9) / 10), n - 1)] : 0;
  const best = n > 0 ? lags[0] : 0;
  console.log(`═══ gRPC stream freshness (Tatum, ${secs}s) ═══`);
  console.log(`  account updates: ${acctUpdates}  (${(acctUpdates / secs).toFixed(0)}/s)`);
  console.log(`  slot lag (RPC_tip − gRPC_account_slot): median ${med}, p90 ${p90}, best ${best}  [≤1=fresh, 3+=slow]`);
  console.log('  (note: RPC tip itself lags ~1 slot, so lag ~0-1 means gRPC keeps pace with the chain)');
  console.log(`  → ~${(med * 400).toFixed(0)}ms median staleness (at ~400ms/slot)`);
  if (med <= 1) {
    console.log('  VERDICT: FRESH — stream keeps pace with block production. Good enough to fire on.');
  } else if (med <= 2) {
    console.log('  VERDICT: OK — ~1 slot behind, usable.');
  } else {
    console.log(`  VERDICT: SLOW — ${med} slots behind, too stale to fire competitively; need a better provider.`);
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
