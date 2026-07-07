//! Kamino continuous liquidation-opportunity monitor (read-only, long-run).
//!
//! Full-scans obligations occasionally to build a watch-set of near-liquidation
//! positions, then polls only that set + its reserves frequently — recomputing
//! health at each reserve's latest cached price. Logs when a position crosses
//! liquidatable (APPEARED) and when it's taken/recovers (RESOLVED, with survival
//! seconds). Over hours this measures real opportunity flow + how fast the
//! competition takes them = the go/no-go for a Kamino liquidation executor.
//!
//! Usage: HELIUS_RPC=<url> [POLL_SECS=15] [FULL_SCAN_SECS=300] [WATCH_RATIO=0.92]
//!        [MIN_COLLATERAL_USD=100] cargo run --release --bin liq_kamino_monitor

use arb_engine::kamino::{self, Obligation, Reserve};
use solana_pubkey::Pubkey;
use std::collections::{HashMap, HashSet};
use std::io::Write;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

fn now_ts() -> u64 { SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs() }

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..4 {
        if let Ok(resp) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = resp.into_json::<serde_json::Value>() { return Some(v); }
        }
        std::thread::sleep(Duration::from_millis(400 << attempt));
    }
    None
}
fn b64(d: &serde_json::Value) -> Option<Vec<u8>> {
    use base64::Engine;
    base64::engine::general_purpose::STANDARD.decode(d.get(0)?.as_str()?).ok()
}
fn get_multiple(endpoint: &str, keys: &[Pubkey], slice_len: u64) -> HashMap<Pubkey, Vec<u8>> {
    let mut out = HashMap::new();
    for chunk in keys.chunks(100) {
        let strs: Vec<String> = chunk.iter().map(|k| k.to_string()).collect();
        let Some(v) = rpc(endpoint, serde_json::json!({
            "jsonrpc":"2.0","id":1,"method":"getMultipleAccounts",
            "params":[strs, {"encoding":"base64","dataSlice":{"offset":0,"length":slice_len}}]
        })) else { continue };
        for (i, acc) in v["result"]["value"].as_array().into_iter().flatten().enumerate() {
            if let Some(bytes) = acc.get("data").and_then(b64) { out.insert(chunk[i], bytes); }
        }
        std::thread::sleep(Duration::from_millis(40));
    }
    out
}

/// Fetch reserves referenced by a set of obligations, decode → map.
fn fetch_reserves(endpoint: &str, obs: &[(Pubkey, Obligation)]) -> HashMap<Pubkey, Reserve> {
    let pks: Vec<Pubkey> = obs.iter()
        .flat_map(|(_, o)| o.deposits.iter().map(|d| d.0).chain(o.borrows.iter().map(|b| b.0)))
        .collect::<HashSet<_>>().into_iter().collect();
    get_multiple(endpoint, &pks, 5016).iter()
        .filter_map(|(pk, raw)| Reserve::decode(raw).map(|r| (*pk, r)))
        .collect()
}

