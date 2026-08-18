// Port of src/decode.rs
//
// Shred swap decoder + Address Lookup Table resolution. Turns a raw
// VersionedTransaction (from a shred) into structured swaps on our pools:
// which venue, direction (sell/buy the base asset), and amount. ALT resolution
// also recovers swaps that reference a pool only via a lookup table — the ones
// our static-key match in the ShredStream feed misses.

import { execFileSync } from 'node:child_process';
import { PublicKey, VersionedMessage, VersionedTransaction } from '@solana/web3.js';
import { pair } from './pools.js';

export const ORCA_PROGRAM = 'whirLbMiicVdio4qvUfM5KAg6Ct8VwpYzGff3uctyCc';
export const RAY_CLMM_PROGRAM = 'CAMMCzo5YL8w4VFF8KVHrK22GGUsp5VTaW7grrKgrWqK';

// Anchor "global:<name>" sighashes. Both Orca & Raydium CLMM use `swap` and
// `swap_v2`; venue is disambiguated by program id, not discriminator.
const DISC_SWAP = Uint8Array.from([0xf8, 0xc6, 0x9e, 0x91, 0xe1, 0x75, 0x87, 0xc8]);
const DISC_SWAP_V2 = Uint8Array.from([0x2b, 0x04, 0xed, 0x0b, 0x1a, 0xc9, 0x1e, 0x62]);

function discEq(data: Uint8Array, disc: Uint8Array): boolean {
  for (let i = 0; i < 8; i++) {
    if (data[i] !== disc[i]) return false;
  }
  return true;
}

export enum Dir {
  SellBase, // base in → price down
  BuyBase, // base out → price up
}

export interface SwapInfo {
  venue: string;
  pool: string;
  dir: Dir;
  amount: bigint; // the instruction's `amount` arg (raw)
  amountIsInput: boolean; // exact-input vs exact-output
  kind: string; // "swap" | "swapV2"
}

/**
 * Caches decoded Address Lookup Tables (pubkey → its address list). ALTs are
 * effectively static, so the cache warms fast and later txns are instant.
 */
export class AltCache {
  private readonly rpc: string;
  private readonly tables: Map<string, PublicKey[]> = new Map();
  private lastFetch: number | undefined;

  constructor(rpc: string) {
    this.rpc = rpc;
  }

  /**
   * Fully resolved account keys for a message: static keys, then ALT
   * writables (per lookup order), then ALT readonlys — matching Solana's
   * account index layout. Returns undefined if any referenced ALT can't be
   * fetched/decoded.
   */
  resolveKeys(msg: VersionedMessage): PublicKey[] | undefined {
    const keys: PublicKey[] = [...msg.staticAccountKeys];
    const lookups = msg.addressTableLookups;
    if (lookups.length === 0) return keys; // no ALTs
    const writable: PublicKey[] = [];
    const readonly: PublicKey[] = [];
    for (const lookup of lookups) {
      const table = this.getTable(lookup.accountKey);
      if (table === undefined) return undefined;
      for (const i of lookup.writableIndexes) {
        const addr = table[i];
        if (addr === undefined) return undefined;
        writable.push(addr);
      }
      for (const i of lookup.readonlyIndexes) {
        const addr = table[i];
        if (addr === undefined) return undefined;
        readonly.push(addr);
      }
    }
    keys.push(...writable, ...readonly);
    return keys;
  }

  /**
   * Lightweight pool-touch check for the trigger feed: static keys first (no
   * RPC), then ALT-referenced addresses by index (cached table, no big
   * alloc). Returns the matching venue. NOTE: cold ALTs block on an RPC
   * fetch — fine for measurement (cache warms fast); production would
   * pre-load the hot ALTs instead of fetching in the hot path.
   */
  touchesPool(msg: VersionedMessage, pools: [PublicKey, string][]): string | undefined {
    for (const k of msg.staticAccountKeys) {
      for (const [p, v] of pools) {
        if (k.equals(p)) return v;
      }
    }
    for (const lookup of msg.addressTableLookups) {
      if (!this.ensureTable(lookup.accountKey)) continue;
      const table = this.tables.get(lookup.accountKey.toBase58())!;
      for (const i of [...lookup.writableIndexes, ...lookup.readonlyIndexes]) {
        const addr = table[i];
        if (addr === undefined) continue;
        for (const [p, v] of pools) {
          if (addr.equals(p)) return v;
        }
      }
    }
    return undefined;
  }

