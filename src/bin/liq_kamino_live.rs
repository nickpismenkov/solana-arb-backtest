//! Kamino LIVE-health finder — recomputes each obligation's health from CURRENT
//! reserve prices (replicating refresh_obligation), instead of trusting the
//! obligation's stored (possibly stale) values.
//!
//! Two outputs:
//!   1. VALIDATION — for fresh obligations, recomputed vs stored aggregates
//!      should match (proves the recompute math against on-chain truth).
//!   2. ALPHA — obligations that are liquidatable at current prices, especially
//!      ones whose STORED values still say healthy (stale → a refresh_obligation
//!      would flag them; catching these ahead of the crank is the only edge).
//!
//! Reserve prices come from each reserve's cached market_price (refresh_reserve),
//! which stays fresh because reserves are cranked constantly — so we sidestep
//! Scope. Freshness of those prices is reported.
//!
//! Usage: HELIUS_RPC=<url> [MARKET=all] [MIN_COLLATERAL_USD=50] [NEAR=25]
//!        cargo run --release --bin liq_kamino_live

use arb_engine::kamino::{self, Obligation, Reserve};
use solana_pubkey::Pubkey;
use std::collections::{HashMap, HashSet};
use std::time::Duration;

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..4 {
        if let Ok(resp) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = resp.into_json::<serde_json::Value>() { return Some(v); }
        }
        std::thread::sleep(Duration::from_millis(400 << attempt));
    }
    None
}

