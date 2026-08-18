// Shared control flow for the 5 liq_*_executor binaries (src/bin/liq_executor.rs,
// liq_jupiter_executor.rs, liq_kamino_executor.rs, liq_save_executor.rs,
// liq_stream_executor.rs). Those 5 Rust files (~3800 lines combined) are
// near-duplicate "scan -> compute health -> arm -> simulate -> fire -> log"
// executor loops, one per lending protocol/feed-source (see PLAN.md). This
// module ports the DUPLICATED boilerplate once; the protocol-specific
// decision/arm/fire logic (which accounts to scan, how to size a seize, how to
// build the fire tx) stays in each thin `src/bin/liq*Executor.ts` entrypoint.
//
// What lives here:
//   - now()/nowUs()                    unix seconds / micros
//   - rpc()                            raw JSON-RPC POST w/ retry+backoff
//   - b64()                            pull base64 account data -> Buffer
//   - getMultiple()                    batched getMultipleAccounts
//   - mintOwner()                      resolve a mint's token-program owner (cached)
//   - latestBlockhash()/currentSlot()/solBalance()
//   - simulateTxB64()/simulateBundle()  raw simulate{Transaction,Bundle} helpers
//   - logLatency()                     the raw per-fire latency.jsonl line logger
//     (observe.ts already has logDecision/logTrade — reused as-is by each
//     executor for its own DecisionLog/TradeLog row shape; nothing to add here)
//   - BaseCfg + loadBaseCfg()          the tuning knobs identical across all 5
//   - CrankCtx + loadCrankCtx()        self-crank plumbing shared by
//     liqExecutor/liqSaveExecutor (Hermes cache + Jito tips)
//   - DailyTipBudget                   the daily-tip-cap + wallet-floor guard
//   - runLoop()                        the poll/tick -> handle -> cooldown
//     orchestration shape common to all 5 main loops

import 'dotenv/config';
import * as fs from 'node:fs';
import { Keypair, PublicKey, type VersionedTransaction } from '@solana/web3.js';
import bs58 from 'bs58';
import { getTipAccounts, defaultBlockEngine, sendBundle, sendSender } from './jito.js';
import { realizedUsdc } from './observe.js';
import { spawnHermesCache, type HermesCache } from './pythAccumulator.js';
import { buildCrankTxs } from './pythCrank.js';

export function now(): number {
  return Math.floor(Date.now() / 1000);
}

export function nowUs(): number {
  return Date.now() * 1000;
}

export function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/** Raw JSON-RPC POST with 4x exponential-backoff retry (400ms << attempt). */
export async function rpc(endpoint: string, body: unknown): Promise<any | undefined> {
  for (let attempt = 0; attempt < 4; attempt++) {
    try {
      const res = await fetch(endpoint, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(body),
      });
      return await res.json();
    } catch {
      // fall through to retry
    }
    await sleep(400 << attempt);
  }
  return undefined;
}

/** Pull the base64 account-data string out of an RPC JSON `data` field -> Buffer. */
export function b64(data: any): Buffer | undefined {
  const s = data?.[0];
  if (typeof s !== 'string') return undefined;
  try {
    return Buffer.from(s, 'base64');
  } catch {
    return undefined;
  }
}

/** Serialize + base64-encode a VersionedTransaction (the wire form sendTransaction/simulateTransaction take). */
export function txToB64(tx: { serialize(): Uint8Array }): string {
  return Buffer.from(tx.serialize()).toString('base64');
}

/** Batched getMultipleAccounts (100 keys/call) -> pubkey (base58) -> raw bytes. */
export async function getMultiple(endpoint: string, keys: PublicKey[]): Promise<Map<string, Buffer>> {
  const out = new Map<string, Buffer>();
  for (let i = 0; i < keys.length; i += 100) {
    const chunk = keys.slice(i, i + 100);
    const strs = chunk.map((k) => k.toBase58());
    const v = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'getMultipleAccounts',
      params: [strs, { encoding: 'base64' }],
    });
    const arr = v?.result?.value;
    if (!Array.isArray(arr)) continue;
    for (let j = 0; j < arr.length; j++) {
      const bytes = arr[j]?.data !== undefined ? b64(arr[j].data) : undefined;
      if (bytes !== undefined) out.set(chunk[j].toBase58(), bytes);
    }
  }
  return out;
}

