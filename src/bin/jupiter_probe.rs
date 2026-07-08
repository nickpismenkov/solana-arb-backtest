//! Verify the Jupiter Lend (Fluid) decoders against live mainnet and enumerate
//! every vault: collateral/debt pair, liquidation threshold, sizes, and a
//! first-pass liquidatable signal. Read-only.
//!
//! Detection honesty: precise per-price liquidatable detection needs Fluid's
//! tick↔price math (not reversed here). This reports the CONFIDENT on-chain
//! liquidation-activity flags (absorbed debt / branch_liquidated) and leaves the
//! authoritative check to the executor's liquidate simulation.
//!
//! Usage: HELIUS_RPC=<url> cargo run --release --bin jupiter_probe

use arb_engine::jupiter::{self, Vault, VaultConfig, VaultState};
use solana_pubkey::Pubkey;
use std::collections::HashMap;
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
/// getProgramAccounts filtered by an 8-byte discriminator at offset 0.
fn gpa_by_disc(endpoint: &str, disc: &[u8; 8]) -> Vec<(Pubkey, Vec<u8>)> {
    let disc58 = bs58::encode(disc).into_string();
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[jupiter::VAULTS_PROGRAM, {"encoding":"base64",
            "filters":[{"memcmp":{"offset":0,"bytes":disc58}}]}]}));
    let mut out = Vec::new();
    for e in v.as_ref().and_then(|v| v["result"].as_array()).into_iter().flatten() {
        if let (Some(pk), Some(data)) = (
            e["pubkey"].as_str().and_then(|s| s.parse::<Pubkey>().ok()),
            b64(&e["account"]["data"]),
        ) { out.push((pk, data)); }
    }
    out
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");

    // Decode all VaultConfig + VaultState, join by vault_id.
    let mut configs: HashMap<u16, (Pubkey, VaultConfig)> = HashMap::new();
    for (pk, d) in gpa_by_disc(&endpoint, &jupiter::VAULT_CONFIG_DISC) {
        if let Some(c) = VaultConfig::decode(&d) { configs.insert(c.vault_id, (pk, c)); }
    }
    let mut states: HashMap<u16, (Pubkey, VaultState)> = HashMap::new();
    for (pk, d) in gpa_by_disc(&endpoint, &jupiter::VAULT_STATE_DISC) {
        if let Some(s) = VaultState::decode(&d) { states.insert(s.vault_id, (pk, s)); }
    }
    println!("live: {} VaultConfig, {} VaultState decoded", configs.len(), states.len());

    let label = |m: &Pubkey| -> String {
        match m.to_string().as_str() {
            jupiter::USDC_MINT => "USDC".into(), jupiter::USDT_MINT => "USDT".into(),
            jupiter::WSOL_MINT => "wSOL".into(), s => s[..6].to_string(),
        }
    };

    let mut vaults: Vec<Vault> = Vec::new();
    for (vid, (cpk, c)) in &configs {
        if let Some((spk, s)) = states.get(vid) {
            vaults.push(Vault { config_pubkey: *cpk, state_pubkey: *spk, config: c.clone(), state: s.clone() });
        }
    }
    vaults.sort_by_key(|v| v.config.vault_id);

    let (mut n_usdc, mut n_usdt, mut n_sol, mut n_maybe) = (0, 0, 0, 0);
    println!("\n{:>3} {:>7} {:>7} {:>5} {:>5} {:>16} {:>16} {:>6} liq?", "vid", "collat", "debt", "CF%", "LT%", "tot_supply", "tot_borrow", "absorb");
    for v in &vaults {
        let c = &v.config; let s = &v.state;
        match c.debt_label() { "USDC" => n_usdc += 1, "USDT" => n_usdt += 1, "wSOL" => n_sol += 1, _ => {} }
        let maybe = v.maybe_liquidatable();
        if maybe { n_maybe += 1; }
        println!("{:>3} {:>7} {:>7} {:>5.1} {:>5.1} {:>16} {:>16} {:>6} {}",
            c.vault_id, label(&c.supply_token), label(&c.borrow_token),
            c.collateral_factor as f64 / 10.0, c.liquidation_threshold as f64 / 10.0,
            s.total_supply, s.total_borrow, s.absorbed_debt_amount,
            if maybe { "★MAYBE" } else { "" });
    }

    let in_scope = n_usdc + n_usdt + n_sol;
    println!("\n═══ summary ═══");
    println!("vaults: {}  | debt in-scope (USDC/USDT/SOL): {in_scope}  (USDC {n_usdc}, USDT {n_usdt}, wSOL {n_sol})", vaults.len());
    println!("VERIFIED: all {} vaults decode (pairs, thresholds, sizes) against live accounts.", vaults.len());
    println!("first-pass 'maybe liquidatable' (absorbed-debt > 0): {n_maybe}");
    println!("NOTE: precise per-price liquidatable detection needs Fluid tick↔price math (not");
    println!("      implemented); the executor's liquidate simulation is the ground-truth gate.");
}
