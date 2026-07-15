//! marginfi continuous liquidation monitor (read-only, real-data test).
//!
//! The go/no-go experiment: does a *profitable* liquidation opportunity ever
//! sit around long enough for us to take it, or is it gone in milliseconds?
//!
//! Architecture (getProgramAccounts over 500k accounts is ~850MB, far too heavy
//! to loop): scan ONCE to build a watch-set of accounts near liquidation, then
//! on a fast loop poll only (a) oracle prices and (b) the watch-set's fresh
//! account data — a few cheap getMultipleAccounts calls. We detect the instant
//! a watched account crosses underwater (APPEARED) and when it recovers / gets
//! liquidated (RESOLVED), logging how long it stayed takeable. Full re-scan
//! every FULL_REFRESH_SECS to catch new entrants.
//!
//! Usage: HELIUS_RPC=<url> [POLL_SECS=5] [FULL_REFRESH_SECS=600]
//!        [WATCH_RATIO=0.85] [MIN_COLLATERAL_USD=50] cargo run --release --bin liq_monitor

use arb_engine::liquidation::{self as liq, Bank, BankMap, MarginfiAccount, PriceMap};
use solana_pubkey::Pubkey;
use std::collections::{HashMap, HashSet};
use std::io::Write;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

const MARGINFI_PROGRAM: &str = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA";
const MARGINFI_GROUP: &str = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8";

fn now_ts() -> u64 {
    SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs()
}

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

fn b64(data: &serde_json::Value) -> Option<Vec<u8>> {
    use base64::Engine;
    base64::engine::general_purpose::STANDARD.decode(data.get(0)?.as_str()?).ok()
}

/// Batch getMultipleAccounts (100/call) → pubkey → raw bytes.
fn get_multiple(endpoint: &str, keys: &[Pubkey]) -> HashMap<Pubkey, Vec<u8>> {
    let mut out = HashMap::new();
    for chunk in keys.chunks(100) {
        let strs: Vec<String> = chunk.iter().map(|k| k.to_string()).collect();
        let Some(v) = rpc(endpoint, serde_json::json!({
            "jsonrpc":"2.0","id":1,"method":"getMultipleAccounts",
            "params":[strs, {"encoding":"base64"}]
        })) else { continue };
        for (i, acc) in v["result"]["value"].as_array().into_iter().flatten().enumerate() {
            if let Some(bytes) = acc.get("data").and_then(b64) {
                out.insert(chunk[i], bytes);
            }
        }
        std::thread::sleep(Duration::from_millis(40));
    }
    out
}

/// A watched account: its address, authority, and last-known balances.
struct Watched {
    authority: Pubkey,
    account: MarginfiAccount,
}

