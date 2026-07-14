//! pump_census — read `runs/pump/events.jsonl` (produced by `pump_collect`) and
//! report the ground-truth opportunity size for pump.fun launches:
//!   * launches per hour (and the observation window)
//!   * % of launches that graduate vs die (within the window)
//!   * median / distribution of time-to-graduation
//!   * distribution of peak-price-multiple after launch
//!   * a dev-dump rug proxy (launches where the dev wallet sells, how fast)
//!
//! This answers "is there even money here, and where" BEFORE any capital risk.
//! It is a pure read of the collected file — no RPC, no chain writes.
//!
//! Usage: `cargo run --release --bin pump_census [-- path/to/events.jsonl]`
//!        env: PUMP_OUT (default runs/pump/events.jsonl)

use std::collections::HashMap;
use std::io::{BufRead, BufReader};

/// Per-mint rollup built from the event stream.
#[derive(Default)]
struct Token {
    create_ms: Option<u128>,
    dev: Option<String>,
    /// price_in_sol implied by the create's initial reserves.
    init_price: f64,
    trades: u64,
    buys: u64,
    sells: u64,
    peak_price: f64,
    migrate_ms: Option<u128>,
    /// First time the dev wallet was seen selling (ms).
    dev_first_sell_ms: Option<u128>,
    dev_sold: bool,
}

fn pct(sorted: &[f64], p: f64) -> f64 {
    if sorted.is_empty() {
        return f64::NAN;
    }
    let idx = ((sorted.len() as f64 - 1.0) * p).round() as usize;
    sorted[idx.min(sorted.len() - 1)]
}

/// Reject records whose shape or internal consistency betrays a torn write.
fn record_is_sane(v: &serde_json::Value) -> bool {
    let Some(et) = v["event_type"].as_str() else { return false };
    if !matches!(et, "create" | "buy" | "sell" | "migrate") {
        return false;
    }
    // base58 pubkey = 32-44 chars, signature = 86-88 chars
    let mint_ok = v["mint"].as_str().is_some_and(|m| (32..=44).contains(&m.len()));
    let sig_ok = v["signature"].as_str().is_some_and(|s| (80..=90).contains(&s.len()));
    if !mint_ok || !sig_ok || v["unix_ms"].as_u64().unwrap_or(0) == 0 {
        return false;
    }
    if et == "buy" || et == "sell" {
        // Zero values are legitimate (e.g. dust sells into an emptied curve,
        // vsr = 0 → price 0); torn lines betray themselves by MISSING fields or
        // by a price that disagrees with the reserves it was derived from.
        let Some(vs) = v["virtual_sol_reserves"].as_f64() else { return false };
        let Some(vt) = v["virtual_token_reserves"].as_f64() else { return false };
        let Some(p) = v["price_in_sol"].as_f64() else { return false };
        if !p.is_finite() || p < 0.0 || vs < 0.0 || vt < 0.0 {
            return false;
        }
        if vt > 0.0 {
            let recomputed = (vs / 1e9) / (vt / 1e6);
            if (p - recomputed).abs() > recomputed * 0.01 {
                return false;
            }
        }
    }
    true
}

