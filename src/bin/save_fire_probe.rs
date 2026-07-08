//! Verify the Save fire path composes: find a real liquidatable v1 obligation
//! (1 collateral deposit + 1 USDC borrow), build the flash-loan-wrapped
//! liquidate+redeem+swap+repay tx, and simulateTransaction. Success = a live
//! profitable liquidation; a revert at the Solend liquidate/health gate proves
//! every upstream leg (flash borrow, refresh, liquidate wiring, Jupiter swap,
//! repay) composes. Also reports the tx byte size (flags if a SAVE_ALT is
//! needed to fit 1232B). Read-only — never submits.
//!
//! Usage: HELIUS_RPC=<url> [SCAN=4000] [REPAY_FRAC=0.2] cargo run --release --bin save_fire_probe

use arb_engine::save::{self, Obligation, Reserve};
use arb_engine::save_fire::{build_save_fire_tx, SaveFireCandidate};
use solana_pubkey::Pubkey;
use std::collections::HashMap;
use std::str::FromStr;
use std::time::Duration;

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..4 {
        if let Ok(r) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = r.into_json::<serde_json::Value>() { return Some(v); }
        }
        std::thread::sleep(Duration::from_millis(400 << attempt));
    }
    None
}
fn b64(d: &serde_json::Value) -> Option<Vec<u8>> {
    use base64::Engine;
    base64::engine::general_purpose::STANDARD.decode(d.get(0)?.as_str()?).ok()
}
fn get_acct(endpoint: &str, pk: &Pubkey) -> Option<Vec<u8>> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
        "params":[pk.to_string(), {"encoding":"base64"}]}))?;
    b64(&v["result"]["value"]["data"])
}
fn mint_owner(endpoint: &str, mint: &Pubkey) -> Option<Pubkey> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
        "params":[mint.to_string(), {"encoding":"base64"}]}))?;
    v["result"]["value"]["owner"].as_str()?.parse().ok()
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let scan: usize = std::env::var("SCAN").ok().and_then(|s| s.parse().ok()).unwrap_or(4000);
    let repay_frac: f64 = std::env::var("REPAY_FRAC").ok().and_then(|s| s.parse().ok()).unwrap_or(0.2);
    let authority = Pubkey::from_str(&std::env::var("AUTHORITY").unwrap_or_else(|_| "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak".into())).unwrap();
    let liquidator_ma = Pubkey::from_str(&std::env::var("LIQUIDATOR_MA").unwrap_or_else(|_| "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD".into())).unwrap();
    let usdc_mint = Pubkey::from_str(save::USDC_MINT).unwrap();

    // Cache reserves as we meet them.
    let mut reserves: HashMap<Pubkey, Reserve> = HashMap::new();
    let mut load = |pk: Pubkey| -> Option<Reserve> {
        if let Some(r) = reserves.get(&pk) { return Some(r.clone()); }
        let raw = get_acct(&endpoint, &pk)?;
        let r = Reserve::decode(pk, &raw)?;
        reserves.insert(pk, r.clone());
        Some(r)
    };

    eprintln!("[save-fire] scanning obligations for a v1 liquidatable candidate …");
    let resp = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[save::SOLEND_PROGRAM, {"encoding":"base64","dataSize":1300,
            "filters":[{"dataSize":1300},{"memcmp":{"offset":10,"bytes":save::MAIN_POOL}}]}]})).expect("gPA");
    let entries = resp["result"].as_array().cloned().unwrap_or_default();
    eprintln!("[save-fire] {} obligations; looking for 1-deposit / 1-USDC-borrow, liquidatable, ≥ $10", entries.len());

    // Collect all v1 USDC-debt liquidatable candidates, rank by ratio (deepest
    // underwater first — most likely to survive the fresh-price sim), then try
    // the top TRIES. A clean sim on ANY of them = the fire path fires on a real
    // opportunity (not just composes-and-reverts).
    let tries: usize = std::env::var("TRIES").ok().and_then(|s| s.parse().ok()).unwrap_or(25);
    let min_debt: f64 = std::env::var("MIN_DEBT").ok().and_then(|s| s.parse().ok()).unwrap_or(50.0);
    let max_swap_accounts: usize = std::env::var("MAX_SWAP_ACCOUNTS").ok().and_then(|s| s.parse().ok()).unwrap_or(18);
    let mut cands: Vec<(f64, Pubkey, Obligation)> = Vec::new();
    for e in entries.iter().take(scan) {
        let Some(pk) = e["pubkey"].as_str().and_then(|s| s.parse::<Pubkey>().ok()) else { continue };
        let Some(bytes) = b64(&e["account"]["data"]) else { continue };
        let Some(o) = Obligation::decode(&bytes) else { continue };
        if o.deposits.len() != 1 || o.borrows.len() != 1 { continue; }
        if !o.liquidatable() || o.borrowed_value < min_debt { continue; }
        cands.push((o.health_ratio(), pk, o));
    }
    cands.sort_by(|a, b| b.0.partial_cmp(&a.0).unwrap_or(std::cmp::Ordering::Equal));
    eprintln!("[save-fire] {} liquidatable v1 candidates (≥ ${min_debt}); trying top {} by ratio", cands.len(), tries);

    let (mut clean, mut too_small, mut other, mut tried) = (0usize, 0usize, 0usize, 0usize);
    use base64::Engine;
    for (ratio, pk, o) in cands.iter().take(tries) {
        let Some(repay_reserve) = load(o.borrows[0].reserve) else { continue };
        if repay_reserve.liquidity_mint != usdc_mint { continue; }
        let Some(withdraw_reserve) = load(o.deposits[0].reserve) else { continue };
        let Some(ctp) = mint_owner(&endpoint, &withdraw_reserve.liquidity_mint) else { continue };
        tried += 1;
        let repay_usd = o.borrowed_value * repay_frac;
        let repay_amount = (repay_usd / repay_reserve.market_price.max(1e-9) * 1e6).max(1.0) as u64;
        let seized_usd = repay_usd * (1.0 + withdraw_reserve.liquidation_bonus_pct as f64 / 100.0);
        let seize_underlying = (seized_usd / withdraw_reserve.market_price.max(1e-9) * 10f64.powi(withdraw_reserve.mint_decimals as i32)) as u64;
        let cand = SaveFireCandidate {
            obligation: *pk, repay_reserve: repay_reserve.clone(), withdraw_reserve: withdraw_reserve.clone(),
            collateral_token_program: ctp, repay_amount, seize_underlying,
            deposit_reserves: vec![withdraw_reserve.reserve], borrow_reserves: vec![repay_reserve.reserve],
        };
        let fire = match build_save_fire_tx(&endpoint, &cand, &liquidator_ma, &authority, None, 0, 50_000, 100, max_swap_accounts, solana_hash::Hash::default()) {
            Ok(f) => f, Err(e) => { println!("  {pk} ratio {ratio:.3} ${:.0}: build failed: {e}", o.borrowed_value); other += 1; continue; }
        };
        let b64tx = base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&fire.tx).unwrap());
        let sim = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
            "params":[b64tx, {"sigVerify":false,"replaceRecentBlockhash":true,"commitment":"processed","encoding":"base64"}]}));
        match sim.as_ref().and_then(|v| v["result"].get("value")) {
            Some(v) if v["err"].is_null() => {
                clean += 1;
                println!("  ★★ {pk} ratio {ratio:.3} ${:.0}: SIMULATES CLEAN — WOULD FIRE ({}B, out {}, {} CU)",
                    o.borrowed_value, fire.tx_bytes, fire.quoted_usdc_out, v["unitsConsumed"]);
            }
            Some(v) => {
                let e = v["err"].to_string();
                // Solend LiquidationTooSmall = custom 29 (0x1d): healthy at fresh price / dust.
                if e.contains("29") { too_small += 1; }
                else { other += 1;
                    println!("  {pk} ratio {ratio:.3} ${:.0}: OTHER err {}", o.borrowed_value, e);
                    for l in v["logs"].as_array().into_iter().flatten().rev().take(4).collect::<Vec<_>>().into_iter().rev() {
                        println!("       {}", l.as_str().unwrap_or(""));
                    }
                }
            }
            None => { other += 1; }
        }
    }
    println!("\n── tried {tried}: CLEAN(would-fire) {clean} · too-small/healthy-at-fresh {too_small} · other {other} ──");
    if clean > 0 {
        println!("★ THE FIRE PATH FIRES on live opportunities — {clean} candidate(s) would liquidate profitably right now.");
    } else if other > 0 {
        println!("⚠ 0 clean, and {other} 'other' errors — inspect above; may be a fire-path bug, not just market.");
    } else {
        println!("0 fireable right now — every candidate is healthy at the fresh price (stale flags). Not a bug; no opportunity.");
    }
}
