//! Production marginfi liquidation executor — detection core (DRY_RUN default).
//!
//! Signal design (learned from the emode false-positive): don't replicate
//! marginfi's emode/risk math off-chain — SIMULATE the liquidate and let
//! marginfi itself be the judge. Base-weight health is only a cheap prefilter
//! (a strict superset of truly-liquidatable, since emode only RAISES asset
//! weights), then a `simulateTransaction` gate confirms each candidate:
//!   err == HealthyAccount(6068)  → healthy (emode etc.), skip
//!   anything else / success      → genuinely liquidatable → fire candidate
//!
//! This loop scans, prefilters, simulation-gates, and logs true opportunities.
//! DRY_RUN=1 (default) never builds/sends a fire tx; the atomic fire path
//! (withdraw+swap+repay via Sender) is the next stage.
//!
//! Usage: HELIUS_RPC=<url> [DRY_RUN=1] [MIN_COLLATERAL_USD=100] [PACE_MS=200]
//!        cargo run --release --bin liq_executor

use arb_engine::liquidation::{self as liq, Bank, BankMap, MarginfiAccount, PriceMap};
use arb_engine::marginfi;
use solana_instruction::AccountMeta;
use solana_pubkey::Pubkey;
use std::collections::{HashMap, HashSet};
use std::str::FromStr;
use std::time::Duration;

const MARGINFI_PROGRAM: &str = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA";
const MARGINFI_GROUP: &str = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8";
const TOKEN_PROGRAM: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
const DEFAULT_LIQUIDATOR_MA: &str = "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD";
const DEFAULT_AUTHORITY: &str = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak";
const HEALTHY_ACCOUNT_ERR: u32 = 6068;

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