/** Fetch one account's raw bytes (undefined if absent/unfetchable). */
export async function getAccount(endpoint: string, key: PublicKey): Promise<Buffer | undefined> {
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getAccountInfo',
    params: [key.toBase58(), { encoding: 'base64' }],
  });
  return b64(v?.result?.value?.data);
}

/** Resolve a mint's owning token program (SPL Token vs Token-2022), with a caller-supplied cache. */
export async function mintOwner(
  endpoint: string,
  mint: PublicKey,
  cache?: Map<string, PublicKey>,
): Promise<PublicKey | undefined> {
  const key = mint.toBase58();
  const cached = cache?.get(key);
  if (cached !== undefined) return cached;
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getAccountInfo',
    params: [key, { encoding: 'base64' }],
  });
  const owner = v?.result?.value?.owner;
  if (typeof owner !== 'string') return undefined;
  try {
    const pk = new PublicKey(owner);
    cache?.set(key, pk);
    return pk;
  } catch {
    return undefined;
  }
}

export async function latestBlockhash(endpoint: string): Promise<string | undefined> {
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getLatestBlockhash',
    params: [{ commitment: 'finalized' }],
  });
  const bh = v?.result?.value?.blockhash;
  return typeof bh === 'string' ? bh : undefined;
}

export async function currentSlot(endpoint: string): Promise<bigint> {
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getSlot',
    params: [{ commitment: 'confirmed' }],
  });
  const s = v?.result;
  return typeof s === 'number' ? BigInt(s) : 0n;
}

export async function solBalance(endpoint: string, owner: string): Promise<number> {
  const v = await rpc(endpoint, { jsonrpc: '2.0', id: 1, method: 'getBalance', params: [owner] });
  const l = v?.result?.value;
  return typeof l === 'number' ? l / 1e9 : 0.0;
}

/** simulateTransaction over a base64 tx; returns the RPC `result.value` (has `.err`, `.logs`, ...). */
export async function simulateTxB64(endpoint: string, txB64: string): Promise<any | undefined> {
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'simulateTransaction',
    params: [txB64, { sigVerify: false, replaceRecentBlockhash: true, commitment: 'processed', encoding: 'base64' }],
  });
  return v?.result?.value;
}

/** Simple ok/err readback of simulateTransaction (err === null -> clean). */
export async function simulateOk(endpoint: string, txB64: string): Promise<boolean> {
  const val = await simulateTxB64(endpoint, txB64);
  return val !== undefined && val.err == null;
}

/** Extract the marginfi/Kamino custom program-error code from a sim `err`, if any. */
export function customErrorCode(err: any): number | undefined {
  const c = err?.InstructionError?.[1]?.Custom;
  return typeof c === 'number' ? c : undefined;
}

/** Outcome of simulateBundle: how many leading txs ran clean + the first failure's custom code. */
export interface BundleSim {
  ranOk: number;
  failCode: number | undefined;
}

/**
 * simulateBundle over base64 txs. jito-solana stops at the first failing tx,
 * so `ranOk < txsB64.length` means `txsB64[ranOk]` reverted (or the bundle
 * itself was rejected before running, if the RPC call failed -> undefined).
 */
export async function simulateBundle(endpoint: string, txsB64: string[]): Promise<BundleSim | undefined> {
  const nulls = txsB64.map(() => null);
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'simulateBundle',
    params: [
      { encodedTransactions: txsB64 },
      { skipSigVerify: true, replaceRecentBlockhash: true, preExecutionAccountsConfigs: nulls, postExecutionAccountsConfigs: nulls },
    ],
  });
  if (v?.error != null) return undefined;
  const results: any[] = Array.isArray(v?.result?.value?.transactionResults) ? v.result.value.transactionResults : [];
  let ranOk = 0;
  while (ranOk < results.length && results[ranOk]?.err == null) ranOk += 1;
  const failCode = customErrorCode(results[ranOk]?.err);
  return { ranOk, failCode };
}

