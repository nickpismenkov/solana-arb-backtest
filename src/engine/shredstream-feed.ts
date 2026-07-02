// Leg 1 — ShredStream trigger feed (TS, shredstream.com JS SDK). Emits a
// Trigger the instant a swap touches one of our pools = "how fast WE would have
// seen the dislocating swap." Prices / arb open-close come from the gRPC account
// feed; the shadow harness correlates the two. Runs on the co-located box.

import { ShredListener } from 'shredstream';
import { VersionedTransaction } from '@solana/web3.js';
import bs58 from 'bs58';

const ORCA_POOL = 'Czfq3xZZDmsdGdUyrNLtRhGc47cXcZtLG4crryfu44zE';
// Raydium CLMM SOL/USDC — the pool we price over gRPC (must match grpc-feed).
const RAY_POOL = '3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv';

export interface Trigger {
  venue: string;
  slot: number;
  tsMs: number; // taken at receipt, before downstream work
  sig: string;
}

// NOTE: staticAccountKeys covers direct swaps (pool in the message). v0 txns
// that reference the pool only via an Address Lookup Table won't match without
// ALT resolution — a refinement once we see real hit rates on the box.
export async function startShredstreamFeed(
  onTrigger: (t: Trigger) => void,
  port = Number(process.env.SHREDSTREAM_PORT || 20000),
): Promise<() => void> {
  const listener = ShredListener.bind(port);
  let stopped = false;
  let seen = 0;
  let hits = 0;

  (async () => {
    for await (const batch of listener as AsyncIterable<{ slot: bigint; transactions: Buffer[] }>) {
      if (stopped) break;
      const tsMs = Date.now();
      const slot = Number(batch.slot);
      for (const raw of batch.transactions) {
        seen++;
        let tx: VersionedTransaction;
        try {
          tx = VersionedTransaction.deserialize(new Uint8Array(raw));
        } catch {
          continue; // partial/undecodable shred payload
        }
        const keys = tx.message.staticAccountKeys.map((k) => k.toBase58());
        const venue = keys.includes(ORCA_POOL) ? 'Orca' : keys.includes(RAY_POOL) ? 'Raydium' : null;
        if (!venue) continue;
        hits++;
        onTrigger({ venue, slot, tsMs, sig: tx.signatures[0] ? bs58.encode(tx.signatures[0]) : '' });
      }
    }
  })().catch((e) => console.error('[shredstream] loop error:', e instanceof Error ? e.message : e));

  // Periodic heartbeat so we can confirm the feed is alive + hit rate.
  const hb = setInterval(() => console.error(`[shredstream] txns seen=${seen} pool-hits=${hits}`), 10_000);

  return () => {
    stopped = true;
    clearInterval(hb);
  };
}
