// Leg 1 — ShredStream trigger feed (TS). Consumes shredstream.com's JS SDK
// (decoded transactions over UDP on port 20000) and emits a Trigger the instant
// a swap touches one of our pools — i.e. "how fast WE would have seen the
// dislocating swap." This is the fast leg; prices/arb-open-close come from the
// gRPC account feed. The shadow harness correlates the two.
//
// ⚠ SKELETON — finalize two things against shredstream.com's actual JS SDK
// (grab the JavaScript example from the dashboard's language selector):
//   1) the import + how you create/iterate the listener (mirrors their Rust
//      `ShredListener::bind(port)` → `listener.transactions()`)
//   2) how each decoded tx exposes its account keys (the Rust snippet only
//      showed `tx.signatures[0]`)

const ORCA_POOL = 'Czfq3xZZDmsdGdUyrNLtRhGc47cXcZtLG4crryfu44zE';
const RAY_POOL = '58oQChx4yWmvKdwLLZzBi4ChoCc2fqCUWBkwMihLYQo2';
const VENUE: Record<string, string> = { [ORCA_POOL]: 'Orca', [RAY_POOL]: 'Raydium' };

export interface Trigger {
  venue: string;
  slot: number;
  tsMs: number; // taken at receipt, before any downstream work
  sig: string;
}

// Shape we expect from the SDK per decoded transaction. Adjust field access in
// `accountKeysOf` once we see the real JS SDK types.
interface DecodedTx {
  signatures: string[];
  // e.g. message.accountKeys / staticAccountKeys — confirm on box
  [k: string]: unknown;
}

function accountKeysOf(tx: DecodedTx): string[] {
  // TODO(on-box): replace with the real accessor from shredstream.com's JS SDK.
  const msg = (tx as { message?: { accountKeys?: unknown; staticAccountKeys?: unknown } }).message;
  const keys = (msg?.accountKeys ?? msg?.staticAccountKeys ?? []) as unknown[];
  return keys.map((k) => String(k));
}

export async function startShredstreamFeed(
  onTrigger: (t: Trigger) => void,
  port = Number(process.env.SHREDSTREAM_PORT || 20000),
): Promise<() => void> {
  // TODO(on-box): swap for the real shredstream.com JS SDK, e.g.
  //   const { ShredListener } = await import('shredstream');
  //   const listener = ShredListener.bind(port);
  //   for await (const { slot, transactions } of listener.transactions()) { ... }
  // The loop body below is the real, reusable logic:
  const handle = (slot: number, transactions: DecodedTx[]) => {
    const tsMs = Date.now();
    for (const tx of transactions) {
      const keys = accountKeysOf(tx);
      const venue = keys.includes(ORCA_POOL)
        ? VENUE[ORCA_POOL]
        : keys.includes(RAY_POOL)
          ? VENUE[RAY_POOL]
          : null;
      if (venue) onTrigger({ venue, slot, tsMs, sig: tx.signatures?.[0] ?? '' });
    }
  };
  void handle; // wired to the SDK loop on the box
  console.log(`[shredstream-feed] skeleton ready on udp/${port} — plug in the JS SDK loop on the box.`);
  throw new Error('shredstream JS SDK not wired yet — paste the dashboard JS example and I finalize this adapter.');
}
