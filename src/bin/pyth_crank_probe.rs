//! Simulate the FULL self-crank against the sponsored feed marginfi reads
//! (step-4 gate): fetch a fresh Hermes update, build the two-tx crank
//! (pyth_crank), simulateBundle [setup, fire] with skipSigVerify, and confirm
//! the sponsored feed's publish_time ADVANCES past the live on-chain value.
//! Read-only — nothing is submitted; the payer is a funded mainnet cranker
//! (sim only needs its lamports), the buffer an ephemeral keypair.
//!
//! Usage: HELIUS_RPC=<url> [FEED=<hex feed id>] [PAYER=<pubkey>]
//!        cargo run --release --bin pyth_crank_probe

use arb_engine::pyth_accumulator as acc;
use arb_engine::pyth_crank as crank;
use arb_engine::liquidation::decode_price_update_v2;
use solana_keypair::Keypair;
use solana_message::{v0, VersionedMessage};
use solana_pubkey::Pubkey;
use solana_signer::Signer;
use solana_transaction::versioned::VersionedTransaction;
use std::str::FromStr;
use std::time::Duration;

// Canonical Pyth feed ids (hex).
const SOL: &str = "ef0d8b6fda2ceba41da15d4095d1da392a0d2f8ed0c6c7bc0f4cfac8c280b56d";
const USDC: &str = "eaa020c61cc479712813461ce153894a96a6c00b21ed0cfc2798d1f9a9e9c94a";
// A real, funded sponsored-feed cranker (payer for simulation only).
const DEFAULT_PAYER: &str = "4p16wya1Vw2u9w22oah4yXQgySb6eWKRRLMsEXCreish";

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..4 {
        if let Ok(resp) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = resp.into_json::<serde_json::Value>() { return Some(v); }
        }
        std::thread::sleep(Duration::from_millis(400 << attempt));
    }
    None
}

fn hex32(s: &str) -> [u8; 32] {
    let mut out = [0u8; 32];
    for i in 0..32 { out[i] = u8::from_str_radix(&s[2 * i..2 * i + 2], 16).unwrap(); }
    out
}

fn cu_limit_ix(units: u32) -> solana_instruction::Instruction {
    let mut data = vec![2u8];
    data.extend_from_slice(&units.to_le_bytes());
    solana_instruction::Instruction {
        program_id: Pubkey::from_str("ComputeBudget111111111111111111111111111111").unwrap(),
        accounts: vec![],
        data,
    }
}

/// Unsigned v0 tx (placeholder sigs — simulateBundle runs skipSigVerify).
fn unsigned_tx(payer: &Pubkey, ixs: &[solana_instruction::Instruction], num_signers: usize) -> String {
    use base64::Engine;
    let msg = v0::Message::try_compile(payer, ixs, &[], solana_hash::Hash::default())
        .expect("compile");
    let tx = VersionedTransaction {
        signatures: vec![solana_signature::Signature::default(); num_signers],
        message: VersionedMessage::V0(msg),
    };
    let raw = bincode::serialize(&tx).unwrap();
    eprintln!("[crank] tx: {}B ({} ixs)", raw.len(), ixs.len());
    base64::engine::general_purpose::STANDARD.encode(&raw)
}

