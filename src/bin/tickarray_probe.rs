//! Verify tick-array derivation against the chain (no wallet, no money). Reads
//! each pool's live state, derives the current tick-array PDA, and checks that
//! account actually exists and is owned by the DEX program. If both resolve to
//! real program-owned accounts, the start-index math + PDA seeds are correct —
//! the foundation the swap instructions stand on.
//!
//! Usage: RPC_ENDPOINT=<url> cargo run --release --bin tickarray_probe

use arb_engine::decode::{ORCA_PROGRAM, RAY_CLMM_PROGRAM};
use arb_engine::execute::{
    decode_orca_state, decode_ray_state, orca_start_index, orca_tick_array, ray_start_index,
    ray_tick_array, PoolState,
};
use arb_engine::pools::pair;
use base64::Engine;
use solana_pubkey::Pubkey;
use std::str::FromStr;

fn rpc(endpoint: &str, method: &str, params: serde_json::Value) -> Option<serde_json::Value> {
    let body = serde_json::json!({"jsonrpc":"2.0","id":1,"method":method,"params":params});
    ureq::post(endpoint).send_json(body).ok()?.into_json().ok()
}

fn account(endpoint: &str, key: &str) -> Option<(Vec<u8>, String)> {
    let r = rpc(
        endpoint,
        "getAccountInfo",
        serde_json::json!([key, {"encoding":"base64"}]),
    )?;
    let v = &r["result"]["value"];
    let data = base64::engine::general_purpose::STANDARD
        .decode(v["data"][0].as_str()?)
        .ok()?;
    Some((data, v["owner"].as_str()?.to_string()))
}

fn check(endpoint: &str, label: &str, pool_str: &str, program: &str, state: PoolState, tick_array: Pubkey, start: i32) {
    println!(
        "\n{label}: tick={} spacing={} liquidity={}",
        state.tick, state.tick_spacing, state.liquidity
    );
    println!("  current tick-array start index: {start}");
    println!("  derived tick-array PDA: {tick_array}");
    match account(endpoint, &tick_array.to_string()) {
        Some((data, owner)) => {
            let ok = owner == program;
            println!(
                "  on-chain: owner={owner} len={} → {}",
                data.len(),
                if ok { "✓ program-owned (derivation VALID)" } else { "✗ wrong owner" }
            );
        }
        None => println!("  on-chain: account not found ✗ (derivation wrong or empty array)"),
    }
    let _ = (pool_str, Pubkey::from_str);
}

fn main() {
    let endpoint = std::env::var("RPC_ENDPOINT")
        .unwrap_or_else(|_| "https://api.mainnet-beta.solana.com".to_string());

    let cfg = pair();
    let orca_pool = Pubkey::from_str(&cfg.orca_pool).unwrap();
    let ray_pool = Pubkey::from_str(&cfg.ray_pool).unwrap();

    if let Some((data, _)) = account(&endpoint, &cfg.orca_pool) {
        if let Some(st) = decode_orca_state(&data) {
            let start = orca_start_index(st.tick, st.tick_spacing);
            check(&endpoint, "Orca", &cfg.orca_pool, ORCA_PROGRAM, st, orca_tick_array(&orca_pool, start), start);
        }
    }
    if let Some((data, _)) = account(&endpoint, &cfg.ray_pool) {
        if let Some(st) = decode_ray_state(&data) {
            let start = ray_start_index(st.tick, st.tick_spacing);
            check(&endpoint, "Raydium CLMM", &cfg.ray_pool, RAY_CLMM_PROGRAM, st, ray_tick_array(&ray_pool, start), start);
        }
    }
}
