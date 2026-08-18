// Port of src/observe.rs
//
// Observability for the executor — append-only JSONL ledgers + realized-P&L
// readback + alerts. STRICTLY off the hot path: all writes happen after a
// bundle is submitted; realized P&L is read on a later poll. Mirrors the
// design proven in the (now-retired) JS loop.

import { mkdirSync, appendFileSync } from 'node:fs';

function append<T>(dir: string, file: string, row: T): void {
  try {
    mkdirSync(dir, { recursive: true });
    appendFileSync(`${dir}/${file}`, `${JSON.stringify(row)}\n`);
  } catch {
    // best-effort, mirrors the Rust `let _ =` swallow
  }
}

/** Every evaluated trigger — the denominator (why we did/didn't fire). */
export function logDecision<T>(dir: string, row: T): void {
  append(dir, 'decisions.jsonl', row);
}

/** Every fired bundle — the source of truth (quoted, then resolved P&L). */
export function logTrade<T>(dir: string, row: T): void {
  append(dir, 'trades.jsonl', row);
}

/**
 * Realized USDC delta of the fee payer across a landed tx (actual result, not
 * the quote). undefined if the tx isn't on chain yet.
 */
export async function realizedUsdc(rpc: string, signature: string, owner: string): Promise<number | undefined> {
  const USDC = 'EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v';
  const body = {
    jsonrpc: '2.0',
    id: 1,
    method: 'getTransaction',
    params: [signature, { encoding: 'jsonParsed', maxSupportedTransactionVersion: 0, commitment: 'confirmed' }],
  };
  let v: any;
  try {
    const resp = await fetch(rpc, { method: 'POST', body: JSON.stringify(body) });
    v = await resp.json();
  } catch {
    return undefined;
  }
  const meta = v?.result?.meta;
  if (meta == null) return undefined;
  const sum = (key: string): number => {
    const arr = meta[key];
    if (!Array.isArray(arr)) return 0;
    return arr
      .filter((b: any) => b?.mint === USDC && b?.owner === owner)
      .reduce((acc: number, b: any) => {
        const ui = b?.uiTokenAmount?.uiAmount;
        return typeof ui === 'number' ? acc + ui : acc;
      }, 0);
  };
  return sum('postTokenBalances') - sum('preTokenBalances');
}

/** Fire-and-forget alert to a webhook (Slack/Discord/generic) if set. */
export function alert(webhook: string | undefined, key: string, message: string): void {
  console.error(`[ALERT:${key}] ${message}`);
  if (webhook) {
    fetch(webhook, {
      method: 'POST',
      body: JSON.stringify({ text: `arb [${key}] ${message}` }),
    }).catch(() => {});
  }
}
