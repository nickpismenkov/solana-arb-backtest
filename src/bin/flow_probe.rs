//! Flow probe — two go/no-go measurements before we build the co-bundler:
//!   (1) DIRECT vs ROUTED: of the swaps hitting our pools, how many call the
//!       DEX program top-level (decodable from a shred → co-bundlable) vs only
//!       via CPI (opaque). This is the addressable direct-swap volume.
//!   (2) WINNING TIPS: for cross-venue arbs that landed on our pools, how much
//!       did the winner tip Jito (balance delta of a Jito tip account) and what
//!       did they net (fee-payer USDC delta)? → what it costs to compete.
//!
//! Read-only, RPC-only (use Helius). No money.
//!
//! Usage: RPC_ENDPOINT=<helius> [LIMIT=800] cargo run --release --bin flow_probe

use arb_engine::decode::{ORCA_PROGRAM, RAY_CLMM_PROGRAM};
use arb_engine::jito::{default_block_engine, get_tip_accounts};
use arb_engine::pools::pair;
use std::collections::HashSet;
use std::time::Duration;

const USDC: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..4 {
        if let Ok(resp) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = resp.into_json::<serde_json::Value>() {
                return Some(v);
            }
        }
        std::thread::sleep(Duration::from_millis(300 << attempt));
    }
    None
}

fn recent_sigs(endpoint: &str, pool: &str, limit: u32) -> Vec<String> {
    rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress","params":[pool,{"limit":limit}]}))
        .and_then(|v| v["result"].as_array().cloned())
        .unwrap_or_default()
        .iter()
        .filter(|e| e["err"].is_null())
        .filter_map(|e| e["signature"].as_str().map(String::from))
        .collect()
}

