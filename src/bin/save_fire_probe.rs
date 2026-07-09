//! Verify the Save fire path composes across debt assets: find real liquidatable
//! v1 obligations (1 collateral deposit + 1 borrow, debt ∈ {USDC,USDT,wSOL}),
//! build the flash-loan-wrapped liquidate+redeem+swap+repay tx, and
//! simulateTransaction. Success = a live profitable liquidation (CLEAN sim); a
//! revert at the Solend liquidate/health gate (custom err 29 LiquidationTooSmall
//! = healthy at the fresh price) proves every upstream leg (JupLend flash borrow,
//! refresh, liquidate wiring, Jupiter swap, payback) composes. Reports tx byte
//! size (flags if a SAVE_ALT is needed to fit 1232B). Read-only — never submits.
//!
//! Usage: HELIUS_RPC=<url> [DEBT=all|usdc|usdt|wsol] [TRIES=25] [MIN_DEBT=50]
//!        [REPAY_FRAC=0.2] [RATIO_CAP=3.0] [MAX_SWAP_ACCOUNTS=18]
//!        cargo run --release --bin save_fire_probe

use arb_engine::save::{self, Obligation, Reserve};
use arb_engine::save_fire::{build_save_fire_tx, SaveFireCandidate};
use solana_pubkey::Pubkey;
use std::collections::HashMap;
use std::str::FromStr;
use std::time::Duration;

const CLASSIC_TOKEN_PROGRAM: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";

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

/// Run one debt asset: rank its liquidatable v1 candidates near threshold, build
/// + sim the top TRIES, and tally CLEAN / too-small-at-fresh / other.
#[allow(clippy::too_many_arguments)]
fn run_asset(
    endpoint: &str, label: &str, debt_mint: &Pubkey, entries: &[serde_json::Value],
    reserves: &mut HashMap<Pubkey, Reserve>, authority: &Pubkey,
    tries: usize, min_debt: f64, repay_frac: f64, ratio_cap: f64, max_swap_accounts: usize,
    same_mint_only: bool,
) {
    use base64::Engine;
    // Candidates: v1, this debt mint, liquidatable, ≥ min_debt, ratio ≤ ratio_cap
    // (the cap drops mis-priced-dust obligations — huge borrowed / ~0 unhealthy —
    // that would otherwise sort first and waste every try, matching the engine).
    let mut cands: Vec<(f64, Pubkey, Obligation)> = Vec::new();
    for e in entries {
        let Some(pk) = e["pubkey"].as_str().and_then(|s| s.parse::<Pubkey>().ok()) else { continue };
        let Some(bytes) = b64(&e["account"]["data"]) else { continue };
        let Some(o) = Obligation::decode(&bytes) else { continue };
        if o.deposits.len() != 1 || o.borrows.len() != 1 { continue; }
        if !o.liquidatable() || o.borrowed_value < min_debt { continue; }
        let r = o.health_ratio();
        if r > ratio_cap { continue; }
        // Keep only this debt mint — its reserve is pre-loaded, so this is free.
        if !matches!(reserves.get(&o.borrows[0].reserve), Some(rv) if &rv.liquidity_mint == debt_mint) { continue; }
        cands.push((r, pk, o));
    }
    cands.sort_by(|a, b| b.0.partial_cmp(&a.0).unwrap_or(std::cmp::Ordering::Equal));
    println!("\n== {label} debt: {} liquidatable v1 candidates (≥ ${min_debt}, ratio ≤ {ratio_cap}); trying top {tries} ==",
        cands.len());

    let (mut clean, mut too_small, mut other, mut tried, mut max_bytes) = (0usize, 0usize, 0usize, 0usize, 0usize);
    let debt_tp = Pubkey::from_str(CLASSIC_TOKEN_PROGRAM).unwrap();
    for (ratio, pk, o) in cands.iter() {
        if tried >= tries { break; }
        let Some(repay_reserve) = load(endpoint, reserves, o.borrows[0].reserve) else { continue };
        let Some(withdraw_reserve) = load(endpoint, reserves, o.deposits[0].reserve) else { continue };
        // same_mint_only targets the sub-1232B path (no swap leg): collateral
        // underlying == debt mint. Skip others without spending a try.
        if same_mint_only && withdraw_reserve.liquidity_mint != *debt_mint { continue; }
        let Some(ctp) = mint_owner(endpoint, &withdraw_reserve.liquidity_mint) else { continue };
        tried += 1;
        let debt_dec = 10f64.powi(repay_reserve.mint_decimals as i32);
        let repay_usd = o.borrowed_value * repay_frac;
        let repay_amount = (repay_usd / repay_reserve.market_price.max(1e-9) * debt_dec).max(1.0) as u64;
        let seized_usd = repay_usd * (1.0 + withdraw_reserve.liquidation_bonus_pct as f64 / 100.0);
        let seize_underlying = (seized_usd / withdraw_reserve.market_price.max(1e-9) * 10f64.powi(withdraw_reserve.mint_decimals as i32)) as u64;
        let cand = SaveFireCandidate {
            obligation: *pk, repay_reserve: repay_reserve.clone(), withdraw_reserve: withdraw_reserve.clone(),
            collateral_token_program: ctp, debt_token_program: debt_tp, repay_amount, seize_underlying,
            deposit_reserves: vec![withdraw_reserve.reserve], borrow_reserves: vec![repay_reserve.reserve],
        };
        let same_mint = withdraw_reserve.liquidity_mint == repay_reserve.liquidity_mint;
        let fire = match build_save_fire_tx(endpoint, &cand, authority, None, 0, 50_000, 100, max_swap_accounts, solana_hash::Hash::default()) {
            Ok(f) => f, Err(e) => { println!("  {pk} ratio {ratio:.3} ${:.0}: build failed: {e}", o.borrowed_value); other += 1; continue; }
        };
        max_bytes = max_bytes.max(fire.tx_bytes);
        let b64tx = base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&fire.tx).unwrap());
        let sim = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
            "params":[b64tx, {"sigVerify":false,"replaceRecentBlockhash":true,"commitment":"processed","encoding":"base64"}]}));
        let sm = if same_mint { " same-mint(no-swap)" } else { "" };
        match sim.as_ref().and_then(|v| v["result"].get("value")) {
            Some(v) if v["err"].is_null() => {
                clean += 1;
                println!("  ★★ {pk} ratio {ratio:.3} ${:.0}: SIMULATES CLEAN — WOULD FIRE ({}B{sm}, out {}, {} CU)",
                    o.borrowed_value, fire.tx_bytes, fire.quoted_debt_out, v["unitsConsumed"]);
            }
            Some(v) => {
                let e = v["err"].to_string();
                if e.contains("29") {
                    too_small += 1;
                    println!("  ·  {pk} ratio {ratio:.3} ${:.0}: GATED at Solend liquidate (err 29 = healthy/too-small at fresh price) ({}B{sm}) — wiring composes",
                        o.borrowed_value, fire.tx_bytes);
                } else {
                    other += 1;
                    println!("  {pk} ratio {ratio:.3} ${:.0}: OTHER err {} ({}B{sm})", o.borrowed_value, e, fire.tx_bytes);
                    for l in v["logs"].as_array().into_iter().flatten().rev().take(5).collect::<Vec<_>>().into_iter().rev() {
                        println!("       {}", l.as_str().unwrap_or(""));
                    }
                }
            }
            None => {
                other += 1;
                // No result.value → the RPC rejected the tx pre-execution (most
                // commonly "too large" when > 1232B without a SAVE_ALT).
                let err = sim.as_ref().and_then(|v| v["error"]["message"].as_str()).unwrap_or("no sim value");
                println!("  {pk} ratio {ratio:.3} ${:.0}: sim rejected ({}B{sm}): {err}", o.borrowed_value, fire.tx_bytes);
            }
        }
    }
    println!("── {label}: tried {tried} · CLEAN(would-fire) {clean} · gated-at-liquidate(composes) {too_small} · other {other} · max tx {max_bytes}B ──");
    if max_bytes > 1232 {
        println!("   ⚠ tx exceeds 1232B — a SAVE_ALT is required for live submission (set SAVE_ALT to a deployed ALT).");
    }
}

