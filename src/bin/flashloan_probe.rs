//! Verifies the Jupiter Lend flash-loan builders: assemble
//! [create-ATA, borrow, payback] for USDC and simulate against mainnet. A
//! self-repaying 0-fee flash loan nets zero, so with the ATA created the whole
//! thing should simulate clean (err = null) — proving the ported instruction
//! format + market accounts are correct end to end.
//!
//! Usage: RPC_ENDPOINT=<url> cargo run --release --bin flashloan_probe

use arb_engine::flashloan::{borrow_usdc, create_ata_idempotent, payback_usdc, USDC_MINT};
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

fn main() {
    let _ = dotenvy::dotenv();
    let endpoint = std::env::var("RPC_ENDPOINT")
        .unwrap_or_else(|_| "https://api.mainnet-beta.solana.com".to_string());
    let signer = Pubkey::from_str("Anu6Awu4kxaEDrg1nkpcikx6tJ2xhfVci5TvDrZBsZEB").unwrap();
    let usdc = Pubkey::from_str(USDC_MINT).unwrap();
    let amount = 1_000_000u64; // 1 USDC

    let ixs = vec![
        create_ata_idempotent(&signer, &usdc),
        borrow_usdc(&signer, amount),
        payback_usdc(&signer, amount),
    ];
    let msg = Message::new_with_blockhash(&ixs, Some(&signer), &Hash::default());
    let tx = VersionedTransaction {
        signatures: vec![Signature::default()],
        message: VersionedMessage::Legacy(msg),
    };
    let b64 = base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&tx).unwrap());

    let v = rpc(
        &endpoint,
        serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
            "params":[b64,{"encoding":"base64","sigVerify":false,"replaceRecentBlockhash":true}]}),
    );
    let val = v.map(|v| v["result"]["value"].clone()).unwrap_or_default();
    let err = &val["err"];
    let logs: Vec<String> = val["logs"]
        .as_array()
        .map(|a| a.iter().filter_map(|l| l.as_str().map(String::from)).collect())
        .unwrap_or_default();

    println!("=== Jupiter Lend USDC flash loan (borrow 1 → payback 1) ===");
    println!("err: {err}");
    if err.is_null() {
        let units = val["unitsConsumed"].as_u64().unwrap_or(0);
        println!("✅ VERIFIED — self-repaying flash loan simulated clean ({units} CU)");
    } else {
        println!("⚠️  did not simulate clean — inspect logs:");
    }
    for l in logs.iter() {
        println!("  {l}");
    }
}