/** Append one JSON line to {runDir}/latency.jsonl (best-effort, mirrors observe.ts's swallow-on-error). */
export function logLatency(runDir: string, v: unknown): void {
  try {
    fs.mkdirSync(runDir, { recursive: true });
    fs.appendFileSync(`${runDir}/latency.jsonl`, `${JSON.stringify(v)}\n`);
  } catch {
    // best-effort
  }
}

/** Generic append-only line logger (liqStreamExecutor's stream.jsonl + eprintln mirror). */
export function logLine(runDir: string, file: string, line: string): void {
  try {
    fs.mkdirSync(runDir, { recursive: true });
    fs.appendFileSync(`${runDir}/${file}`, `${line}\n`);
  } catch {
    // best-effort
  }
  console.error(`[fire] ${line}`);
}

// ── Config ───────────────────────────────────────────────────────────────

/** Env-var tuning knobs identical across all 5 executors. */
export interface BaseCfg {
  endpoint: string;
  dryRun: boolean;
  runDir: string;
  minProfitUsd: number;
  tipFractionBps: bigint;
  minTipSol: number;
  maxDailyTipSol: number;
  walletMinSol: number;
  slippageBps: number;
  senderUrl: string;
  tipAccount: PublicKey;
  webhook: string | undefined;
  pollMs: number;
  rescanSecs: number;
  tickPollMs: number;
  handleCooldownSecs: number;
  simCooldownSecs: number;
  heartbeatSecs: number;
}

const DEFAULT_TIP_ACCOUNT = '2nyhqdwKcJZR2vcqCyrYsaPVdAnFoJjiksCXJ7hfEYgD';
export const DEFAULT_AUTHORITY = 'DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak';
export const DEFAULT_LIQUIDATOR_MA = 'B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD';

function envNum(name: string, def: number): number {
  const v = process.env[name];
  if (v === undefined) return def;
  const n = Number.parseFloat(v);
  return Number.isFinite(n) ? n : def;
}
function envInt(name: string, def: number): number {
  const v = process.env[name];
  if (v === undefined) return def;
  const n = Number.parseInt(v, 10);
  return Number.isFinite(n) ? n : def;
}
function envBigint(name: string, def: bigint): bigint {
  const v = process.env[name];
  if (v === undefined) return def;
  try {
    return BigInt(v);
  } catch {
    return def;
  }
}
function envBool(name: string, def: boolean): boolean {
  const v = process.env[name];
  if (v === undefined) return def;
  return v !== '0';
}

/** Read the base tuning config shared by every liq_*_executor variant. */
export function loadBaseCfg(defaults?: Partial<Record<'runDir' | 'pollMs' | 'rescanSecs', number | string>>): BaseCfg {
  const endpoint = process.env.HELIUS_RPC ?? process.env.RPC_HTTP;
  if (!endpoint) throw new Error('HELIUS_RPC');
  const runDirDefault = typeof defaults?.runDir === 'string' ? defaults.runDir : 'runs';
  const pollMsDefault = typeof defaults?.pollMs === 'number' ? defaults.pollMs : 5000;
  const rescanSecsDefault = typeof defaults?.rescanSecs === 'number' ? defaults.rescanSecs : 30;
  return {
    endpoint,
    dryRun: envBool('DRY_RUN', true),
    runDir: process.env.RUN_DIR ?? runDirDefault,
    minProfitUsd: envNum('MIN_PROFIT_USD', 0.5),
    tipFractionBps: envBigint('TIP_FRACTION_BPS', 3000n),
    minTipSol: envNum('MIN_TIP_SOL', 0.0002),
    maxDailyTipSol: envNum('MAX_DAILY_TIP_SOL', 0.05),
    walletMinSol: envNum('WALLET_MIN_SOL', 0.02),
    slippageBps: envInt('SLIPPAGE_BPS', 100),
    senderUrl: process.env.SENDER_URL ?? 'http://ams-sender.helius-rpc.com/fast',
    tipAccount: new PublicKey(process.env.SENDER_TIP_ACCOUNT ?? DEFAULT_TIP_ACCOUNT),
    webhook: process.env.ALERT_WEBHOOK,
    pollMs: envInt('POLL_MS', pollMsDefault),
    rescanSecs: envInt('RESCAN_SECS', rescanSecsDefault),
    tickPollMs: envInt('TICK_POLL_MS', 1),
    handleCooldownSecs: envInt('HANDLE_COOLDOWN_SECS', 20),
    simCooldownSecs: envInt('SIM_COOLDOWN_SECS', 60),
    heartbeatSecs: envInt('HEARTBEAT_SECS', 30),
  };
}