/// Simulation gate: build [start_fl, liquidate(tiny), end_fl] for one candidate
/// and simulate. Returns Some(true) if marginfi considers it liquidatable
/// (proceeds past the health check), Some(false) if HealthyAccount, None on
/// an inconclusive/rpc error.
#[allow(clippy::too_many_arguments)]
fn simulate_gate(
    endpoint: &str, blockhash: &solana_hash::Hash, authority: &Pubkey, liquidator_ma: &Pubkey, tp: &Pubkey,
    liquidatee: &Pubkey, acct: &MarginfiAccount, asset_bank: Pubkey, liab_bank: Pubkey,
    asset_amount: u64, oracle_of: &HashMap<Pubkey, Pubkey>,
) -> Option<bool> {
    use solana_message::{v0, VersionedMessage};
    use solana_transaction::versioned::VersionedTransaction;
    let mut liquidatee_obs = Vec::new();
    for b in &acct.balances {
        liquidatee_obs.push(AccountMeta::new_readonly(b.bank_pk, false));
        liquidatee_obs.push(AccountMeta::new_readonly(*oracle_of.get(&b.bank_pk)?, false));
    }
    let start = marginfi::start_flashloan(liquidator_ma, authority, 2);
    let liq_ix = marginfi::lending_account_liquidate(
        &asset_bank, &liab_bank, liquidator_ma, authority, liquidatee, tp, asset_amount,
        oracle_of.get(&asset_bank)?, oracle_of.get(&liab_bank)?, &liquidatee_obs);
    let end_obs = vec![
        AccountMeta::new_readonly(asset_bank, false), AccountMeta::new_readonly(*oracle_of.get(&asset_bank)?, false),
        AccountMeta::new_readonly(liab_bank, false), AccountMeta::new_readonly(*oracle_of.get(&liab_bank)?, false),
    ];
    let end = marginfi::end_flashloan(liquidator_ma, authority, &end_obs);
    let msg = v0::Message::try_compile(authority, &[start, liq_ix, end], &[], *blockhash).ok()?;
    let tx = VersionedTransaction { signatures: vec![Default::default()], message: VersionedMessage::V0(msg) };
    let b64tx = { use base64::Engine; base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&tx).ok()?) };
    let sim = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
        "params":[b64tx, {"sigVerify":false,"replaceRecentBlockhash":true,"commitment":"processed","encoding":"base64"}]}))?;
    let err = &sim["result"]["value"]["err"];
    if err.is_null() { return Some(true); } // proceeded fully
    // Custom error code?
    let code = err.get("InstructionError").and_then(|e| e.get(1)).and_then(|c| c.get("Custom")).and_then(|c| c.as_u64());
    match code {
        Some(c) if c as u32 == HEALTHY_ACCOUNT_ERR => Some(false),
        Some(_) => Some(true), // reached liquidation logic then failed elsewhere = liquidatable
        None => None,
    }
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let dry_run = std::env::var("DRY_RUN").map(|s| s != "0").unwrap_or(true);
    let min_collateral: f64 = std::env::var("MIN_COLLATERAL_USD").ok().and_then(|s| s.parse().ok()).unwrap_or(100.0);
    let pace = Duration::from_millis(std::env::var("PACE_MS").ok().and_then(|s| s.parse().ok()).unwrap_or(200));
    let liquidator_ma = Pubkey::from_str(&std::env::var("LIQUIDATOR_MA").unwrap_or_else(|_| DEFAULT_LIQUIDATOR_MA.into())).unwrap();
    let authority = Pubkey::from_str(&std::env::var("AUTHORITY").unwrap_or_else(|_| DEFAULT_AUTHORITY.into())).unwrap();
    let tp = Pubkey::from_str(TOKEN_PROGRAM).unwrap();
    eprintln!("[exec] marginfi liquidation executor  DRY_RUN={}  min_collateral=${}", dry_run, min_collateral);

    // Scan + price (base-weight prefilter).
    eprintln!("[exec] scanning marginfi group …");
    let resp = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[MARGINFI_PROGRAM, {"encoding":"base64","dataSlice":{"offset":0,"length":1736},
            "filters":[{"dataSize":liq::MA_SIZE},{"memcmp":{"offset":8,"bytes":MARGINFI_GROUP}}]}]}));
    let entries = resp.as_ref().and_then(|v| v["result"].as_array()).cloned().unwrap_or_default();
    let accts: Vec<(Pubkey, MarginfiAccount)> = entries.iter().filter_map(|e| {
        Some((e["pubkey"].as_str()?.parse().ok()?, MarginfiAccount::decode(&b64(&e["account"]["data"])?)?))
    }).filter(|(_, a): &(Pubkey, MarginfiAccount)| a.balances.iter().any(|b| b.liability_shares > 0.0)).collect();

    let bank_pks: Vec<Pubkey> = accts.iter().flat_map(|(_, a)| a.balances.iter().map(|b| b.bank_pk)).collect::<HashSet<_>>().into_iter().collect();
    let bank_raw = get_multiple(&endpoint, &bank_pks);
    let mut banks: BankMap = HashMap::new();
    let mut oracle_of: HashMap<Pubkey, Pubkey> = HashMap::new();
    for (pk, raw) in &bank_raw { if let Some(bk) = Bank::decode(raw) { oracle_of.insert(*pk, bk.oracle_key); banks.insert(*pk, bk); } }
    let oracle_pks: Vec<Pubkey> = oracle_of.values().copied().collect::<HashSet<_>>().into_iter().collect();
    let mut prices: PriceMap = HashMap::new();
    for (pk, raw) in &get_multiple(&endpoint, &oracle_pks) {
        if let Some((_f, usd, _t)) = liq::decode_price_update_v2(raw) {
            for (bk, oc) in &oracle_of { if oc == pk { prices.insert(*bk, usd); } }
        }
    }

    // Base-weight-liquidatable candidates (superset).
    let mut candidates = 0;
    for (pk, a) in &accts {
        let r = liq::maintenance_health(a, &banks, &prices);
        if r.missing > 0 || !r.health.liquidatable() || r.health.weighted_assets < min_collateral { continue; }
        let assets: Vec<_> = a.balances.iter().filter(|b| b.asset_shares > 0.0).collect();
        let liabs: Vec<_> = a.balances.iter().filter(|b| b.liability_shares > 0.0).collect();
        if assets.len() != 1 || liabs.len() != 1 { continue; }
        candidates += 1;
    }
    eprintln!("[exec] {} borrowers, {} base-weight-liquidatable candidates (collateral ≥ ${}) → simulation-gating …",
        accts.len(), candidates, min_collateral);

    let bh = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getLatestBlockhash","params":[{"commitment":"finalized"}]}))
        .and_then(|v| v["result"]["value"]["blockhash"].as_str().map(String::from)).unwrap();
    let bh = solana_hash::Hash::from_str(&bh).unwrap();

    let (mut confirmed, mut healthy, mut inconclusive) = (0, 0, 0);
    for (pk, a) in &accts {
        let r = liq::maintenance_health(a, &banks, &prices);
        if r.missing > 0 || !r.health.liquidatable() || r.health.weighted_assets < min_collateral { continue; }
        let assets: Vec<_> = a.balances.iter().filter(|b| b.asset_shares > 0.0).cloned().collect();
        let liabs: Vec<_> = a.balances.iter().filter(|b| b.liability_shares > 0.0).cloned().collect();
        if assets.len() != 1 || liabs.len() != 1 { continue; }
        let asset_bank = assets[0].bank_pk;
        let native = assets[0].asset_shares * banks[&asset_bank].asset_share_value;
        let asset_amount = (native * 0.02) as u64;
        match simulate_gate(&endpoint, &bh, &authority, &liquidator_ma, &tp, pk, a, asset_bank, liabs[0].bank_pk, asset_amount, &oracle_of) {
            Some(true) => {
                confirmed += 1;
                println!("★ LIQUIDATABLE (sim-confirmed) {}  collateral≈${:.0}  asset_bank {}…",
                    &a.authority.to_string()[..8], r.health.weighted_assets, &asset_bank.to_string()[..8]);
                if !dry_run { println!("  → [LIVE fire path not yet wired — atomic withdraw+swap+repay is the next stage]"); }
            }
            Some(false) => healthy += 1,
            None => inconclusive += 1,
        }
        std::thread::sleep(pace);
    }
    println!("\n──── executor pass ────");
    println!("sim-confirmed liquidatable: {confirmed}");
    println!("healthy (emode false-positives filtered by sim gate): {healthy}");
    println!("inconclusive: {inconclusive}");
    if confirmed == 0 { println!("→ no genuinely liquidatable marginfi accounts this pass (matches validated Kamino=0)"); }
}
