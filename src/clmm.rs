//! In-memory concentrated-liquidity (Uniswap-v3-style) math for Orca Whirlpool
//! and Raydium CLMM. Given a pool's current sqrtPrice + active liquidity, apply
//! a swap in closed form (WITHIN the current tick — constant liquidity) to get
//! the exact output and post-swap price, then compute the optimal cross-venue
//! arb size and its exact profit. Pure arithmetic — hot-path safe.
//!
//! LIMITATION: assumes the swap stays inside the current tick's liquidity range
//! (no tick crossing). Valid for small/moderate sizes on deep pools; large
//! sizes need tick-array liquidity (Stage 1b). clmm_probe verifies where the
//! within-tick assumption holds vs real on-chain quotes.

use solana_pubkey::Pubkey;
use std::str::FromStr;

/// Normalized pool state for swap math. sqrt_p is the raw Q64.64 sqrt price as
/// a float (sqrt of token1/token0 in raw base units); liquidity is raw L.
#[derive(Clone, Debug)]
pub struct ClmmState {
    pub sqrt_p: f64,
    pub liquidity: f64,
    pub mint0: Pubkey,
    pub mint1: Pubkey,
    pub dec0: i32,
    pub dec1: i32,
    pub fee_bps: f64,
}

fn u128_le(d: &[u8], o: usize) -> u128 {
    let mut b = [0u8; 16];
    b.copy_from_slice(&d[o..o + 16]);
    u128::from_le_bytes(b)
}
fn pk_at(d: &[u8], o: usize) -> Pubkey {
    Pubkey::try_from(&d[o..o + 32]).unwrap()
}

impl ClmmState {
    /// Orca Whirlpool: liquidity u128@49, sqrtPrice u128@65, mintA@101, mintB@181.
    /// Decimals come from config (caller passes base/quote decimals by mint).
    pub fn from_orca(d: &[u8], dec_a: i32, dec_b: i32, fee_bps: f64) -> Option<Self> {
        if d.len() < 213 { return None; }
        Some(Self {
            liquidity: u128_le(d, 49) as f64,
            sqrt_p: u128_le(d, 65) as f64 / 2f64.powi(64),
            mint0: pk_at(d, 101),
            mint1: pk_at(d, 181),
            dec0: dec_a,
            dec1: dec_b,
            fee_bps,
        })
    }

    /// Raydium CLMM: mint0@73, mint1@105, decimals@233/234, liquidity u128@237,
    /// sqrtPriceX64 u128@253.
    pub fn from_ray(d: &[u8], fee_bps: f64) -> Option<Self> {
        if d.len() < 269 { return None; }
        Some(Self {
            liquidity: u128_le(d, 237) as f64,
            sqrt_p: u128_le(d, 253) as f64 / 2f64.powi(64),
            mint0: pk_at(d, 73),
            mint1: pk_at(d, 105),
            dec0: d[233] as i32,
            dec1: d[234] as i32,
            fee_bps,
        })
    }

    /// Apply a swap of `amount_in` raw units of the input token (token0 if
    /// `zero_for_one`, else token1). Returns raw `amount_out` of the other token.
    /// Within-tick closed form. Fee taken off the input first.
    pub fn apply_swap(&self, zero_for_one: bool, amount_in: f64) -> f64 {
        if amount_in <= 0.0 || self.liquidity <= 0.0 { return 0.0; }
        let amt = amount_in * (1.0 - self.fee_bps / 1e4);
        let (l, sp) = (self.liquidity, self.sqrt_p);
        if zero_for_one {
            // token0 in → price decreases. 1/sp_new = 1/sp + amt/L
            let sp_new = l * sp / (l + amt * sp);
            l * (sp - sp_new) // token1 out
        } else {
            // token1 in → price increases. sp_new = sp + amt/L
            let sp_new = sp + amt / l;
            l * (1.0 / sp - 1.0 / sp_new) // token0 out
        }
    }

    /// Post-swap copy (mutates sqrt_p as if `amount_in` were swapped). Used to
    /// predict a victim's effect before building our arb.
    pub fn after_swap(&self, zero_for_one: bool, amount_in: f64) -> ClmmState {
        let mut s = self.clone();
        if amount_in > 0.0 && self.liquidity > 0.0 {
            let amt = amount_in * (1.0 - self.fee_bps / 1e4);
            s.sqrt_p = if zero_for_one {
                self.liquidity * self.sqrt_p / (self.liquidity + amt * self.sqrt_p)
            } else {
                self.sqrt_p + amt / self.liquidity
            };
        }
        s
    }

    /// UI price of token0 in token1 (e.g. USDC per SOL if token0=SOL).
    pub fn ui_price(&self) -> f64 {
        self.sqrt_p * self.sqrt_p * 10f64.powi(self.dec0 - self.dec1)
    }
}

/// Simulate the full round trip for a given USDC borrow amount (raw USDC units):
/// buy base on `buy` pool, sell it on `sell` pool. Returns net USDC profit (raw,
/// can be negative). `base` = the base mint (e.g. wSOL). Both pools must be the
/// same token pair.
pub fn round_trip_profit(buy: &ClmmState, sell: &ClmmState, base: &Pubkey, borrow_usdc: f64) -> f64 {
    // Buy base with USDC on `buy`: input is USDC.
    let usdc_is_0_buy = buy.mint0 != *base;
    let base_out = buy.apply_swap(usdc_is_0_buy, borrow_usdc);
    if base_out <= 0.0 { return -borrow_usdc; }
    // Sell that base for USDC on `sell`: input is base.
    let base_is_0_sell = sell.mint0 == *base;
    let usdc_out = sell.apply_swap(base_is_0_sell, base_out);
    usdc_out - borrow_usdc
}

/// Find the borrow size (raw USDC) that maximises round-trip profit, and that
/// profit, via ternary search over a concave curve. `max_usdc` caps the search.
pub fn optimal_arb(a: &ClmmState, b: &ClmmState, base: &Pubkey, max_usdc: f64) -> (f64, f64) {
    // Try both directions (buy A/sell B, and buy B/sell A); optimise each.
    let opt = |buy: &ClmmState, sell: &ClmmState| -> (f64, f64) {
        let (mut lo, mut hi) = (0.0f64, max_usdc);
        for _ in 0..60 {
            let m1 = lo + (hi - lo) / 3.0;
            let m2 = hi - (hi - lo) / 3.0;
            if round_trip_profit(buy, sell, base, m1) < round_trip_profit(buy, sell, base, m2) {
                lo = m1;
            } else {
                hi = m2;
            }
        }
        let size = (lo + hi) / 2.0;
        (size, round_trip_profit(buy, sell, base, size))
    };
    let (s1, p1) = opt(a, b);
    let (s2, p2) = opt(b, a);
    if p1 >= p2 { (s1, p1) } else { (s2, p2) }
}

pub fn wsol() -> Pubkey {
    Pubkey::from_str("So11111111111111111111111111111111111111112").unwrap()
}
