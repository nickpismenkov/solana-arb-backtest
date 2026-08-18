// Port of src/pump.rs
//
// pump.fun / PumpSwap recon + decoders — **Phase 1 (measure-first)**.
//
// Everything here is derived from CAPTURED REAL MAINNET TRANSACTIONS, never
// from memory (repo doctrine). Program IDs, event discriminators, and byte
// offsets are each verified against >=2 live txs; the Rust unit tests (not
// ported — no test harness set up yet) pin the layouts to real captured
// bytes so a future pump upgrade that shifts a field trips a test instead of
// silently corrupting the collector.
//
// This module is fully self-contained and shares NO state with the
// liquidation engine. It is pure observation: nothing here signs or submits
// a transaction.
//
// ── What was verified (see the PR body for the exact signatures) ────────────
// * Bonding-curve program `6EF8…F6P` — owns every `BondingCurve` PDA and emits
//   the anchor self-CPI event logs (`Program data: …`) we decode.
// * PumpSwap AMM `pAMM…fXEA` — the graduated venue; a bonding curve migrates
//   into a PumpSwap pool at completion.
// * The current mainnet program is the **Token-2022 variant** (mints/curve
//   token accounts live under `TokenzQd…`), and the instruction set has grown
//   V2/V3 variants (Create/CreateV2, Buy/BuyV2, Sell/SellV2, MigrateV2). We
//   therefore decode the **anchor event logs**, whose layout is stable across
//   those instruction variants, rather than per-instruction discriminators.
//
// ── Anchor event self-CPI logs ──────────────────────────────────────────────
// pump emits structured events as base64 in `Program data:` log lines. The
// first 8 bytes are the anchor event discriminator (`sha256("event:<Name>")`).
// We match on those to route Create / Trade / Migrate.

import { PublicKey } from '@solana/web3.js';

// ── Program IDs (verified: getAccountInfo owner/executable + real txs) ────────

/** pump.fun bonding-curve program. VERIFIED: it is the `owner` of every
 * `BondingCurve` account we fetched, and every create/buy/sell/migrate tx we
 * pulled is a top-level invoke of this program. */
export const PUMP_PROGRAM = '6EF8rrecthR5Dkzon8Nwu78hRvfCKubJ14M5uBEwF6P';

/** PumpSwap AMM program (post-graduation venue). VERIFIED: `executable=true`,
 * owned by the BPF upgradeable loader, and it is account #10 of the
 * `MigrateV2` instruction (the pool the curve migrates into). */
export const PUMPSWAP_AMM = 'pAMMBay6oceH9fJKBRHGP5D4bD4sWpmSwMn52FMfXEA';

/** pump.fun fee program (`GetFees` CPI seen in every trade). Informational. */
export const PUMP_FEE_PROGRAM = 'pfeeUxB6jkeY1Hxd7CsFCAjcbHA9rWtchMGdZ6VojVZ';

/** Migration authority — the signer of `MigrateV2`. Its transaction history
 * is, in effect, the list of graduations. VERIFIED across multiple migrate
 * txs. */
export const MIGRATION_AUTHORITY = '39azUYFWPz3VHgKCf3VChUwbpURdCHRxjWVowf5jUJjg';

/** PDA seed for a token's bonding-curve account: `["bonding-curve", mint]`.
 * VERIFIED: findProgramAddress(["bonding-curve", mint], PUMP_PROGRAM) equals
 * the `bonding_curve` field of the CreateEvent. */
export const BONDING_CURVE_SEED = Buffer.from('bonding-curve');

/** pump.fun tokens are minted with 6 decimals and a 1e15 raw total supply
 * (= 1,000,000,000 whole tokens). VERIFIED from CreateEvent.token_total_supply. */
export const PUMP_TOKEN_DECIMALS = 6;

// ── Anchor event discriminators (first 8 bytes of a `Program data:` blob) ─────