  private ensureTable(key: PublicKey): boolean {
    const cacheKey = key.toBase58();
    if (this.tables.has(cacheKey)) return true;
    // Cap cold-ALT fetches at ~10/s. Failed fetches aren't cached, so
    // without this a rate-limited RPC triggers a self-sustaining retry
    // flood off the txn firehose, starving every other consumer of the
    // endpoint (e.g. the executor's blockhash refresh). Hot ALTs recur
    // constantly, so the cache still warms within seconds.
    if (this.lastFetch !== undefined && Date.now() - this.lastFetch < 100) {
      return false;
    }
    this.lastFetch = Date.now();
    const data = fetchAccountData(this.rpc, key);
    if (data !== undefined) {
      // ALT: LookupTableMeta is 56 bytes; addresses follow, 32 bytes each.
      if (data.length >= 56) {
        const addrs: PublicKey[] = [];
        for (let o = 56; o + 32 <= data.length; o += 32) {
          addrs.push(new PublicKey(data.subarray(o, o + 32)));
        }
        this.tables.set(cacheKey, addrs);
        return true;
      }
    }
    return false;
  }

  private getTable(key: PublicKey): PublicKey[] | undefined {
    if (this.ensureTable(key)) {
      return this.tables.get(key.toBase58());
    }
    return undefined;
  }
}

/** Decode all swaps on our pools in a transaction, given its resolved keys. */
export function decodeSwaps(txn: VersionedTransaction, keys: PublicKey[]): SwapInfo[] {
  const cfg = pair();
  const orcaPool = new PublicKey(cfg.orcaPool);
  const rayPool = new PublicKey(cfg.rayPool);
  const orcaProg = new PublicKey(ORCA_PROGRAM);
  const rayProg = new PublicKey(RAY_CLMM_PROGRAM);
  const rayVault0 = new PublicKey(cfg.rayVault0);

  const instructions = txn.message.compiledInstructions;

  const out: SwapInfo[] = [];
  for (const ix of instructions) {
    const program = keys[ix.programIdIndex];
    if (program === undefined) continue;
    let venue: string;
    if (program.equals(orcaProg)) {
      venue = 'Orca';
    } else if (program.equals(rayProg)) {
      venue = 'Raydium';
    } else {
      continue;
    }
    if (ix.data.length < 8) continue;
    let kind: string;
    if (discEq(ix.data, DISC_SWAP)) {
      kind = 'swap';
    } else if (discEq(ix.data, DISC_SWAP_V2)) {
      kind = 'swapV2';
    } else {
      continue;
    }

    // Resolve this instruction's accounts to pubkeys.
    const ixKeys: PublicKey[] = [];
    for (const a of ix.accountKeyIndexes) {
      const k = keys[a];
      if (k !== undefined) ixKeys.push(k);
    }

    let pool: string;
    let dir: Dir;
    if (venue === 'Orca') {
      if (!ixKeys.some((k) => k.equals(orcaPool))) continue;
      // swap args: amount u64, thresh u64, sqrtLimit u128, amtSpecIsInput bool, aToB bool
      if (ix.data.length < 42) continue;
      // NOTE: assumes mintA is the base asset (true for the default
      // SOL/USDC pool; verify per pair) → A→B = sell base.
      const aToB = ix.data[41] !== 0;
      pool = cfg.orcaPool;
      dir = aToB ? Dir.SellBase : Dir.BuyBase;
    } else {
      if (!ixKeys.some((k) => k.equals(rayPool))) continue;
      // Raydium CLMM swap accounts: input_vault at index 5.
      const inputVault = ixKeys[5];
      if (inputVault === undefined) continue;
      dir = inputVault.equals(rayVault0) ? Dir.SellBase : Dir.BuyBase;
      pool = cfg.rayPool;
    }

    const amount = new DataView(ix.data.buffer, ix.data.byteOffset, ix.data.byteLength).getBigUint64(8, true);
    // Orca: amountSpecifiedIsInput @40; Raydium CLMM: isBaseInput @40.
    const amountIsInput = ix.data.length > 40 ? ix.data[40] !== 0 : true;
    out.push({
      venue,
      pool,
      dir,
      amount,
      amountIsInput,
      kind,
    });
  }
  return out;
}

// Node has no synchronous fetch; shell out to curl (blocking) to match the
// Rust's synchronous `ureq::post(..).send_json(..)` call in `AltCache`,
// whose callers (resolve_keys/touches_pool) are themselves synchronous.
function fetchAccountData(rpc: string, key: PublicKey): Uint8Array | undefined {
  try {
    const body = JSON.stringify({
      jsonrpc: '2.0',
      id: 1,
      method: 'getAccountInfo',
      params: [key.toBase58(), { encoding: 'base64' }],
    });
    const out = execFileSync('curl', ['-s', '-X', 'POST', '-H', 'content-type: application/json', '-d', body, rpc], {
      encoding: 'utf8',
    });
    const json: any = JSON.parse(out);
    const b64 = json?.result?.value?.data?.[0];
    if (typeof b64 !== 'string') return undefined;
    return Uint8Array.from(Buffer.from(b64, 'base64'));
  } catch {
    return undefined;
  }
}
