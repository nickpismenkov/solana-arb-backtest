//! Capture a REAL marginfi liquidation that embeds a Pyth price update, and
//! dump the Pyth-receiver instruction(s) — program, discriminator, accounts,
//! data length — so the embedded-update builder is derived from observed
//! mainnet truth (the marginfi/Kamino lesson). Top liquidators post the fresh
//! Pyth price IN THEIR OWN TX (post_update / post_update_atomic) right before
//! the liquidate, so they don't wait for anyone's crank. This finds an example
//! to copy.
//!
//! Usage: HELIUS_RPC=<url> [SAMPLES=3] [LIMIT=8000] cargo run --release --bin mfi_pyth_decode

use std::time::Duration;

const MARGINFI: &str = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA";
const PYTH_RECEIVER: &str = "rec5EKMGg6MxZYaMdyBfgwp4d5rB9T1VQH5pJv5LtFJ";
// LendingAccountLiquidate disc.
const DISC_LIQ: [u8; 8] = [214, 169, 151, 213, 251, 167, 86, 219];

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
    println!("  {label}: prog={} disc={:02x?} data_len={}",
        ix["programId"].as_str().unwrap_or("?"), &data[..data.len().min(8)], data.len());
    for (i, a) in ix["accounts"].as_array().into_iter().flatten().enumerate() {
        println!("    [{i:2}] {}", a.as_str().unwrap_or("?"));
    }
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let samples: usize = std::env::var("SAMPLES").ok().and_then(|s| s.parse().ok()).unwrap_or(3);
    let limit: u32 = std::env::var("LIMIT").ok().and_then(|s| s.parse().ok()).unwrap_or(8000);

    // Page marginfi signatures back until we have `limit`.
    let mut sigs: Vec<String> = Vec::new();
    let mut before: Option<String> = None;
    while (sigs.len() as u32) < limit {
        let mut params = serde_json::json!({"limit": 1000});
        if let Some(b) = &before { params["before"] = serde_json::json!(b); }
        let page = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress",
            "params":[MARGINFI, params]})).and_then(|v| v["result"].as_array().cloned()).unwrap_or_default();
        if page.is_empty() { break; }
        before = page.last().and_then(|e| e["signature"].as_str().map(String::from));
        sigs.extend(page.iter().filter(|e| e["err"].is_null()).filter_map(|e| e["signature"].as_str().map(String::from)));
        eprintln!("[pyth] paged: {} sigs", sigs.len());
    }

    let mut found = 0usize;
    for sig in &sigs {
        if found >= samples { break; }
        let Some(tx) = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getTransaction",
            "params":[sig, {"encoding":"jsonParsed","maxSupportedTransactionVersion":0,"commitment":"confirmed"}]})) else { continue };
        let result = &tx["result"];
        if result.is_null() { continue; }

        let mut all: Vec<(String, serde_json::Value)> = Vec::new();
        for (ti, ix) in result["transaction"]["message"]["instructions"].as_array().into_iter().flatten().enumerate() {
            all.push((format!("top[{ti}]"), ix.clone()));
        }
        for inner in result["meta"]["innerInstructions"].as_array().into_iter().flatten() {
            let p = inner["index"].as_u64().unwrap_or(0);
            for (ii, ix) in inner["instructions"].as_array().into_iter().flatten().enumerate() {
                all.push((format!("inner[{p}.{ii}]"), ix.clone()));
            }
        }
        let has_liq = all.iter().any(|(_, ix)| {
            ix["programId"] == MARGINFI &&
            bs58::decode(ix["data"].as_str().unwrap_or("")).into_vec().map(|d| d.len() >= 8 && d[..8] == DISC_LIQ).unwrap_or(false)
        });
        let has_pyth = all.iter().any(|(_, ix)| ix["programId"] == PYTH_RECEIVER);
        if !(has_liq && has_pyth) { std::thread::sleep(Duration::from_millis(50)); continue; }

        found += 1;
        println!("\n════ marginfi liq + Pyth update #{found}: {sig}");
        println!("  fee payer: {}", result["transaction"]["message"]["accountKeys"][0]["pubkey"]);
        for (label, ix) in &all {
            if ix["programId"] == PYTH_RECEIVER {
                dump_ix(&format!("{label} PYTH"), ix);
            } else if ix["programId"] == MARGINFI {
                let data = bs58::decode(ix["data"].as_str().unwrap_or("")).into_vec().unwrap_or_default();
                if data.len() >= 8 && data[..8] == DISC_LIQ { println!("  {label} MARGINFI_LIQUIDATE"); }
            }
        }
        std::thread::sleep(Duration::from_millis(50));
    }
    if found == 0 { println!("no marginfi-liq-with-embedded-Pyth-update found in {} sigs — raise LIMIT", sigs.len()); }
}
