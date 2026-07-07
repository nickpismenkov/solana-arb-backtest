//! Decode a real Pyth receiver `post_update` / `post_update_atomic` instruction
//! from mainnet to pin the exact discriminator, account layout, and data shape
//! before building our own crank ix. Receiver traffic is constant, so a small
//! scan finds examples fast. Also reports which target price-feed account each
//! writes (to confirm we can crank marginfi's sponsored feed, e.g. Dpw1EAVr…).
//!
//! Usage: HELIUS_RPC=<url> [SAMPLES=4] [LIMIT=400] cargo run --release --bin pyth_recv_decode

use std::collections::HashMap;
use std::time::Duration;

const PYTH_RECEIVER: &str = "rec5EKMGg6MxZYaMdyBfgwp4d5rB9T1VQH5pJv5LtFJ";

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..4 {
        if let Ok(resp) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = resp.into_json::<serde_json::Value>() { return Some(v); }
        }
        std::thread::sleep(Duration::from_millis(400 << attempt));
    }
    None
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let samples: usize = std::env::var("SAMPLES").ok().and_then(|s| s.parse().ok()).unwrap_or(4);
    let limit: u32 = std::env::var("LIMIT").ok().and_then(|s| s.parse().ok()).unwrap_or(400);

    let sigs = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress",
        "params":[PYTH_RECEIVER, {"limit": limit}]}))
        .and_then(|v| v["result"].as_array().cloned()).unwrap_or_default();
    let sigs: Vec<String> = sigs.iter().filter(|e| e["err"].is_null()).filter_map(|e| e["signature"].as_str().map(String::from)).collect();
    eprintln!("[recv] {} receiver signatures", sigs.len());

    // disc → (count, example accounts, example data_len)
    let mut seen: HashMap<String, usize> = HashMap::new();
    let mut dumped = 0usize;
    for sig in &sigs {
        if dumped >= samples { break; }
        let Some(tx) = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getTransaction",
            "params":[sig, {"encoding":"jsonParsed","maxSupportedTransactionVersion":0,"commitment":"confirmed"}]})) else { continue };
        let result = &tx["result"];
        if result.is_null() { continue; }
        let mut ixs: Vec<serde_json::Value> = result["transaction"]["message"]["instructions"].as_array().cloned().unwrap_or_default();
        for inner in result["meta"]["innerInstructions"].as_array().into_iter().flatten() {
            ixs.extend(inner["instructions"].as_array().cloned().unwrap_or_default());
        }
        for ix in &ixs {
            if ix["programId"] != PYTH_RECEIVER { continue; }
            let data = bs58::decode(ix["data"].as_str().unwrap_or("")).into_vec().unwrap_or_default();
            if data.len() < 8 { continue; }
            let disc: String = data[..8].iter().map(|b| format!("{b:02x}")).collect();
            let n = seen.entry(disc.clone()).or_default();
            *n += 1;
            if *n == 1 && dumped < samples {
                dumped += 1;
                println!("\n════ receiver ix disc={disc}  data_len={}  sig={sig}", data.len());
                for (i, a) in ix["accounts"].as_array().into_iter().flatten().enumerate() {
                    println!("    [{i:2}] {}", a.as_str().unwrap_or("?"));
                }
            }
        }
        std::thread::sleep(Duration::from_millis(40));
    }
    println!("\n──── disc histogram ────");
    let mut v: Vec<_> = seen.iter().collect(); v.sort_by_key(|(_, n)| std::cmp::Reverse(**n));
    for (d, n) in v { println!("  {d}  ×{n}"); }
}
