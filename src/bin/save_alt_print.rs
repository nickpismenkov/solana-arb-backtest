//! Print the FIXED accounts a Save (Solend) liquidation fire tx needs in its
//! dedicated address-lookup-table (SAVE_ALT), so the JupLend-flash-loan-wrapped
//! `liquidate_and_redeem` + swap + payback tx fits under the 1232-byte
//! single-packet limit. Without an ALT the wrapped cross-mint tx is ~1716–1936B
//! (see the Save widen PR / save_fire_probe); moving these fixed accounts off the
//! static keys (~31B saved each) brings it under 1232 — exactly as jup_alt_print
//! / the Kamino ALT do for their paths.
//!
//! What's FIXED (goes in the ALT) vs per-fire (stays inline / rides Jupiter's ALTs):
//!   FIXED  — programs + sysvars; the Solend main pool + its lending-market
//!            authority; and, for EACH supported debt asset (USDC/USDT/wSOL): the
//!            Solend debt (repay) reserve + its sub-accounts (liquidity supply,
//!            pyth/switchboard oracles, collateral mint/supply, fee receiver), the
//!            JupLend flash-market account set (reserve/token/rate_model/vault +
//!            globals), and the wallet's debt ATA. A given fire uses only ONE debt
//!            asset, but the ALT holds all three so any is covered.
//!   PER-FIRE — the obligation, the COLLATERAL (withdraw) reserve + its
//!            sub-accounts, and the collateral→debt swap route (rides Jupiter's
//!            own ALTs). These vary per liquidation.
//!
//! The account lists are pulled from the REAL ix builders (flashloan::borrow +
//! the decoded Reserve fields), so they are guaranteed to match what
//! build_save_fire_tx actually references — no hand-maintained duplicate list.
//!
//! Setup (one-time; ALT creation needs wallet signing — do this on the box):
//!   solana address-lookup-table create --keypair ~/arb-keypair.json -u <rpc>
//!   solana address-lookup-table extend <TABLE> --addresses "$(save_alt_print | paste -sd, -)" …
//! Then export SAVE_ALT=<TABLE> for liq_save_executor / save_fire_probe.
//!
//! Usage: HELIUS_RPC=<url> [AUTHORITY=<pk>] cargo run --release --bin save_alt_print

use arb_engine::flashloan;
use arb_engine::save::{self, Reserve};
use solana_pubkey::Pubkey;
use std::collections::HashSet;
use std::str::FromStr;
use std::time::Duration;

const DEFAULT_AUTHORITY: &str = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak";
const TOKEN: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
const TOKEN22: &str = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb";
const ATA_PROGRAM: &str = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL";
const SYSTEM: &str = "11111111111111111111111111111111";
const COMPUTE_BUDGET: &str = "ComputeBudget111111111111111111111111111111";
const JUPITER_PROGRAM: &str = "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4";

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..4 {
        if let Ok(r) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = r.into_json::<serde_json::Value>() {
                return Some(v);
            }
        }
        std::thread::sleep(Duration::from_millis(400 << attempt));
    }
    None
}

fn get_reserve(endpoint: &str, pk: &Pubkey) -> Option<Reserve> {
    use base64::Engine;
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
        "params":[pk.to_string(), {"encoding":"base64"}]}))?;
    let raw = base64::engine::general_purpose::STANDARD
        .decode(v["result"]["value"]["data"].get(0)?.as_str()?)
        .ok()?;
    Reserve::decode(*pk, &raw)
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC")
        .or_else(|_| std::env::var("RPC_HTTP"))
        .expect("set HELIUS_RPC (needed to read the debt reserves' oracle/supply sub-accounts)");
    let authority =
        Pubkey::from_str(&std::env::var("AUTHORITY").unwrap_or_else(|_| DEFAULT_AUTHORITY.into())).unwrap();

    let main_pool = Pubkey::from_str(save::MAIN_POOL).unwrap();

    // Ordered, deduped accumulator (preserve first-seen order for readable output).
    let mut seen = HashSet::new();
    let mut out: Vec<String> = Vec::new();
    let push = |pk: String, seen: &mut HashSet<String>, out: &mut Vec<String>| {
        if seen.insert(pk.clone()) {
            out.push(pk);
        }
    };

    // ── programs + sysvars + Solend globals ──
    for s in [
        save::SOLEND_PROGRAM,
        flashloan::JUP_LEND_PROGRAM,
        JUPITER_PROGRAM,
        TOKEN,
        TOKEN22,
        ATA_PROGRAM,
        SYSTEM,
        COMPUTE_BUDGET,
        save::MAIN_POOL,
    ] {
        push(s.to_string(), &mut seen, &mut out);
    }
    push(save::lending_market_authority(&main_pool).to_string(), &mut seen, &mut out);

    // ── per debt asset (USDC/USDT/wSOL): Solend reserve sub-accounts + JupLend flash set + ATA ──
    let debts = [
        ("USDC", save::USDC_RESERVE, save::USDC_MINT),
        ("USDT", save::USDT_RESERVE, save::USDT_MINT),
        ("wSOL", save::WSOL_RESERVE, save::WSOL_MINT),
    ];
    let token = Pubkey::from_str(TOKEN).unwrap();
    for (label, reserve_str, mint_str) in debts {
        let reserve_pk = Pubkey::from_str(reserve_str).unwrap();
        let mint = Pubkey::from_str(mint_str).unwrap();

        // Solend debt-reserve fixed sub-accounts, straight from the decoded reserve
        // (these are exactly what refresh_reserve + liquidate_and_redeem reference
        // for the repay side).
        match get_reserve(&endpoint, &reserve_pk) {
            Some(r) => {
                for pk in [
                    r.reserve,
                    r.liquidity_mint,
                    r.liquidity_supply,
                    r.pyth_oracle,
                    r.switchboard_oracle,
                    r.collateral_mint,
                    r.collateral_supply,
                    r.fee_receiver,
                ] {
                    push(pk.to_string(), &mut seen, &mut out);
                }
            }
            None => eprintln!("[save-alt] WARN could not fetch {label} reserve {reserve_pk} — its sub-accounts are missing from this list; re-run with a working RPC"),
        }

        // JupLend flash-market fixed set — pulled from the REAL borrow ix so it
        // matches build_save_fire_tx exactly (signer/ATA/mint/reserve/token/
        // rate_model/vault + JupLend globals).
        if let Some(ix) = flashloan::borrow(&authority, &mint, 0) {
            for m in ix.accounts {
                push(m.pubkey.to_string(), &mut seen, &mut out);
            }
        }

        // Wallet's debt ATA (classic SPL — USDC/USDT/wSOL are all classic).
        push(flashloan::ata_for(&authority, &mint, &token).to_string(), &mut seen, &mut out);
    }

    // ── wallet ──
    push(authority.to_string(), &mut seen, &mut out);

    for a in &out {
        println!("{a}");
    }
    eprintln!("[save-alt] {} fixed accounts. Create the ALT + extend with these, then export SAVE_ALT=<table>.", out.len());
    eprintln!("[save-alt] lending_market_authority = {}", save::lending_market_authority(&main_pool));
}
