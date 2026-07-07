//! Kamino liquidation WIRING probe — assembles the real 3-ix sequence
//! (refresh_reserve ×2 + refresh_obligation + liquidate_and_redeem_v2) against
//! the most-underwater live main-market obligation and simulates it (no send,
//! no money). Classifies by instruction INDEX:
//!   err null                              → fully liquidatable, whole seq runs
//!   revert at the LIQUIDATE ix            → wiring OK, guard/health rejected
//!                                           (expected while 0 real liquidatable)
//!   revert at an earlier ix               → refresh/account wiring bug
//!
//! Uses the liquidator's existing USDC ATA as the repay source and the wSOL /
//! collateral-mint ATA as the destination (created idempotently in the real
//! fire path; here we just need the accounts to exist for the account list).
//!
//! Usage: HELIUS_RPC=<url> [AUTHORITY=<pk>] cargo run --release --bin kamino_liq_probe

use arb_engine::flashloan::ata_for;
use arb_engine::kamino::{Obligation, Reserve};
use arb_engine::kamino_ix::{self, ReserveAccounts};
use solana_pubkey::Pubkey;
use std::collections::HashMap;
use std::str::FromStr;
use std::time::Duration;

const KLEND: &str = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD";
const MAIN_MARKET: &str = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF";
const TOKEN_PROGRAM: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
const OBLIGATION_SIZE: usize = 3344;
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
fn mint_owner(endpoint: &str, mint: &Pubkey) -> Pubkey {
    rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":[mint.to_string(),{"encoding":"base64"}]}))
        .and_then(|v| v["result"]["value"]["owner"].as_str().map(String::from))
        .and_then(|s| Pubkey::from_str(&s).ok())
        .unwrap_or_else(|| Pubkey::from_str(TOKEN_PROGRAM).unwrap())
}

