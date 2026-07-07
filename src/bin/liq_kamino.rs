//! Kamino liquidatable-obligation finder (read-only, Stage 1 live test).
//!
//! Scans every klend Obligation, reads its STORED health values (no oracle
//! needed — Kamino pre-computes them), and lists who is liquidatable
//! (borrow_factor_adjusted_debt ≥ unhealthy_borrow_value), ranked by seizable
//! collateral. Reports staleness: a "fresh" liquidatable obligation is a
//! high-confidence opportunity; a "stale" one needs an on-chain refresh to
//! confirm (its stored values predate the latest price).
//!
//! Usage: HELIUS_RPC=<url> [MARKET=<pubkey|all>] [MIN_COLLATERAL_USD=50]
//!        [NEAR=25] [STALE_SLOTS=150] cargo run --release --bin liq_kamino

use arb_engine::kamino::{self, Obligation};
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

fn b64(data: &serde_json::Value) -> Option<Vec<u8>> {
    use base64::Engine;
    base64::engine::general_purpose::STANDARD.decode(data.get(0)?.as_str()?).ok()
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC")
        .or_else(|_| std::env::var("RPC_HTTP"))
        .expect("HELIUS_RPC in .env");
    let market = std::env::var("MARKET").unwrap_or_else(|_| "all".into());
    let min_collateral: f64 = std::env::var("MIN_COLLATERAL_USD").ok().and_then(|s| s.parse().ok()).unwrap_or(50.0);
    let near_n: usize = std::env::var("NEAR").ok().and_then(|s| s.parse().ok()).unwrap_or(25);
    let stale_slots: u64 = std::env::var("STALE_SLOTS").ok().and_then(|s| s.parse().ok()).unwrap_or(150);

    // Current slot → staleness age of each obligation.
    let cur_slot = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"confirmed"}]}))
        .and_then(|v| v["result"].as_u64()).unwrap_or(0);

    // Obligations: dataSize filter, dataSlice trims to the fields we read.
    let mut filters = vec![serde_json::json!({"dataSize": kamino::OBLIGATION_SIZE})];
    if market != "all" {
        filters.push(serde_json::json!({"memcmp":{"offset":32,"bytes":market}}));
    }
    eprintln!("[kamino] getProgramAccounts (market={}) …", if market=="all" {"all"} else {&market[..8]});
    let resp = rpc(&endpoint, serde_json::json!({
        "jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[kamino::KLEND_PROGRAM, {
            "encoding":"base64",
            "dataSlice":{"offset":0,"length":2272},
            "filters": filters
        }]
    }));
    let entries = resp.as_ref().and_then(|v| v["result"].as_array()).cloned().unwrap_or_default();
    eprintln!("[kamino] {} obligations, current slot {}", entries.len(), cur_slot);
    if entries.is_empty() {
        eprintln!("[kamino] nothing returned — RPC must support getProgramAccounts");
        return;
    }

    let obs: Vec<Obligation> = entries.iter()
        .filter_map(|e| b64(&e["account"]["data"]))
        .filter_map(|bytes| Obligation::decode(&bytes))
        .filter(|o| o.borrowed_value > 0.0)
        .collect();

    // Liquidatable, split by freshness, ranked by seizable collateral.
    let mut liq: Vec<&Obligation> = obs.iter()
        .filter(|o| o.liquidatable() && o.deposited_value >= min_collateral).collect();
    liq.sort_by(|a, b| b.deposited_value.partial_cmp(&a.deposited_value).unwrap_or(std::cmp::Ordering::Equal));
    let is_fresh = |o: &Obligation| !o.stale && cur_slot.saturating_sub(o.last_update_slot) <= stale_slots;
    let dust = obs.iter().filter(|o| o.liquidatable() && o.deposited_value < min_collateral).count();

    println!("\n════ Kamino liquidatable finder ════");
    println!("borrowers scanned:       {}", obs.len());
    let fresh_liq = liq.iter().filter(|o| is_fresh(o)).count();
    println!("LIQUIDATABLE (collateral ≥ ${:.0}): {}   [{} fresh, {} stale, +{} dust]",
        min_collateral, liq.len(), fresh_liq, liq.len() - fresh_liq, dust);
    for o in liq.iter().take(50) {
        let age = cur_slot.saturating_sub(o.last_update_slot);
        let tag = if is_fresh(o) { "FRESH" } else { "stale" };
        println!("  {} {}…  collateral=${:.2}  debt=${:.2}  thresh=${:.2}  ratio={:.4}  (age {}sl)",
            tag, &o.owner.to_string()[..8], o.deposited_value, o.bf_adjusted_debt,
            o.unhealthy_borrow_value, o.ratio(), age);
    }

    // Closest healthy obligations with real collateral — monitor candidates.
    let mut near: Vec<&Obligation> = obs.iter()
        .filter(|o| !o.liquidatable() && o.deposited_value >= min_collateral && o.unhealthy_borrow_value > 0.0)
        .collect();
    near.sort_by(|a, b| b.ratio().partial_cmp(&a.ratio()).unwrap_or(std::cmp::Ordering::Equal));
    println!("\nclosest to liquidation (debt/threshold → 1.0):");
    for o in near.iter().take(near_n) {
        println!("  {}…  ratio={:.4}  debt=${:.2}  thresh=${:.2}  collateral=${:.2}",
            &o.owner.to_string()[..8], o.ratio(), o.bf_adjusted_debt, o.unhealthy_borrow_value, o.deposited_value);
    }
    println!();
}
