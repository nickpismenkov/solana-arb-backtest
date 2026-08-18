// Port of src/grpc.rs
//
// Yellowstone gRPC account-subscription feed → price Ticks. The swappable
// measurement feed (a ShredStream feed will emit the same Tick later). Sends
// ticks over an unbounded channel so the harness owns the detector loop.

import bs58 from 'bs58';
import type { Tick } from './detector.js';
import { orcaPrice, pair, rayClmmPrice } from './pools.js';

function nowMs(): number {
  return Date.now();
}

interface RawAccountUpdate {
  slot: number;
  pubkey: Uint8Array;
  data: Uint8Array;
}

/**
 * Opens the Yellowstone Geyser `subscribe` stream (accounts filter on the
 * Orca + Raydium pools) and yields raw account updates.
 *
 * NOT IMPLEMENTED: requires the Yellowstone Geyser .proto definitions
 * (yellowstone-grpc-client / yellowstone-grpc-proto in the Rust source),
 * which are not vendored in this repo and unreachable offline in this
 * sandbox, so no working gRPC stub can be generated.
 */
async function* subscribeGeyserAccounts(
  _endpoint: string,
  _xToken: string,
): AsyncGenerator<RawAccountUpdate> {
  throw new Error(
    'not implemented: requires the Yellowstone Geyser .proto definitions, unavailable in this sandbox',
  );
}

export async function runGrpcFeed(
  endpoint: string,
  xToken: string,
  tx: (t: Tick) => void,
): Promise<void> {
  for await (const acc of subscribeGeyserAccounts(endpoint, xToken)) {
    const slot = acc.slot;
    const pk = bs58.encode(acc.pubkey);
    let venue: string;
    let price: number | undefined;
    if (pk === pair().orcaPool) {
      venue = 'Orca';
      price = orcaPrice(acc.data);
    } else if (pk === pair().rayPool) {
      venue = 'Raydium';
      price = rayClmmPrice(acc.data);
    } else {
      continue;
    }
    if (price !== undefined) {
      tx({ venue, price, slot, tsMs: nowMs() });
    }
  }
}