/** Load KEYPAIR_PATH if set; panics (LIVE) or falls back to AUTHORITY/default (DRY_RUN) otherwise. */
export function loadKeypair(dryRun: boolean): { kp: Keypair | undefined; authority: PublicKey } {
  const p = process.env.KEYPAIR_PATH;
  let kp: Keypair | undefined;
  if (p) {
    const bytes: number[] = JSON.parse(fs.readFileSync(p, 'utf8'));
    kp = Keypair.fromSecretKey(Uint8Array.from(bytes));
  }
  if (kp === undefined && !dryRun) throw new Error('LIVE needs KEYPAIR_PATH');
  const authority = kp?.publicKey ?? new PublicKey(process.env.AUTHORITY ?? DEFAULT_AUTHORITY);
  return { kp, authority };
}

// ── Self-crank plumbing (liqExecutor + liqSaveExecutor) ─────────────────

/** Everything the self-crank fire path needs, spun up once at boot. */
export interface CrankCtx {
  on: boolean;
  hermes: HermesCache;
  tips: PublicKey[];
  blockEngine: string;
  maxBlobAgeMs: number;
}

const FALLBACK_JITO_TIP = 'DttWaMuVvTiduZRnguLF7jNxTgiMBZ1hyAumKUiL2KRL';

/** Pick a Jito tip account round-robin-ish (matches the Rust `now() % len`). */
export function pickTip(ctx: CrankCtx): PublicKey | undefined {
  if (ctx.tips.length === 0) return undefined;
  return ctx.tips[now() % ctx.tips.length];
}

/**
 * Boot the self-crank context: fetch Jito tip accounts (falling back to a
 * known-good tip if the fetch fails), and spawn the Hermes blob poller.
 * `on` gates whether crank mode is even attempted (CRANK env, default true).
 */
export async function loadCrankCtx(logPrefix: string, onOverride?: boolean): Promise<CrankCtx> {
  const crankOn = (onOverride ?? true) && envBool('CRANK', true);
  const blockEngine = defaultBlockEngine();
  let tips: PublicKey[] = [];
  if (crankOn) {
    try {
      tips = await getTipAccounts(blockEngine);
    } catch {
      tips = [];
    }
    if (tips.length === 0) {
      console.error(`${logPrefix} getTipAccounts failed — using fallback Jito tip list`);
      tips = [new PublicKey(FALLBACK_JITO_TIP)];
    }
  }
  const hermesUrl = process.env.HERMES ?? 'https://hermes.pyth.network';
  const maxBlobAgeMs = envInt('MAX_BLOB_AGE_MS', 3000);
  const hermes = spawnHermesCache(hermesUrl, [], 400);
  console.error(`${logPrefix} self-crank mode: ${crankOn ? 'ENABLED' : 'off'}`);
  return { on: crankOn, hermes, tips, blockEngine, maxBlobAgeMs };
}

// ── Daily tip budget + wallet floor guard ────────────────────────────────

/** Tracks tips spent today (UTC day boundary) against MAX_DAILY_TIP_SOL, and gates the wallet floor. */
export class DailyTipBudget {
  private spent = 0.0;
  private day: number;
  readonly maxDailySol: number;

  constructor(maxDailySol: number) {
    this.maxDailySol = maxDailySol;
    this.day = Math.floor(now() / 86_400);
  }

