//! Backward-looking residual scan: replay the last N hours of landed swaps on
//! both pools from chain history and reconstruct the cross-venue gap timeline.
//! Complements backrun_probe (live, sub-slot) with slot-level coverage over a
//! full day — including hours we weren't listening.
//!
//! Method: getSignaturesForAddress on each pool → getTransaction (jsonParsed)
//! → the pool's vault balance deltas give each swap's execution price. A CLMM
//! price only moves on swaps, so the last execution price on a venue ≈ its
//! current price until the next swap. Caveat: execution price is the swap's
//! average (mid of pre/post marginal price), so large swaps read ~half their
//! own price impact as "gap" — treat counts near the floor as upper bounds.
//!
//! Usage: RPC_ENDPOINT=<url> HOURS=24 [pair env vars] \
//!   cargo run --release --bin history_probe

use arb_engine::pools::pair;
use std::collections::HashMap;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

const TIP_CUSHION_BPS: f64 = 1.0;
// Backstop for very active pools (liquid controls). When hit, the window is
// truncated and the report says so — never silently.
const MAX_TX_PER_POOL: usize = 8000;

fn rpc_call(rpc: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..5 {
        match ureq::post(rpc).send_json(body.clone()) {
            Ok(resp) => return resp.into_json().ok(),
            // Back off hard on rate limits — a 429 storm is slower than pacing.
            Err(ureq::Error::Status(429, _)) => {
                std::thread::sleep(Duration::from_millis(1500 * (attempt + 1)))
            }
            Err(_) if attempt < 4 => std::thread::sleep(Duration::from_millis(400 << attempt)),
            Err(e) => {
                eprintln!("rpc error (giving up): {e}");
                return None;
            }
        }
    }
    None
}

fn account_data(rpc: &str, addr: &str) -> Option<Vec<u8>> {
    let v = rpc_call(
        rpc,
        serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
            "params":[addr,{"encoding":"base64"}]}),
    )?;
    use base64::Engine;
    base64::engine::general_purpose::STANDARD
        .decode(v["result"]["value"]["data"][0].as_str()?)
        .ok()
}

fn pk_at(d: &[u8], o: usize) -> String {
    bs58::encode(&d[o..o + 32]).into_string()
}

struct Swap {
    slot: u64,
    block_time: i64,
    venue: &'static str,
    price: f64,   // quote per base (execution price)
    base_ui: f64, // |base delta| of the swap
}

/// All pool signatures newer than `cutoff` (unix secs), oldest capped by
/// MAX_TX_PER_POOL. Returns (sigs, truncated).
fn signatures_since(rpc: &str, pool: &str, cutoff: i64) -> (Vec<String>, bool) {
    let mut sigs = Vec::new();
    let mut before: Option<String> = None;
    loop {
        let mut params = serde_json::json!({"limit": 1000});
        if let Some(b) = &before {
            params["before"] = serde_json::json!(b);
        }
        let v = match rpc_call(
            rpc,
            serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress",
                "params":[pool, params]}),
        ) {
            Some(v) => v,
            None => break,
        };
        let Some(arr) = v["result"].as_array() else { break };
        if arr.is_empty() {
            break;
        }
        let mut reached_cutoff = false;
        for e in arr {
            let bt = e["blockTime"].as_i64().unwrap_or(0);
            if bt != 0 && bt < cutoff {
                reached_cutoff = true;
                break;
            }
            if e["err"].is_null() {
                sigs.push(e["signature"].as_str().unwrap_or_default().to_string());
            }
        }
        if reached_cutoff {
            return (sigs, false);
        }
        if sigs.len() >= MAX_TX_PER_POOL {
            return (sigs, true);
        }
        before = arr
            .last()
            .and_then(|e| e["signature"].as_str())
            .map(String::from);
        if before.is_none() {
            break;
        }
    }
    (sigs, false)
}

