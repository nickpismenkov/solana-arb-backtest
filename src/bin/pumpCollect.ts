// Port of src/bin/pump_collect.rs
//
// pump_collect — real-time, read-only recorder of every pump.fun bonding-curve
// event (token create / buy / sell / migrate) to `runs/pump/events.jsonl`.
//
// ── Transport ────────────────────────────────────────────────────────────────
// **Helius WebSocket `logsSubscribe`** with a `mentions:[pump_program]` filter,
// at `processed` commitment. Chosen because:
//   * It is real-time (sub-second) and works with the standard Helius API key
//     in `.env` (`HELIUS_RPC` → derive the `wss://` host). No gRPC/Laserstream
//     add-on or separate credential is required. (The repo's `GRPC_ENDPOINT`
//     is a Tatum gateway, not a Helius Yellowstone stream, so we do not depend
//     on it here.)
//   * pump emits every event as an anchor self-CPI **`Program data:` log
//     blob**, so the log stream alone carries the full structured payload
//     (mint, amounts, reserves, dev, …) — no second `getTransaction`
//     round-trip, hence lowest latency. See `../lib/pump.js` for the verified
//     layouts.
//
// Trade-off / caveat: `logsSubscribe` can, under load, drop or lag; it is the
// standard-tier tool though, and for a measurement collector completeness is
// "best effort, high coverage" rather than "every single tx". If a run needs
// guaranteed completeness, back-fill later with getSignaturesForAddress. This
// binary favours latency + simplicity, which is what Phase-1 recon needs.
//
// Robustness: reconnects with capped backoff on any drop, flushes each event
// to disk immediately, prints a heartbeat every 10s (events/sec, launches
// seen, migrations seen). It NEVER signs or submits a transaction.
//
// Usage: `HELIUS_RPC=<url> tsx src/bin/pumpCollect.ts`
//        env: PUMP_WS (override ws url), PUMP_OUT (override output path).
//
// WebSocket client: the Rust binary uses `tokio_tungstenite` + `futures`.
// This port uses the `ws` package (already a dependency of ts-port, see
// package.json and ../lib/pyth.ts for the established usage pattern) rather
// than Node's built-in global `WebSocket`, since `ws` gives us ping/pong
// frame control and a Node `EventEmitter`-style API that maps directly onto
// the Rust `tokio_tungstenite` message loop below.

import 'dotenv/config';
import * as fs from 'node:fs';
import * as path from 'node:path';
import WebSocket from 'ws';
import {
  bondingCurvePda,
  MIGRATION_AUTHORITY,
  parseProgramDataB64,
  priceInSol,
  PUMP_PROGRAM,
  PUMP_TOKEN_DECIMALS,
  pumpEventKind,
  type PumpEvent,
} from '../lib/pump.js';

function nowMs(): number {
  return Date.now();
}

/** Derive the Helius `wss://` endpoint from the `https://` RPC url (keeps the
 * `?api-key=…`). Override entirely with `PUMP_WS`. */
function wsUrl(): string {
  const override = process.env.PUMP_WS;
  if (override !== undefined) return override;
  const http = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (http === undefined) throw new Error('set HELIUS_RPC (or RPC_HTTP)');
  return http.replace('https://', 'wss://').replace('http://', 'ws://');
}

interface Counts {
  total: number;
  creates: number;
  buys: number;
  sells: number;
  migrates: number;
}

function newCounts(): Counts {
  return { total: 0, creates: 0, buys: 0, sells: 0, migrates: 0 };
}

