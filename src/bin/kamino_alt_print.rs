//! Emit the address set for the Kamino liquidation ALT: fixed accounts
//! (programs, sysvars, main market + authority + scope, USDC repay-reserve set,
//! JupLend flash-loan constants, wallet + USDC ATA) plus the TOP-K collateral
//! reserves by deposit frequency, each with its 5 liquidate sub-accounts. This
//! compresses the fire tx under 1232 bytes for the common collateral; rare
//! collateral falls back to inline (executor logs + skips if it overflows).
//!
//! Setup (one-time):
//!   solana address-lookup-table create --keypair ~/arb-keypair.json -u <rpc>
//!   solana address-lookup-table extend <TABLE> --addresses "$(kamino_alt_print | paste -sd, -)" …
//!
//! Usage: HELIUS_RPC=<url> [AUTHORITY=<pk>] [TOP_K=20] cargo run --release --bin kamino_alt_print

use arb_engine::kamino_ix::{lending_market_authority, ReserveAccounts};
use solana_pubkey::Pubkey;
use std::collections::HashMap;
use std::str::FromStr;
use std::time::Duration;

const KLEND: &str = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD";
const MAIN_MARKET: &str = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF";
const USDC_MINT: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const OBLIGATION_SIZE: usize = 3344;
const RESERVE_SIZE: usize = 8624;
const DEFAULT_AUTHORITY: &str = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak";

// JupLend flash-loan constants (from flashloan.rs).
const JUP_LEND_PROGRAM: &str = "jupgfSgfuAXv4B6R2Uxu85Z1qdzgju79s6MfZekN6XS";
const JUP_M: &[&str] = &[
    "ALXWtv2P4GqH1B7Lq731joag52yRBRqmHV4naiXPTYWL",
    "94vK29npVbyRHXH63rRcTiSr26SFhrQTzbpNJuhQEDu",
    "J9dyC4pBTBPvzzPh7J9rhFhg8RvgerDNKkUH9kEwGMsj",
    "5pjzT5dFTsXcwixoab1QDLvZQvpYJxJeBphkyfHGn688",
    "BmkUoKMFYBxNSzWXyUjyMJjMAaVz4d8ZnxwwmhDCUXFB",
    "7s1da8DduuBFqGra5bJBjpnvL5E9mGzCuMk1Qkh4or2Z",
    "jupeiUmn818Jg1ekPURTpr4mFo29p46vygyykFJ3wZC",
];

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
    let authority = std::env::var("AUTHORITY").unwrap_or_else(|_| DEFAULT_AUTHORITY.into());
    let top_k: usize = std::env::var("TOP_K").ok().and_then(|s| s.parse().ok()).unwrap_or(20);
    let market = Pubkey::from_str(MAIN_MARKET).unwrap();
    let auth_pk = Pubkey::from_str(&authority).unwrap();
    let usdc = Pubkey::from_str(USDC_MINT).unwrap();

    // Rank collateral reserves by deposit frequency across main-market obligations.
    let resp = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[KLEND, {"encoding":"base64","dataSlice":{"offset":0,"length":2288},
            "filters":[{"dataSize":OBLIGATION_SIZE},{"memcmp":{"offset":32,"bytes":MAIN_MARKET}}]}]}));
    let entries = resp.as_ref().and_then(|v| v["result"].as_array()).cloned().unwrap_or_default();
    let mut freq: HashMap<Pubkey, u32> = HashMap::new();
    for e in &entries {
        let Some(ob) = b64(&e["account"]["data"]).and_then(|d| arb_engine::kamino::Obligation::decode(&d)) else { continue };
        for (r, _) in &ob.deposits { *freq.entry(*r).or_default() += 1; }
    }
    let mut ranked: Vec<(Pubkey, u32)> = freq.into_iter().collect();
    ranked.sort_by(|a, b| b.1.cmp(&a.1));
    let top: Vec<Pubkey> = ranked.iter().take(top_k).map(|(r, _)| *r).collect();
    eprintln!("[alt] {} obligations, top {} collateral reserves by deposit count", entries.len(), top.len());
    for (r, n) in ranked.iter().take(top_k) { eprintln!("  {} : {}", r, n); }

    // Fixed accounts.
    let mut addrs: Vec<String> = vec![
        KLEND.into(),
        "FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr".into(),
        "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4".into(),
        JUP_LEND_PROGRAM.into(),
        "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA".into(),
        "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb".into(),
        "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL".into(),
        "11111111111111111111111111111111".into(),
        "ComputeBudget111111111111111111111111111111".into(),
        "Sysvar1nstructions1111111111111111111111111".into(),
        MAIN_MARKET.into(),
        lending_market_authority(&market).to_string(),
        USDC_MINT.into(),
        authority.clone(),
        // USDC ATA (repay source + swap out).
        arb_engine::flashloan::ata_for(&auth_pk, &usdc, &Pubkey::from_str("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA").unwrap()).to_string(),
    ];
    addrs.extend(JUP_M.iter().map(|s| s.to_string()));

    // Find the main-market USDC reserve (always the v1 repay side).
    let usdc_resp = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[KLEND, {"encoding":"base64",
            "filters":[{"dataSize":RESERVE_SIZE},{"memcmp":{"offset":32,"bytes":MAIN_MARKET}},
                       {"memcmp":{"offset":128,"bytes":USDC_MINT}}]}]}));
    let usdc_reserve = usdc_resp.as_ref().and_then(|v| v["result"].as_array())
        .and_then(|a| a.first())
        .and_then(|e| e["pubkey"].as_str())
        .and_then(|s| Pubkey::from_str(s).ok())
        .expect("USDC reserve");
    eprintln!("[alt] USDC repay reserve: {usdc_reserve}");

    // USDC repay-reserve + top collateral reserves, each with its 5 sub-accounts.
    let mut reserve_pks = vec![usdc_reserve];
    reserve_pks.extend(top);
    let strs: Vec<String> = reserve_pks.iter().map(|k| k.to_string()).collect();
    let v = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getMultipleAccounts",
        "params":[strs, {"encoding":"base64"}]})).expect("reserves");
    for (i, acc) in v["result"]["value"].as_array().into_iter().flatten().enumerate() {
        let Some(data) = acc.get("data").and_then(b64) else { continue };
        let Some(r) = ReserveAccounts::decode(reserve_pks[i], &data) else { continue };
        for a in [r.reserve, r.liquidity_mint, r.liquidity_supply, r.fee_receiver, r.collateral_mint, r.collateral_supply, r.scope_prices] {
            addrs.push(a.to_string());
        }
    }

    // Dedup, preserve order.
    let mut seen = std::collections::HashSet::new();
    for a in addrs.into_iter().filter(|a| seen.insert(a.clone())) {
        println!("{a}");
    }
}
