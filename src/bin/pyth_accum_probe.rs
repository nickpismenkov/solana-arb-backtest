//! Verify the Hermes accumulator parser against a live update: fetch SOL+USDC,
//! print the VAA length and each update's feed id + message/proof lengths. The
//! split must match what a real mainnet crank tx carried (~247B VAA, ~396B per
//! update) and the feed ids must equal the requested ones — proving we can feed
//! the crank ixs correctly.
//!
//! Usage: [HERMES=https://hermes.pyth.network] cargo run --release --bin pyth_accum_probe

use arb_engine::pyth_accumulator as acc;

fn hexs(b: &[u8]) -> String { b.iter().map(|x| format!("{x:02x}")).collect() }

// Canonical Pyth feed ids (hex).
const SOL: &str = "ef0d8b6fda2ceba41da15d4095d1da392a0d2f8ed0c6c7bc0f4cfac8c280b56d";
const USDC: &str = "eaa020c61cc479712813461ce153894a96a6c00b21ed0cfc2798d1f9a9e9c94a";

fn main() {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let hermes = std::env::var("HERMES").unwrap_or_else(|_| "https://hermes.pyth.network".into());
    let update = acc::fetch_hermes(&hermes, &[SOL, USDC]).expect("fetch+parse");
    println!("VAA: {} bytes", update.vaa.len());
    println!("{} price updates:", update.updates.len());
    for u in &update.updates {
        let fid = u.feed_id().map(|f| hexs(&f)).unwrap_or_default();
        println!("  feed {}…  message {}B  proof {}B", &fid[..16.min(fid.len())], u.message.len(), u.proof.len());
    }
    let ids: Vec<String> = update.updates.iter().filter_map(|u| u.feed_id().map(|f| hexs(&f))).collect();
    let ok = ids.iter().any(|i| i == SOL) && ids.iter().any(|i| i == USDC);
    if ok && !update.vaa.is_empty() {
        println!("★ parser VERIFIED — VAA extracted, both requested feeds present with message+proof");
    } else {
        println!("✗ mismatch — got feeds {ids:?}");
    }
}