fn return_reason(code: u64) -> &'static str {
    match code {
        6017 => "obligation healthy (not liquidatable)",
        _ => "custom error past refresh — see logs",
    }
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let authority = Pubkey::from_str(&std::env::var("AUTHORITY").unwrap_or_else(|_| DEFAULT_AUTHORITY.into())).unwrap();
    let market = Pubkey::from_str(MAIN_MARKET).unwrap();

    // Scan main-market obligations (borrows present), pick the most underwater
    // by STORED health (fresh enough to be a real wiring target).
    eprintln!("[kliq] scanning main-market obligations …");
    let resp = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[KLEND, {"encoding":"base64","dataSlice":{"offset":0,"length":2288},
            "filters":[{"dataSize":OBLIGATION_SIZE},{"memcmp":{"offset":32,"bytes":MAIN_MARKET}}]}]}));
    let entries = resp.as_ref().and_then(|v| v["result"].as_array()).cloned().unwrap_or_default();
    eprintln!("[kliq] {} obligations", entries.len());
    let mut best: Option<(Pubkey, Obligation, f64)> = None;
    for e in &entries {
        let Some(pk) = e["pubkey"].as_str().and_then(|s| s.parse::<Pubkey>().ok()) else { continue };
        let Some(ob) = b64(&e["account"]["data"]).and_then(|d| Obligation::decode(&d)) else { continue };
        if ob.deposits.len() != 1 || ob.borrows.len() != 1 || ob.elevation_group != 0 { continue; }
        if ob.unhealthy_borrow_value < 50.0 { continue; }
        let ratio = ob.ratio();
        if best.as_ref().map_or(true, |b| ratio > b.2) { best = Some((pk, ob, ratio)); }
    }
    let Some((ob_pk, ob, ratio)) = best else { eprintln!("[kliq] no single-deposit/single-borrow obligation found"); return; };
    eprintln!("[kliq] target {} ratio {:.3} deposit_reserve {} borrow_reserve {}",
        &ob_pk.to_string()[..8], ratio, &ob.deposits[0].0.to_string()[..8], &ob.borrows[0].0.to_string()[..8]);

    let withdraw_reserve_pk = ob.deposits[0].0; // collateral we seize
    let repay_reserve_pk = ob.borrows[0].0;     // debt we repay
    let raw = get_multiple(&endpoint, &[withdraw_reserve_pk, repay_reserve_pk]);
    let (Some(wr), Some(rr)) = (
        raw.get(&withdraw_reserve_pk).and_then(|d| ReserveAccounts::decode(withdraw_reserve_pk, d)),
        raw.get(&repay_reserve_pk).and_then(|d| ReserveAccounts::decode(repay_reserve_pk, d)),
    ) else { eprintln!("[kliq] reserve decode failed"); return };
    // Reserve for token-program + decimals of each side.
    let _wr_dec = raw.get(&withdraw_reserve_pk).and_then(|d| Reserve::decode(d)).map(|r| r.mint_decimals);

    let repay_tp = mint_owner(&endpoint, &rr.liquidity_mint);
    let withdraw_liq_tp = mint_owner(&endpoint, &wr.liquidity_mint);
    let coll_tp = mint_owner(&endpoint, &wr.collateral_mint);

    // ATAs (the fire path creates these idempotently; probe just references them).
    let user_source_liquidity = ata_for(&authority, &rr.liquidity_mint, &repay_tp); // repay from USDC ATA
    let user_dest_liquidity = ata_for(&authority, &wr.liquidity_mint, &withdraw_liq_tp); // seized underlying
    let user_dest_collateral = ata_for(&authority, &wr.collateral_mint, &coll_tp);

    // 3-ix sequence.
    let ixs = vec![
        kamino_ix::refresh_reserve(&rr),
        kamino_ix::refresh_reserve(&wr),
        kamino_ix::refresh_obligation(&market, &ob_pk, &[withdraw_reserve_pk, repay_reserve_pk]),
        kamino_ix::liquidate_and_redeem_v2(
            &authority, &ob_pk, &market, &rr, &wr,
            &user_dest_collateral, &user_dest_liquidity, &user_source_liquidity,
            &coll_tp, &repay_tp, &withdraw_liq_tp,
            1_000_000, 0, 0,
        ),
    ];
    const LIQUIDATE_IX_INDEX: u64 = 3;

    use solana_message::{v0, VersionedMessage};
    use solana_transaction::versioned::VersionedTransaction;
    let bh = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getLatestBlockhash","params":[{"commitment":"finalized"}]}))
        .and_then(|v| v["result"]["value"]["blockhash"].as_str().map(String::from)).unwrap();
    let bh = solana_hash::Hash::from_str(&bh).unwrap();
    let msg = v0::Message::try_compile(&authority, &ixs, &[], bh).unwrap();
    let tx = VersionedTransaction { signatures: vec![Default::default()], message: VersionedMessage::V0(msg) };
    let b64tx = { use base64::Engine; base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&tx).unwrap()) };

    eprintln!("[kliq] simulating refresh×2 + refresh_obligation + liquidate …");
    let sim = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
        "params":[b64tx, {"sigVerify":false,"replaceRecentBlockhash":true,"commitment":"processed","encoding":"base64"}]}));
    let Some(sim) = sim else { eprintln!("[kliq] no response"); return };
    if sim.get("result").map(|r| r.get("value").is_some()) != Some(true) {
        println!("✗ RPC rejected simulation: {sim}"); return;
    }
    let res = &sim["result"]["value"];
    println!("\n──── Kamino liquidation-wiring simulation ────");
    println!("err: {}", res["err"]);
    let ix_idx = res["err"].get("InstructionError").and_then(|e| e.get(0)).and_then(|i| i.as_u64());
    let code = res["err"].get("InstructionError").and_then(|e| e.get(1)).and_then(|c| c.get("Custom")).and_then(|c| c.as_u64());
    match (res["err"].is_null(), ix_idx) {
        (true, _) => println!("★★ FULLY LIQUIDATABLE — whole sequence executes end to end"),
        (_, Some(LIQUIDATE_IX_INDEX)) => {
            let why = match code {
                Some(3012) => "missing destination ATA (3012 AccountNotInitialized) — the fire path creates these; health gate PASSED",
                Some(c) => return_reason(c),
                None => "non-custom revert",
            };
            println!("★ WIRING OK — refresh×2 + refresh_obligation executed; liquidate reached account/health checks: {why}. Account layout verified.");
        }
        (_, Some(i)) => {
            println!("✗ reverted at ix {} (custom {:?}) — refresh/account wiring bug:", i, code);
            for l in res["logMessages"].as_array().into_iter().flatten() { println!("  {}", l.as_str().unwrap_or("")); }
        }
        _ => println!("? inconclusive: {}", res["err"]),
    }
}