/** `sha256("event:TradeEvent")[..8]` — emitted on every buy and sell. */
export const TRADE_EVENT_DISC = Buffer.from([0xbd, 0xdb, 0x7f, 0xd3, 0x4e, 0xe6, 0x61, 0xee]);
/** `sha256("event:CreateEvent")[..8]` — emitted on a new token launch. */
export const CREATE_EVENT_DISC = Buffer.from([0x1b, 0x72, 0xa9, 0x4d, 0xde, 0xeb, 0x63, 0x76]);
/** Migrate/complete event — emitted inside the `MigrateV2` tx. Discriminator
 * captured live; only its `mint` field (byte offset 50) is decoded, see below. */
export const MIGRATE_EVENT_DISC = Buffer.from([0xb1, 0x31, 0x0c, 0xd2, 0xa0, 0x76, 0xa7, 0x74]);

// ── Reserve -> price helpers ──────────────────────────────────────────────────

/** Price of one **raw** token unit in **lamports**, straight from the
 * constant-product reserves (`virtual_sol_reserves / virtual_token_reserves`). */
export function priceLamportsPerRawToken(virtualSol: bigint, virtualToken: bigint): number {
  if (virtualToken === 0n) return 0.0;
  return Number(virtualSol) / Number(virtualToken);
}

/** Price of one **whole** token in **SOL**, accounting for the 9 lamport
 * decimals of SOL and `tokenDecimals` of the token (6 for pump). This is the
 * number you compare across launches for the "peak multiple" census. */
export function priceInSol(virtualSol: bigint, virtualToken: bigint, tokenDecimals: number): number {
  if (virtualToken === 0n) return 0.0;
  const sol = Number(virtualSol) / 1e9;
  const tokens = Number(virtualToken) / 10 ** tokenDecimals;
  return sol / tokens;
}

/**
 * Raw token units received for paying `solIn` lamports **into** the bonding
 * curve (pre-fee, pure constant-product). `virtualSol`/`virtualToken` are the
 * curve's virtual reserves *before* the trade.
 *
 * pump.fun's curve is a constant product `k = vsol * vtoken`. Paying `solIn`
 * raises `vsol` to `vsol + solIn`, so tokens out = `vtoken - k/(vsol+solIn)`
 * = `vtoken * solIn / (vsol + solIn)`. VERIFIED to the lamport against a real
 * captured dev-buy: the pump `TradeEvent.sol_amount` is exactly the SOL that
 * enters the curve, and its `token_amount` equals this function's output. The
 * pump/creator fee is charged **separately, on top** of `sol_amount` (125 bps
 * in the captured tx) — model it outside this pure curve function.
 */
export function curveBuyTokensOut(virtualSol: bigint, virtualToken: bigint, solIn: bigint): bigint {
  if (solIn === 0n) return 0n;
  const den = virtualSol + solIn;
  if (den === 0n) return 0n;
  // Result is strictly < virtualToken, so it always fits in u64.
  return (virtualToken * solIn) / den;
}

/**
 * Lamports of SOL received for selling `tokensIn` raw token units **into**
 * the curve (pre-fee, pure constant-product). Symmetric to
 * `curveBuyTokensOut`: sol out = `vsol * tokensIn / (vtoken + tokensIn)`. The
 * trading fee is charged separately on the SOL received — apply it outside.
 */
export function curveSellSolOut(virtualSol: bigint, virtualToken: bigint, tokensIn: bigint): bigint {
  if (tokensIn === 0n) return 0n;
  const den = virtualToken + tokensIn;
  if (den === 0n) return 0n;
  // Result is strictly < virtualSol, so it always fits in u64.
  return (virtualSol * tokensIn) / den;
}

/** Derive a token's bonding-curve PDA from its mint. */
export function bondingCurvePda(mint: PublicKey): PublicKey {
  const program = new PublicKey(PUMP_PROGRAM);
  return PublicKey.findProgramAddressSync([BONDING_CURVE_SEED, mint.toBuffer()], program)[0];
}

