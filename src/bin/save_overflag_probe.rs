//! Quantify the Save two-tier gating fix + calibrate the on-chain fire gate on
//! LIVE mainnet data — read-only.
//!
//! The overflag bug: the executor flagged obligations "liquidatable" off the
//! LAZER-projected ratio, then ran a full simulateTransaction/Bundle on each. But
//! Solend settles at the ON-CHAIN oracle price, and Lazer leads/diverges — so the
//! flagged set was dominated by phantoms (healthy on-chain), a per-cycle sim flood
//! that starves a real opportunity's sim budget.
//!
//! The fix: Lazer NARROWS the watch-set; the ON-CHAIN price GATES the sim. Only
//! obligations liquidatable at the on-chain oracle price earn a sim, ranked by USD
//! deficit and capped top-K (MAX_FIRE_PER_CYCLE).
//!
//! CALIBRATION (task point 4): an obligation's STORED borrowed/unhealthy values
//! are lazily updated by Solend (only when someone refresh_obligation's it), so a
//! marginally-over-threshold obligation can sit "stored-liquidatable" while a fresh
//! refresh_reserve (fresh Pyth price) shows it healthy — the "healthy at fresh
//! price" sim rejects. This probe RE-COMPUTES each obligation's health from the
//! freshly-fetched reserve prices + amounts (cToken exchange rate from the reserve
//! bytes) and reports, for the stored-liquidatable set: (a) how many stay
//! liquidatable at the fresh RESERVE price (the calibrated fire gate), and (b) the
//! per-cycle sim reduction. If (a) is still hundreds, the residual phantoms are
//! live-Pyth-vs-cranked-reserve drift the top-K cap must absorb.
//!
//! Usage: HELIUS_RPC=<url> [MIN_DEBT=100] [WATCH_RATIO=0.85] [ARM_RATIO=0.97]
//!        [RATIO_CAP=3.0] [MAX_FIRE=4] cargo run --release --bin save_overflag_probe

use arb_engine::save::{self, Obligation, Reserve};
use arb_engine::save_engine::Engine;
use solana_pubkey::Pubkey;
use std::collections::{HashMap, HashSet};
use std::str::FromStr;
use std::time::Duration;

const LAZER_USDT: u32 = 8;

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
fn get_multiple(endpoint: &str, keys: &[Pubkey]) -> HashMap<Pubkey, Vec<u8>> {
    let mut out = HashMap::new();
    for chunk in keys.chunks(100) {
        let strs: Vec<String> = chunk.iter().map(|k| k.to_string()).collect();
        let Some(v) = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getMultipleAccounts",
            "params":[strs, {"encoding":"base64"}]})) else { continue };
        for (i, acc) in v["result"]["value"].as_array().into_iter().flatten().enumerate() {
            if let Some(b) = acc.get("data").and_then(b64) { out.insert(chunk[i], b); }
        }
    }
    out
}

// ── Solend reserve layout offsets not carried by save::Reserve (needed to derive
//    the cToken→underlying exchange rate = total_liquidity / mint_total_supply).
const R_AVAILABLE_AMOUNT: usize = 171; // u64  (liquidity available)
const R_LIQ_BORROWED_WADS: usize = 179; // u128 (liquidity borrowed, WAD)
const R_COLL_MINT_SUPPLY: usize = 259; // u64  (cToken total supply)
fn u64le(d: &[u8], o: usize) -> Option<u64> { Some(u64::from_le_bytes(d.get(o..o + 8)?.try_into().ok()?)) }
fn u128wad(d: &[u8], o: usize) -> Option<f64> { Some(u128::from_le_bytes(d.get(o..o + 16)?.try_into().ok()?) as f64 / 1e18) }