  /** Roll the budget over at the UTC-day boundary. Call once per loop tick. */
  rollDay(): void {
    const d = Math.floor(now() / 86_400);
    if (d !== this.day) {
      this.day = d;
      this.spent = 0.0;
    }
  }

  wouldExceed(tipSol: number): boolean {
    return this.spent + tipSol > this.maxDailySol;
  }

  /** Record a tip as spent (call only once the fire is confirmed landed). */
  add(tipSol: number): void {
    this.spent += tipSol;
  }
}

/** True if `authority`'s SOL balance is below `walletMinSol` (the pre-fire floor guard). */
export async function belowWalletFloor(endpoint: string, authority: PublicKey, walletMinSol: number): Promise<boolean> {
  const bal = await solBalance(endpoint, authority.toBase58());
  return bal < walletMinSol;
}

// ── Sign + submit (Sender-or-crank-bundle) + P&L readback ──────────────────
// Shared by liqExecutor.ts and liqSaveExecutor.ts (both have a Sender mode and
// a Jito crank-bundle mode); liqKaminoExecutor.ts uses only the Sender half
// (pass `crank: undefined`).

export type SubmitResult = { ok: true; signature: string; bundleId: string | undefined } | { ok: false; error: string };

/**
 * Stamp `freshBh` onto `tx`, sign with `kp`, and submit: a plain Helius Sender
 * tx when `crank` is undefined or `mode` isn't 'crank'; otherwise a
 * [crank_setup, crank_fire, tx] Jito bundle built fresh from the hottest
 * Hermes blob for `feedId` (retried up to 3x on a 429). Returns the base58
 * signature either way so the caller can log/readback against it.
 */
export async function signAndSubmit(
  tx: VersionedTransaction,
  kp: Keypair,
  freshBh: string,
  senderUrl: string,
  crank: (CrankCtx & { authority: PublicKey; feedId: Buffer }) | undefined,
): Promise<SubmitResult> {
  (tx.message as { recentBlockhash: string }).recentBlockhash = freshBh;
  tx.sign([kp]);
  const signature = bs58.encode(tx.signatures[0]!);
  const txB64 = txToB64(tx);

  if (crank === undefined) {
    try {
      await sendSender(senderUrl, txB64);
      return { ok: true, signature, bundleId: undefined };
    } catch (e) {
      return { ok: false, error: String(e) };
    }
  }

  const u = crank.hermes.updateFor(crank.feedId);
  if (u === undefined) return { ok: false, error: 'no Hermes blob for feed' };
  if (u.ageMs > crank.maxBlobAgeMs) return { ok: false, error: `Hermes blob stale (${u.ageMs}ms) — not bundling` };
  const ctxs = buildCrankTxs(crank.authority, u.vaa, [u.update], 0, 0, freshBh);
  ctxs.stampAndSign(kp, freshBh);
  const [setupB64, crankB64] = ctxs.toB64();
  let last = '';
  for (let attempt = 0; attempt < 3; attempt++) {
    try {
      const bundleId = await sendBundle(crank.blockEngine, [setupB64, crankB64, txB64]);
      return { ok: true, signature, bundleId };
    } catch (e) {
      const msg = String(e);
      if (msg.includes('429') && attempt < 2) {
        last = msg;
        await sleep(250);
      } else {
        return { ok: false, error: msg };
      }
    }
  }
  return { ok: false, error: last };
}

/**
 * Fire-and-forget: poll for the fee payer's realized USDC delta on `signature`
 * at [5,15,45]s, calling `onLanded(pnl)` the first time it resolves (records
 * the tip as spent + logs/alerts landed-P&L), or `onMiss()` if it never
 * confirms in that window (bundle status is looked up there for context).
 */
export function spawnPnlReadback(
  endpoint: string,
  signature: string,
  owner: string,
  onLanded: (pnl: number) => void,
  onMiss: () => void,
): void {
  void (async () => {
    for (const waitSecs of [5, 15, 45]) {
      await sleep(waitSecs * 1000);
      const pnl = await realizedUsdc(endpoint, signature, owner);
      if (pnl !== undefined) {
        onLanded(pnl);
        return;
      }
    }
    onMiss();
  })();
}
