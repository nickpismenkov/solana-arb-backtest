//! marginfi liquidatable-account finder (read-only, Stage 1 live test).
//!
//! Scans every MarginfiAccount in the main group, prices each position from the
//! on-chain Pyth oracle the protocol itself reads (PriceUpdateV2), computes
//! maintenance health, and lists who is liquidatable (+ the closest near-misses,
//! so we can see how tight the market is). No money moves.
//!
//! Usage: HELIUS_RPC=<https json-rpc url> cargo run --release --bin liq_finder
//!   [NEAR=20]  how many near-liquidation accounts to show

use arb_engine::liquidation::{
    self as liq, Bank, BankMap, MarginfiAccount, PriceMap,
};
use solana_pubkey::Pubkey;
use std::collections::{HashMap, HashSet};
use std::time::Duration;

const MARGINFI_PROGRAM: &str = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA";
const MARGINFI_GROUP: &str = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8";

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
    let s = data.get(0)?.as_str()?;
    base64::engine::general_purpose::STANDARD.decode(s).ok()
}

/// Batch getMultipleAccounts (100 keys/call) → map pubkey → raw bytes.
fn get_multiple(endpoint: &str, keys: &[Pubkey]) -> HashMap<Pubkey, Vec<u8>> {
    let mut out = HashMap::new();
    for chunk in keys.chunks(100) {
        let strs: Vec<String> = chunk.iter().map(|k| k.to_string()).collect();
        let Some(v) = rpc(endpoint, serde_json::json!({
            "jsonrpc":"2.0","id":1,"method":"getMultipleAccounts",
            "params":[strs, {"encoding":"base64"}]
        })) else { continue };
        for (i, acc) in v["result"]["value"].as_array().into_iter().flatten().enumerate() {
            if let Some(bytes) = acc.get("data").and_then(b64) {
                out.insert(chunk[i], bytes);
            }
        }
        std::thread::sleep(Duration::from_millis(50));
    }
    out
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC")
        .or_else(|_| std::env::var("RPC_HTTP"))
        .expect("HELIUS_RPC (a getProgramAccounts-capable JSON-RPC url) in .env");
    let near_n: usize = std::env::var("NEAR").ok().and_then(|s| s.parse().ok()).unwrap_or(20);

    // 1) All MarginfiAccounts in the main group. dataSlice trims to the balances
    //    region (1736 B) so the payload is ~half. Server-side dataSize filter
    //    still guarantees these are full 2312-byte accounts.
    eprintln!("[finder] getProgramAccounts (group {}) …", &MARGINFI_GROUP[..8]);
    let resp = rpc(&endpoint, serde_json::json!({
        "jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[MARGINFI_PROGRAM, {
            "encoding":"base64",
            "dataSlice":{"offset":0,"length":1736},
            "filters":[
                {"dataSize": liq::MA_SIZE},
                {"memcmp":{"offset":8,"bytes":MARGINFI_GROUP}}
            ]
        }]
    }));
    let accounts_json = resp.as_ref().and_then(|v| v["result"].as_array()).cloned().unwrap_or_default();
    eprintln!("[finder] {} accounts in group", accounts_json.len());
    if accounts_json.is_empty() {
        eprintln!("[finder] nothing returned — check RPC supports getProgramAccounts + the group filter");
        return;
    }

    let accounts: Vec<MarginfiAccount> = accounts_json.iter()
        .filter_map(|e| e["account"]["data"].get(0).and_then(|_| b64(&e["account"]["data"])))
        .filter_map(|bytes| MarginfiAccount::decode(&bytes))
        .filter(|a| !a.balances.is_empty())
        .collect();
    let borrowers: Vec<&MarginfiAccount> = accounts.iter()
        .filter(|a| a.balances.iter().any(|b| b.liability_shares > 0.0))
        .collect();
    eprintln!("[finder] {} accounts with balances, {} with an open borrow", accounts.len(), borrowers.len());

    // 2) Fetch every referenced Bank.
    let bank_pks: Vec<Pubkey> = borrowers.iter()
        .flat_map(|a| a.balances.iter().map(|b| b.bank_pk))
        .collect::<HashSet<_>>().into_iter().collect();
    eprintln!("[finder] fetching {} banks …", bank_pks.len());
    let bank_raw = get_multiple(&endpoint, &bank_pks);
    let mut banks: BankMap = HashMap::new();
    let mut oracle_of: HashMap<Pubkey, Pubkey> = HashMap::new();
    for (pk, raw) in &bank_raw {
        if let Some(bank) = Bank::decode(raw) {
            oracle_of.insert(*pk, bank.oracle_key);
            banks.insert(*pk, bank);
        }
    }

    // 3) Price each bank from its on-chain Pyth oracle (PriceUpdateV2).
    let oracle_pks: Vec<Pubkey> = oracle_of.values().copied().collect::<HashSet<_>>().into_iter().collect();
    eprintln!("[finder] fetching {} oracle accounts …", oracle_pks.len());
    let oracle_raw = get_multiple(&endpoint, &oracle_pks);
    let mut oracle_price: HashMap<Pubkey, f64> = HashMap::new();
    for (pk, raw) in &oracle_raw {
        if let Some((_feed, usd, _t)) = liq::decode_price_update_v2(raw) {
            oracle_price.insert(*pk, usd);
        }
    }
    let mut prices: PriceMap = HashMap::new();
    for (bank_pk, oracle_pk) in &oracle_of {
        if let Some(&p) = oracle_price.get(oracle_pk) {
            prices.insert(*bank_pk, p);
        }
    }
    eprintln!("[finder] priced {}/{} banks", prices.len(), banks.len());

    // 4) Health for every borrower.
    let mut scored: Vec<(f64, f64, &MarginfiAccount)> = borrowers.iter().map(|a| {
        let h = liq::maintenance_health(a, &banks, &prices);
        (h.ratio(), h.value(), *a)
    }).collect();
    scored.sort_by(|x, y| y.0.partial_cmp(&x.0).unwrap_or(std::cmp::Ordering::Equal));

    let liquidatable: Vec<_> = scored.iter().filter(|(_, v, _)| *v < 0.0).collect();
    println!("\n════ marginfi liquidatable finder ════");
    println!("borrowers scanned: {}", borrowers.len());
    println!("LIQUIDATABLE now:  {}", liquidatable.len());
    for (ratio, val, a) in liquidatable.iter().take(50) {
        println!("  authority {}…  liab/asset={:.4}  health={:+.2} USD",
            &a.authority.to_string()[..8], ratio, val);
    }
    println!("\nclosest to liquidation (ratio→1.0 from below):");
    for (ratio, val, a) in scored.iter().filter(|(_, v, _)| *v >= 0.0).take(near_n) {
        println!("  {}…  liab/asset={:.4}  buffer={:+.2} USD", &a.authority.to_string()[..8], ratio, val);
    }
    println!();
}
