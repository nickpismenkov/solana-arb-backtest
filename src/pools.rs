//! Pool constants + on-chain price decode. Byte offsets verified against
//! mainnet (see git history / RUN.md). Same Q64.64 sqrt-price math for both
//! venues — Orca Whirlpool and Raydium CLMM.

pub const SOL: &str = "So11111111111111111111111111111111111111112";
pub const USDC: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";

pub const ORCA_POOL: &str = "Czfq3xZZDmsdGdUyrNLtRhGc47cXcZtLG4crryfu44zE";
// Raydium CLMM SOL/USDC — the active 4bp pool ($5.8M TVL), one account holds
// sqrtPrice so it updates on every swap (no vault-ratio lag).
pub const RAY_CLMM_POOL: &str = "3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv";

fn u128_le(d: &[u8], o: usize) -> u128 {
    let mut b = [0u8; 16];
    b.copy_from_slice(&d[o..o + 16]);
    u128::from_le_bytes(b)
}

fn mint_at(d: &[u8], o: usize) -> String {
    bs58::encode(&d[o..o + 32]).into_string()
}

fn dec_of(mint: &str) -> Option<i32> {
    match mint {
        SOL => Some(9),
        USDC => Some(6),
        _ => None,
    }
}

/// Orca Whirlpool: sqrtPrice (Q64.64) @65, mintA @101, mintB @181.
pub fn orca_price(d: &[u8]) -> Option<f64> {
    if d.len() < 213 {
        return None;
    }
    let sqrt = u128_le(d, 65) as f64 / 2f64.powi(64);
    let mint_a = mint_at(d, 101);
    let mint_b = mint_at(d, 181);
    let ui_b_per_a = sqrt * sqrt * 10f64.powi(dec_of(&mint_a)? - dec_of(&mint_b)?);
    Some(if mint_a == SOL { ui_b_per_a } else { 1.0 / ui_b_per_a })
}

/// Raydium CLMM PoolState: mint0 @73, mint1 @105, decimals @233/234,
/// sqrtPriceX64 @253.
pub fn ray_clmm_price(d: &[u8]) -> Option<f64> {
    if d.len() < 269 {
        return None;
    }
    let mint0 = mint_at(d, 73);
    let (dec0, dec1) = (d[233] as i32, d[234] as i32);
    let sqrt = u128_le(d, 253) as f64 / 2f64.powi(64);
    let ui_t1_per_t0 = sqrt * sqrt * 10f64.powi(dec0 - dec1);
    Some(if mint0 == SOL { ui_t1_per_t0 } else { 1.0 / ui_t1_per_t0 })
}