/// Full scan → banks, bank→oracle map, and the near-liquidation watch-set
/// (keyed by account pubkey). Heavy; run rarely.
fn full_scan(
    endpoint: &str,
    watch_ratio: f64,
    min_collateral: f64,
) -> (BankMap, HashMap<Pubkey, Pubkey>, HashMap<Pubkey, Watched>) {
    eprintln!("[monitor] full scan: getProgramAccounts (group {}) …", &MARGINFI_GROUP[..8]);
    let resp = rpc(endpoint, serde_json::json!({
        "jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[MARGINFI_PROGRAM, {
            "encoding":"base64",
            "dataSlice":{"offset":0,"length":1736},
            "filters":[
                {"dataSize": liq::MA_SIZE},
                {"memcmp":{"offset":8,"bytes":MARGINFI_GROUP}}
            ]
        }]
    }));
    let entries = resp.as_ref().and_then(|v| v["result"].as_array()).cloned().unwrap_or_default();
    eprintln!("[monitor]   {} accounts", entries.len());

    // Decode borrowers, remembering the account pubkey.
    let mut borrowers: Vec<(Pubkey, MarginfiAccount)> = Vec::new();
    for e in &entries {
        let Some(pk) = e["pubkey"].as_str().and_then(|s| s.parse::<Pubkey>().ok()) else { continue };
        let Some(bytes) = b64(&e["account"]["data"]) else { continue };
        let Some(acct) = MarginfiAccount::decode(&bytes) else { continue };
        if acct.balances.iter().any(|b| b.liability_shares > 0.0) {
            borrowers.push((pk, acct));
        }
    }

    // Banks + oracle prices (once) to score the watch-set.
    let bank_pks: Vec<Pubkey> = borrowers.iter()
        .flat_map(|(_, a)| a.balances.iter().map(|b| b.bank_pk))
        .collect::<HashSet<_>>().into_iter().collect();
    let bank_raw = get_multiple(endpoint, &bank_pks);
    let mut banks: BankMap = HashMap::new();
    let mut bank_oracle: HashMap<Pubkey, Pubkey> = HashMap::new();
    for (pk, raw) in &bank_raw {
        if let Some(bank) = Bank::decode(raw) {
            bank_oracle.insert(*pk, bank.oracle_key);
            banks.insert(*pk, bank);
        }
    }
    let prices = refresh_prices(endpoint, &banks, &bank_oracle);

    // Watch-set: fully-priced borrowers within striking distance.
    let mut watch = HashMap::new();
    for (pk, acct) in borrowers {
        let r = liq::maintenance_health(&acct, &banks, &prices);
        if r.missing > 0 { continue; }
        if r.health.weighted_assets < min_collateral { continue; }
        if r.health.ratio() >= watch_ratio {
            watch.insert(pk, Watched { authority: acct.authority, account: acct });
        }
    }
    eprintln!("[monitor]   watch-set: {} accounts (liab/asset ≥ {:.2}, collateral ≥ ${:.0})",
        watch.len(), watch_ratio, min_collateral);
    (banks, bank_oracle, watch)
}

