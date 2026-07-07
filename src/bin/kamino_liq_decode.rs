//! Capture REAL Kamino (KLend) liquidation transactions and dump the exact
//! instruction sequence — account lists (resolved through ALTs via jsonParsed)
//! and data bytes — for refresh_reserve / refresh_obligation /
//! liquidate_obligation_and_redeem_reserve_collateral. The builders in
//! kamino.rs are derived from THESE captured bytes, not from docs (the
//! marginfi lesson: build from observed mainnet truth, verify by simulation).
//!
//! Usage: HELIUS_RPC=<url> [SAMPLES=3] [LIMIT=1000] cargo run --release --bin kamino_liq_decode

use std::time::Duration;

const KLEND: &str = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD";
const DISC_LIQ_V1: [u8; 8] = [177, 71, 154, 188, 226, 133, 74, 55];
const DISC_LIQ_V2: [u8; 8] = [162, 161, 35, 143, 30, 187, 185, 103];

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..4 {
        if let Ok(resp) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = resp.into_json::<serde_json::Value>() { return Some(v); }
        }
        std::thread::sleep(Duration::from_millis(400 << attempt));
    }
    None
}

fn dump_ix(label: &str, ix: &serde_json::Value) {
    let data = bs58::decode(ix["data"].as_str().unwrap_or("")).into_vec().unwrap_or_default();
    let disc = &data[..data.len().min(8)];
    println!("  {label}: disc={:02x?} data_len={} rest={:02x?}", disc, data.len(), &data[data.len().min(8)..]);
    for (i, a) in ix["accounts"].as_array().into_iter().flatten().enumerate() {
        println!("    [{i:2}] {}", a.as_str().unwrap_or("?"));
    }
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let samples: usize = std::env::var("SAMPLES").ok().and_then(|s| s.parse().ok()).unwrap_or(3);
    let limit: u32 = std::env::var("LIMIT").ok().and_then(|s| s.parse().ok()).unwrap_or(1000);

    // Page back until we have enough signatures (one page ≈ a minute of KLend
    // activity; liquidations are ~1 per 5 min).
    let mut sigs: Vec<String> = Vec::new();
    let mut before: Option<String> = None;
    while (sigs.len() as u32) < limit {
        let mut params = serde_json::json!({"limit": 1000});
        if let Some(b) = &before { params["before"] = serde_json::json!(b); }
        let page = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress",
            "params":[KLEND, params]}))
            .and_then(|v| v["result"].as_array().cloned()).unwrap_or_default();
        if page.is_empty() { break; }
        before = page.last().and_then(|e| e["signature"].as_str().map(String::from));
        sigs.extend(page.iter().filter(|e| e["err"].is_null())
            .filter_map(|e| e["signature"].as_str().map(String::from)));
        eprintln!("[decode] paged: {} signatures", sigs.len());
    }

    let mut found = 0usize;
    for sig in &sigs {
        if found >= samples { break; }
        let Some(tx) = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getTransaction",
            "params":[sig, {"encoding":"jsonParsed","maxSupportedTransactionVersion":0,"commitment":"confirmed"}]})) else { continue };
        let result = &tx["result"];
        if result.is_null() { continue; }

        // Gather ALL KLend instructions in execution order: top-level + inner.
        let mut klend_ixs: Vec<(String, serde_json::Value)> = Vec::new();
        for (ti, ix) in result["transaction"]["message"]["instructions"].as_array().into_iter().flatten().enumerate() {
            if ix["programId"] == KLEND { klend_ixs.push((format!("top[{ti}]"), ix.clone())); }
        }
        for inner in result["meta"]["innerInstructions"].as_array().into_iter().flatten() {
            let parent = inner["index"].as_u64().unwrap_or(0);
            for (ii, ix) in inner["instructions"].as_array().into_iter().flatten().enumerate() {
                if ix["programId"] == KLEND { klend_ixs.push((format!("inner[{parent}.{ii}]"), ix.clone())); }
            }
        }
        let has_liq = klend_ixs.iter().any(|(_, ix)| {
            let data = bs58::decode(ix["data"].as_str().unwrap_or("")).into_vec().unwrap_or_default();
            data.len() >= 8 && (data[..8] == DISC_LIQ_V1 || data[..8] == DISC_LIQ_V2)
        });
        if !has_liq { std::thread::sleep(Duration::from_millis(60)); continue; }

        found += 1;
        println!("\n════ liquidation tx #{found}: {sig}");
        println!("  fee payer: {}", result["transaction"]["message"]["accountKeys"][0]["pubkey"]);
        for (label, ix) in &klend_ixs {
            let data = bs58::decode(ix["data"].as_str().unwrap_or("")).into_vec().unwrap_or_default();
            let name = if data.len() >= 8 {
                match &data[..8] {
                    d if *d == DISC_LIQ_V1 => "LIQUIDATE_v1",
                    d if *d == DISC_LIQ_V2 => "LIQUIDATE_v2",
                    _ => "other",
                }
            } else { "?" };
            dump_ix(&format!("{label} {name}"), ix);
        }
        std::thread::sleep(Duration::from_millis(60));
    }
    if found == 0 { println!("no liquidation found in the last {} txs — raise LIMIT", sigs.len()); }
}