/// Full scan → all debt obligations (with their account pubkey).
fn full_scan(endpoint: &str) -> Vec<(Pubkey, Obligation)> {
    let resp = rpc(endpoint, serde_json::json!({
        "jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[kamino::KLEND_PROGRAM, {"encoding":"base64","dataSlice":{"offset":0,"length":2288},
            "filters":[{"dataSize": kamino::OBLIGATION_SIZE}]}]
    }));
    resp.as_ref().and_then(|v| v["result"].as_array()).cloned().unwrap_or_default().iter()
        .filter_map(|e| {
            let pk = e["pubkey"].as_str()?.parse::<Pubkey>().ok()?;
            let o = Obligation::decode(&b64(&e["account"]["data"])?)?;
            if o.borrows.is_empty() { None } else { Some((pk, o)) }
        }).collect()
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let poll = Duration::from_secs(std::env::var("POLL_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(15));
    let full_scan_secs: u64 = std::env::var("FULL_SCAN_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(300);
    let watch_ratio: f64 = std::env::var("WATCH_RATIO").ok().and_then(|s| s.parse().ok()).unwrap_or(0.92);
    let min_collateral: f64 = std::env::var("MIN_COLLATERAL_USD").ok().and_then(|s| s.parse().ok()).unwrap_or(100.0);

    let _ = std::fs::create_dir_all("runs");
    let mut log = std::fs::OpenOptions::new().create(true).append(true).open("runs/kamino_opportunities.jsonl")
        .expect("open log");
    macro_rules! emit { ($v:expr) => {{ let _ = writeln!(log, "{}", $v); let _ = log.flush(); }} }

    eprintln!("[kmon] poll={}s full_scan={}s watch_ratio={} min_collateral=${}",
        poll.as_secs(), full_scan_secs, watch_ratio, min_collateral);

    // watch-set: account pubkey → obligation (positions refreshed each poll)
    let mut watch: HashMap<Pubkey, Obligation> = HashMap::new();
    let mut last_full = 0u64;
    let mut open: HashMap<Pubkey, (u64, f64, String)> = HashMap::new(); // acct → (first_ts, peak_collateral, owner)
    let (mut appeared, mut resolved) = (0u64, 0u64);
    let mut durations: Vec<u64> = Vec::new();

    loop {
        // Periodic full scan → refresh watch-set (near-liquidation + trustworthy).
        if now_ts().saturating_sub(last_full) >= full_scan_secs {
            eprintln!("[kmon {}] full scan …", now_ts());
            let all = full_scan(&endpoint);
            let reserves = fetch_reserves(&endpoint, &all);
            let mut new_watch = HashMap::new();
            for (pk, o) in all {
                let r = kamino::recompute(&o, &reserves);
                if r.trustworthy() && r.deposited_value >= min_collateral && r.ratio() >= watch_ratio {
                    new_watch.insert(pk, o);
                }
            }
            eprintln!("[kmon {}] watch-set: {} obligations (ratio ≥ {}, collateral ≥ ${})",
                now_ts(), new_watch.len(), watch_ratio, min_collateral);
            watch = new_watch;
            last_full = now_ts();
        }

        // Poll: fresh obligation data + fresh reserve prices → recompute.
        let watch_pks: Vec<Pubkey> = watch.keys().copied().collect();
        for (pk, raw) in get_multiple(&endpoint, &watch_pks, 2288) {
            if let Some(o) = Obligation::decode(&raw) { watch.insert(pk, o); }
        }
        let obs: Vec<(Pubkey, Obligation)> = watch.iter().map(|(k, v)| (*k, v.clone())).collect();
        let reserves = fetch_reserves(&endpoint, &obs);

        let ts = now_ts();
        let mut cur_liq = 0usize;
        let mut tightest = 0.0f64;
        for (pk, o) in &obs {
            let r = kamino::recompute(o, &reserves);
            if !r.trustworthy() { continue; }
            tightest = tightest.max(r.ratio());
            if r.liquidatable() && r.deposited_value >= min_collateral {
                cur_liq += 1;
                let e = open.entry(*pk).or_insert_with(|| {
                    appeared += 1;
                    emit!(serde_json::json!({"ts":ts,"event":"appeared","protocol":"kamino",
                        "account":pk.to_string(),"owner":o.owner.to_string(),
                        "collateral_usd":(r.deposited_value*100.0).round()/100.0,
                        "debt_usd":(r.bf_adjusted_debt*100.0).round()/100.0,
                        "ratio":(r.ratio()*10000.0).round()/10000.0}));
                    eprintln!("[APPEARED {}] {}… collateral=${:.0} ratio={:.4}", ts, &o.owner.to_string()[..8], r.deposited_value, r.ratio());
                    (ts, r.deposited_value, o.owner.to_string())
                });
                e.1 = e.1.max(r.deposited_value);
            } else if let Some((t0, peak, owner)) = open.remove(pk) {
                resolved += 1;
                let dur = ts.saturating_sub(t0);
                durations.push(dur);
                emit!(serde_json::json!({"ts":ts,"event":"resolved","protocol":"kamino",
                    "account":pk.to_string(),"owner":owner,"seen_secs":dur,
                    "peak_collateral_usd":(peak*100.0).round()/100.0}));
                eprintln!("[RESOLVED {}] {}… after {}s (peak ${:.0})", ts, &owner[..8], dur, peak);
            }
        }
        durations.sort_unstable();
        let med = durations.get(durations.len()/2).copied().unwrap_or(0);
        eprintln!("[kmon {ts}] watch={} liq_now={} open={} | appeared={} resolved={} med_survival={}s | tightest {:.4}",
            watch.len(), cur_liq, open.len(), appeared, resolved, med, tightest);

        std::thread::sleep(poll);
    }
}
