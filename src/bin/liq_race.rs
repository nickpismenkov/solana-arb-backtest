//! Race monitor — the decisive "are we losing on SPEED or on TIP?" diagnostic.
//!
//! Scans actual on-chain liquidations that COMPETITORS won on Solend, then
//! cross-references our own detection log ({RUN_DIR}/decisions.jsonl, which
//! records the obligation + timestamp each time we processed it). For every
//! real liquidation it classifies us as:
//!   AHEAD   — we had flagged that obligation BEFORE it was liquidated → we
//!             lost the fill on ACTION/TIP (auction), not detection.
//!   BEHIND  — we flagged it only AFTER it was already liquidated → detection
//!             was too slow.
//!   MISSED  — we never saw it at all → detection missed it entirely (poll too
//!             slow / not in the watch-set).
//! The AHEAD vs BEHIND/MISSED split tells us exactly which bottleneck to fix.
//!
//! Usage: HELIUS_RPC=<url> [RUN_DIR=runs/save] [PAGES=6] cargo run --release --bin liq_race

use std::collections::HashMap;
use std::io::{BufRead, BufReader};
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

/// Earliest unix-second we logged each obligation, from our decisions ledger.
fn our_first_seen(run_dir: &str) -> HashMap<String, u64> {
    let mut m: HashMap<String, u64> = HashMap::new();
    let path = format!("{run_dir}/decisions.jsonl");
    if let Ok(f) = std::fs::File::open(&path) {
        for line in BufReader::new(f).lines().map_while(Result::ok) {
            let Ok(v) = serde_json::from_str::<serde_json::Value>(&line) else { continue };
            // Save/Kamino key the account as "obligation"; marginfi as "liquidatee".
            let acct = v.get("obligation").or_else(|| v.get("liquidatee")).and_then(|x| x.as_str());
            let t = v.get("t").and_then(|x| x.as_u64());
            if let (Some(a), Some(t)) = (acct, t) {
                m.entry(a.to_string()).and_modify(|e| *e = (*e).min(t)).or_insert(t);
            }
        }
    }
    m
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let run_dir = std::env::var("RUN_DIR").unwrap_or_else(|_| "runs/save".into());
    let pages: usize = std::env::var("PAGES").ok().and_then(|s| s.parse().ok()).unwrap_or(6);

    let seen = our_first_seen(&run_dir);
    eprintln!("[race] loaded {} distinct obligations we logged (from {run_dir}/decisions.jsonl)", seen.len());

    // Page recent Solend signatures.
    let mut sigs: Vec<(String, Option<u64>)> = Vec::new();
    let mut before: Option<String> = None;
    for _ in 0..pages {
        let mut params = serde_json::json!({"limit": 1000});
        if let Some(b) = &before { params["before"] = serde_json::json!(b); }
        let page = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress",
            "params":[SOLEND_PROGRAM, params]})).and_then(|v| v["result"].as_array().cloned()).unwrap_or_default();
        if page.is_empty() { break; }
        before = page.last().and_then(|e| e["signature"].as_str().map(String::from));
        for e in &page {
            if e["err"].is_null() { sigs.push((e["signature"].as_str().unwrap_or("").into(), e["blockTime"].as_u64())); }
        }
    }
    eprintln!("[race] scanning {} Solend txs for competitor liquidations …", sigs.len());

    let (mut ahead, mut behind, mut missed, mut total) = (0u32, 0u32, 0u32, 0u32);
    let mut ahead_secs: Vec<i64> = Vec::new();
    for (sig, bt) in &sigs {
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
            // obligation account index: tag 17 (LiquidateAndRedeem) → [10]; tag 12 → [6].
            let idx = match tag { 17 => 10, 12 => 6, _ => continue };
            let accts = ix["accounts"].as_array().cloned().unwrap_or_default();
            let Some(obl) = accts.get(idx).and_then(|a| a.as_str()) else { continue };
            total += 1;
            let landed = bt.unwrap_or(0);
            match seen.get(obl) {
                Some(&our_t) if our_t <= landed => {
                    ahead += 1;
                    ahead_secs.push(landed as i64 - our_t as i64);
                }
                Some(_) => behind += 1,
                None => missed += 1,
            }
        }
        std::thread::sleep(Duration::from_millis(15));
    }

    println!("\n═══ race analysis (Solend, vs our {run_dir} detections) ═══");
    println!("competitor liquidations seen: {total}");
    println!("  AHEAD  (we flagged it BEFORE it was liquidated → lost on TIP/ACTION): {ahead}");
    println!("  BEHIND (flagged only after it was already gone → detection slow):     {behind}");
    println!("  MISSED (never saw it at all → detection missed entirely):             {missed}");
    if !ahead_secs.is_empty() {
        ahead_secs.sort_unstable();
        let med = ahead_secs[ahead_secs.len() / 2];
        println!("  of the AHEAD ones, median lead time: {med}s (we had this long to win the auction)");
    }
    println!("\nVERDICT:");
    if total == 0 {
        println!("  no competitor liquidations in window — raise PAGES or run during activity.");
    } else if ahead >= behind + missed {
        println!("  Mostly AHEAD → detection is NOT the bottleneck; we're losing the AUCTION.");
        println!("  Optimize: tip sizing (TIP_FRACTION_BPS), arm coverage, submit colocation — not detection.");
    } else {
        println!("  Mostly BEHIND/MISSED → DETECTION SPEED is the bottleneck.");
        println!("  Optimize: event-driven Lazer trigger (kill polls), watch-set coverage, crank front-run.");
    }
}
