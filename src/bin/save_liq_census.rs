//! Ground truth: how many liquidations ACTUALLY happened on Save/Solend in the
//! recent window, and how many were in OUR scope (v1: single-collateral,
//! single-USDC-debt)? If real in-scope liquidations happened and our bot fired
//! zero, that's a bug/miss — not "no opportunity." Scans the program's recent
//! liquidate txs (tag 12/17), extracts repay reserve (debt) + withdraw reserve
//! (collateral) from the ix accounts, and tallies USDC-debt vs other.
//!
//! Usage: HELIUS_RPC=<url> [PAGES=6] cargo run --release --bin save_liq_census

use std::collections::HashMap;
use std::time::Duration;

const SOLEND_PROGRAM: &str = "So1endDq2YkqhipRh3WViPa8hdiSpxWy6z3Z6tMCpAo";
const USDC_RESERVE: &str = "BgxfHJDzm44T7XG68MYKx7YisTjZu73tVovyZSjJMpmw";

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
    let pages: usize = std::env::var("PAGES").ok().and_then(|s| s.parse().ok()).unwrap_or(6);

    // Page recent program signatures.
    let mut sigs: Vec<(String, Option<i64>)> = Vec::new();
    let mut before: Option<String> = None;
    for _ in 0..pages {
        let mut params = serde_json::json!({"limit": 1000});
        if let Some(b) = &before { params["before"] = serde_json::json!(b); }
        let page = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress",
            "params":[SOLEND_PROGRAM, params]})).and_then(|v| v["result"].as_array().cloned()).unwrap_or_default();
        if page.is_empty() { break; }
        before = page.last().and_then(|e| e["signature"].as_str().map(String::from));
        for e in &page {
            if e["err"].is_null() {
                sigs.push((e["signature"].as_str().unwrap_or("").to_string(), e["blockTime"].as_i64()));
            }
        }
        eprintln!("[census] paged {} sigs", sigs.len());
    }
    let span_h = match (sigs.first().and_then(|s| s.1), sigs.last().and_then(|s| s.1)) {
        (Some(newest), Some(oldest)) => (newest - oldest) as f64 / 3600.0, _ => 0.0,
    };

    let (mut liqs, mut usdc_debt, mut sol_collateral) = (0u32, 0u32, 0u32);
    let mut collateral_reserves: HashMap<String, u32> = HashMap::new();
    let mut examples: Vec<String> = Vec::new();
    for (sig, _bt) in &sigs {
        let Some(tx) = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getTransaction",
            "params":[sig, {"encoding":"jsonParsed","maxSupportedTransactionVersion":0,"commitment":"confirmed"}]})) else { continue };
        let result = &tx["result"];
        if result.is_null() { continue; }
        let mut ixs: Vec<serde_json::Value> = result["transaction"]["message"]["instructions"].as_array().cloned().unwrap_or_default();
        for inner in result["meta"]["innerInstructions"].as_array().into_iter().flatten() {
            ixs.extend(inner["instructions"].as_array().cloned().unwrap_or_default());
        }
        for ix in &ixs {
            if ix["programId"] != SOLEND_PROGRAM { continue; }
            let data = bs58::decode(ix["data"].as_str().unwrap_or("")).into_vec().unwrap_or_default();
            let Some(&tag) = data.first() else { continue };
            if tag != 12 && tag != 17 { continue; }
            liqs += 1;
            // tag 17 accounts: [3]=repay_reserve, [5]=withdraw_reserve.
            let accts = ix["accounts"].as_array().cloned().unwrap_or_default();
            let repay = accts.get(3).and_then(|a| a.as_str()).unwrap_or("");
            let withdraw = accts.get(5).and_then(|a| a.as_str()).unwrap_or("").to_string();
            if repay == USDC_RESERVE { usdc_debt += 1; }
            *collateral_reserves.entry(withdraw).or_default() += 1;
            if examples.len() < 8 {
                examples.push(format!("{sig}  repay={} withdraw={}",
                    if repay == USDC_RESERVE { "USDC" } else { &repay[..8.min(repay.len())] },
                    &accts.get(5).and_then(|a| a.as_str()).unwrap_or("?")[..8]));
            }
        }
        std::thread::sleep(Duration::from_millis(15));
    }

    println!("\n═══ Save/Solend liquidation census ═══");
    println!("window: {} txs over ~{:.1} h", sigs.len(), span_h);
    println!("LIQUIDATIONS that actually happened: {liqs}");
    println!("  of which USDC-debt (our v1 scope for debt): {usdc_debt}");
    println!("  → est rate: {:.1} liquidations/hour, {:.1} USDC-debt/hour",
        if span_h > 0.0 { liqs as f64 / span_h } else { 0.0 },
        if span_h > 0.0 { usdc_debt as f64 / span_h } else { 0.0 });
    println!("\ncollateral reserves seized (top):");
    let mut cr: Vec<_> = collateral_reserves.iter().collect();
    cr.sort_by_key(|(_, n)| std::cmp::Reverse(**n));
    for (r, n) in cr.iter().take(8) { println!("  {n:3}  {}", &r[..16.min(r.len())]); }
    println!("\nexamples:");
    for e in &examples { println!("  {e}"); }
    if usdc_debt == 0 && liqs > 0 {
        println!("\n→ liquidations happened but NONE were USDC-debt — our v1 debt scope is the gap.");
    } else if usdc_debt > 0 {
        println!("\n→ {usdc_debt} USDC-debt liquidations happened that we should be able to fire — if we fired 0, investigate why (shape filter, sizing, or timing).");
    }
}
