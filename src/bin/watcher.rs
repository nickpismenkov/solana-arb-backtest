//! Ground-truth competitor watcher — SEPARATE process, fully off the executor's
//! hot path. Rolling scan of both pools' recent transactions; a tx whose
//! resolved account set touches BOTH pools is a cross-venue arb. If the signer
//! isn't us, a competitor captured it. We estimate their profit (fee-payer USDC
//! delta) and cross-reference our own decisions.jsonl to classify what happened
//! on our side: never-triggered / skipped / fired-and-lost. Appends missed.jsonl.
//!
//! This is the only way to see the opportunities our own logs can't — the ones
//! we didn't act on or lost. Runs on RPC, seconds-lagged; never touches the
//! executor.
//!
//! Usage: RPC_ENDPOINT=<url> [RUN_DIR=runs] [POLL_SECS=10] [OUR_WALLET=<pk>] \
//!   cargo run --release --bin watcher

use arb_engine::pools::pair;
use std::collections::HashSet;
use std::fs::{create_dir_all, File, OpenOptions};
use std::io::{BufRead, Write};
use std::time::Duration;

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..4 {
        if let Ok(resp) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = resp.into_json::<serde_json::Value>() {
                return Some(v);
            }
        }
        std::thread::sleep(Duration::from_millis(400 << attempt));
    }
    None
}

fn recent_sigs(endpoint: &str, pool: &str, limit: u32) -> Vec<(String, u64)> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress",
        "params":[pool,{"limit":limit}]}));
    v.and_then(|v| v["result"].as_array().cloned())
        .unwrap_or_default()
        .iter()
        .filter(|e| e["err"].is_null())
        .filter_map(|e| Some((e["signature"].as_str()?.to_string(), e["slot"].as_u64().unwrap_or(0))))
        .collect()
}

/// Full resolved account key set (static + ALT-loaded) + fee payer + USDC delta.
fn tx_touch_and_profit(endpoint: &str, sig: &str) -> Option<(HashSet<String>, String, f64)> {
    const USDC: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getTransaction",
        "params":[sig,{"encoding":"jsonParsed","maxSupportedTransactionVersion":0,"commitment":"confirmed"}]}))?;
    let r = &v["result"];
    if r.is_null() || !r["meta"]["err"].is_null() {
        return None;
    }
    let mut keys: HashSet<String> = HashSet::new();
    for k in r["transaction"]["message"]["accountKeys"].as_array().into_iter().flatten() {
        if let Some(s) = k["pubkey"].as_str() {
            keys.insert(s.to_string());
        }
    }
    for grp in ["writable", "readonly"] {
        for k in r["meta"]["loadedAddresses"][grp].as_array().into_iter().flatten() {
            if let Some(s) = k.as_str() {
                keys.insert(s.to_string());
            }
        }
    }
    let payer = r["transaction"]["message"]["accountKeys"][0]["pubkey"].as_str()?.to_string();
    let sum = |key: &str| -> f64 {
        r["meta"][key].as_array().into_iter().flatten()
            .filter(|b| b["mint"] == USDC && b["owner"] == payer.as_str())
            .filter_map(|b| b["uiTokenAmount"]["uiAmount"].as_f64())
            .sum()
    };
    let profit = sum("postTokenBalances") - sum("preTokenBalances");
    Some((keys, payer, profit))
}

/// Slots we triggered / fired on, from our decisions ledger.
fn our_slots(dir: &str) -> (HashSet<u64>, HashSet<u64>) {
    let (mut triggered, mut fired) = (HashSet::new(), HashSet::new());
    if let Ok(f) = File::open(format!("{dir}/decisions.jsonl")) {
        for line in std::io::BufReader::new(f).lines().map_while(Result::ok) {
            if let Ok(v) = serde_json::from_str::<serde_json::Value>(&line) {
                if let Some(slot) = v["slot"].as_u64() {
                    triggered.insert(slot);
                    if v["fired"] == true {
                        fired.insert(slot);
                    }
                }
            }
        }
    }
    (triggered, fired)
}

fn main() {
    let _ = dotenvy::dotenv();
    let endpoint = std::env::var("RPC_ENDPOINT").expect("RPC_ENDPOINT");
    let dir = std::env::var("RUN_DIR").unwrap_or_else(|_| "runs".into());
    let poll = std::env::var("POLL_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(10u64);
    let our_wallet = std::env::var("OUR_WALLET").unwrap_or_default();
    let cfg = pair();
    let _ = create_dir_all(&dir);

    eprintln!("watcher {} — scanning for cross-venue arbs every {poll}s → {dir}/missed.jsonl", cfg.label);
    let mut seen: HashSet<String> = HashSet::new();
    let (mut competitor_wins, mut our_wins) = (0u64, 0u64);

    loop {
        let mut sigs: Vec<(String, u64)> = Vec::new();
        for pool in [&cfg.orca_pool, &cfg.ray_pool] {
            sigs.extend(recent_sigs(&endpoint, pool, 40));
        }
        let (triggered, fired) = our_slots(&dir);

        for (sig, slot) in sigs {
            if !seen.insert(sig.clone()) {
                continue;
            }
            let Some((keys, payer, profit)) = tx_touch_and_profit(&endpoint, &sig) else { continue };
            // Cross-venue arb = touches BOTH pools in one tx.
            if !(keys.contains(&cfg.orca_pool) && keys.contains(&cfg.ray_pool)) {
                continue;
            }
            let ours = !our_wallet.is_empty() && payer == our_wallet;
            // The arb lands a few slots after the victim we'd have triggered on;
            // match our trigger/fire within a small window ending at the arb slot.
            let in_window = |set: &HashSet<u64>| (0..=5).any(|d| set.contains(&slot.saturating_sub(d)));
            let status = if ours {
                our_wins += 1;
                "we_won"
            } else if in_window(&fired) {
                "fired_lost"
            } else if in_window(&triggered) {
                "triggered_skipped"
            } else {
                "not_triggered"
            };
            if !ours {
                competitor_wins += 1;
            }
            let row = serde_json::json!({
                "sig": sig, "payer": payer, "competitor": !ours,
                "est_profit_usd": profit, "our_status": status,
            });
            if let Ok(mut f) = OpenOptions::new().create(true).append(true).open(format!("{dir}/missed.jsonl")) {
                let _ = writeln!(f, "{row}");
            }
            eprintln!("arb {} by {}… profit ${:.4} [{}]", &sig[..12.min(sig.len())], &payer[..8.min(payer.len())], profit, status);
        }
        // Cap the seen-set so it doesn't grow unbounded.
        if seen.len() > 20_000 {
            seen.clear();
        }
        eprintln!("[watcher] competitor_wins={competitor_wins} our_wins={our_wins}");
        std::thread::sleep(Duration::from_secs(poll));
    }
}
