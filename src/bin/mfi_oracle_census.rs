//! Census of marginfi bank oracles — groups the group's banks by oracle_setup
//! and inspects each oracle account (owner program, size, disc) so we know
//! exactly which decoders to build for full pricing coverage. Read-only.
//!
//! Usage: HELIUS_RPC=<url> cargo run --release --bin mfi_oracle_census

use arb_engine::liquidation::Bank;
use solana_pubkey::Pubkey;
use std::collections::HashMap;
use std::time::Duration;

const MARGINFI_PROGRAM: &str = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA";
const MARGINFI_GROUP: &str = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8";
const BANK_SIZE: u64 = 1864;

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

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");

    // Banks of the main group (Bank.group at offset 41).
    let resp = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[MARGINFI_PROGRAM, {"encoding":"base64",
            "filters":[{"dataSize":BANK_SIZE},{"memcmp":{"offset":41,"bytes":MARGINFI_GROUP}}]}]}));
    let entries = resp.as_ref().and_then(|v| v["result"].as_array()).cloned().unwrap_or_default();
    println!("{} banks in group", entries.len());

    let mut by_setup: HashMap<u8, Vec<(Pubkey, Bank)>> = HashMap::new();
    for e in &entries {
        let Some(pk) = e["pubkey"].as_str().and_then(|s| s.parse().ok()) else { continue };
        let Some(raw) = b64(&e["account"]["data"]) else { continue };
        let Some(bank) = Bank::decode(&raw) else { continue };
        by_setup.entry(bank.oracle_setup).or_default().push((pk, bank));
    }

    for (setup, banks) in {
        let mut v: Vec<_> = by_setup.iter().collect();
        v.sort_by_key(|(s, _)| **s);
        v
    } {
        println!("\n──── oracle_setup={} ({} banks)", setup, banks.len());
        for (pk, bank) in banks.iter().take(6) {
            let info = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
                "params":[bank.oracle_key.to_string(), {"encoding":"base64"}]}));
            let (owner, len, disc) = info.as_ref()
                .map(|v| &v["result"]["value"])
                .map(|val| {
                    let owner = val["owner"].as_str().unwrap_or("MISSING").to_string();
                    let data = val["data"].get(0).and_then(|s| s.as_str())
                        .map(|s| { use base64::Engine; base64::engine::general_purpose::STANDARD.decode(s).unwrap_or_default() })
                        .unwrap_or_default();
                    let disc = data.iter().take(8).map(|b| format!("{b:02x}")).collect::<String>();
                    (owner, data.len(), disc)
                }).unwrap_or(("?".into(), 0, String::new()));
            println!("  bank {}…  mint {}…  oracle {}  owner {}  len {} disc {}",
                &pk.to_string()[..8], &bank.mint.to_string()[..8], bank.oracle_key, owner, len, disc);
        }
        if banks.len() > 6 { println!("  … +{} more", banks.len() - 6); }
    }
}