// ── BondingCurve account layout ──────────────────────────────────────────────
// VERIFIED against a live account whose reserves matched a TradeEvent from the
// same slot exactly (all four reserves + supply). Total account data = 151
// bytes (8 disc + 5×u64 + 1 bool + 32 creator + trailing zero pad).
//
//   offset  field
//   0       8-byte account discriminator (17 b7 f8 37 60 d8 ac 60)
//   8       virtual_token_reserves : u64
//   16      virtual_sol_reserves   : u64
//   24      real_token_reserves    : u64
//   32      real_sol_reserves      : u64
//   40      token_total_supply     : u64
//   48      complete               : bool  (1 = graduated / migrating)
//   49      creator                : pubkey (32)

/** A decoded `BondingCurve` account — the on-chain price/graduation state. */
export interface BondingCurve {
  virtualTokenReserves: bigint;
  virtualSolReserves: bigint;
  realTokenReserves: bigint;
  realSolReserves: bigint;
  tokenTotalSupply: bigint;
  /** True once the curve has filled and is graduating to PumpSwap. */
  complete: boolean;
  creator: PublicKey;
}

/** Account discriminator for a `BondingCurve` (anchor `sha256("account:BondingCurve")`). */
export const BONDING_CURVE_ACCOUNT_DISC = Buffer.from([0x17, 0xb7, 0xf8, 0x37, 0x60, 0xd8, 0xac, 0x60]);

/** Decode raw account data. Returns `null` if the discriminator does not
 * match or the buffer is short (i.e. it is not a BondingCurve account). */
export function decodeBondingCurve(data: Buffer): BondingCurve | null {
  if (data.length < 81 || !data.subarray(0, 8).equals(BONDING_CURVE_ACCOUNT_DISC)) return null;
  return {
    virtualTokenReserves: data.readBigUInt64LE(8),
    virtualSolReserves: data.readBigUInt64LE(16),
    realTokenReserves: data.readBigUInt64LE(24),
    realSolReserves: data.readBigUInt64LE(32),
    tokenTotalSupply: data.readBigUInt64LE(40),
    complete: data[48] !== 0,
    creator: new PublicKey(data.subarray(49, 81)),
  };
}

/** Price of one whole token in SOL from the current virtual reserves. */
export function bondingCurvePriceInSol(bc: BondingCurve): number {
  return priceInSol(bc.virtualSolReserves, bc.virtualTokenReserves, PUMP_TOKEN_DECIMALS);
}

// ── TradeEvent (buy / sell) ──────────────────────────────────────────────────
// VERIFIED against >=2 real events (a Sell and a dev Buy). Core layout:
//   0    disc(8)                 40  sol_amount:u64
//   8    mint:pubkey(32)         48  token_amount:u64
//   56   is_buy:bool             57  user:pubkey(32)
//   89   timestamp:i64           97  virtual_sol_reserves:u64
//   105  virtual_token_reserves  113 real_sol_reserves:u64
//   121  real_token_reserves:u64 129 fee_recipient:pubkey(32)
//   161  fee_basis_points:u64    169 fee:u64
//   177  creator:pubkey(32)      209 creator_fee_basis_points:u64
//   217  creator_fee:u64         (further fields: volume accounting + a "buy"/
//                                 "sell" string — not needed, left undecoded)

/** A decoded buy or sell on the bonding curve. */
export interface TradeEvent {
  mint: PublicKey;
  /** Lamports of SOL that moved (paid on a buy, received on a sell). */
  solAmount: bigint;
  /** Raw token units that moved. */
  tokenAmount: bigint;
  isBuy: boolean;
  /** The trader. */
  user: PublicKey;
  timestamp: bigint;
  virtualSolReserves: bigint;
  virtualTokenReserves: bigint;
  realSolReserves: bigint;
  realTokenReserves: bigint;
  /** The token's creator (dev). Present in the current layout; `null` on the
   * older/shorter variant. Useful for the dev-dump rug proxy. */
  creator: PublicKey | null;
}