fn feed_state(endpoint: &str, feed: &Pubkey) -> Option<(f64, i64)> {
    use base64::Engine;
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
        "params":[feed.to_string(), {"encoding":"base64"}]}))?;
    let bytes = base64::engine::general_purpose::STANDARD
        .decode(v["result"]["value"]["data"].get(0)?.as_str()?).ok()?;
    decode_price_update_v2(&bytes).map(|(_, usd, ts)| (usd, ts))
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let feed_hex = std::env::var("FEED").unwrap_or_else(|_| SOL.into());
    let payer = Pubkey::from_str(&std::env::var("PAYER").unwrap_or_else(|_| DEFAULT_PAYER.into())).expect("PAYER");
    let feed_id = hex32(&feed_hex);

    // Where marginfi looks: shard-0 sponsored feed PDAs.
    let feed_acct = crank::sponsored_feed(0, &feed_id);
    println!("sponsored feed (shard 0): {feed_acct}");
    println!("  (USDC shard-0 ref: {})", crank::sponsored_feed(0, &hex32(USDC)));

    let pre = feed_state(&endpoint, &feed_acct);
    match pre {
        Some((usd, ts)) => println!("live feed:  price=${usd:.4} publish_time={ts}"),
        None => println!("live feed: <not found / undecodable>"),
    }

    // Fresh Hermes update for this feed.
    let hermes = std::env::var("HERMES").unwrap_or_else(|_| "https://hermes.pyth.network".into());
    let update = acc::fetch_hermes(&hermes, &[&feed_hex]).expect("hermes fetch+parse");
    println!("hermes: VAA {}B, {} update(s)", update.vaa.len(), update.updates.len());
    let mu = update.updates.iter().find(|u| u.feed_id() == Some(feed_id)).expect("feed in blob");

    // Two-tx crank with an ephemeral buffer.
    let buffer = Keypair::new();
    let ixs = crank::build_crank_ixs(&payer, &buffer.pubkey(), &update.vaa, std::slice::from_ref(mu), 0, 0)
        .expect("build crank ixs");
    let mut setup = vec![cu_limit_ix(30_000)];
    setup.extend(ixs.setup);
    let mut fire = vec![cu_limit_ix(500_000)];
    fire.extend(ixs.fire);
    let setup_b64 = unsigned_tx(&payer, &setup, 2); // payer + buffer keypair
    let fire_b64 = unsigned_tx(&payer, &fire, 1);

    let v = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateBundle",
        "params":[{"encodedTransactions":[setup_b64, fire_b64]}, {
            "skipSigVerify": true,
            "replaceRecentBlockhash": true,
            "preExecutionAccountsConfigs": [null, null],
            "postExecutionAccountsConfigs": [null, {"encoding":"base64","addresses":[feed_acct.to_string()]}]
        }]})).expect("simulateBundle");
    if let Some(e) = v.get("error").filter(|e| !e.is_null()) {
        println!("⛔ simulateBundle error: {e}");
        std::process::exit(1);
    }
    let val = &v["result"]["value"];
    println!("simulateBundle summary: {}", val["summary"]);
    let mut post: Option<(f64, i64)> = None;
    for (i, r) in val["transactionResults"].as_array().into_iter().flatten().enumerate() {
        println!("  tx[{i}] err={} cu={}", r["err"], r["unitsConsumed"]);
        if !r["err"].is_null() {
            for l in r["logs"].as_array().into_iter().flatten() {
                println!("    {}", l.as_str().unwrap_or_default());
            }
        }
        for a in r["postExecutionAccounts"].as_array().into_iter().flatten() {
            use base64::Engine;
            let Some(b) = a["data"].get(0).and_then(|d| d.as_str())
                .and_then(|s| base64::engine::general_purpose::STANDARD.decode(s).ok()) else { continue };
            post = decode_price_update_v2(&b).map(|(_, usd, ts)| (usd, ts));
        }
    }

    match (pre, post) {
        (Some((pre_usd, pre_ts)), Some((usd, ts))) => {
            println!("post-crank: price=${usd:.4} publish_time={ts}");
            let adv = ts - pre_ts;
            if adv > 0 {
                println!("★ CRANK VERIFIED — publish_time advanced {adv}s past the live feed \
                          (${pre_usd:.4}@{pre_ts} → ${usd:.4}@{ts})");
            } else {
                println!("✗ publish_time did NOT advance ({pre_ts} → {ts}) — feed already fresher than Hermes blob?");
            }
        }
        (None, Some((usd, ts))) => println!("post-crank: price=${usd:.4} publish_time={ts} (no live baseline)"),
        _ => println!("✗ no post-execution feed state returned — check simulateBundle output above"),
    }
}
