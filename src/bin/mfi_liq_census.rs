//! Census the accounts our emode-aware maintenance_health flags as liquidatable
//! (ratio ≥ 1.0, on-chain prices), categorized by shape — to explain why the
//! live engine still reports a large "liquidatable now" after the emode fix:
//! are they FIREABLE v1 accounts (1 collateral / 1 USDC debt / crankable), or
//! non-v1 accounts the fire path skips anyway (multi-position, non-USDC debt)?
//!
//! Usage: HELIUS_RPC=<url> [SAMPLE=5] cargo run --release --bin mfi_liq_census

use arb_engine::liquidation::{self as liq, Bank, BankMap, MarginfiAccount, PriceMap};
use arb_engine::marginfi;
use arb_engine::pyth_crank;
use solana_pubkey::Pubkey;
use std::collections::{HashMap, HashSet};
use std::str::FromStr;
use std::time::Duration;

const MARGINFI_PROGRAM: &str = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA";
const MARGINFI_GROUP: &str = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8";

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
    let sample: usize = std::env::var("SAMPLE").ok().and_then(|s| s.parse().ok()).unwrap_or(5);
    let usdc_bank = Pubkey::from_str(marginfi::USDC_BANK).unwrap();

    let resp = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[MARGINFI_PROGRAM, {"encoding":"base64","dataSlice":{"offset":0,"length":1736},
            "filters":[{"dataSize":liq::MA_SIZE},{"memcmp":{"offset":8,"bytes":MARGINFI_GROUP}}]}]})).expect("scan");
    let accts: Vec<(Pubkey, MarginfiAccount)> = resp["result"].as_array().cloned().unwrap_or_default().iter().filter_map(|e| {
        Some((e["pubkey"].as_str()?.parse().ok()?, MarginfiAccount::decode(&b64(&e["account"]["data"])?)?))
    }).filter(|(_, a): &(Pubkey, MarginfiAccount)| a.balances.iter().any(|b| b.liability_shares > 0.0)).collect();
    let bank_pks: Vec<Pubkey> = accts.iter().flat_map(|(_, a)| a.balances.iter().map(|b| b.bank_pk)).collect::<HashSet<_>>().into_iter().collect();
    let bank_raw = get_multiple(&endpoint, &bank_pks);
    let mut banks: BankMap = HashMap::new();
    let mut oracle_of = HashMap::new();
    for (pk, r) in &bank_raw { if let Some(bk) = Bank::decode(r) { oracle_of.insert(*pk, bk.oracle_key); banks.insert(*pk, bk); } }
    let oracle_pks: Vec<Pubkey> = oracle_of.values().copied().collect::<HashSet<_>>().into_iter().collect();
    let oracle_raw = get_multiple(&endpoint, &oracle_pks);
    let mut price_by_oracle: HashMap<Pubkey, f64> = HashMap::new();
    for (pk, r) in &oracle_raw { if let Some(p) = liq::decode_oracle_price(r) { price_by_oracle.insert(*pk, p); } }
    let mut crankable = HashSet::new();
    for (bank, oracle) in &oracle_of {
        if let Some((fid, _, _)) = oracle_raw.get(oracle).and_then(|r| liq::decode_price_update_v2(r)) {
            if pyth_crank::sponsored_feed(0, &fid) == *oracle { crankable.insert(*bank); }
        }
    }
    let prices: PriceMap = oracle_of.iter().filter_map(|(bk, oc)| Some((*bk, *price_by_oracle.get(oc)?))).collect();
    println!("{} borrowers, {} banks priced, {} crankable", accts.len(), prices.len(), crankable.len());

    // MIN_COLLATERAL mirrors the executor's filter (default 100) — excludes the
    // tiny/mis-priced (weighted-assets≈0, absurd-ratio) accounts.
    let min_collateral: f64 = std::env::var("MIN_COLLATERAL_USD").ok().and_then(|s| s.parse().ok()).unwrap_or(100.0);

    // Categorize the liquidatable (emode-aware, on-chain price) set by shape,
    // AFTER the min-collateral filter — i.e. what the engine would actually see.
    let (mut v1_usdc, mut v1_crank, mut v1_nonusdc, mut multi, mut missing, mut tiny) = (0u32, 0u32, 0u32, 0u32, 0u32, 0u32);
    let mut examples: Vec<(f64, String, String)> = Vec::new();
    for (pk, a) in &accts {
        let r = liq::maintenance_health(a, &banks, &prices);
        if r.missing > 0 { if r.health.liquidatable() { missing += 1; } continue; }
        if !r.health.liquidatable() { continue; }
        if r.health.weighted_assets < min_collateral { tiny += 1; continue; }
        let assets: Vec<_> = a.balances.iter().filter(|b| b.asset_shares > 0.0).collect();
        let liabs: Vec<_> = a.balances.iter().filter(|b| b.liability_shares > 0.0).collect();
        let cat = if assets.len() == 1 && liabs.len() == 1 {
            if liabs[0].bank_pk == usdc_bank {
                v1_usdc += 1;
                if crankable.contains(&assets[0].bank_pk) { v1_crank += 1; }
                "v1_USDC_debt(FIREABLE)"
            } else { v1_nonusdc += 1; "v1_nonUSDC_debt(skip)" }
        } else { multi += 1; "multi(skip)" };
        if examples.len() < sample * 4 {
            examples.push((r.health.ratio(), cat.into(), pk.to_string()));
        }
    }
    println!("\nLIQUIDATABLE (emode-aware, on-chain price, collateral ≥ ${min_collateral}):");
    println!("  v1 USDC-debt  (FIREABLE): {v1_usdc}   (of which crankable: {v1_crank})");
    println!("  v1 non-USDC-debt (skip):  {v1_nonusdc}");
    println!("  multi-position   (skip):  {multi}");
    println!("  below min-collateral (excluded): {tiny}");
    println!("  incomplete/missing price: {missing}");
    println!("\nexamples (ratio, category, account):");
    examples.sort_by(|a, b| b.0.partial_cmp(&a.0).unwrap());
    for (ratio, cat, pk) in examples.iter().take(sample * 2) {
        println!("  {ratio:.3}  {cat:14}  {pk}");
    }
}