fn b64(data: &serde_json::Value) -> Option<Vec<u8>> {
    use base64::Engine;
    base64::engine::general_purpose::STANDARD.decode(data.get(0)?.as_str()?).ok()
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

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP"))
        .expect("HELIUS_RPC in .env");
    let market = std::env::var("MARKET").unwrap_or_else(|_| "all".into());
    let min_collateral: f64 = std::env::var("MIN_COLLATERAL_USD").ok().and_then(|s| s.parse().ok()).unwrap_or(50.0);
    let near_n: usize = std::env::var("NEAR").ok().and_then(|s| s.parse().ok()).unwrap_or(25);

    let cur_slot = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"confirmed"}]}))
        .and_then(|v| v["result"].as_u64()).unwrap_or(0);

    // 1) Obligations (dataSlice through has_debt @2287).
    let mut filters = vec![serde_json::json!({"dataSize": kamino::OBLIGATION_SIZE})];
    if market != "all" { filters.push(serde_json::json!({"memcmp":{"offset":32,"bytes":market}})); }
    eprintln!("[live] getProgramAccounts obligations …");
    let resp = rpc(&endpoint, serde_json::json!({
        "jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[kamino::KLEND_PROGRAM, {"encoding":"base64","dataSlice":{"offset":0,"length":2288},"filters":filters}]
    }));
    let entries = resp.as_ref().and_then(|v| v["result"].as_array()).cloned().unwrap_or_default();
    let obs: Vec<Obligation> = entries.iter()
        .filter_map(|e| b64(&e["account"]["data"]))
        .filter_map(|b| Obligation::decode(&b))
        .filter(|o| !o.borrows.is_empty())
        .collect();
    eprintln!("[live] {} obligations with debt, current slot {}", obs.len(), cur_slot);
    if obs.is_empty() { return; }

    // 2) Fetch + decode every referenced reserve (need through borrow_factor @5008).
    let reserve_pks: Vec<Pubkey> = obs.iter()
        .flat_map(|o| o.deposits.iter().map(|d| d.0).chain(o.borrows.iter().map(|b| b.0)))
        .collect::<HashSet<_>>().into_iter().collect();
    eprintln!("[live] fetching {} reserves …", reserve_pks.len());
    let reserve_raw = get_multiple(&endpoint, &reserve_pks, 5016);
    let mut reserves: HashMap<Pubkey, Reserve> = HashMap::new();
    for (pk, raw) in &reserve_raw {
        if let Some(r) = Reserve::decode(raw) { reserves.insert(*pk, r); }
    }
    // Reserve price freshness (these cached prices drive the recompute).
    let mut ages: Vec<u64> = reserves.values().map(|r| cur_slot.saturating_sub(r.price_slot)).collect();
    ages.sort_unstable();
    let med_age = ages.get(ages.len()/2).copied().unwrap_or(0);
    eprintln!("[live] decoded {} reserves; cached-price age median {}sl (~{}s), max {}sl",
        reserves.len(), med_age, med_age * 2 / 5, ages.last().copied().unwrap_or(0));

    // Population diagnostics.
    let n_elev = obs.iter().filter(|o| o.elevation_group != 0).count();
    let n_stale = obs.iter().filter(|o| o.stale).count();
    let n_trust = obs.iter().filter(|o| kamino::recompute(o, &reserves).trustworthy()).count();
    eprintln!("[live] population: {} debt obs | {} elevation-group | {} stale | {} trustworthy(non-elev, fully priced)",
        obs.len(), n_elev, n_stale, n_trust);

    // 3) Validation: recomputed vs stored. Compare on trustworthy obligations
    // whose recompute used fresh-enough reserve prices AND that were refreshed
    // recently (so stored ≈ current). Match ⇒ recompute math is correct.
    let mut val_err: Vec<f64> = Vec::new();
    let mut shown = 0;
    println!("\n──── VALIDATION: recomputed vs stored ────");
    for o in &obs {
        let r = kamino::recompute(o, &reserves);
        if !r.trustworthy() || o.unhealthy_borrow_value < 100.0 { continue; }
        // both the obligation and the reserve prices it uses must be recent.
        if cur_slot.saturating_sub(o.last_update_slot) > 300 { continue; }
        if cur_slot.saturating_sub(r.oldest_price_slot) > 300 { continue; }
        let err = (r.unhealthy_borrow_value - o.unhealthy_borrow_value).abs() / o.unhealthy_borrow_value;
        val_err.push(err);
        if shown < 10 {
            println!("  stored unhealthy=${:.2} debt=${:.2} depos=${:.2}  |  recomp unhealthy=${:.2} debt=${:.2} depos=${:.2}  (err {:.2}%)",
                o.unhealthy_borrow_value, o.bf_adjusted_debt, o.deposited_value,
                r.unhealthy_borrow_value, r.bf_adjusted_debt, r.deposited_value, err*100.0);
            shown += 1;
        }
    }
    val_err.sort_by(|a,b| a.partial_cmp(b).unwrap());
    if val_err.is_empty() {
        println!("  (no obligation with both itself + its reserve prices fresh enough to validate)");
    } else {
        println!("  → {} validated, median error {:.3}%, p90 {:.3}%",
            val_err.len(), val_err[val_err.len()/2]*100.0, val_err[(val_err.len()*9/10).min(val_err.len()-1)]*100.0);
    }

    // ALPHA: liquidatable at current prices, ranked by seizable collateral.
    struct Hit<'a> { o: &'a Obligation, r: kamino::Recomputed, stored_liq: bool }
    let mut hits: Vec<Hit> = Vec::new();
    let mut near: Vec<(f64, &Obligation, kamino::Recomputed)> = Vec::new();
    for o in &obs {
        let r = kamino::recompute(o, &reserves);
        if !r.trustworthy() || r.deposited_value < min_collateral { continue; }
        if r.liquidatable() {
            hits.push(Hit { o, r, stored_liq: o.liquidatable() });
        } else if r.ratio() > 0.90 {
            near.push((r.ratio(), o, r));
        }
    }
    hits.sort_by(|a,b| b.r.deposited_value.partial_cmp(&a.r.deposited_value).unwrap_or(std::cmp::Ordering::Equal));
    let hidden_alpha = hits.iter().filter(|h| !h.stored_liq).count();

    println!("\n════ Kamino LIVE liquidatable (recomputed at current prices) ════");
    println!("obligations w/ debt: {}   collateral ≥ ${:.0}", obs.len(), min_collateral);
    println!("LIQUIDATABLE NOW: {}   [{} already flagged by stored values, {} HIDDEN (stored says healthy = stale alpha)]",
        hits.len(), hits.len()-hidden_alpha, hidden_alpha);
    for h in hits.iter().take(40) {
        let age = cur_slot.saturating_sub(h.o.last_update_slot);
        println!("  {} {}…  collateral=${:.2}  debt=${:.2}  thresh=${:.2}  ratio={:.4}  (obl age {}sl{})",
            if h.stored_liq {"known"} else {"ALPHA"}, &h.o.owner.to_string()[..8],
            h.r.deposited_value, h.r.bf_adjusted_debt, h.r.unhealthy_borrow_value, h.r.ratio(),
            age, if h.o.stale {", stale"} else {""});
    }

    near.sort_by(|a,b| b.0.partial_cmp(&a.0).unwrap_or(std::cmp::Ordering::Equal));
    println!("\nclosest to liquidation at current prices (ratio→1.0):");
    for (ratio, o, r) in near.iter().take(near_n) {
        println!("  {}…  ratio={:.4}  debt=${:.2}  thresh=${:.2}  collateral=${:.2}",
            &o.owner.to_string()[..8], ratio, r.bf_adjusted_debt, r.unhealthy_borrow_value, r.deposited_value);
    }
    println!();
}
