// Port of src/pyth_accumulator.rs
//
// Parse Pyth's Hermes "accumulator update" blob into the pieces the on-chain
// crank consumes: the Wormhole VAA (guardian-signed merkle root) and, per
// feed, the price MESSAGE + its merkle PROOF. This is the data layer of the
// self-crank pipeline (step 3): Hermes gives us the signed update; the VAA
// goes to the Wormhole-verify program, and each {message, proof} goes to the
// Pyth push wrapper which writes the sponsored feed marginfi reads.
//
// Format (AccumulatorUpdateData, VERIFIED against a live SOL update — the split
// matches the ~247B VAA / ~396B per-update seen in a real mainnet crank tx):
//   magic "PNAU" (0x504e4155) . major u8 . minor u8 .
//   trailing_hdr_len u8 + that many bytes .
//   update_type u8 (0 = WormholeMerkle) .
//   vaa_len u16 (BE) + vaa bytes .
//   num_updates u8 .
//   [ message_len u16 (BE) + message . proof (merkle path: u8 count + count x 20B) ] x num_updates
//
// The 32-byte feed id lives inside each message (a PriceFeedMessage: variant
// u8=0, then feed_id[32], ...) so we can match the update to a bank's feed.

const MAGIC = 0x504e_4155; // "PNAU"

export class MerkleUpdate {
  /** The price message (PriceFeedMessage bytes) — variant byte then feed_id@1. */
  message: Buffer;
  /**
   * Merkle proof path: a length-prefixed list of 20-byte hashes, kept as the
   * raw wire bytes (count u8 + count*20) so it re-serializes verbatim.
   */
  proof: Buffer;

  constructor(message: Buffer, proof: Buffer) {
    this.message = message;
    this.proof = proof;
  }

  /** 32-byte Pyth feed id (offset 1: after the 1-byte message variant). */
  feedId(): Buffer | undefined {
    if (this.message.length < 33) return undefined;
    return this.message.subarray(1, 33);
  }

  /**
   * USD price from the PriceFeedMessage (big-endian, like the rest of the
   * accumulator wire format: variant u8, feed_id[32], price i64@33,
   * conf u64@41, expo i32@49). This is EXACTLY the price the crank posts
   * on-chain, so health judged at it predicts the liquidate ix outcome.
   */
  priceUsd(): number | undefined {
    if (this.message.length < 53) return undefined;
    const px = this.message.readBigInt64BE(33);
    const expo = this.message.readInt32BE(49);
    return Number(px) * 10 ** expo;
  }
}

export interface AccumulatorUpdate {
  /** Guardian-signed Wormhole VAA (goes to the verify program). */
  vaa: Buffer;
  updates: MerkleUpdate[];
}

class Cur {
  b: Buffer;
  i: number;

  constructor(b: Buffer) {
    this.b = b;
    this.i = 0;
  }

  u8(): number {
    if (this.i >= this.b.length) throw new Error('eof u8');
    const v = this.b[this.i];
    this.i += 1;
    return v;
  }

  u16be(): number {
    const hi = this.u8();
    const lo = this.u8();
    return (hi << 8) | lo;
  }

  take(n: number): Buffer {
    if (this.i + n > this.b.length) throw new Error(`eof take ${n}`);
    const s = this.b.subarray(this.i, this.i + n);
    this.i += n;
    return s;
  }
}

/** Parse one base64 accumulator blob (as Hermes returns in binary.data[0]). */
export function parseBase64(b64: string): AccumulatorUpdate {
  const bytes = Buffer.from(b64, 'base64');
  return parse(bytes);
}

export function parse(bytes: Buffer): AccumulatorUpdate {
  const c = new Cur(bytes);
  const magic = c.take(4).readUInt32BE(0);
  if (magic !== MAGIC) throw new Error(`bad magic ${magic.toString(16)}`);
  c.u8(); // major
  c.u8(); // minor
  const trailing = c.u8();
  c.take(trailing); // skip trailing header
  const updateType = c.u8();
  if (updateType !== 0) throw new Error(`unsupported update_type ${updateType}`);
  const vaaLen = c.u16be();
  const vaa = Buffer.from(c.take(vaaLen));
  const num = c.u8();
  const updates: MerkleUpdate[] = [];
  for (let k = 0; k < num; k++) {
    const msgLen = c.u16be();
    const message = Buffer.from(c.take(msgLen));
    // proof = count u8 + count x 20-byte hashes (kept as raw wire bytes).
    const proofStart = c.i;
    const count = c.u8();
    c.take(count * 20);
    const proof = Buffer.from(bytes.subarray(proofStart, c.i));
    updates.push(new MerkleUpdate(message, proof));
  }
  return { vaa, updates };
}

/** Fetch the latest signed update for a set of hex feed ids from Hermes. */
export async function fetchHermes(hermes: string, feedIdsHex: string[]): Promise<AccumulatorUpdate> {
  const ids = feedIdsHex.map((f) => `&ids[]=${f}`).join('');
  const url = `${hermes}/v2/updates/price/latest?encoding=base64${ids}`;
  const resp = await fetch(url);
  const v: any = await resp.json();
  const b64 = v?.binary?.data?.[0];
  if (typeof b64 !== 'string') throw new Error(`no binary.data: ${JSON.stringify(v)}`);
  return parseBase64(b64);
}