/** Decode a `Program data:` blob whose first 8 bytes are `TRADE_EVENT_DISC`. */
export function decodeTradeEvent(data: Buffer): TradeEvent | null {
  if (data.length < 129 || !data.subarray(0, 8).equals(TRADE_EVENT_DISC)) return null;
  const creator = data.length >= 209 ? new PublicKey(data.subarray(177, 209)) : null;
  return {
    mint: new PublicKey(data.subarray(8, 40)),
    solAmount: data.readBigUInt64LE(40),
    tokenAmount: data.readBigUInt64LE(48),
    isBuy: data[56] !== 0,
    user: new PublicKey(data.subarray(57, 89)),
    timestamp: data.readBigInt64LE(89),
    virtualSolReserves: data.readBigUInt64LE(97),
    virtualTokenReserves: data.readBigUInt64LE(105),
    realSolReserves: data.readBigUInt64LE(113),
    realTokenReserves: data.readBigUInt64LE(121),
    creator,
  };
}

/** Whole-token price implied by this event's post-trade virtual reserves. */
export function tradeEventPriceInSol(t: TradeEvent): number {
  return priceInSol(t.virtualSolReserves, t.virtualTokenReserves, PUMP_TOKEN_DECIMALS);
}

// ── CreateEvent (new launch) ─────────────────────────────────────────────────
// VERIFIED against a CreateV2 tx. Layout (three leading anchor strings, so it
// is variable-length up to `mint`):
//   0   disc(8)
//   8   name:string  symbol:string  uri:string    (each u32 len + bytes)
//   ..  mint:pubkey  bonding_curve:pubkey  user:pubkey(dev)  creator:pubkey
//   ..  timestamp:i64
//   ..  virtual_token_reserves:u64  virtual_sol_reserves:u64
//   ..  real_token_reserves:u64     token_total_supply:u64

/** A decoded new-token launch. */
export interface CreateEvent {
  name: string;
  symbol: string;
  uri: string;
  mint: PublicKey;
  bondingCurve: PublicKey;
  /** Wallet that submitted the create (the "dev"). */
  user: PublicKey;
  /** The recorded creator (== `user` in every sample; distinct field on-chain). */
  creator: PublicKey;
  timestamp: bigint;
  virtualTokenReserves: bigint;
  virtualSolReserves: bigint;
  realTokenReserves: bigint;
  tokenTotalSupply: bigint;
}

/** Read an anchor `String` (u32 little-endian length prefix + UTF-8 bytes) at
 * `off`, returning the string and the offset just past it. Returns `null` if
 * the buffer is too short. Lossy on invalid UTF-8 is not modeled — Buffer's
 * `toString('utf8')` already replaces invalid sequences, matching Rust's
 * `String::from_utf8_lossy`. */
function readString(d: Buffer, off: number): { s: string; next: number } | null {
  if (off + 4 > d.length) return null;
  const len = d.readUInt32LE(off);
  const start = off + 4;
  const end = start + len;
  if (end > d.length) return null;
  return { s: d.subarray(start, end).toString('utf8'), next: end };
}

/** Decode a `Program data:` blob whose first 8 bytes are `CREATE_EVENT_DISC`. */
export function decodeCreateEvent(data: Buffer): CreateEvent | null {
  if (data.length < 8 || !data.subarray(0, 8).equals(CREATE_EVENT_DISC)) return null;
  let o = 8;
  const name = readString(data, o);
  if (name === null) return null;
  o = name.next;
  const symbol = readString(data, o);
  if (symbol === null) return null;
  o = symbol.next;
  const uri = readString(data, o);
  if (uri === null) return null;
  o = uri.next;
  if (o + 32 > data.length) return null;
  const mint = new PublicKey(data.subarray(o, o + 32));
  o += 32;
  if (o + 32 > data.length) return null;
  const bondingCurve = new PublicKey(data.subarray(o, o + 32));
  o += 32;
  if (o + 32 > data.length) return null;
  const user = new PublicKey(data.subarray(o, o + 32));
  o += 32;
  if (o + 32 > data.length) return null;
  const creator = new PublicKey(data.subarray(o, o + 32));
  o += 32;
  if (o + 8 > data.length) return null;
  const timestamp = data.readBigInt64LE(o);
  o += 8;
  if (o + 8 > data.length) return null;
  const virtualTokenReserves = data.readBigUInt64LE(o);
  o += 8;
  if (o + 8 > data.length) return null;
  const virtualSolReserves = data.readBigUInt64LE(o);
  o += 8;
  if (o + 8 > data.length) return null;
  const realTokenReserves = data.readBigUInt64LE(o);
  o += 8;
  if (o + 8 > data.length) return null;
  const tokenTotalSupply = data.readBigUInt64LE(o);
  return {
    name: name.s,
    symbol: symbol.s,
    uri: uri.s,
    mint,
    bondingCurve,
    user,
    creator,
    timestamp,
    virtualTokenReserves,
    virtualSolReserves,
    realTokenReserves,
    tokenTotalSupply,
  };
}

