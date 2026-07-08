//! Dump a REAL sponsored-feed crank tx from mainnet so the crank builder is
//! derived from observed truth (the marginfi/Kamino lesson): scan recent Pyth
//! receiver txs for one that goes through the PUSH WRAPPER (program id starts
//! "pythWSns" — the only writer of the shared sponsored feeds marginfi reads),
//! then print the FULL instruction sequence — Wormhole encoded-VAA
//! init/write/verify, the wrapper update — with full program ids, account
//! lists (signer/writable flags), discriminators, and data hex. Also decodes
//! the target PriceUpdateV2 feed (feed id, write_authority, publish_time).
//!
//! Usage: HELIUS_RPC=<url> [SAMPLES=2] [LIMIT=300] cargo run --release --bin pyth_crank_decode

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

fn hexs(b: &[u8]) -> String { b.iter().map(|x| format!("{x:02x}")).collect() }

fn b64acc(v: &serde_json::Value) -> Option<Vec<u8>> {
    use base64::Engine;
    base64::engine::general_purpose::STANDARD.decode(v["result"]["value"]["data"].get(0)?.as_str()?).ok()
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let samples: usize = std::env::var("SAMPLES").ok().and_then(|s| s.parse().ok()).unwrap_or(2);
    let limit: u32 = std::env::var("LIMIT").ok().and_then(|s| s.parse().ok()).unwrap_or(300);

    let sigs = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress",
        "params":[PYTH_RECEIVER, {"limit": limit}]}))
        .and_then(|v| v["result"].as_array().cloned()).unwrap_or_default();
    let sigs: Vec<String> = sigs.iter().filter(|e| e["err"].is_null())
        .filter_map(|e| e["signature"].as_str().map(String::from)).collect();
    eprintln!("[crank] {} receiver signatures", sigs.len());

    let mut found = 0usize;
    let mut feeds_seen: HashMap<String, usize> = HashMap::new();
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
        // Sponsored-feed cranks go through the push wrapper.
        let wrapper: Option<String> = all.iter()
            .filter_map(|(_, ix)| ix["programId"].as_str())
            .find(|p| p.starts_with("pythWSns")).map(String::from);
        let Some(wrapper) = wrapper else { std::thread::sleep(Duration::from_millis(30)); continue };

        found += 1;
        println!("\n════ sponsored crank #{found}: {sig}");
        println!("  push wrapper program: {wrapper}");
        println!("  ── accountKeys (s=signer w=writable) ──");
        for (i, k) in result["transaction"]["message"]["accountKeys"].as_array().into_iter().flatten().enumerate() {
            println!("    [{i:2}] {} {}{}", k["pubkey"].as_str().unwrap_or("?"),
                if k["signer"].as_bool().unwrap_or(false) { "s" } else { "-" },
                if k["writable"].as_bool().unwrap_or(false) { "w" } else { "-" });
        }
        println!("  ── instruction sequence ──");
        for (label, ix) in &all {
            let prog = ix["programId"].as_str().unwrap_or("?");
            let data = bs58::decode(ix["data"].as_str().unwrap_or("")).into_vec().unwrap_or_default();
            let disc = hexs(&data[..data.len().min(8)]);
            println!("  {label}: prog={prog}");
            println!("      disc={disc} data_len={}", data.len());
            // Full data hex for everything except huge VAA-write chunks (cap 96B shown).
            if data.len() <= 96 { println!("      data={}", hexs(&data)); }
            else { println!("      data[..96]={}…", hexs(&data[..96])); }
            for (i, a) in ix["accounts"].as_array().into_iter().flatten().enumerate() {
                println!("      [{i:2}] {}", a.as_str().unwrap_or("?"));
            }
        }
        // Target feed = writable non-signer account of the wrapper's ix that
        // decodes as PriceUpdateV2. Just decode every wrapper-ix account.
        for (_, ix) in all.iter().filter(|(_, ix)| ix["programId"] == wrapper.as_str()) {
            for a in ix["accounts"].as_array().into_iter().flatten() {
                let Some(pk) = a.as_str() else { continue };
                let Some(info) = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
                    "params":[pk, {"encoding":"base64"}]})) else { continue };
                let Some(bytes) = b64acc(&info) else { continue };
                if let Some((fid, usd, ts)) = arb_engine::liquidation::decode_price_update_v2(&bytes) {
                    let wa = bytes.get(8..40).map(hexs).unwrap_or_default();
                    let self_hex = hexs(&bs58::decode(pk).into_vec().unwrap_or_default());
                    println!("  ── target feed {pk}");
                    println!("      feed_id={} price=${usd:.4} publish_time={ts}", hexs(&fid));
                    println!("      write_authority==self: {}", wa == self_hex);
                    *feeds_seen.entry(pk.to_string()).or_default() += 1;
                }
            }
        }
        std::thread::sleep(Duration::from_millis(40));
    }
    println!("\n──── sponsored feeds seen ────");
    for (f, n) in &feeds_seen { println!("  {f}  ×{n}"); }
    if found == 0 { println!("no push-wrapper crank in {} sigs — raise LIMIT", sigs.len()); }
}
