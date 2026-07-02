//! Leg-3 execution primitives: read live pool state (tick/sqrtPrice/liquidity)
//! and derive the tick-array accounts a swap must reference. These are the
//! fiddly, get-them-exactly-right pieces the swap instructions are built on;
//! verified against the chain by `tickarray_probe` before we assemble any tx.

use solana_pubkey::Pubkey;

use crate::decode::{ORCA_PROGRAM, RAY_CLMM_PROGRAM};

#[derive(Clone, Copy, Debug)]
pub struct PoolState {
    pub sqrt_price: u128,
    pub tick: i32,
    pub tick_spacing: u16,
    pub liquidity: u128,
}

fn u16le(d: &[u8], o: usize) -> u16 {
    u16::from_le_bytes(d[o..o + 2].try_into().unwrap())
}
fn i32le(d: &[u8], o: usize) -> i32 {
    i32::from_le_bytes(d[o..o + 4].try_into().unwrap())
}
fn u128le(d: &[u8], o: usize) -> u128 {
    u128::from_le_bytes(d[o..o + 16].try_into().unwrap())
}

/// Orca Whirlpool: tickSpacing@41, liquidity@49, sqrtPrice@65, tickCurrent@81.
pub fn decode_orca_state(d: &[u8]) -> Option<PoolState> {
    if d.len() < 85 {
        return None;
    }
    Some(PoolState {
        tick_spacing: u16le(d, 41),
        liquidity: u128le(d, 49),
        sqrt_price: u128le(d, 65),
        tick: i32le(d, 81),
    })
}

/// Raydium CLMM: tickSpacing@235, liquidity@237, sqrtPriceX64@253, tickCurrent@269.
pub fn decode_ray_state(d: &[u8]) -> Option<PoolState> {
    if d.len() < 273 {
        return None;
    }
    Some(PoolState {
        tick_spacing: u16le(d, 235),
        liquidity: u128le(d, 237),
        sqrt_price: u128le(d, 253),
        tick: i32le(d, 269),
    })
}

/// Floor division (toward negative infinity) — tick indices go negative.
fn floor_div(a: i32, b: i32) -> i32 {
    let q = a / b;
    let r = a % b;
    if r != 0 && ((r < 0) != (b < 0)) {
        q - 1
    } else {
        q
    }
}

// Orca packs 88 ticks per array; Raydium CLMM packs 60.
pub fn orca_start_index(tick: i32, spacing: u16) -> i32 {
    let n = 88 * spacing as i32;
    floor_div(tick, n) * n
}
pub fn ray_start_index(tick: i32, spacing: u16) -> i32 {
    let n = 60 * spacing as i32;
    floor_div(tick, n) * n
}

/// Orca tick-array PDA: seeds ["tick_array", whirlpool, ASCII(start_index)].
pub fn orca_tick_array(pool: &Pubkey, start: i32) -> Pubkey {
    let s = start.to_string();
    Pubkey::find_program_address(
        &[b"tick_array", pool.as_ref(), s.as_bytes()],
        &Pubkey::from_str_const(ORCA_PROGRAM),
    )
    .0
}

/// Raydium CLMM tick-array PDA: seeds ["tick_array", pool, i32 BE(start_index)].
pub fn ray_tick_array(pool: &Pubkey, start: i32) -> Pubkey {
    Pubkey::find_program_address(
        &[b"tick_array", pool.as_ref(), &start.to_be_bytes()],
        &Pubkey::from_str_const(RAY_CLMM_PROGRAM),
    )
    .0
}
