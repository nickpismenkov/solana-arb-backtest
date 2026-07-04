//! Verifies the shared arb builder (arb::build_arb_tx — the exact code the
//! executor runs) by simulating BOTH directions against mainnet via the ALT.
//! With no spread each direction reverts at leg2 (insufficient funds) — the
//! profit-or-revert guard. A structural error would look different (bad meta,
//! sqrt-limit, layout).
//!
//! Usage: RPC_ENDPOINT=<url> ALT_ADDRESS=<alt> [BORROW_USDC=500] [SIGNER=<pubkey>] \
//!   [SHOW_LOGS=1] cargo run --release --bin arb_probe

use arb_engine::arb::{build_arb_tx, load_alt, PoolData};
use arb_engine::pools::pair;
use base64::Engine;
use solana_hash::Hash;
use solana_pubkey::Pubkey;
use std::str::FromStr;

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..4 {
        if let Ok(resp) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = resp.into_json::<serde_json::Value>() {
                return Some(v);
            }
        }
        std::thread::sleep(std::time::Duration::from_millis(400 << attempt));
    }
    None
}

fn account_data(endpoint: &str, addr: &str) -> Vec<u8> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":[addr,{"encoding":"base64"}]})).expect("rpc");
    base64::engine::general_purpose::STANDARD.decode(v["result"]["value"]["data"][0].as_str().expect("data")).unwrap()
}

// Instruction order in build_arb_tx: [0 cu_limit, 1 cu_price, 2 ata, 3 ata,
// 4 borrow, 5 leg1, 6 leg2(guard), 7 payback, 8 tip]. Only a leg2 revert is
// the guard doing its job; a revert anywhere else is a structural bug.
const LEG2_IX: u64 = 6;

fn classify(err: &serde_json::Value, _logs: &[String]) -> String {
    if err.is_null() {
        return "✅ SIMULATED CLEAN — a profitable arb exists right now; tx would land".into();
    }
    match err["InstructionError"][0].as_u64() {
        Some(LEG2_IX) => "✅ GUARD WORKING — borrow+leg1 executed, leg2 reverted (no spread → profit-or-revert)".into(),
        Some(ix) => format!("❌ STRUCTURAL ERROR — reverted at instruction {ix} (before the guard): {err}"),
        None => format!("⚠️  inconclusive — {err}"),
    }
}

fn main() {
    let _ = dotenvy::dotenv();
    let endpoint = std::env::var("RPC_ENDPOINT").unwrap_or_else(|_| "https://api.mainnet-beta.solana.com".into());
    let alt_addr = std::env::var("ALT_ADDRESS").expect("set ALT_ADDRESS");
    let borrow_ui: f64 = std::env::var("BORROW_USDC").ok().and_then(|s| s.parse().ok()).unwrap_or(500.0);
    let borrow_amount = (borrow_ui * 1e6) as u64;
    let cfg = pair();
    let signer_str = std::env::var("SIGNER").unwrap_or_else(|_| "Anu6Awu4kxaEDrg1nkpcikx6tJ2xhfVci5TvDrZBsZEB".into());
    let signer = Pubkey::from_str(&signer_str).expect("bad SIGNER pubkey");
    let show_logs = std::env::var("SHOW_LOGS").map(|v| v == "1").unwrap_or(false);

    let alt = load_alt(&alt_addr, &account_data(&endpoint, &alt_addr));
    let pools = PoolData {
        orca: account_data(&endpoint, &cfg.orca_pool),
        ray: account_data(&endpoint, &cfg.ray_pool),
    };

    println!("arb-probe {} borrow {} USDC — verifying both directions via arb::build_arb_tx\n", cfg.label, borrow_ui);

    for orca_first in [true, false] {
        let dir = if orca_first { "orca→ray (buy Orca, sell Ray)" } else { "ray→orca (buy Ray, sell Orca)" };
        let tx = build_arb_tx(&pools, signer, &alt, borrow_amount, orca_first, None, 0, 10_000, Hash::default(), 0)
            .expect("build");
        let raw = bincode::serialize(&tx).unwrap();
        let b64 = base64::engine::general_purpose::STANDARD.encode(&raw);
        let v = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
            "params":[b64,{"encoding":"base64","sigVerify":false,"replaceRecentBlockhash":true}]}));
        if let Some(e) = v.as_ref().and_then(|v| v.get("error")).filter(|e| !e.is_null()) {
            println!("=== {dir} ===\n  ⛔ not simulated: {}\n", e["message"].as_str().unwrap_or_default());
            continue;
        }
        let val = v.map(|v| v["result"]["value"].clone()).unwrap_or_default();
        let logs: Vec<String> = val["logs"].as_array().map(|a| a.iter().filter_map(|l| l.as_str().map(String::from)).collect()).unwrap_or_default();
        println!("=== {dir} ===");
        println!("  signer {signer} | tx {} bytes | err {}", raw.len(), val["err"]);
        println!("  {}", classify(&val["err"], &logs));
        if show_logs {
            for l in &logs {
                println!("    {l}");
            }
        }
        println!();
    }
}