fn pct(sorted: &[f64], p: f64) -> f64 {
    if sorted.is_empty() { return 0.0; }
    let i = ((sorted.len() as f64 - 1.0) * p).round() as usize;
    sorted[i]
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("RPC_ENDPOINT").expect("RPC_ENDPOINT (use Helius)");
    let limit: u32 = std::env::var("LIMIT").ok().and_then(|s| s.parse().ok()).unwrap_or(800);
    let sleep_ms: u64 = std::env::var("SLEEP_MS").ok().and_then(|s| s.parse().ok()).unwrap_or(25);
    let cfg = pair();

    let tip_accounts: HashSet<String> = get_tip_accounts(&default_block_engine())
        .unwrap_or_default()
        .iter().map(|p| p.to_string()).collect();
    eprintln!("flow-probe {} — {} Jito tip accounts; scanning ~{limit} sigs/pool", cfg.label, tip_accounts.len());

    // Gather unique recent signatures across both pools.
    let mut sigs: HashSet<String> = HashSet::new();
    for pool in [&cfg.orca_pool, &cfg.ray_pool] {
        sigs.extend(recent_sigs(&endpoint, pool, limit));
    }
    eprintln!("scanning {} unique txns…", sigs.len());

    let (mut direct, mut routed, mut arbs) = (0u64, 0u64, 0u64);
    let mut arb_tips: Vec<f64> = Vec::new();    // lamports
    let mut arb_profits: Vec<f64> = Vec::new(); // USDC
    let mut all_tips: Vec<f64> = Vec::new();
    let mut scanned = 0u64;

    let mut fetch_fail = 0u64;
    for sig in &sigs {
        std::thread::sleep(Duration::from_millis(sleep_ms));
        let v = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getTransaction",
            "params":[sig,{"encoding":"jsonParsed","maxSupportedTransactionVersion":0,"commitment":"confirmed"}]}));
        let Some(v) = v else { fetch_fail += 1; continue };
        let r = &v["result"];
        if r.is_null() || !r["meta"]["err"].is_null() { continue; }
        scanned += 1;

        // Full ordered account list: static keys, then loaded writable, readonly.
        let mut keys: Vec<String> = r["transaction"]["message"]["accountKeys"].as_array().into_iter().flatten()
            .filter_map(|k| k["pubkey"].as_str().map(String::from)).collect();
        for grp in ["writable", "readonly"] {
            for k in r["meta"]["loadedAddresses"][grp].as_array().into_iter().flatten() {
                if let Some(s) = k.as_str() { keys.push(s.to_string()); }
            }
        }
        let key_set: HashSet<&String> = keys.iter().collect();
        let touch_orca = key_set.contains(&cfg.orca_pool);
        let touch_ray = key_set.contains(&cfg.ray_pool);
        if !touch_orca && !touch_ray { continue; }

        // Top-level program IDs.
        let top: HashSet<String> = r["transaction"]["message"]["instructions"].as_array().into_iter().flatten()
            .filter_map(|i| i["programId"].as_str().map(String::from)).collect();
        let dex_top_level = top.contains(ORCA_PROGRAM) || top.contains(RAY_CLMM_PROGRAM);
        if dex_top_level { direct += 1; } else { routed += 1; }

        // Tip: balance delta of any Jito tip account in this tx.
        let pre = r["meta"]["preBalances"].as_array();
        let post = r["meta"]["postBalances"].as_array();
        let mut tip = 0.0;
        if let (Some(pre), Some(post)) = (pre, post) {
            for (i, k) in keys.iter().enumerate() {
                if tip_accounts.contains(k) {
                    let d = post.get(i).and_then(|x| x.as_f64()).unwrap_or(0.0)
                        - pre.get(i).and_then(|x| x.as_f64()).unwrap_or(0.0);
                    if d > 0.0 { tip += d; }
                }
            }
        }
        if tip > 0.0 { all_tips.push(tip); }

        // Cross-venue arb = touches BOTH pools.
        if touch_orca && touch_ray {
            arbs += 1;
            if tip > 0.0 { arb_tips.push(tip); }
            // fee-payer USDC delta = arb profit
            let payer = r["transaction"]["message"]["accountKeys"][0]["pubkey"].as_str().unwrap_or("");
            let sum = |key: &str| -> f64 {
                r["meta"][key].as_array().into_iter().flatten()
                    .filter(|b| b["mint"] == USDC && b["owner"] == payer)
                    .filter_map(|b| b["uiTokenAmount"]["uiAmount"].as_f64()).sum()
            };
            arb_profits.push(sum("postTokenBalances") - sum("preTokenBalances"));
        }
    }

    let tot = (direct + routed).max(1);
    println!("\n═══ FLOW ({scanned} pool txns scanned, {fetch_fail} fetch fails) ═══");
    println!("DIRECT swaps (DEX top-level → decodable/co-bundlable): {direct} ({:.1}%)", 100.0 * direct as f64 / tot as f64);
    println!("ROUTED swaps (DEX via CPI → opaque in shred):          {routed} ({:.1}%)", 100.0 * routed as f64 / tot as f64);

    println!("\n═══ WINNING TIPS (SOL) ═══");
    let report = |label: &str, v: &mut Vec<f64>| {
        v.sort_by(|a, b| a.partial_cmp(b).unwrap());
        if v.is_empty() { println!("{label}: (none)"); return; }
        println!("{label}: n={} | p25={:.5} med={:.5} p75={:.5} max={:.5}",
            v.len(), pct(v, 0.25)/1e9, pct(v, 0.5)/1e9, pct(v, 0.75)/1e9, pct(v, 1.0)/1e9);
    };
    report("all pool-tx tips", &mut all_tips);
    report("cross-venue ARB tips", &mut arb_tips);

    println!("\n═══ ARBS ({arbs} cross-venue) ═══");
    arb_profits.sort_by(|a, b| a.partial_cmp(b).unwrap());
    if !arb_profits.is_empty() {
        println!("payer USDC profit: med={:.4} max={:.4} (note: excl. tip/fees; many are $0 routed user swaps)",
            pct(&arb_profits, 0.5), pct(&arb_profits, 1.0));
    }
}