/// cToken → underlying-native multiplier for a reserve (total_liquidity /
/// mint_total_supply). ~1.0+, grows with interest. Both sides in base units.
fn ctoken_rate(raw: &[u8]) -> Option<f64> {
    let total_liq = u64le(raw, R_AVAILABLE_AMOUNT)? as f64 + u128wad(raw, R_LIQ_BORROWED_WADS)?;
    let supply = u64le(raw, R_COLL_MINT_SUPPLY)? as f64;
    if supply <= 0.0 { return None; }
    Some(total_liq / supply)
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let min_debt: f64 = std::env::var("MIN_DEBT").ok().and_then(|s| s.parse().ok()).unwrap_or(100.0);
    let watch_ratio: f64 = std::env::var("WATCH_RATIO").ok().and_then(|s| s.parse().ok()).unwrap_or(0.85);
    let arm_ratio: f64 = std::env::var("ARM_RATIO").ok().and_then(|s| s.parse().ok()).unwrap_or(0.97);
    let ratio_cap: f64 = std::env::var("RATIO_CAP").ok().and_then(|s| s.parse().ok()).unwrap_or(3.0);
    let max_fire: usize = std::env::var("MAX_FIRE").ok().and_then(|s| s.parse().ok()).unwrap_or(4);

    // Debt reserves (USDC/USDT/wSOL) — the accepted debt set. Keep raw bytes too.
    let mut reserves: HashMap<Pubkey, Reserve> = HashMap::new();
    let mut raw_of: HashMap<Pubkey, Vec<u8>> = HashMap::new();
    for res in [save::USDC_RESERVE, save::USDT_RESERVE, save::WSOL_RESERVE] {
        let pk = Pubkey::from_str(res).unwrap();
        if let Some(d) = get_acct(&endpoint, &pk) {
            if let Some(r) = Reserve::decode(pk, &d) { reserves.insert(pk, r); raw_of.insert(pk, d); }
        }
    }
    let debt_reserves: HashSet<Pubkey> = reserves.keys().copied().collect();

    eprintln!("[overflag] scanning main-pool obligations …");
    let resp = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[save::SOLEND_PROGRAM, {"encoding":"base64","dataSize":1300,
            "filters":[{"dataSize":1300},{"memcmp":{"offset":10,"bytes":save::MAIN_POOL}}]}]})).expect("gPA");
    let entries = resp["result"].as_array().cloned().unwrap_or_default();

    let mut obls: Vec<(Pubkey, Obligation)> = Vec::new();
    for e in &entries {
        let Some(pk) = e["pubkey"].as_str().and_then(|s| s.parse::<Pubkey>().ok()) else { continue };
        let Some(d) = b64(&e["account"]["data"]) else { continue };
        let Some(o) = Obligation::decode(&d) else { continue };
        if o.deposits.len() != 1 || o.borrows.len() != 1 { continue; }
        if !debt_reserves.contains(&o.borrows[0].reserve) { continue; }
        if o.borrowed_value < min_debt { continue; }
        obls.push((pk, o));
    }

    // Load collateral reserves (raw + decoded).
    let coll_pks: Vec<Pubkey> = obls.iter().map(|(_, o)| o.deposits[0].reserve).collect::<HashSet<_>>().into_iter().collect();
    for (pk, raw) in get_multiple(&endpoint, &coll_pks) {
        if let Some(r) = Reserve::decode(pk, &raw) { reserves.insert(pk, r); raw_of.insert(pk, raw); }
    }

    let mut mint_feed = arb_engine::lazer::mint_feed_map();
    mint_feed.insert(Pubkey::from_str(save::USDT_MINT).unwrap(), LAZER_USDT);

    let snap: HashMap<u32, f64> = HashMap::new();
    let mut engine = Engine::new(min_debt, ratio_cap);
    engine.rebuild(&obls, &reserves, &mint_feed, watch_ratio, &snap);

    let arm_tier = engine.crossed(&snap, arm_ratio).len();
    // OLD gate: the obligation's own stored borrowed_value > unhealthy_borrow_value
    // (what the executor used to flag + sim). NEW gate: the engine's fresh-price
    // fire tier (amounts × freshly-fetched reserve prices).
    let stored_liq: Vec<(Pubkey, f64, f64)> = obls.iter().filter_map(|(pk, o)| {
        let r = o.health_ratio();
        (o.liquidatable() && r <= ratio_cap).then_some((*pk, o.borrowed_value - o.unhealthy_borrow_value, r))
    }).collect();
    let fresh_fire = engine.onchain_liquidatable_ranked();

    // Independent cross-check of the engine's fresh gate — mirror it exactly:
    // reprice ONLY the collateral side of unhealthy_borrow_value by the collateral
    // reserve-price MOVE (fresh collateral USD / stored deposit market_value); keep
    // Solend's authoritative borrowed_value for the debt side.
    let obls_by_pk: HashMap<Pubkey, &Obligation> = obls.iter().map(|(pk, o)| (*pk, o)).collect();
    let fresh_health = |pk: &Pubkey| -> Option<(f64, f64, f64)> {
        let o = obls_by_pk.get(pk)?;
        let d = &o.deposits[0];
        let coll = reserves.get(&d.reserve)?;
        let rate = ctoken_rate(raw_of.get(&d.reserve)?)?;
        let underlying = d.deposited_amount as f64 * rate / 10f64.powi(coll.mint_decimals as i32);
        let fresh_deposit_usd = underlying * coll.market_price;
        let coll_move = if d.market_value > 1e-9 { fresh_deposit_usd / d.market_value } else { 1.0 };
        let fresh_unhealthy = o.unhealthy_borrow_value * coll_move;
        let fresh_borrow = o.borrowed_value;
        let ratio = if fresh_unhealthy <= 0.0 { 0.0 } else { fresh_borrow / fresh_unhealthy };
        Some((fresh_borrow, fresh_unhealthy, ratio))
    };

    // DIAGNOSTIC only (NOT the executor gate): a collateral-repriced recompute
    // that estimates how many stored-liquidatable obligations are stale-high
    // phantoms (healthy once the collateral is repriced to the fresh reserve
    // price). It is noisy — off-chain collateral valuation diverges ~10-15% from
    // Solend's stored deposit market_value (cToken-rate/interest), which is exactly
    // why the executor does NOT gate on a re-price: it gates on Solend's
    // authoritative stored verdict and LEARNS the phantoms by sim (they err-29 →
    // sim-rejected cooldown → dropped from the live fire set).
    let est_phantom = stored_liq.iter()
        .filter(|(pk, _, _)| matches!(fresh_health(pk), Some((_, _, fr)) if fr <= 1.0))
        .count();

    println!("\n=== Save two-tier gating — live mainnet ===");
    println!("scanned obligations (main-pool, 1300B) ....... {}", entries.len());
    println!("v1 / accepted-debt / ≥ ${min_debt:.0} .............. {}", obls.len());
    println!("engine watch-set ({watch_ratio} ≤ ratio ≤ {ratio_cap}) ..... {}  (NEVER simulated)", engine.accounts.len());
    println!("within arm({arm_ratio}) — OLD arm sim-set ....... {arm_tier}  (was sim'd every cycle)");
    println!("on-chain liquidatable (Solend stored verdict) ... {}  ← NEW fire gate (== engine.onchain_liquidatable)", fresh_fire.len());
    println!("  of which look like stale-high phantoms (diag) ... {est_phantom}  (sim-rejected → cooldown → dropped from live fire)");
    println!("fire cap (MAX_FIRE_PER_CYCLE) ................. {max_fire}");
    println!("\nper-cycle sims: OLD arm phase churned ~{arm_tier} (8/cycle) → NEW arms only the fire tier, ≤ {max_fire}/cycle");
    println!("(the fire tier converges below {max_fire} as phantoms sim-reject and enter the {}s cooldown)", 60);

    println!("\nDIAGNOSTIC — stored obligation market_value vs collateral repriced @ fresh reserve px");
    println!("(the collateral gap is the staleness — reserve px moved since the obligation's");
    println!(" last on-chain refresh, which is what left the stored health stale-high):");
    for (pk, _deficit, _r) in stored_liq.iter().take(6) {
        let o = obls_by_pk[pk];
        let d = &o.deposits[0];
        let b = &o.borrows[0];
        let coll = &reserves[&d.reserve];
        let debt = &reserves[&b.reserve];
        let rate = ctoken_rate(&raw_of[&d.reserve]).unwrap_or(0.0);
        let underlying = d.deposited_amount as f64 * rate / 10f64.powi(coll.mint_decimals as i32);
        let fresh_dep = underlying * coll.market_price;
        let fresh_bor = b.borrowed_amount_wads / 10f64.powi(debt.mint_decimals as i32) * debt.market_price;
        println!("  {}", pk);
        println!("     borrow  stored ${:.2}  fresh ${fresh_bor:.2}  (debt px ${:.4})", b.market_value, debt.market_price);
        println!("     deposit stored ${:.2}  fresh ${fresh_dep:.2}  (coll px ${:.4}, cToken rate {rate:.5}, liq_thr {}%)",
            d.market_value, coll.market_price, coll.liquidation_threshold_pct);
    }

    println!("\ntop fire-tier candidates (Solend stored deficit), stored-ratio vs collateral-fresh-ratio:");
    for (pk, deficit) in fresh_fire.iter().take(10) {
        let sr = engine.onchain_ratio_of(pk).unwrap_or(0.0);
        let (fr_s, flip) = match fresh_health(pk) {
            Some((_, _, fr)) => (format!("{fr:.4}"), if fr <= 1.0 { " → likely healthy@fresh (phantom; will sim-reject → cooldown)" } else { "" }),
            None => ("n/a".into(), ""),
        };
        println!("  {}  stored deficit ${deficit:.0} r{sr:.4}  fresh r{fr_s}{flip}", pk);
    }
}