// -- Hermes cache: keep the latest signed blob hot for the fire path --------

/**
 * Latest Hermes blob for a (settable) feed set, refreshed by a background
 * loop so the crank fire path never waits on an HTTP round-trip. One blob
 * covers the whole set: a single VAA proves every update in it.
 */
export class HermesCache {
  private latestVal: { update: AccumulatorUpdate; t: number } | undefined;
  private feeds: string[];

  constructor(feedIdsHex: string[]) {
    this.latestVal = undefined;
    this.feeds = feedIdsHex;
  }

  /** Latest parsed blob and its age (ms). undefined until the first successful fetch. */
  latest(): { update: AccumulatorUpdate; ageMs: number } | undefined {
    if (this.latestVal === undefined) return undefined;
    return { update: this.latestVal.update, ageMs: Date.now() - this.latestVal.t };
  }

  /** The update for one feed from the latest blob, with the blob's age. */
  updateFor(feedId: Buffer): { update: MerkleUpdate; vaa: Buffer; ageMs: number } | undefined {
    const l = this.latest();
    if (l === undefined) return undefined;
    const mu = l.update.updates.find((u) => {
      const id = u.feedId();
      return id !== undefined && id.equals(feedId);
    });
    if (mu === undefined) return undefined;
    return { update: mu, vaa: l.update.vaa, ageMs: l.ageMs };
  }

  /** Replace the polled feed set (hex ids); takes effect next poll. */
  setFeeds(feedIdsHex: string[]): void {
    this.feeds = feedIdsHex;
  }

  /** @internal */
  getFeeds(): string[] {
    return this.feeds;
  }

  /** @internal */
  setLatest(update: AccumulatorUpdate): void {
    this.latestVal = { update, t: Date.now() };
  }
}

/**
 * Spawn the poll loop. Errors are retried on the next tick; the cache keeps
 * the last good blob (callers gate on age).
 */
export function spawnHermesCache(hermes: string, feedIdsHex: string[], intervalMs: number): HermesCache {
  const cache = new HermesCache(feedIdsHex);
  void (async () => {
    for (;;) {
      const feeds = cache.getFeeds();
      if (feeds.length > 0) {
        try {
          const u = await fetchHermes(hermes, feeds);
          cache.setLatest(u);
        } catch (e) {
          console.error(`[hermes] fetch failed (will retry): ${e}`);
        }
      }
      await new Promise((resolve) => setTimeout(resolve, intervalMs));
    }
  })();
  return cache;
}

/**
 * Fetch the latest signed update for a set of hex feed ids from Hermes.
 * Streaming Hermes: hold ONE SSE connection open and update the cache on every
 * pushed price (latency ~10-30ms vs the 400ms poll), so the blob we crank stays
 * in lockstep with the Lazer trigger. Reconnects on drop / feed change. Feeds are
 * fixed at spawn (our crankable set); set_feeds triggers a reconnect.
 */
export function spawnHermesStream(hermes: string, feedIdsHex: string[]): HermesCache {
  const cache = new HermesCache(feedIdsHex);
  void (async () => {
    for (;;) {
      const feeds = cache.getFeeds();
      if (feeds.length === 0) {
        await new Promise((resolve) => setTimeout(resolve, 500));
        continue;
      }
      const ids = feeds.map((f) => `&ids[]=${f}`).join('');
      const url = `${hermes}/v2/updates/price/stream?encoding=base64${ids}`;
      try {
        // Read timeout catches a stalled stream (Hermes pushes many/sec, so 20s idle = dead) -> reconnect.
        const controller = new AbortController();
        const idleTimeout = () =>
          setTimeout(() => controller.abort(), 20_000);
        let timer = idleTimeout();
        const resp = await fetch(url, { signal: controller.signal });
        if (resp.body === null) throw new Error('no response body');
        const reader = resp.body.getReader();
        const decoder = new TextDecoder();
        let buf = '';
        for (;;) {
          const { done, value } = await reader.read();
          clearTimeout(timer);
          if (done) break;
          timer = idleTimeout();
          buf += decoder.decode(value, { stream: true });
          let nl: number;
          while ((nl = buf.indexOf('\n')) !== -1) {
            const line = buf.slice(0, nl);
            buf = buf.slice(nl + 1);
            if (!line.startsWith('data:')) continue;
            const payload = line.slice('data:'.length).trim();
            if (payload.length === 0) continue;
            try {
              const v = JSON.parse(payload);
              const b64 = v?.binary?.data?.[0];
              if (typeof b64 === 'string') {
                const u = parseBase64(b64);
                cache.setLatest(u);
              }
            } catch {
              // ignore malformed frame
            }
            // feeds changed -> break to reconnect with the new set
            if (JSON.stringify(cache.getFeeds()) !== JSON.stringify(feeds)) break;
          }
          if (JSON.stringify(cache.getFeeds()) !== JSON.stringify(feeds)) break;
        }
        clearTimeout(timer);
      } catch (e) {
        console.error(`[hermes-stream] disconnected (${e}); reconnecting`);
      }
      await new Promise((resolve) => setTimeout(resolve, 300)); // reconnect backoff
    }
  })();
  return cache;
}
