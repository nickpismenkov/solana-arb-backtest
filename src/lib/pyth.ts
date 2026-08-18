// Port of src/pyth.rs
//
// Pyth Lazer ("Pyth Pro") price feed — a reconnecting WebSocket client that
// streams live prices into a shared in-memory table. This is the fast trigger
// + fair-value source for the liquidation engine: Lazer delivers ms-grade
// updates so we can PRE-BUILD a liquidation the instant a price approaches the
// threshold, then fire the moment it crosses.
//
// Auth: Bearer token via PYTH_LAZER_TOKEN (never hardcode; .env only).
// Endpoint + subscribe shape are VERIFIED live against the SOL/USDC feeds.
//
// Feeds are numeric IDs (SOL=6, USDC=7, BTC=1, ETH=2). The parsed message
// carries the raw integer price; the exponent is per-feed static (all the
// crypto majors are -8). We request "exponent" as a property and fall back to
// -8 if the server omits it.

import WebSocket from 'ws';

/**
 * Pyth Lazer WebSocket endpoint. Default is the Cloudflare-anycast host
 * (pyth-lazer.dourolabs.app) which routes to the nearest edge — measured ~2.5x
 * faster to connect than the pinned pyth-lazer-0 origin. Override with
 * LAZER_URL; measure `curl -w %{time_connect}` to each candidate from the box
 * and pick the lowest. (Also available: pyth-lazer-0/-1 direct origins.)
 */
export function lazerUrl(): string {
  return process.env.LAZER_URL ?? 'wss://pyth-lazer.dourolabs.app/v1/stream';
}

/**
 * Lazer delivery channel. Default `real_time` = lowest latency (each update
 * pushed the instant it's computed, 1-50ms), vs `fixed_rate@50ms` which only
 * snapshots every 50ms and adds up to that much batching latency to detect_lag.
 * Override with LAZER_CHANNEL (e.g. `fixed_rate@1ms`, `fixed_rate@50ms`).
 */
export function lazerChannel(): string {
  return process.env.LAZER_CHANNEL ?? 'real_time';
}

const DEFAULT_EXPONENT = -8;

/** A single price observation, already scaled to a real number (price * 10^exp). */
export interface PricePoint {
  price: number;
  /** Lazer publish timestamp, microseconds since epoch (0 if absent). */
  tsUs: number;
}

/** Shared map feed_id -> latest price. */
export type PriceTable = Map<number, PricePoint>;

export function newTable(): PriceTable {
  return new Map();
}

/** Read the latest price for a feed, if we've received one. */
export function get(table: PriceTable, feedId: number): PricePoint | undefined {
  return table.get(feedId);
}

/**
 * Spawn the Lazer feed as a background task. Reconnects forever on drop/error.
 * The returned table is updated in place as prices arrive.
 */
export function spawnLazer(token: string, feedIds: number[], table: PriceTable): void {
  void (async () => {
    let backoffMs = 500;
    for (;;) {
      try {
        await run(token, feedIds, table);
        // Clean close — reconnect promptly.
        backoffMs = 500;
      } catch (e) {
        console.error(`[pyth-lazer] ${e}; reconnecting in ${backoffMs}ms`);
      }
      await new Promise((resolve) => setTimeout(resolve, backoffMs));
      backoffMs = Math.min(backoffMs * 2, 10_000);
    }
  })();
}

/**
 * One connection lifecycle: connect, subscribe, pump updates into the table.
 * Resolves on a clean server close, rejects on any failure (-> reconnect).
 */
function run(token: string, feedIds: number[], table: PriceTable): Promise<void> {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(lazerUrl(), {
      headers: { Authorization: `Bearer ${token}` },
    });

    ws.on('open', () => {
      const sub = {
        type: 'subscribe',
        subscriptionId: 1,
        priceFeedIds: feedIds,
        properties: ['price', 'exponent'],
        formats: [],
        channel: lazerChannel(),
        deliveryFormat: 'json',
      };
      ws.send(JSON.stringify(sub));
    });

    ws.on('message', (data: WebSocket.Data) => {
      if (typeof data !== 'string' && !Buffer.isBuffer(data)) return;
      applyUpdate(data.toString(), table);
    });

    ws.on('ping', (data) => {
      ws.pong(data);
    });

    ws.on('close', () => {
      resolve();
    });

    ws.on('error', (e) => {
      reject(e);
    });
  });
}

/**
 * Parse a Lazer text frame and update the table. Ignores non-price frames
 * (e.g. the initial {"type":"subscribed"} ack) — EXCEPT subscription errors,
 * which must be loud: a rejected subscription (e.g. a feed that doesn't
 * support the channel) otherwise looks exactly like a dead-calm market with
 * 0/N feeds live.
 */
function applyUpdate(text: string, table: PriceTable): void {
  let v: any;
  try {
    v = JSON.parse(text);
  } catch {
    return;
  }
  const t = typeof v?.type === 'string' ? v.type : undefined;
  if (t !== undefined && (t.toLowerCase() === 'subscriptionerror' || t.toLowerCase() === 'error')) {
    console.error(`[pyth-lazer] SUBSCRIPTION REJECTED: ${text} — no prices will flow; fix the feed list`);
    return;
  }
  const parsed = v?.parsed ?? {};
  const feeds = parsed?.priceFeeds;
  if (!Array.isArray(feeds)) return;

  // timestampUs may arrive as a string or a number depending on delivery.
  let tsUs = 0;
  const rawTs = parsed?.timestampUs;
  if (typeof rawTs === 'string') {
    const n = Number.parseInt(rawTs, 10);
    if (Number.isFinite(n)) tsUs = n;
  } else if (typeof rawTs === 'number') {
    tsUs = rawTs;
  }

  for (const f of feeds) {
    const id = f?.priceFeedId;
    if (typeof id !== 'number') continue;
    // price arrives as a string integer (may also be numeric).
    let raw: number | undefined;
    if (typeof f?.price === 'string') {
      const n = Number.parseFloat(f.price);
      raw = Number.isFinite(n) ? n : undefined;
    } else if (typeof f?.price === 'number') {
      raw = f.price;
    }
    if (raw === undefined) continue;
    const exp = typeof f?.exponent === 'number' ? f.exponent : DEFAULT_EXPONENT;
    const price = raw * 10 ** exp;
    table.set(id, { price, tsUs });
  }
}
