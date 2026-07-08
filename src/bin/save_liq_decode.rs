//! Recon for the Save (formerly Solend) liquidation integration: derive the
//! liquidate instruction from captured mainnet truth (the marginfi/Kamino
//! lesson). Save is the original SPL token-lending model — a NATIVE program, so
//! each instruction is identified by its first data byte (a u8 tag), not an
//! 8-byte Anchor discriminator.
//!
//! Two passes over recent program txs: (1) histogram the instruction tags to
//! see what exists and how often, (2) dump the first example of each tag with
//! full account list + data, so we can identify the liquidate ix (the classic
//! LiquidateObligation is tag 12; Solend's atomic
//! LiquidateObligationAndRedeemReserveCollateral is a later tag) and its exact
//! account layout before building anything.
//!
//! Usage: HELIUS_RPC=<url> [PAGES=3] cargo run --release --bin save_liq_decode


use std::time::Duration;

const SOLEND_PROGRAM: &str = "So1endDq2YkqhipRh3WViPa8hdiSpxWy6z3Z6tMCpAo";

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..4 {
        if let Ok(r) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = r.into_json::<serde_json::Value>() { return Some(v); }
        }
        std::thread::sleep(Duration::from_millis(400 << attempt));
    }
    None
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let pages: usize = std::env::var("PAGES").ok().and_then(|s| s.parse().ok()).unwrap_or(3);

    // Page back through recent program signatures.
    let mut sigs: Vec<String> = Vec::new();
    let mut before: Option<String> = None;
    for _ in 0..pages {
        let mut params = serde_json::json!({"limit": 1000});
        if let Some(b) = &before { params["before"] = serde_json::json!(b); }
        let page = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress",
            "params":[SOLEND_PROGRAM, params]})).and_then(|v| v["result"].as_array().cloned()).unwrap_or_default();
        if page.is_empty() { break; }
        before = page.last().and_then(|e| e["signature"].as_str().map(String::from));
        sigs.extend(page.iter().filter(|e| e["err"].is_null()).filter_map(|e| e["signature"].as_str().map(String::from)));
        eprintln!("[save] paged: {} sigs", sigs.len());
    }

    // Targeted: dump the FULL tx for the liquidate tags only —
    // 12 = LiquidateObligation, 17 = LiquidateObligationAndRedeemReserveCollateral.
    // Print every Solend ix in the tx (tag + accounts + data) so we get the
    // liquidate account layout AND the surrounding refresh_reserve/obligation ixs.
    let want: [u8; 2] = [12, 17];
    let target: usize = std::env::var("TARGET").ok().and_then(|s| s.parse().ok()).unwrap_or(3);
    let mut found = 0usize;
    for sig in &sigs {
        if found >= target { break; }
        let Some(tx) = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getTransaction",
            "params":[sig, {"encoding":"jsonParsed","maxSupportedTransactionVersion":0,"commitment":"confirmed"}]})) else { continue };
        let result = &tx["result"];
        if result.is_null() { continue; }
        let mut ixs: Vec<serde_json::Value> = result["transaction"]["message"]["instructions"].as_array().cloned().unwrap_or_default();
        for inner in result["meta"]["innerInstructions"].as_array().into_iter().flatten() {
            ixs.extend(inner["instructions"].as_array().cloned().unwrap_or_default());
        }
        let has_liq = ixs.iter().any(|ix| ix["programId"] == SOLEND_PROGRAM
            && bs58::decode(ix["data"].as_str().unwrap_or("")).into_vec().map(|d| d.first().map(|t| want.contains(t)).unwrap_or(false)).unwrap_or(false));
        if !has_liq { std::thread::sleep(Duration::from_millis(20)); continue; }
        found += 1;
        println!("\n════════ LIQUIDATION tx #{found}: {sig}");
        println!("  fee payer: {}", result["transaction"]["message"]["accountKeys"][0]["pubkey"]);
        for ix in &ixs {
            if ix["programId"] != SOLEND_PROGRAM { continue; }
            let data = bs58::decode(ix["data"].as_str().unwrap_or("")).into_vec().unwrap_or_default();
            if data.is_empty() { continue; }
            let tag = data[0];
            let name = match tag { 3=>"RefreshReserve",7=>"RefreshObligation",12=>"LiquidateObligation",
                17=>"LiquidateObligationAndRedeemReserveCollateral",_=>"?" };
            println!("  ── tag {tag} {name}  ({}B data)  data={}", data.len(), data.iter().map(|b| format!("{b:02x}")).collect::<String>());
            for (i, a) in ix["accounts"].as_array().into_iter().flatten().enumerate() {
                println!("      [{i:2}] {}", a.as_str().unwrap_or("?"));
            }
        }
    }
    if found == 0 { println!("no liquidation (tag 12/17) in {} sigs — raise PAGES", sigs.len()); }
}