// ── Migrate event ────────────────────────────────────────────────────────────
// The MigrateV2 tx emits an event with `MIGRATE_EVENT_DISC`. Its full field
// layout is NOT fully decoded (fields before `mint` are not needed and left
// undocumented — being honest); the token `mint` sits at a fixed byte offset
// of 50, VERIFIED stable across 3 migrate events (all ending in "pump").

/** A decoded graduation. Only `mint` is extracted from the event; the new
 * PumpSwap pool is derivable from the migrate instruction accounts if needed. */
export interface MigrateEvent {
  mint: PublicKey;
}

const MIGRATE_MINT_OFFSET = 50;

/** Decode a `Program data:` blob whose first 8 bytes are `MIGRATE_EVENT_DISC`. */
export function decodeMigrateEvent(data: Buffer): MigrateEvent | null {
  if (data.length < MIGRATE_MINT_OFFSET + 32 || !data.subarray(0, 8).equals(MIGRATE_EVENT_DISC)) return null;
  return { mint: new PublicKey(data.subarray(MIGRATE_MINT_OFFSET, MIGRATE_MINT_OFFSET + 32)) };
}

// ── Unified event ────────────────────────────────────────────────────────────

/** Any pump.fun event decoded from a `Program data:` log blob. */
export type PumpEvent =
  | { kind: 'Create'; value: CreateEvent }
  | { kind: 'Trade'; value: TradeEvent }
  | { kind: 'Migrate'; value: MigrateEvent };

/** Route a raw event blob (already base64-decoded) by its discriminator. */
export function parsePumpEvent(data: Buffer): PumpEvent | null {
  if (data.length < 8) return null;
  const disc = data.subarray(0, 8);
  if (disc.equals(CREATE_EVENT_DISC)) {
    const ev = decodeCreateEvent(data);
    return ev === null ? null : { kind: 'Create', value: ev };
  }
  if (disc.equals(TRADE_EVENT_DISC)) {
    const ev = decodeTradeEvent(data);
    return ev === null ? null : { kind: 'Trade', value: ev };
  }
  if (disc.equals(MIGRATE_EVENT_DISC)) {
    const ev = decodeMigrateEvent(data);
    return ev === null ? null : { kind: 'Migrate', value: ev };
  }
  return null;
}

/** Short tag used in the collector's JSONL `event_type` field. */
export function pumpEventKind(ev: PumpEvent): 'create' | 'buy' | 'sell' | 'migrate' {
  switch (ev.kind) {
    case 'Create':
      return 'create';
    case 'Trade':
      return ev.value.isBuy ? 'buy' : 'sell';
    case 'Migrate':
      return 'migrate';
  }
}

/** Decode the base64 payload of a `Program data:` log line into a `PumpEvent`.
 * Non-pump / unrecognised blobs return `null`. */
export function parseProgramDataB64(b64: string): PumpEvent | null {
  let bytes: Buffer;
  try {
    bytes = Buffer.from(b64, 'base64');
  } catch {
    return null;
  }
  return parsePumpEvent(bytes);
}

// (tests omitted — no test harness set up yet; the Rust `#[cfg(test)]` module
// pinned BondingCurve/TradeEvent/CreateEvent/MigrateEvent layouts and the
// curve_buy/curve_sell math against real captured mainnet bytes.)