/// Vault deltas → execution price for one landed tx.
fn swap_from_tx(
    rpc: &str,
    sig: &str,
    venue: &'static str,
    base_vault: &str,
    quote_vault: &str,
    base_dec: i32,
    quote_dec: i32,
) -> Option<Swap> {
    let v = rpc_call(
        rpc,
        serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getTransaction",
            "params":[sig,{"encoding":"jsonParsed","maxSupportedTransactionVersion":0,
                           "commitment":"confirmed"}]}),
    )?;
    let r = &v["result"];
    if r.is_null() || !r["meta"]["err"].is_null() {
        return None;
    }
    let keys: Vec<&str> = r["transaction"]["message"]["accountKeys"]
        .as_array()?
        .iter()
        .filter_map(|k| k["pubkey"].as_str())
        .collect();
    let balance = |bals: &serde_json::Value, vault: &str| -> i128 {
        bals.as_array()
            .into_iter()
            .flatten()
            .find(|b| {
                b["accountIndex"]
                    .as_u64()
                    .and_then(|i| keys.get(i as usize))
                    .map(|k| *k == vault)
                    .unwrap_or(false)
            })
            .and_then(|b| b["uiTokenAmount"]["amount"].as_str())
            .and_then(|s| s.parse().ok())
            .unwrap_or(0)
    };
    let meta = &r["meta"];
    let d_base = balance(&meta["postTokenBalances"], base_vault)
        - balance(&meta["preTokenBalances"], base_vault);
    let d_quote = balance(&meta["postTokenBalances"], quote_vault)
        - balance(&meta["preTokenBalances"], quote_vault);
    if d_base == 0 || d_quote == 0 {
        return None; // not a swap on this pool (liquidity op, or vault untouched)
    }
    let base_ui = (d_base.unsigned_abs() as f64) / 10f64.powi(base_dec);
    let quote_ui = (d_quote.unsigned_abs() as f64) / 10f64.powi(quote_dec);
    Some(Swap {
        slot: r["slot"].as_u64()?,
        block_time: r["blockTime"].as_i64().unwrap_or(0),
        venue,
        price: quote_ui / base_ui,
        base_ui,
    })
}

fn median(mut v: Vec<f64>) -> f64 {
    if v.is_empty() {
        return 0.0;
    }
    v.sort_by(|a, b| a.partial_cmp(b).unwrap());
    v[v.len() / 2]
}

