// Port of src/jup.rs
//
// Jupiter swap API client (lite-api.jup.ag, keyless) — quote + composable
// swap instructions for ARBITRARY mint pairs. Built for the liquidation fire
// path (seized collateral -> debt token can be any mint, unlike the arb path's
// fixed pool basket). The arb hot path keeps its direct Orca/Ray builders (no
// HTTP hop); liquidations are block-granularity, so a quote round-trip at
// build time is affordable.

import { type AccountMeta, AddressLookupTableAccount, PublicKey, TransactionInstruction } from '@solana/web3.js';

/**
 * API base. Defaults to the keyless lite-api (rate-limited); override with
 * JUP_API_BASE to a Pro endpoint (e.g. https://api.jup.ag) under heavy load —
 * running several executors hammers lite-api and gets 429'd.
 */
function apiBase(): string {
  return process.env.JUP_API_BASE || 'https://lite-api.jup.ag';
}
function quoteUrl(): string {
  return `${apiBase()}/swap/v1/quote`;
}
function swapIxUrl(): string {
  return `${apiBase()}/swap/v1/swap-instructions`;
}

/**
 * Jupiter API key (paid api.jup.ag tier). When set, sent as `x-api-key` on
 * every request — lifts the keyless lite-api rate limit that otherwise 429s
 * live fires.
 */
function apiKey(): string | undefined {
  const k = process.env.JUP_API_KEY;
  return k && k.length > 0 ? k : undefined;
}

/**
 * Number of HTTP attempts (JUP_MAX_RETRIES = retries, so attempts = retries+1).
 * Default 5 attempts (block-granular backtest / batch use). The FIRE PATH sets
 * JUP_MAX_RETRIES=0 so a 429 fails INSTANTLY instead of sleeping 150+300+600+
 * 1200+2400=4.65s — a >300ms fire has already lost the race, and a multi-second
 * hang also holds a MAX_INFLIGHT slot, starving the fast direct-DEX fires.
 */
function maxAttempts(): number {
  const r = process.env.JUP_MAX_RETRIES;
  if (r === undefined) return 5;
  const parsed = Number.parseInt(r, 10);
  return Number.isFinite(parsed) ? parsed + 1 : 5;
}

/**
 * Optional hard per-request timeout (JUP_HTTP_TIMEOUT_MS). The fire path sets
 * a tight bound so a slow/stalled Jupiter never parks a firing thread for
 * seconds.
 */
function httpTimeoutMs(): number | undefined {
  const t = process.env.JUP_HTTP_TIMEOUT_MS;
  if (t === undefined) return undefined;
  const parsed = Number.parseInt(t, 10);
  return Number.isFinite(parsed) ? parsed : undefined;
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function fetchWithTimeout(url: string, init: RequestInit): Promise<Response> {
  const timeoutMs = httpTimeoutMs();
  if (timeoutMs === undefined) return fetch(url, init);
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);
  try {
    return await fetch(url, { ...init, signal: controller.signal });
  } finally {
    clearTimeout(timer);
  }
}

/** GET with exponential backoff on 429 / 5xx (the lite-api throttles under load). */
async function getJsonRetry(url: string): Promise<any> {
  const attempts = maxAttempts();
  let delay = 150;
  const key = apiKey();
  const headers: Record<string, string> = key ? { 'x-api-key': key } : {};
  for (let attempt = 0; attempt < attempts; attempt++) {
    const res = await fetchWithTimeout(url, { method: 'GET', headers });
    if (res.ok) return res.json();
    if ((res.status === 429 || res.status >= 500) && attempt + 1 < attempts) {
      await sleep(delay);
      delay *= 2;
      continue;
    }
    const text = await res.text().catch(() => '');
    throw new Error(`jup GET ${res.status}: ${text}`);
  }
  throw new Error('jup GET: exhausted retries (429/5xx)');
}

/** POST with the same backoff. */
async function postJsonRetry(url: string, body: unknown): Promise<any> {
  const attempts = maxAttempts();
  let delay = 150;
  const key = apiKey();
  const headers: Record<string, string> = { 'content-type': 'application/json' };
  if (key) headers['x-api-key'] = key;
  for (let attempt = 0; attempt < attempts; attempt++) {
    const res = await fetchWithTimeout(url, { method: 'POST', headers, body: JSON.stringify(body) });
    if (res.ok) return res.json();
    if ((res.status === 429 || res.status >= 500) && attempt + 1 < attempts) {
      await sleep(delay);
      delay *= 2;
      continue;
    }
    const text = await res.text().catch(() => '');
    throw new Error(`jup POST ${res.status}: ${text}`);
  }
  throw new Error('jup POST: exhausted retries (429/5xx)');
}

/** A quoted swap ready to splice into our own v0 transaction. */
export interface SwapPlan {
  /**
   * setup + swap + cleanup, in order. Jupiter's compute-budget ixs are
   * dropped — the enclosing tx owns its budget.
   */
  instructions: TransactionInstruction[];
  altAddresses: PublicKey[];
  quotedOut: bigint;
  /**
   * Jupiter's own slippage floor (min-out for ExactIn); the fire path's real
   * guard is repayAll, this just reverts earlier/cheaper.
   */
  minOut: bigint;
}

