//! Live verification of Pyth Lazer pre-positioning (run on the box, where
//! PYTH_LAZER_TOKEN lives). Streams the volatile majors, scans the marginfi
//! watch-set once, then each interval recomputes health with Lazer prices
//! blended over the on-chain baseline and prints the nearest-to-liquidation
//! accounts + the Lazer-vs-on-chain price delta per major. Confirms (a) the
//! feed is live, (b) the mint→feed mapping resolves banks, and (c) Lazer leads
//! the on-chain oracle (nonzero delta = the pre-positioning edge).
//!
//! Usage: HELIUS_RPC=<url> PYTH_LAZER_TOKEN=<token> [INTERVAL_MS=2000]
//!        cargo run --release --bin lazer_probe

use arb_engine::lazer;
use arb_engine::liquidation::{self as liq, Bank, BankMap, MarginfiAccount, PriceMap};
use arb_engine::pyth;
use solana_pubkey::Pubkey;
use std::collections::{HashMap, HashSet};
use std::time::Duration;

const MARGINFI_PROGRAM: &str = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA";
const MARGINFI_GROUP: &str = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8";

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

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let token = std::env::var("PYTH_LAZER_TOKEN").expect("PYTH_LAZER_TOKEN (lives on the box)");
    let interval = Duration::from_millis(std::env::var("INTERVAL_MS").ok().and_then(|s| s.parse().ok()).unwrap_or(2000));

    let table = pyth::new_table();
    lazer::spawn_lazer_thread(token, lazer::arm_feed_ids(), table.clone());
    eprintln!("[lazer] subscribed to majors; scanning marginfi group …");

    // Scan borrowers + banks + on-chain oracle prices once (baseline).
    let resp = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[MARGINFI_PROGRAM, {"encoding":"base64","dataSlice":{"offset":0,"length":1736},
            "filters":[{"dataSize":liq::MA_SIZE},{"memcmp":{"offset":8,"bytes":MARGINFI_GROUP}}]}]}));
    let entries = resp.as_ref().and_then(|v| v["result"].as_array()).cloned().unwrap_or_default();
    let accts: Vec<(Pubkey, MarginfiAccount)> = entries.iter().filter_map(|e| {
        Some((e["pubkey"].as_str()?.parse().ok()?, MarginfiAccount::decode(&b64(&e["account"]["data"])?)?))
    }).filter(|(_, a): &(Pubkey, MarginfiAccount)| a.balances.iter().any(|b| b.liability_shares > 0.0)).collect();
    let bank_pks: Vec<Pubkey> = accts.iter().flat_map(|(_, a)| a.balances.iter().map(|b| b.bank_pk)).collect::<HashSet<_>>().into_iter().collect();
    let mut banks: BankMap = HashMap::new();
    let mut oracle_of: HashMap<Pubkey, Pubkey> = HashMap::new();
    for (pk, raw) in &get_multiple(&endpoint, &bank_pks) {
        if let Some(bk) = Bank::decode(raw) { oracle_of.insert(*pk, bk.oracle_key); banks.insert(*pk, bk); }
    }
    let oracle_pks: Vec<Pubkey> = oracle_of.values().copied().collect::<HashSet<_>>().into_iter().collect();
    let mut on_chain: PriceMap = HashMap::new();
    for (pk, raw) in &get_multiple(&endpoint, &oracle_pks) {
        if let Some(usd) = liq::decode_oracle_price(raw) {
            for (bk, oc) in &oracle_of { if oc == pk { on_chain.insert(*bk, usd); } }
        }
    }
    let map = lazer::mint_feed_map();
    eprintln!("[lazer] {} borrowers, {} banks, {} on-chain-priced", accts.len(), banks.len(), on_chain.len());

    loop {
        std::thread::sleep(interval);
        if pyth::get(&table, lazer::LAZER_SOL).is_none() {
            eprintln!("[lazer] waiting for first tick …");
            continue;
        }
        let (blended, led) = lazer::blend(&banks, &on_chain, &table, &map);
        // Lazer-vs-on-chain delta on SOL (the leading-edge proof).
        let sol_delta = banks.iter().find(|(_, b)| map.get(&b.mint) == Some(&lazer::LAZER_SOL))
            .and_then(|(pk, _)| Some((on_chain.get(pk)?, blended.get(pk)?)))
            .map(|(oc, lz)| format!("SOL on-chain ${oc:.2} → Lazer ${lz:.2} (Δ{:+.2})", lz - oc))
            .unwrap_or_else(|| "no SOL bank".into());

        // Nearest-to-liquidation by Lazer-blended health.
        let mut ranked: Vec<(Pubkey, f64, f64)> = accts.iter().filter_map(|(pk, a)| {
            let r = liq::maintenance_health(a, &banks, &blended);
            (r.missing == 0 && r.health.weighted_assets >= 100.0).then_some((*pk, r.health.ratio(), r.health.weighted_assets))
        }).collect();
        ranked.sort_by(|a, b| b.1.total_cmp(&a.1));
        println!("\n[{}] {}  ({} banks Lazer-led)", lazer::status(&table), sol_delta, led);
        for (pk, ratio, assets) in ranked.iter().take(5) {
            println!("  {}  ratio {:.4}  collateral ${:.0}{}",
                &pk.to_string()[..8], ratio, assets, if *ratio >= 1.0 { "  ← LIQUIDATABLE (Lazer)" } else { "" });
        }
    }
}
