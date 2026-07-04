//! Temporary local verification probe (not for prod):
//! 1. send_bundle with a garbage tx → expect a 400 whose Jito response body is captured
//! 2. getLatestBlockhash twice, 3s apart → expect different hashes

use arb_engine::jito::{default_block_engine, get_tip_accounts, send_bundle};
use std::time::Duration;

fn latest_blockhash(endpoint: &str) -> Option<String> {
    let body = serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getLatestBlockhash","params":[{"commitment":"confirmed"}]});
    let v: serde_json::Value = ureq::post(endpoint).send_json(body).ok()?.into_json().ok()?;
    v["result"]["value"]["blockhash"].as_str().map(|s| s.to_string())
}

fn main() {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let be = default_block_engine();
    let rpc = std::env::var("RPC_ENDPOINT").unwrap_or_else(|_| "https://api.mainnet-beta.solana.com".into());

    println!("── 1. Jito connectivity (getTipAccounts) ──");
    match get_tip_accounts(&be) {
        Ok(t) => println!("OK: {} tip accounts", t.len()),
        Err(e) => println!("FAIL: {e}"),
    }

    println!("── 2. send_bundle with garbage tx → expect error WITH response body ──");
    match send_bundle(&be, &["aGVsbG8gd29ybGQ=".to_string()]) {
        Ok(id) => println!("UNEXPECTED OK: {id}"),
        Err(e) => println!("error captured: {e}"),
    }

    println!("── 3. blockhash freshness (2 samples, 3s apart) ──");
    let a = latest_blockhash(&rpc);
    std::thread::sleep(Duration::from_secs(3));
    let b = latest_blockhash(&rpc);
    match (a, b) {
        (Some(a), Some(b)) if a != b => println!("OK: hashes differ ({}… vs {}…)", &a[..8], &b[..8]),
        (Some(a), Some(b)) => println!("FAIL: identical hash after 3s ({a} == {b})"),
        _ => println!("FAIL: RPC call failed"),
    }
}