/**
 * ExactIn quote. `maxAccounts` bounds route complexity so the swap fits in a
 * tx that already carries the flashloan + liquidate accounts.
 */
export async function quote(inputMint: PublicKey, outputMint: PublicKey, amountIn: bigint, slippageBps: number, maxAccounts: number): Promise<any> {
  const url =
    `${quoteUrl()}?inputMint=${inputMint.toBase58()}&outputMint=${outputMint.toBase58()}&amount=${amountIn}` +
    `&slippageBps=${slippageBps}&swapMode=ExactIn&maxAccounts=${maxAccounts}`;
  const v = await getJsonRetry(url);
  if (v?.error !== undefined) {
    throw new Error(`jup quote: ${v.error}`);
  }
  if (typeof v?.outAmount !== 'string') {
    throw new Error(`jup quote: no route (${JSON.stringify(v)})`);
  }
  return v;
}

function decodeIx(v: any): TransactionInstruction {
  const programIdStr = v?.programId;
  if (typeof programIdStr !== 'string') throw new Error('ix missing programId');
  const programId = new PublicKey(programIdStr);
  const keys: AccountMeta[] = [];
  for (const a of v?.accounts ?? []) {
    const pubkeyStr = a?.pubkey;
    if (typeof pubkeyStr !== 'string') throw new Error('acct missing pubkey');
    keys.push({
      pubkey: new PublicKey(pubkeyStr),
      isSigner: a?.isSigner ?? false,
      isWritable: a?.isWritable ?? false,
    });
  }
  const data = Buffer.from(typeof v?.data === 'string' ? v.data : '', 'base64');
  return new TransactionInstruction({ programId, keys, data });
}

/**
 * Turn a quote into instructions signable by `user`. `wrapSol` only matters
 * when a leg is native SOL: the fire path swaps token ATAs directly (false,
 * marginfi withdraw lands wSOL in the wSOL ATA); a wallet-balance swap needs
 * the wrap (true).
 */
export async function swapInstructions(quoteResponse: any, user: PublicKey, wrapSol: boolean): Promise<SwapPlan> {
  const body = {
    quoteResponse,
    userPublicKey: user.toBase58(),
    wrapAndUnwrapSol: wrapSol,
    dynamicComputeUnitLimit: false,
  };
  const v = await postJsonRetry(swapIxUrl(), body);
  if (typeof v?.swapInstruction !== 'object' || v.swapInstruction === null) {
    throw new Error(`jup swap-instructions: ${JSON.stringify(v)}`);
  }
  const instructions: TransactionInstruction[] = [];
  for (const setupIx of v?.setupInstructions ?? []) {
    instructions.push(decodeIx(setupIx));
  }
  instructions.push(decodeIx(v.swapInstruction));
  if (v?.cleanupInstruction && typeof v.cleanupInstruction === 'object') {
    instructions.push(decodeIx(v.cleanupInstruction));
  }
  const altAddresses: PublicKey[] = [];
  for (const a of v?.addressLookupTableAddresses ?? []) {
    if (typeof a === 'string') {
      try {
        altAddresses.push(new PublicKey(a));
      } catch {
        // skip unparseable address
      }
    }
  }
  const quotedOut = BigInt(quoteResponse?.outAmount ?? '0');
  const minOut = BigInt(quoteResponse?.otherAmountThreshold ?? '0');
  return { instructions, altAddresses, quotedOut, minOut };
}

/**
 * Fetch + decode the plan's lookup tables so the caller can compile a v0
 * message (addresses start at byte 56 of an ALT account).
 * ALT cache: address-lookup-tables are STATIC (LIQ_ALT never changes), so RPC-
 * fetch each once and reuse from RAM. Removes a ~45ms getMultipleAccounts from
 * every fire build — the hot-path killer for on-the-fly (uncached) fires.
 */
const altCache = new Map<string, AddressLookupTableAccount>();

function loadAlt(altAddr: string, altAccountData: Buffer): AddressLookupTableAccount {
  const state = AddressLookupTableAccount.deserialize(altAccountData);
  return new AddressLookupTableAccount({ key: new PublicKey(altAddr), state });
}

async function fetchOneAlt(endpoint: string, addr: PublicKey): Promise<AddressLookupTableAccount> {
  const res = await fetch(endpoint, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({
      jsonrpc: '2.0',
      id: 1,
      method: 'getAccountInfo',
      params: [addr.toBase58(), { encoding: 'base64' }],
    }),
  });
  const v: any = await res.json();
  const b64 = v?.result?.value?.data?.[0];
  if (typeof b64 !== 'string') {
    throw new Error(`ALT ${addr.toBase58()} not found`);
  }
  const data = Buffer.from(b64, 'base64');
  return loadAlt(addr.toBase58(), data);
}

export async function fetchAlts(endpoint: string, addrs: PublicKey[]): Promise<AddressLookupTableAccount[]> {
  const out: AddressLookupTableAccount[] = [];
  for (const a of addrs) {
    const key = a.toBase58();
    const cached = altCache.get(key);
    if (cached) {
      out.push(cached);
      continue;
    }
    const alt = await fetchOneAlt(endpoint, a); // one-time RPC, then cached forever
    altCache.set(key, alt);
    out.push(alt);
  }
  return out;
}
