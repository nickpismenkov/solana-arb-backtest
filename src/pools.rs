//! Pool pair config + on-chain price decode. Byte offsets verified against
//! mainnet (see git history / RUN.md). Same Q64.64 sqrt-price math for both
//! venues — Orca Whirlpool and Raydium CLMM.
//!
//! The pair is env-configurable so the same tooling re-points at any pair that
//! has a pool on both venues (defaults = liquid SOL/USDC). Env:
//!   PAIR_LABEL, ORCA_POOL, RAY_CLMM_POOL, BASE_MINT, BASE_DECIMALS,
//!   QUOTE_DECIMALS, ORCA_FEE_BPS, RAY_FEE_BPS, RAY_VAULT0

use std::sync::OnceLock;

pub struct PairCfg {
    pub label: String,
    pub orca_pool: String,
    pub ray_pool: String,
    /// Orientation: prices are quote-per-base on both venues.
    pub base_mint: String,
    /// The Whirlpool account doesn't carry decimals (Ray CLMM does).
    pub base_dec: i32,
    pub quote_dec: i32,
    pub orca_fee_bps: f64,
    pub ray_fee_bps: f64,
    /// Ray CLMM token_vault_0 — input vault == this ⇒ base is being sold.
    pub ray_vault0: String,
}

impl PairCfg {
    pub fn round_trip_fee_bps(&self) -> f64 {
        self.orca_fee_bps + self.ray_fee_bps
    }
}

fn env_or(key: &str, default: &str) -> String {
    std::env::var(key).unwrap_or_else(|_| default.to_string())
}

fn env_num<T: std::str::FromStr>(key: &str, default: T) -> T {
    std::env::var(key).ok().and_then(|s| s.parse().ok()).unwrap_or(default)
}

/// Lazily loaded pair config. Call after dotenv has run (all bins do).
pub fn pair() -> &'static PairCfg {
    static CFG: OnceLock<PairCfg> = OnceLock::new();
    CFG.get_or_init(|| PairCfg {
        label: env_or("PAIR_LABEL", "SOL/USDC"),
        orca_pool: env_or("ORCA_POOL", "Czfq3xZZDmsdGdUyrNLtRhGc47cXcZtLG4crryfu44zE"),
        // Default Raydium CLMM SOL/USDC — the active 4bp pool; one account
        // holds sqrtPrice so it updates on every swap (no vault-ratio lag).
        ray_pool: env_or("RAY_CLMM_POOL", "3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv"),
        base_mint: env_or("BASE_MINT", "So11111111111111111111111111111111111111112"),
        base_dec: env_num("BASE_DECIMALS", 9),
        quote_dec: env_num("QUOTE_DECIMALS", 6),
        orca_fee_bps: env_num("ORCA_FEE_BPS", 4.0),
        ray_fee_bps: env_num("RAY_FEE_BPS", 4.0),
        ray_vault0: env_or("RAY_VAULT0", "4ct7br2vTPzfdmY3S5HLtTxcGSBfn6pnw98hsS6v359A"),
    })
}

fn u128_le(d: &[u8], o: usize) -> u128 {
    let mut b = [0u8; 16];
    b.copy_from_slice(&d[o..o + 16]);
    u128::from_le_bytes(b)
}

fn mint_at(d: &[u8], o: usize) -> String {
    bs58::encode(&d[o..o + 32]).into_string()
}

/// Orca Whirlpool: sqrtPrice (Q64.64) @65, mintA @101, mintB @181.
pub fn orca_price(d: &[u8]) -> Option<f64> {
    if d.len() < 213 {
        return None;
    }
    let cfg = pair();
    let sqrt = u128_le(d, 65) as f64 / 2f64.powi(64);
    let mint_a = mint_at(d, 101);
    let base_is_a = mint_a == cfg.base_mint;
    let (dec_a, dec_b) = if base_is_a {
        (cfg.base_dec, cfg.quote_dec)
    } else {
        (cfg.quote_dec, cfg.base_dec)
    };
    let ui_b_per_a = sqrt * sqrt * 10f64.powi(dec_a - dec_b);
    Some(if base_is_a { ui_b_per_a } else { 1.0 / ui_b_per_a })
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
    Some(if mint0 == pair().base_mint { ui_t1_per_t0 } else { 1.0 / ui_t1_per_t0 })
}