fn load(endpoint: &str, reserves: &mut HashMap<Pubkey, Reserve>, pk: Pubkey) -> Option<Reserve> {
    if let Some(r) = reserves.get(&pk) { return Some(r.clone()); }
    let raw = get_acct(endpoint, &pk)?;
    let r = Reserve::decode(pk, &raw)?;
    reserves.insert(pk, r.clone());
    Some(r)
}
fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let debt = std::env::var("DEBT").unwrap_or_else(|_| "all".into()).to_lowercase();
    let tries: usize = std::env::var("TRIES").ok().and_then(|s| s.parse().ok()).unwrap_or(25);
    let min_debt: f64 = std::env::var("MIN_DEBT").ok().and_then(|s| s.parse().ok()).unwrap_or(50.0);
    let repay_frac: f64 = std::env::var("REPAY_FRAC").ok().and_then(|s| s.parse().ok()).unwrap_or(0.2);
    let ratio_cap: f64 = std::env::var("RATIO_CAP").ok().and_then(|s| s.parse().ok()).unwrap_or(3.0);
    let max_swap_accounts: usize = std::env::var("MAX_SWAP_ACCOUNTS").ok().and_then(|s| s.parse().ok()).unwrap_or(18);
    let same_mint_only = std::env::var("SAMEMINT").map(|s| s != "0").unwrap_or(false);
    let authority = Pubkey::from_str(&std::env::var("AUTHORITY").unwrap_or_else(|_| "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak".into())).unwrap();

    // Pre-load the three debt reserves so the mint match is free.
    let mut reserves: HashMap<Pubkey, Reserve> = HashMap::new();
    for res in [save::USDC_RESERVE, save::USDT_RESERVE, save::WSOL_RESERVE] {
        let pk = Pubkey::from_str(res).unwrap();
        let _ = load(&endpoint, &mut reserves, pk);
    }

    eprintln!("[save-fire] scanning main-pool obligations …");
    let resp = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[save::SOLEND_PROGRAM, {"encoding":"base64","dataSize":1300,
            "filters":[{"dataSize":1300},{"memcmp":{"offset":10,"bytes":save::MAIN_POOL}}]}]})).expect("gPA");
    let entries = resp["result"].as_array().cloned().unwrap_or_default();
    eprintln!("[save-fire] {} obligations; debt filter = {debt}", entries.len());

    let assets: Vec<(&str, &str)> = match debt.as_str() {
        "usdc" => vec![("USDC", save::USDC_MINT)],
        "usdt" => vec![("USDT", save::USDT_MINT)],
        "wsol" | "sol" => vec![("wSOL", save::WSOL_MINT)],
        _ => vec![("USDC", save::USDC_MINT), ("USDT", save::USDT_MINT), ("wSOL", save::WSOL_MINT)],
    };
    for (label, mint) in assets {
        let m = Pubkey::from_str(mint).unwrap();
        run_asset(&endpoint, label, &m, &entries, &mut reserves, &authority,
            tries, min_debt, repay_frac, ratio_cap, max_swap_accounts, same_mint_only);
    }
}