fn main() {
    let _ = dotenvy::dotenv();
    let rpc = std::env::var("RPC_ENDPOINT").expect("set RPC_ENDPOINT");
    let hours: f64 = std::env::var("HOURS").ok().and_then(|s| s.parse().ok()).unwrap_or(24.0);
    let cfg = pair();
    let fee_bps = cfg.round_trip_fee_bps();
    let cutoff = SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs() as i64
        - (hours * 3600.0) as i64;

    // Vault addresses from the pool accounts (offsets verified on mainnet).
    let orca = account_data(&rpc, &cfg.orca_pool).expect("fetch orca pool");
    let ray = account_data(&rpc, &cfg.ray_pool).expect("fetch ray pool");
    let orca_base_is_a = pk_at(&orca, 101) == cfg.base_mint;
    let (orca_base_v, orca_quote_v) = if orca_base_is_a {
        (pk_at(&orca, 133), pk_at(&orca, 213))
    } else {
        (pk_at(&orca, 213), pk_at(&orca, 133))
    };
    let ray_base_is_0 = pk_at(&ray, 73) == cfg.base_mint;
    let (ray_base_v, ray_quote_v) = if ray_base_is_0 {
        (pk_at(&ray, 137), pk_at(&ray, 169))
    } else {
        (pk_at(&ray, 169), pk_at(&ray, 137))
    };

    println!(
        "history-probe — pair {}, floor {fee_bps}bp (+{TIP_CUSHION_BPS}bp cushion), last {hours}h\n",
        cfg.label
    );

    let mut swaps: Vec<Swap> = Vec::new();
    for (venue, pool, bv, qv) in [
        ("Orca", cfg.orca_pool.as_str(), &orca_base_v, &orca_quote_v),
        ("Raydium", cfg.ray_pool.as_str(), &ray_base_v, &ray_quote_v),
    ] {
        let (sigs, truncated) = signatures_since(&rpc, pool, cutoff);
        if truncated {
            println!("⚠ {venue}: hit the {MAX_TX_PER_POOL}-tx cap — window truncated, report covers less than {hours}h on this venue.");
        }
        println!("{venue}: {} landed txs in window, fetching…", sigs.len());
        let mut n = 0u32;
        for (i, sig) in sigs.iter().enumerate() {
            if let Some(s) = swap_from_tx(&rpc, sig, venue, bv, qv, cfg.base_dec, cfg.quote_dec) {
                swaps.push(s);
                n += 1;
            }
            if (i + 1) % 200 == 0 {
                eprintln!("  {venue}: {}/{} fetched…", i + 1, sigs.len());
            }
            std::thread::sleep(Duration::from_millis(120)); // ~8 rps, under RPC rate limits
        }
        println!("{venue}: {n} swaps decoded");
    }

    swaps.sort_by_key(|s| s.slot);

    // Replay: last exec price per venue is that venue's standing price (CLMM
    // price only moves on swaps). On every swap, measure the cross-venue gap.
    let (mut last_orca, mut last_ray) = (f64::NAN, f64::NAN);
    let mut gaps: Vec<f64> = Vec::new();
    let mut clears: Vec<(u64, i64, f64, f64)> = Vec::new(); // slot, time, gap, swap size
    let mut open_slot: Option<u64> = None;
    let mut lifetimes: Vec<u64> = Vec::new();
    let mut by_hour: HashMap<i64, u32> = HashMap::new();
    for s in &swaps {
        if s.venue == "Orca" {
            last_orca = s.price;
        } else {
            last_ray = s.price;
        }
        if !(last_orca.is_finite() && last_ray.is_finite()) {
            continue;
        }
        let gap = ((last_ray - last_orca) / last_orca.min(last_ray) * 10_000.0).abs();
        gaps.push(gap);
        if gap > fee_bps {
            if open_slot.is_none() {
                open_slot = Some(s.slot);
                clears.push((s.slot, s.block_time, gap, s.base_ui));
                *by_hour.entry((s.block_time / 3600) % 24).or_default() += 1;
            }
        } else if let Some(o) = open_slot.take() {
            lifetimes.push(s.slot - o);
        }
    }

    println!("\n──────── history-probe report ({}h) ────────", hours);
    println!("swaps decoded: {} (both venues)", swaps.len());
    if gaps.is_empty() {
        println!("no overlapping price data — one venue had no swaps in the window.");
        return;
    }
    println!("cross-venue gap at each swap: median {:.1} bp, max {:.1} bp",
        median(gaps.clone()), gaps.iter().cloned().fold(0.0, f64::max));
    println!("fee-clearing episodes (>{fee_bps}bp): {}", clears.len());
    let strong: Vec<_> = clears.iter().filter(|c| c.2 > fee_bps + TIP_CUSHION_BPS).collect();
    println!("  above floor+cushion (>{:.0}bp): {}", fee_bps + TIP_CUSHION_BPS, strong.len());
    if !clears.is_empty() {
        println!("  gap at open: median {:.1} bp, max {:.1} bp",
            median(clears.iter().map(|c| c.2).collect()),
            clears.iter().map(|c| c.2).fold(0.0, f64::max));
        if !lifetimes.is_empty() {
            let mut lt = lifetimes.clone();
            lt.sort_unstable();
            println!("  episode lifetime: median {} slots (~{:.1}s), max {} slots",
                lt[lt.len() / 2], lt[lt.len() / 2] as f64 * 0.4, lt[lt.len() - 1]);
        }
        let mut hours_sorted: Vec<_> = by_hour.iter().collect();
        hours_sorted.sort();
        let hist = hours_sorted
            .iter()
            .map(|(h, n)| format!("{h:02}h:{n}"))
            .collect::<Vec<_>>()
            .join(" ");
        println!("  episodes by UTC hour: {hist}");
        println!("\nlast 10 episodes:");
        for (slot, bt, gap, size) in clears.iter().rev().take(10) {
            let t = chrono_lite(*bt);
            println!("  slot {slot} {t} gap {gap:.1}bp (trigger swap {size:.3} base)");
        }
    }
}

/// Tiny UTC formatter (no chrono dep): unix secs → "HH:MM:SS".
fn chrono_lite(secs: i64) -> String {
    let s = secs.rem_euclid(86_400);
    format!("{:02}:{:02}:{:02}Z", s / 3600, (s % 3600) / 60, s % 60)
}