async function main(): Promise<void> {
  const outPath = process.env.PUMP_OUT ?? 'runs/pump/events.jsonl';
  const dir = path.dirname(outPath);
  if (dir !== '') {
    fs.mkdirSync(dir, { recursive: true });
  }

  // Node's fs has no direct equivalent of Rust's `File::try_lock` (advisory
  // exclusive lock) in the standard `fs` API. We approximate the "refuse to
  // double-write" invariant by opening the file exclusive-create-or-append
  // via a sentinel lockfile next to the output, which is the closest portable
  // primitive Node offers without a native addon.
  const lockPath = `${outPath}.lock`;
  let lockFd: number;
  try {
    lockFd = fs.openSync(lockPath, 'wx');
  } catch (e) {
    console.error(
      `[pump_collect] ${outPath} is locked by another pump_collect (${lockPath} exists); refusing to double-write. exiting.`,
    );
    process.exit(1);
  }
  const releaseLock = (): void => {
    try {
      fs.closeSync(lockFd);
      fs.unlinkSync(lockPath);
    } catch {
      // best-effort cleanup
    }
  };
  process.on('exit', releaseLock);
  process.on('SIGINT', () => {
    releaseLock();
    process.exit(0);
  });
  process.on('SIGTERM', () => {
    releaseLock();
    process.exit(0);
  });

  const fd = fs.openSync(outPath, 'a');

  console.error(`[pump_collect] program ${PUMP_PROGRAM}`);
  console.error(`[pump_collect] appending events to ${outPath}`);
  console.error('[pump_collect] transport: Helius WebSocket logsSubscribe (read-only)');

  const counts = newCounts();
  let backoff = 500;

  for (;;) {
    try {
      await runOnce(fd, counts);
      console.error('[pump_collect] stream closed cleanly; reconnecting');
      backoff = 500;
    } catch (e) {
      console.error(`[pump_collect] error: ${String(e)}; reconnecting in ${backoff}ms`);
      await sleep(backoff);
      backoff = Math.min(backoff * 2, 15_000);
    }
  }
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/** One connection lifecycle: connect, subscribe, drain notifications until the
 * socket drops. Resolves on clean close, rejects on any failure (→ reconnect). */
function runOnce(fd: number, counts: Counts): Promise<void> {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(wsUrl());

    let hbTimer: ReturnType<typeof setInterval> | undefined;
    let lastTotal = counts.total;
    let lastAt = Date.now();
    let settled = false;

    const finishOk = (): void => {
      if (settled) return;
      settled = true;
      if (hbTimer !== undefined) clearInterval(hbTimer);
      resolve();
    };
    const finishErr = (e: unknown): void => {
      if (settled) return;
      settled = true;
      if (hbTimer !== undefined) clearInterval(hbTimer);
      reject(e instanceof Error ? e : new Error(String(e)));
    };

    ws.on('open', () => {
      const sub = {
        jsonrpc: '2.0',
        id: 1,
        method: 'logsSubscribe',
        params: [{ mentions: [PUMP_PROGRAM] }, { commitment: 'processed' }],
      };
      ws.send(JSON.stringify(sub));
      console.error('[pump_collect] subscribed; waiting for events…');

      hbTimer = setInterval(() => {
        const dt = Math.max((Date.now() - lastAt) / 1000, 1e-9);
        const rate = (counts.total - lastTotal) / dt;
        console.error(
          `[pump_collect] hb: ${rate.toFixed(0)} ev/s | total ${counts.total} (create ${counts.creates}, buy ${counts.buys}, sell ${counts.sells}, migrate ${counts.migrates})`,
        );
        lastTotal = counts.total;
        lastAt = Date.now();
      }, 10_000);
    });

    ws.on('message', (data: WebSocket.Data) => {
      const text = typeof data === 'string' ? data : data.toString();
      handleNotification(text, fd, counts);
    });

    ws.on('ping', (data: Buffer) => {
      ws.pong(data);
    });

    ws.on('close', () => {
      finishOk();
    });

    ws.on('error', (e: Error) => {
      finishErr(e);
    });
  });
}

/** Parse one `logsNotification` frame, decode any pump event blobs in it, and
 * append a JSONL record per decoded event. */
