//! Verifies the Jupiter Lend flash-loan builders: assemble
//! [create-ATA, borrow, payback] for EACH wired debt asset (USDC/USDT/wSOL) and
//! simulate against mainnet. A self-repaying 0-fee flash loan nets zero, so with
//! the ATA created each should simulate clean (err = null) — proving the ported
//! instruction format + per-asset market accounts are correct end to end. This
//! is the ground-truth check for the derived USDT/wSOL flash markets.
//!
//! Usage: RPC_ENDPOINT=<url> cargo run --release --bin flashloan_probe

use arb_engine::flashloan::{borrow, create_ata_idempotent_for, payback, USDC_MINT, USDT_MINT, WSOL_MINT};
use base64::Engine;
use solana_hash::Hash;
use solana_message::{legacy::Message, VersionedMessage};
use solana_pubkey::Pubkey;
use solana_signature::Signature;
use solana_transaction::versioned::VersionedTransaction;
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

fn probe(endpoint: &str, signer: &Pubkey, tp: &Pubkey, name: &str, mint: &Pubkey, amount: u64) -> bool {
    let ixs = vec![
        create_ata_idempotent_for(signer, mint, tp),
        borrow(signer, mint, amount).expect("wired market"),
        payback(signer, mint, amount).expect("wired market"),
    ];
    let msg = Message::new_with_blockhash(&ixs, Some(signer), &Hash::default());
    let tx = VersionedTransaction {
        signatures: vec![Signature::default()],
        message: VersionedMessage::Legacy(msg),
    };
    let b64 = base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&tx).unwrap());
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
        "params":[b64,{"encoding":"base64","sigVerify":false,"replaceRecentBlockhash":true}]}));
    let val = v.map(|v| v["result"]["value"].clone()).unwrap_or_default();
    let err = &val["err"];
    println!("\n=== Jupiter Lend {name} flash loan (borrow {amount} → payback {amount}) ===");
    println!("err: {err}");
    if err.is_null() {
        let units = val["unitsConsumed"].as_u64().unwrap_or(0);
        println!("✅ {name} VERIFIED — self-repaying flash loan simulated clean ({units} CU)");
        true
    } else {
        println!("⚠️  {name} did not simulate clean — inspect logs:");
        for l in val["logs"].as_array().into_iter().flatten() {
            println!("  {}", l.as_str().unwrap_or(""));
        }
        false
    }
}

fn main() {
    let _ = dotenvy::dotenv();
    let endpoint = std::env::var("RPC_ENDPOINT").or_else(|_| std::env::var("HELIUS_RPC"))
        .unwrap_or_else(|_| "https://api.mainnet-beta.solana.com".to_string());
    let signer = Pubkey::from_str("Anu6Awu4kxaEDrg1nkpcikx6tJ2xhfVci5TvDrZBsZEB").unwrap();
    let tp = Pubkey::from_str("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA").unwrap();

    let usdc = Pubkey::from_str(USDC_MINT).unwrap();
    let usdt = Pubkey::from_str(USDT_MINT).unwrap();
    let wsol = Pubkey::from_str(WSOL_MINT).unwrap();

    let mut ok = 0;
    ok += probe(&endpoint, &signer, &tp, "USDC", &usdc, 1_000_000) as u32;
    ok += probe(&endpoint, &signer, &tp, "USDT", &usdt, 1_000_000) as u32;
    ok += probe(&endpoint, &signer, &tp, "wSOL", &wsol, 10_000_000) as u32; // 0.01 SOL

    println!("\n── {ok}/3 flash markets verified ──");
}