fn main() {
    let path = std::env::args()
        .nth(1)
        .or_else(|| std::env::var("PUMP_OUT").ok())
        .unwrap_or_else(|| "runs/pump/events.jsonl".into());

    let f = match std::fs::File::open(&path) {
        Ok(f) => f,
        Err(e) => {
            eprintln!("cannot open {path}: {e}");
            std::process::exit(1);
        }
    };

    let mut tokens: HashMap<String, Token> = HashMap::new();
    let (mut n_events, mut n_create, mut n_buy, mut n_sell, mut n_migrate) = (0u64, 0u64, 0u64, 0u64, 0u64);
    let mut n_skipped = 0u64;
    let (mut ts_min, mut ts_max) = (u128::MAX, 0u128);

    for line in BufReader::new(f).lines().map_while(Result::ok) {
        let line = line.trim();
        if line.is_empty() {
            continue;
        }
        let Ok(v) = serde_json::from_str::<serde_json::Value>(line) else {
            n_skipped += 1;
            continue;
        };
        // Torn-line guard: interleaved writers have produced lines that still
        // parse as JSON but carry woven-together garbage fields. Require a sane
        // base shape, and on trades a price consistent with the raw reserves.
        if !record_is_sane(&v) {
            n_skipped += 1;
            continue;
        }
        n_events += 1;
        let ts = v["unix_ms"].as_u64().unwrap_or(0) as u128;
        if ts > 0 {
            ts_min = ts_min.min(ts);
            ts_max = ts_max.max(ts);
        }
        let mint = v["mint"].as_str().unwrap_or("").to_string();
        if mint.is_empty() {
            continue;
        }
        let et = v["event_type"].as_str().unwrap_or("");
        let t = tokens.entry(mint).or_default();
        match et {
            "create" => {
                n_create += 1;
                t.create_ms = Some(ts);
                t.dev = v["dev"].as_str().map(str::to_string);
                let vs = v["init_virtual_sol_reserves"].as_f64().unwrap_or(0.0);
                let vt = v["init_virtual_token_reserves"].as_f64().unwrap_or(0.0);
                if vt > 0.0 {
                    // price_in_sol with 6 decimals: (vs/1e9)/(vt/1e6)
                    t.init_price = (vs / 1e9) / (vt / 1e6);
                }
            }
            "buy" | "sell" => {
                if et == "buy" {
                    n_buy += 1;
                    t.buys += 1;
                } else {
                    n_sell += 1;
                    t.sells += 1;
                }
                t.trades += 1;
                let p = v["price_in_sol"].as_f64().unwrap_or(0.0);
                if p > t.peak_price {
                    t.peak_price = p;
                }
                // dev-dump proxy: this sell's actor is the create's dev wallet.
                if et == "sell" {
                    if let (Some(dev), Some(actor)) = (t.dev.clone(), v["actor"].as_str()) {
                        if dev == actor {
                            t.dev_sold = true;
                            t.dev_first_sell_ms.get_or_insert(ts);
                        }
                    }
                }
            }
            "migrate" => {
                n_migrate += 1;
                t.migrate_ms.get_or_insert(ts);
            }
            _ => {}
        }
    }

    if n_events == 0 {
        eprintln!("no events in {path}");
        std::process::exit(1);
    }

    let span_ms = ts_max.saturating_sub(ts_min);
    let span_hours = span_ms as f64 / 3_600_000.0;

    println!("═══ pump.fun census — {path} ═══");
    println!(
        "events {n_events}  (create {n_create}, buy {n_buy}, sell {n_sell}, migrate {n_migrate})  [skipped {n_skipped} torn/malformed lines]"
    );
    println!(
        "observation window: {:.2} min ({:.3} h)  |  distinct mints seen: {}",
        span_ms as f64 / 60_000.0,
        span_hours,
        tokens.len()
    );
    if span_hours > 0.0 {
        println!(
            "launch rate: {:.1} launches/hour  ({} creates in window)",
            n_create as f64 / span_hours,
            n_create
        );
        println!(
            "migration rate: {:.2} migrations/hour  ({} migrates in window)",
            n_migrate as f64 / span_hours,
            n_migrate
        );
    }

    // ── Launch-cohort analysis: only mints whose CREATE we captured in-window ──
    let cohort: Vec<&Token> = tokens
        .values()
        .filter(|t| t.create_ms.is_some())
        .collect();
    println!("\n── launch cohort (create seen in-window): {} ──", cohort.len());
    if cohort.is_empty() {
        println!("(no creates captured in this window — run the collector longer)");
        return;
    }

    let graduated = cohort.iter().filter(|t| t.migrate_ms.is_some()).count();
    let no_trades = cohort.iter().filter(|t| t.trades == 0).count();
    let dev_dumped = cohort.iter().filter(|t| t.dev_sold).count();

    println!(
        "graduated (migrated) in-window: {}/{} = {:.2}%",
        graduated,
        cohort.len(),
        100.0 * graduated as f64 / cohort.len() as f64
    );
    println!(
        "died-so-far proxy (0 trades after launch): {}/{} = {:.2}%",
        no_trades,
        cohort.len(),
        100.0 * no_trades as f64 / cohort.len() as f64
    );
    println!(
        "NOTE: window is short, so 'graduated %' is a floor and 'died %' is a\n"
    );
    println!(
        "      ceiling — most launches' fate falls outside the capture window."
    );

    // time-to-graduation for the ones that did migrate in-window
    let mut ttg: Vec<f64> = cohort
        .iter()
        .filter_map(|t| match (t.create_ms, t.migrate_ms) {
            (Some(c), Some(m)) if m >= c => Some((m - c) as f64 / 1000.0),
            _ => None,
        })
        .collect();
    ttg.sort_by(|a, b| a.partial_cmp(b).unwrap());
    if !ttg.is_empty() {
        println!(
            "\ntime-to-graduation (s): p50 {:.0}  p25 {:.0}  p75 {:.0}  min {:.0}  max {:.0}  (n={})",
            pct(&ttg, 0.50), pct(&ttg, 0.25), pct(&ttg, 0.75),
            ttg[0], ttg[ttg.len() - 1], ttg.len()
        );
    } else {
        println!("\ntime-to-graduation: no create→migrate pair fully inside the window");
    }

    // peak price multiple vs the launch price, for launches that traded
    let mut mult: Vec<f64> = cohort
        .iter()
        .filter(|t| t.trades > 0 && t.init_price > 0.0 && t.peak_price > 0.0)
        .map(|t| t.peak_price / t.init_price)
        .collect();
    mult.sort_by(|a, b| a.partial_cmp(b).unwrap());
    if !mult.is_empty() {
        println!(
            "\npeak price multiple (peak / launch price), launches that traded (n={}):",
            mult.len()
        );
        println!(
            "  p10 {:.2}x  p50 {:.2}x  p75 {:.2}x  p90 {:.2}x  p99 {:.2}x  max {:.2}x",
            pct(&mult, 0.10), pct(&mult, 0.50), pct(&mult, 0.75),
            pct(&mult, 0.90), pct(&mult, 0.99), mult[mult.len() - 1]
        );
        for thr in [2.0, 5.0, 10.0, 50.0] {
            let c = mult.iter().filter(|&&m| m >= thr).count();
            println!(
                "  ≥{:>4.0}x : {:>5} / {} = {:.1}%",
                thr, c, mult.len(), 100.0 * c as f64 / mult.len() as f64
            );
        }
    }

    // dev-dump rug proxy
    println!(
        "\ndev-dump proxy: {}/{} = {:.2}% of launches had the dev wallet SELL in-window",
        dev_dumped,
        cohort.len(),
        100.0 * dev_dumped as f64 / cohort.len() as f64
    );
    let mut dev_dt: Vec<f64> = cohort
        .iter()
        .filter_map(|t| match (t.create_ms, t.dev_first_sell_ms) {
            (Some(c), Some(s)) if s >= c => Some((s - c) as f64 / 1000.0),
            _ => None,
        })
        .collect();
    dev_dt.sort_by(|a, b| a.partial_cmp(b).unwrap());
    if !dev_dt.is_empty() {
        println!(
            "  dev time-to-first-sell (s): p50 {:.1}  p25 {:.1}  min {:.1}  (n={})",
            pct(&dev_dt, 0.50), pct(&dev_dt, 0.25), dev_dt[0], dev_dt.len()
        );
    }
    println!(
        "  (sell-revert honeypots are NOT visible here: the collector records only\n   \
         successful txs. That rug flavour needs a failed-tx scan — future work.)"
    );
}
