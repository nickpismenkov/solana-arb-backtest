// Port of src/execute.rs
//
// Leg-3 execution primitives: read live pool state (tick/sqrtPrice/liquidity)
// and derive the tick-array accounts a swap must reference. These are the
// fiddly, get-them-exactly-right pieces the swap instructions are built on;
// verified against the chain by `tickarray_probe` before we assemble any tx.

import { PublicKey } from '@solana/web3.js';
import { ORCA_PROGRAM, RAY_CLMM_PROGRAM } from './decode.js';

export interface PoolState {
  sqrtPrice: bigint;
  tick: number;
  tickSpacing: number;
  liquidity: bigint;
}

function u16le(d: Uint8Array, o: number): number {
  return new DataView(d.buffer, d.byteOffset, d.byteLength).getUint16(o, true);
}
function i32le(d: Uint8Array, o: number): number {
  return new DataView(d.buffer, d.byteOffset, d.byteLength).getInt32(o, true);
}
function u128le(d: Uint8Array, o: number): bigint {
  const view = new DataView(d.buffer, d.byteOffset, d.byteLength);
  const lo = view.getBigUint64(o, true);
  const hi = view.getBigUint64(o + 8, true);
  return (hi << 64n) | lo;
}

/** Orca Whirlpool: tickSpacing@41, liquidity@49, sqrtPrice@65, tickCurrent@81. */
export function decodeOrcaState(d: Uint8Array): PoolState | undefined {
  if (d.length < 85) return undefined;
  return {
    tickSpacing: u16le(d, 41),
    liquidity: u128le(d, 49),
    sqrtPrice: u128le(d, 65),
    tick: i32le(d, 81),
  };
}

/** Raydium CLMM: tickSpacing@235, liquidity@237, sqrtPriceX64@253, tickCurrent@269. */
export function decodeRayState(d: Uint8Array): PoolState | undefined {
  if (d.length < 273) return undefined;
  return {
    tickSpacing: u16le(d, 235),
    liquidity: u128le(d, 237),
    sqrtPrice: u128le(d, 253),
    tick: i32le(d, 269),
  };
}

/** Floor division (toward negative infinity) — tick indices go negative. */
function floorDiv(a: number, b: number): number {
  const q = Math.trunc(a / b);
  const r = a % b;
  if (r !== 0 && r < 0 !== b < 0) {
    return q - 1;
  }
  return q;
}

// Orca packs 88 ticks per array; Raydium CLMM packs 60.
export function orcaStartIndex(tick: number, spacing: number): number {
  const n = 88 * spacing;
  return floorDiv(tick, n) * n;
}
export function rayStartIndex(tick: number, spacing: number): number {
  const n = 60 * spacing;
  return floorDiv(tick, n) * n;
}

/** Orca tick-array PDA: seeds ["tick_array", whirlpool, ASCII(start_index)]. */
export function orcaTickArray(pool: PublicKey, start: number): PublicKey {
  const s = start.toString();
  const [pda] = PublicKey.findProgramAddressSync(
    [Buffer.from('tick_array'), pool.toBuffer(), Buffer.from(s, 'ascii')],
    new PublicKey(ORCA_PROGRAM),
  );
  return pda;
}

/** Raydium CLMM tick-array PDA: seeds ["tick_array", pool, i32 BE(start_index)]. */
export function rayTickArray(pool: PublicKey, start: number): PublicKey {
  const be = Buffer.alloc(4);
  be.writeInt32BE(start, 0);
  const [pda] = PublicKey.findProgramAddressSync(
    [Buffer.from('tick_array'), pool.toBuffer(), be],
    new PublicKey(RAY_CLMM_PROGRAM),
  );
  return pda;
}
