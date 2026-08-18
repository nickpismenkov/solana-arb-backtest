// Port of src/jito.rs
//
// Jito bundle submission via the block engine: getTipAccounts + sendBundle,
// plus a SOL tip-transfer instruction helper. Region-matched to the box
// (Amsterdam) by default. Used only when we go live — building/holding a
// bundle costs nothing; you pay only if it lands (and a guarded arb that
// isn't profitable reverts → the bundle never lands).

import bs58 from 'bs58';
import { PublicKey } from '@solana/web3.js';

export function defaultBlockEngine(): string {
  return process.env.JITO_BLOCK_ENGINE ?? 'https://amsterdam.mainnet.block-engine.jito.wtf';
}

/** Fetch the current Jito tip accounts (pick one at random per bundle). */
export async function getTipAccounts(blockEngine: string): Promise<PublicKey[]> {
  const url = `${blockEngine}/api/v1/bundles`;
  const body = { jsonrpc: '2.0', id: 1, method: 'getTipAccounts', params: [] };
  const resp = await fetch(url, { method: 'POST', body: JSON.stringify(body) });
  const json: any = await resp.json();
  const arr = json?.result;
  if (!Array.isArray(arr)) {
    throw new Error(`getTipAccounts: no result (${JSON.stringify(json)})`);
  }
  const out: PublicKey[] = [];
  for (const s of arr) {
    if (typeof s !== 'string') continue;
    try {
      out.push(new PublicKey(s));
    } catch {
      continue;
    }
  }
  return out;
}

/**
 * Submit a single signed tx via Helius Sender (dual-routes to validators +
 * Jito for fast landing; no 1/sec Jito-unauth cap). Requires a tip ≥0.0002 SOL
 * as a transfer to a Jito tip account inside the tx (we already include one).
 * skipPreflight=true — Sender blasts, doesn't simulate; our guard is the real
 * check. Returns the signature.
 */
export async function sendSender(senderUrl: string, txB64: string): Promise<string> {
  const body = {
    jsonrpc: '2.0',
    id: 1,
    method: 'sendTransaction',
    params: [txB64, { encoding: 'base64', skipPreflight: true, maxRetries: 0 }],
  };
  const resp = await fetch(senderUrl, { method: 'POST', body: JSON.stringify(body) });
  if (!resp.ok) {
    const b = await resp.text().catch(() => '');
    throw new Error(`sender HTTP ${resp.status}: ${b}`);
  }
  const json: any = await resp.json();
  if (json?.error != null) {
    throw new Error(`sender error: ${JSON.stringify(json.error)}`);
  }
  const sig = json?.result;
  if (typeof sig !== 'string') {
    throw new Error('sender: no signature');
  }
  return sig;
}

/**
 * Post-hoc status of a submitted bundle: "Landed", "Failed" (dropped — e.g.
 * our guard would revert, or we lost the race), "Pending", or "Invalid"
 * (expired/never seen). Off the hot path — call seconds after firing.
 */
export async function bundleStatus(blockEngine: string, bundleId: string): Promise<string | undefined> {
  const url = `${blockEngine}/api/v1/getInflightBundleStatuses`;
  const body = {
    jsonrpc: '2.0',
    id: 1,
    method: 'getInflightBundleStatuses',
    params: [[bundleId]],
  };
  try {
    const resp = await fetch(url, { method: 'POST', body: JSON.stringify(body) });
    const json: any = await resp.json();
    const status = json?.result?.value?.[0]?.status;
    return typeof status === 'string' ? status : undefined;
  } catch {
    return undefined;
  }
}

/**
 * Submit an atomic bundle (base64-encoded txs). Returns the bundle id.
 * JITO_BUNDLE_ENCODING=base58 re-encodes and submits via Jito's default
 * (base58) path instead — diagnostic for base64-path drops.
 */
export async function sendBundle(blockEngine: string, txsB64: string[]): Promise<string> {
  const url = `${blockEngine}/api/v1/bundles`;
  const useB58 = process.env.JITO_BUNDLE_ENCODING === 'base58';
  const body = useB58
    ? {
        jsonrpc: '2.0',
        id: 1,
        method: 'sendBundle',
        params: [txsB64.map((b64) => bs58.encode(Buffer.from(b64, 'base64')))],
      }
    : {
        jsonrpc: '2.0',
        id: 1,
        method: 'sendBundle',
        params: [txsB64, { encoding: 'base64' }],
      };
  const resp = await fetch(url, { method: 'POST', body: JSON.stringify(body) });
  if (!resp.ok) {
    const b = await resp.text().catch(() => '');
    throw new Error(`sendBundle HTTP ${resp.status}: ${b}`);
  }
  const json: any = await resp.json();
  if (json?.error != null) {
    throw new Error(`sendBundle error: ${JSON.stringify(json.error)}`);
  }
  return typeof json?.result === 'string' ? json.result : '';
}
