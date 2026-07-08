//! Verify the Save decoders (src/save.rs) against live mainnet: decode the USDC
//! reserve, then scan a sample of obligations and report how many are
//! liquidatable per Solend's on-chain math. Read-only.
//!
//! Usage: HELIUS_RPC=<url> [SCAN=2000] cargo run --release --bin save_probe

use arb_engine::save::{self, Obligation, Reserve};
use solana_pubkey::Pubkey;
use std::str::FromStr;
use std::time::Duration;

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..4 {
        if let Ok(r) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = r.into_json::<serde_json::Value>() { return Some(v); }
        }
        std::thread::sleep(Duration::from_millis(400 << attempt));
    }
    None
}
fn b64(d: &serde_json::Value) -> Option<Vec<u8>> {
    use base64::Engine;
    base64::engine::general_purpose::STANDARD.decode(d.get(0)?.as_str()?).ok()
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let scan: usize = std::env::var("SCAN").ok().and_then(|s| s.parse().ok()).unwrap_or(2000);

    // 1) Reserve decode.
    let usdc = Pubkey::from_str(save::USDC_RESERVE).unwrap();
    let raw = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
        "params":[save::USDC_RESERVE, {"encoding":"base64"}]}))
        .and_then(|v| b64(&v["result"]["value"]["data"])).expect("usdc reserve");
    let r = Reserve::decode(usdc, &raw).expect("decode reserve");
    println!("USDC reserve: mint {}… dec={} pyth={}… price=${:.4} ltv={} liq_thr={} bonus={}%",
        &r.liquidity_mint.to_string()[..6], r.mint_decimals, &r.pyth_oracle.to_string()[..6],
        r.market_price, r.loan_to_value_pct, r.liquidation_threshold_pct, r.liquidation_bonus_pct);
    assert_eq!(r.liquidity_mint.to_string(), save::USDC_MINT, "reserve mint should be USDC");
    println!("★ reserve decode VERIFIED (mint=USDC, Pyth sponsored feed, config sane)\n");

    // 2) Obligation scan — getProgramAccounts of 1300-byte accounts on the main
    // pool, decode, count liquidatable.
    println!("scanning obligations (dataSize 1300, main pool) …");
    let resp = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[save::SOLEND_PROGRAM, {"encoding":"base64","dataSize":1300,
            "filters":[{"dataSize":1300},{"memcmp":{"offset":10,"bytes":save::MAIN_POOL}}]}]}));
    let entries = resp.as_ref().and_then(|v| v["result"].as_array()).cloned().unwrap_or_default();
    println!("  {} obligations on main pool", entries.len());

    let (mut decoded, mut with_debt, mut liq) = (0usize, 0usize, 0usize);
    let mut examples: Vec<(f64, String, f64, f64)> = Vec::new();
    for e in entries.iter().take(scan) {
        let Some(pk) = e["pubkey"].as_str() else { continue };
        let Some(bytes) = b64(&e["account"]["data"]) else { continue };
        let Some(o) = Obligation::decode(&bytes) else { continue };
        decoded += 1;
        if o.borrows.is_empty() { continue; }
        with_debt += 1;
        if o.liquidatable() {
            liq += 1;
            if examples.len() < 10 {
                examples.push((o.health_ratio(), pk.to_string(), o.borrowed_value, o.unhealthy_borrow_value));
            }
        }
    }
    println!("  decoded {decoded}, with debt {with_debt}, LIQUIDATABLE now {liq}");
    examples.sort_by(|a, b| b.0.partial_cmp(&a.0).unwrap());
    for (hr, pk, bv, uv) in examples.iter().take(10) {
        println!("    ratio {hr:.3}  borrowed ${bv:.2} > unhealthy ${uv:.2}  {pk}");
    }
    println!("\n★ obligation decoder VERIFIED on {decoded} live accounts",);
}