function handleNotification(text: string, fd: number, counts: Counts): void {
  let v: any;
  try {
    v = JSON.parse(text);
  } catch {
    return;
  }
  const result = v?.params?.result;
  if (result === undefined || result === null) return; // subscription ack or unrelated frame
  const slot: number = typeof result.context?.slot === 'number' ? result.context.slot : 0;
  const value = result.value;
  // Skip failed txs — a reverted tx's event logs would be misleading.
  if (value === undefined || value === null || value.err !== null) return;
  const signature: string = typeof value.signature === 'string' ? value.signature : '';
  const logs: unknown[] | undefined = Array.isArray(value.logs) ? value.logs : undefined;
  if (logs === undefined) return;

  const ts = nowMs();
  for (const line of logs) {
    if (typeof line !== 'string') continue;
    const prefix = 'Program data: ';
    if (!line.startsWith(prefix)) continue;
    const b64 = line.slice(prefix.length);
    const ev = parseProgramDataB64(b64);
    if (ev === null) continue;
    const rec = toRecord(ev, ts, slot, signature);
    counts.total += 1;
    switch (pumpEventKind(ev)) {
      case 'create':
        counts.creates += 1;
        break;
      case 'buy':
        counts.buys += 1;
        break;
      case 'sell':
        counts.sells += 1;
        break;
      case 'migrate':
        counts.migrates += 1;
        break;
    }
    // One write per record, matching the Rust `write_all` of a single
    // pre-joined string: writing line-by-line with a buffered call avoids
    // interleaving fragments if writers ever overlap.
    const out = `${JSON.stringify(rec)}\n`;
    try {
      fs.writeSync(fd, out);
    } catch {
      // best-effort; do not crash the collector on a transient write error
    }
  }
}

/** Build the JSONL record for one event. Fields common to all: unix_ms, slot,
 * signature, event_type, mint, bonding_curve, actor, sol_amount, token_amount. */
function toRecord(ev: PumpEvent, ts: number, slot: number, sig: string): Record<string, unknown> {
  const rec: Record<string, unknown> = {
    unix_ms: ts,
    slot,
    signature: sig,
    event_type: pumpEventKind(ev),
  };
  switch (ev.kind) {
    case 'Create': {
      const c = ev.value;
      rec.mint = c.mint.toString();
      rec.bonding_curve = c.bondingCurve.toString();
      rec.actor = c.user.toString();
      rec.dev = c.user.toString();
      rec.creator = c.creator.toString();
      rec.block_time = Number(c.timestamp);
      rec.name = c.name;
      rec.symbol = c.symbol;
      rec.uri = c.uri;
      rec.sol_amount = 0;
      rec.token_amount = 0;
      rec.init_virtual_sol_reserves = Number(c.virtualSolReserves);
      rec.init_virtual_token_reserves = Number(c.virtualTokenReserves);
      rec.init_real_token_reserves = Number(c.realTokenReserves);
      rec.token_total_supply = Number(c.tokenTotalSupply);
      break;
    }
    case 'Trade': {
      const t = ev.value;
      rec.mint = t.mint.toString();
      rec.bonding_curve = bondingCurvePda(t.mint).toString();
      rec.actor = t.user.toString();
      if (t.creator !== null) rec.creator = t.creator.toString();
      rec.block_time = Number(t.timestamp);
      rec.sol_amount = Number(t.solAmount);
      rec.token_amount = Number(t.tokenAmount);
      rec.virtual_sol_reserves = Number(t.virtualSolReserves);
      rec.virtual_token_reserves = Number(t.virtualTokenReserves);
      rec.real_sol_reserves = Number(t.realSolReserves);
      rec.real_token_reserves = Number(t.realTokenReserves);
      rec.price_in_sol = priceInSol(t.virtualSolReserves, t.virtualTokenReserves, PUMP_TOKEN_DECIMALS);
      break;
    }
    case 'Migrate': {
      const m = ev.value;
      rec.mint = m.mint.toString();
      rec.bonding_curve = bondingCurvePda(m.mint).toString();
      rec.actor = MIGRATION_AUTHORITY;
      rec.sol_amount = 0;
      rec.token_amount = 0;
      break;
    }
  }
  return rec;
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