/// Fetch + decode the oracle accounts, returning bank_pk → USD price.
fn refresh_prices(endpoint: &str, banks: &BankMap, bank_oracle: &HashMap<Pubkey, Pubkey>) -> PriceMap {
    let oracle_pks: Vec<Pubkey> = bank_oracle.values().copied().collect::<HashSet<_>>().into_iter().collect();
    let oracle_raw = get_multiple(endpoint, &oracle_pks);
    let mut oracle_price: HashMap<Pubkey, f64> = HashMap::new();
    for (pk, raw) in &oracle_raw {
        if let Some((_f, usd, _t)) = liq::decode_price_update_v2(raw) {
            oracle_price.insert(*pk, usd);
        }
    }
    let mut prices = PriceMap::new();
    for (bank_pk, oracle_pk) in bank_oracle {
        if banks.contains_key(bank_pk) {
            if let Some(&p) = oracle_price.get(oracle_pk) {
                prices.insert(*bank_pk, p);
            }
        }
    }
    prices
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC")
        .or_else(|_| std::env::var("RPC_HTTP"))
        .expect("HELIUS_RPC in .env");
    let poll = Duration::from_secs(std::env::var("POLL_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(5));
    let full_refresh = std::env::var("FULL_REFRESH_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(600u64);
    let watch_ratio: f64 = std::env::var("WATCH_RATIO").ok().and_then(|s| s.parse().ok()).unwrap_or(0.85);
    let min_collateral: f64 = std::env::var("MIN_COLLATERAL_USD").ok().and_then(|s| s.parse().ok()).unwrap_or(50.0);

    let log_path = "runs/liq_events.jsonl";
    let _ = std::fs::create_dir_all("runs");
    let mut log = std::fs::OpenOptions::new().create(true).append(true).open(log_path)
        .expect("open liq_events.jsonl");
    let mut emit = |v: serde_json::Value| {
        let _ = writeln!(log, "{v}");
        let _ = log.flush();
    };

    eprintln!("[monitor] poll={}s full_refresh={}s watch_ratio={} min_collateral=${}",
        poll.as_secs(), full_refresh, watch_ratio, min_collateral);

    let (mut banks, mut bank_oracle, mut watch) = full_scan(&endpoint, watch_ratio, min_collateral);
    let mut last_full = now_ts();
    // account → (ts first seen liquidatable, peak collateral, peak deficit)
    let mut open: HashMap<Pubkey, (u64, f64, f64)> = HashMap::new();
    let (mut appeared, mut resolved) = (0u64, 0u64);
    let mut resolve_durations: Vec<u64> = Vec::new();

    loop {
        // Periodic full re-scan for new entrants + fresh positions.
        if now_ts().saturating_sub(last_full) >= full_refresh {
            let (b, bo, w) = full_scan(&endpoint, watch_ratio, min_collateral);
            banks = b; bank_oracle = bo;
            // Keep open events even if an account drops out of the watch-set.
            watch = w;
            last_full = now_ts();
        }

        let prices = refresh_prices(&endpoint, &banks, &bank_oracle);

        // Fresh account data for the watch-set (positions change too, not just price).
        let watch_pks: Vec<Pubkey> = watch.keys().copied().collect();
        let fresh = get_multiple(&endpoint, &watch_pks);
        for (pk, raw) in &fresh {
            if let Some(a) = MarginfiAccount::decode(raw) {
                if let Some(w) = watch.get_mut(pk) { w.account = a; }
            }
        }

        let ts = now_ts();
        let mut cur_liq = 0usize;
        let mut tightest = (f64::NEG_INFINITY, Pubkey::default(), 0.0f64);
        for (pk, w) in &watch {
            let r = liq::maintenance_health(&w.account, &banks, &prices);
            if r.missing > 0 { continue; }
            let ratio = r.health.ratio();
            let deficit = r.health.value();
            let collateral = r.health.weighted_assets;
            if ratio > tightest.0 { tightest = (ratio, w.authority, collateral); }

            let is_liq = deficit < 0.0 && collateral >= min_collateral;
            if is_liq {
                cur_liq += 1;
                let e = open.entry(*pk).or_insert_with(|| {
                    appeared += 1;
                    emit(serde_json::json!({
                        "ts": ts, "event": "appeared", "account": pk.to_string(),
                        "authority": w.authority.to_string(),
                        "collateral_usd": (collateral*100.0).round()/100.0,
                        "deficit_usd": (deficit*100.0).round()/100.0,
                        "ratio": (ratio*10000.0).round()/10000.0,
                    }));
                    eprintln!("[APPEARED {}] {}… collateral=${:.0} deficit={:+.2}",
                        ts, &w.authority.to_string()[..8], collateral, deficit);
                    (ts, collateral, deficit)
                });
                e.1 = e.1.max(collateral);
                e.2 = e.2.min(deficit);
            } else if let Some((t0, peak_col, peak_def)) = open.remove(pk) {
                resolved += 1;
                let dur = ts.saturating_sub(t0);
                resolve_durations.push(dur);
                emit(serde_json::json!({
                    "ts": ts, "event": "resolved", "account": pk.to_string(),
                    "authority": w.authority.to_string(), "seen_secs": dur,
                    "peak_collateral_usd": (peak_col*100.0).round()/100.0,
                    "peak_deficit_usd": (peak_def*100.0).round()/100.0,
                }));
                eprintln!("[RESOLVED {}] {}… after {}s (peak collateral ${:.0}, peak deficit {:+.2})",
                    ts, &w.authority.to_string()[..8], dur, peak_col, peak_def);
            }
        }

        resolve_durations.sort_unstable();
        let med = resolve_durations.get(resolve_durations.len() / 2).copied().unwrap_or(0);
        eprintln!("[monitor {ts}] watch={} liq_now={} open={} | total appeared={} resolved={} med_lifetime={}s | tightest {:.4} ({}…)",
            watch.len(), cur_liq, open.len(), appeared, resolved, med,
            tightest.0, &tightest.1.to_string()[..8.min(tightest.1.to_string().len())]);

        std::thread::sleep(poll);
    }
}
