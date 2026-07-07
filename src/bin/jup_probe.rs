//! Verify the Jupiter swap client end-to-end by mainnet SIMULATION (no send):
//! quote 0.005 SOL → USDC for the live wallet, decode the swap-instructions
//! response, fetch its lookup tables, compile a v0 tx, and simulate. Success =
//! err null with real CU spent — proves quote parse, ix decode, ALT fetch, and
//! v0 compile are all correct before the fire path trusts them.
//!
//! Usage: HELIUS_RPC=<url> [AUTHORITY=<pk>] [AMOUNT_LAMPORTS=5000000] cargo run --release --bin jup_probe

use arb_engine::arb::cu_limit_ix;
use arb_engine::jup;
use solana_pubkey::Pubkey;
use std::str::FromStr;
use std::time::Duration;

const SOL_MINT: &str = "So11111111111111111111111111111111111111112";
const USDC_MINT: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const DEFAULT_AUTHORITY: &str = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak";

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..4 {
        if let Ok(resp) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = resp.into_json::<serde_json::Value>() { return Some(v); }
        }
        std::thread::sleep(Duration::from_millis(400 << attempt));
    }
    None
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let authority = Pubkey::from_str(&std::env::var("AUTHORITY").unwrap_or_else(|_| DEFAULT_AUTHORITY.into())).unwrap();
    let amount: u64 = std::env::var("AMOUNT_LAMPORTS").ok().and_then(|s| s.parse().ok()).unwrap_or(5_000_000);
    let sol = Pubkey::from_str(SOL_MINT).unwrap();
    let usdc = Pubkey::from_str(USDC_MINT).unwrap();

    eprintln!("[jup] quoting {} lamports SOL → USDC …", amount);
    let quote = jup::quote(&sol, &usdc, amount, 50, 30).expect("quote");
    eprintln!("[jup] route: in={} out={} ({} hops)",
        quote["inAmount"], quote["outAmount"],
        quote["routePlan"].as_array().map(|r| r.len()).unwrap_or(0));

    let plan = jup::swap_instructions(&quote, &authority, true).expect("swap-instructions");
    eprintln!("[jup] {} instructions, {} lookup tables, quoted_out={} min_out={}",
        plan.instructions.len(), plan.alt_addresses.len(), plan.quoted_out, plan.min_out);

    let alts = jup::fetch_alts(&endpoint, &plan.alt_addresses).expect("fetch ALTs");
    for a in &alts { eprintln!("[jup]   ALT {} ({} addresses)", a.key, a.addresses.len()); }

    // Compile [cu_limit, setup…, swap, cleanup…] and simulate.
    use solana_message::{v0, VersionedMessage};
    use solana_transaction::versioned::VersionedTransaction;
    let mut ixs = vec![cu_limit_ix(1_400_000)];
    ixs.extend(plan.instructions);
    let msg = v0::Message::try_compile(&authority, &ixs, &alts, solana_hash::Hash::default()).expect("compile v0");
    let tx = VersionedTransaction { signatures: vec![Default::default()], message: VersionedMessage::V0(msg) };
    let raw = bincode::serialize(&tx).unwrap();
    eprintln!("[jup] tx size: {} bytes (limit 1232)", raw.len());
    let b64tx = { use base64::Engine; base64::engine::general_purpose::STANDARD.encode(&raw) };

    let sim = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
        "params":[b64tx, {"sigVerify":false,"replaceRecentBlockhash":true,"commitment":"processed","encoding":"base64"}]}))
        .expect("simulate");
    let res = &sim["result"]["value"];
    println!("\n──── jup swap simulation ────");
    println!("err: {}", res["err"]);
    println!("unitsConsumed: {}", res["unitsConsumed"]);
    if res["err"].is_null() {
        println!("★ VERIFIED — Jupiter-built swap executes clean via our decode/compile path");
    } else if let Some(logs) = res["logMessages"].as_array() {
        for l in logs { println!("  {}", l.as_str().unwrap_or("")); }
    }
}
