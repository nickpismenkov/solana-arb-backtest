// Port of src/shredstream.rs
//
// Leg-1 ShredStream trigger feed (shredstream.com pure-Rust SDK). Emits a
// Trigger the instant a swap touches one of our pools = how fast WE'd see the
// dislocating swap. The reassembly loop is blocking, so it runs on its own
// event loop tick and sends Triggers over a callback; prices/arb come from
// the gRPC (later: shred-sourced) feed and the harness correlates the two.
//
// NOTE: matches pools via static account keys — v0 txns that reference a pool
// only through an Address Lookup Table won't match until ALT resolution lands
// (watch the pool-hit heartbeat vs txns-seen to gauge the gap).

import dgram from 'node:dgram';
import { PublicKey, type VersionedTransaction } from '@solana/web3.js';
import { decodeSwaps, AltCache, type SwapInfo } from './decode.js';
import { pair } from './pools.js';

export interface Trigger {
  venue: string;
  slot: number;
  tsMs: number; // stamped at receipt, before downstream work
  sig: string;
  raw: Uint8Array; // serialized victim tx (for co-bundle [victim, arb])
  swaps: SwapInfo[]; // decoded direct swaps on our pools (empty if routed/CPI)
}

function nowMs(): number {
  return Date.now();
}

interface ReassembledTxn {
  message: any; // VersionedMessage
  signatures: string[];
  raw: Uint8Array; // serialized tx bytes
}

/**
 * Parses a bound UDP datagram into a reassembled `(slot, transactions)` pair.
 *
 * NOT IMPLEMENTED: requires shredstream.com's pure-Rust UDP SDK (the
 * `shredstream` crate) for raw Solana shred reassembly, which has no TS
 * equivalent available offline in this sandbox.
 */
function decodeShredDatagram(_msg: Buffer): { slot: number; txns: ReassembledTxn[] } {
  throw new Error(
    'not implemented: requires shredstream.com raw shred-reassembly SDK, unavailable in this sandbox',
  );
}

/**
 * Binds the ShredStream UDP listener. Logs a `txns seen / pool-hits`
 * heartbeat every ~10s.
 *
 * If `rpc` is set, ALTs are resolved so pool touches referenced via lookup
 * tables are caught too (essential — ~all routed swaps use ALTs). If
 * undefined, falls back to a static-key match (misses routed swaps).
 */
export function runShredstreamFeed(port: number, rpc: string | undefined, tx: (t: Trigger) => void): void {
  const pools: [PublicKey, string][] = [
    [new PublicKey(pair().orcaPool), 'Orca'],
    [new PublicKey(pair().rayPool), 'Raydium'],
  ];

  const alt = rpc !== undefined ? new AltCache(rpc) : undefined;
  const socket = dgram.createSocket('udp4');

  socket.on('error', (e) => {
    console.error(`[shredstream] bind udp/${port} failed: ${e}`);
    socket.close();
  });

  let seen = 0;
  let hits = 0;
  let lastHb = Date.now();

  socket.on('message', (msg) => {
    const { slot, txns } = decodeShredDatagram(msg);
    const tsMs = nowMs();
    for (const txn of txns) {
      seen += 1;
      let venue: string | undefined;
      if (alt !== undefined) {
        venue = alt.touchesPool(txn.message, pools);
      } else {
        const keys: PublicKey[] = txn.message.staticAccountKeys();
        venue = pools.find(([p]) => keys.some((k) => k.equals(p)))?.[1];
      }
      if (venue === undefined) continue;
      hits += 1;
      // Pool-hit (rare vs total txns) → do the expensive work here: fully
      // resolve keys and decode direct swaps to attach to the trigger, so
      // the executor's hot path does zero RPC/resolution. Routed/CPI swaps
      // decode to empty (not co-bundlable).
      // decodeShredDatagram is unimplemented (see above), so `txn` is never a
      // real reassembled transaction at runtime; the cast just satisfies
      // decodeSwaps's VersionedTransaction parameter type until it is.
      const versionedTxn = txn as unknown as VersionedTransaction;
      let swaps: SwapInfo[];
      if (alt !== undefined) {
        const keys = alt.resolveKeys(txn.message);
        swaps = keys !== undefined ? decodeSwaps(versionedTxn, keys) : [];
      } else {
        swaps = decodeSwaps(versionedTxn, txn.message.staticAccountKeys());
      }
      const sig = txn.signatures[0] ?? '';
      tx({ venue, slot, tsMs, sig, raw: txn.raw, swaps });
    }
    if (Date.now() - lastHb >= 10_000) {
      console.error(`[shredstream] txns seen=${seen} pool-hits=${hits}`);
      lastHb = Date.now();
    }
  });

  socket.bind(port, () => {
    console.error(`[shredstream] listening on udp/${port} (ALT resolution: ${alt !== undefined ? 'on' : 'off (static-key only)'})`);
  });
}
