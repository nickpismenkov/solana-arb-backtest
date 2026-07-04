//! End-to-end LANDING certification: fire a Jito bundle that does NOT depend
//! on any market spread — [flash-borrow 1 USDC, payback 1 USDC, tip] — so it
//! always succeeds and should land on-chain. Proves the whole live path:
//! signing, blockhash, flash loan, bundle submission, tip payment, readback.
//! Cost when it lands: tip + base fee + priority (~10k lamports, <$0.01).
//!
//! Default is simulate-only. LIVE=1 submits for real.
//! MODE=jito (default) submits as a Jito bundle; MODE=rpc submits the SAME tx
//! via plain sendTransaction — bisects "tx invalid" from "Jito bundle path
//! broken": if rpc lands and jito doesn't, the tx is fine and Jito is the issue.
//!
//! Usage: RPC_ENDPOINT=<url> KEYPAIR_PATH=<path> [LIVE=1] [MODE=jito|rpc] \
//!   [TIP_LAMPORTS=5000] cargo run --release --bin land_probe

use arb_engine::arb::{cu_limit_ix, cu_price_ix, transfer_ix};
use arb_engine::flashloan::{borrow_usdc, create_ata_idempotent, payback_usdc, USDC_MINT};
use arb_engine::jito::{bundle_status, default_block_engine, get_tip_accounts, send_bundle};
use base64::Engine;
use solana_hash::Hash;
use solana_keypair::Keypair;
use solana_message::{v0, VersionedMessage};
use solana_pubkey::Pubkey;
use solana_signer::Signer;
use solana_transaction::versioned::VersionedTransaction;
use std::str::FromStr;
use std::time::Duration;

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..3 {
        if let Ok(resp) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = resp.into_json::<serde_json::Value>() {
                return Some(v);
            }
        }
        std::thread::sleep(Duration::from_millis(300 << attempt));
    }
    None
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("RPC_ENDPOINT").expect("RPC_ENDPOINT");
    let keypair_path = std::env::var("KEYPAIR_PATH").expect("KEYPAIR_PATH");
    let live = std::env::var("LIVE").map(|v| v == "1").unwrap_or(false);
    let mode = std::env::var("MODE").unwrap_or_else(|_| "jito".into());
    let tip_lamports: u64 = std::env::var("TIP_LAMPORTS").ok().and_then(|s| s.parse().ok()).unwrap_or(5000);
    let block_engine = default_block_engine();

    let bytes: Vec<u8> = serde_json::from_str(&std::fs::read_to_string(&keypair_path).expect("read keypair")).expect("parse keypair");
    let kp = Keypair::try_from(&bytes[..]).expect("keypair");
    let signer = kp.pubkey();
    let usdc = Pubkey::from_str(USDC_MINT).unwrap();

    let tip_to = *get_tip_accounts(&block_engine).expect("tip accounts").first().expect("no tip accounts");
    // FINALIZED blockhash: visible to every bank (confirmed-fresh hashes can be
    // rejected as BlockhashNotFound by validators/preflight still on finalized).
    // Still ~60s of validity left — plenty for a probe.
    let bh_resp = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getLatestBlockhash","params":[{"commitment":"finalized"}]}))
        .expect("blockhash");
    let bh_str = bh_resp["result"]["value"]["blockhash"].as_str().expect("blockhash str").to_string();
    println!("blockhash {} (slot {}, lastValidBlockHeight {})", bh_str, bh_resp["result"]["context"]["slot"], bh_resp["result"]["value"]["lastValidBlockHeight"]);
    let bh = Hash::from_str(&bh_str).unwrap();

    // No-spread-required bundle: borrow 1 USDC, pay it straight back, tip.
    // BARE=1 drops the flash-loan legs (isolates "Jito filters the flash-loan
    // program" — a bare self-transfer + tip has nothing left to object to).
    let bare = std::env::var("BARE").map(|v| v == "1").unwrap_or(false);
    let with_ata = std::env::var("ATA").map(|v| v == "1").unwrap_or(false);
    let ixs = if bare {
        let mut v = vec![cu_limit_ix(50_000), cu_price_ix(10_000)];
        if with_ata {
            v.push(create_ata_idempotent(&signer, &usdc));
        }
        v.push(transfer_ix(signer, signer, 1_000));
        v.push(transfer_ix(signer, tip_to, tip_lamports));
        v
    } else {
        vec![
            cu_limit_ix(200_000),
            cu_price_ix(10_000),
            create_ata_idempotent(&signer, &usdc),
            borrow_usdc(&signer, 1_000_000),
            payback_usdc(&signer, 1_000_000),
            transfer_ix(signer, tip_to, tip_lamports),
        ]
    };
    let msg = v0::Message::try_compile(&signer, &ixs, &[], bh).expect("compile");
    let mut tx = VersionedTransaction {
        signatures: vec![solana_signature::Signature::default()],
        message: VersionedMessage::V0(msg),
    };
    tx.signatures[0] = kp.sign_message(&tx.message.serialize());
    let sig = tx.signatures[0].to_string();
    let raw = bincode::serialize(&tx).unwrap();
    let b64 = base64::engine::general_purpose::STANDARD.encode(&raw);

    println!("land-probe: signer={signer} tx={}B tip={} lamports sig={sig}", raw.len(), tip_lamports);

    // Always simulate first — refuse to submit a tx that wouldn't succeed.
    let sim = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
        "params":[b64,{"encoding":"base64","sigVerify":false,"replaceRecentBlockhash":true}]}))
        .expect("simulate");
    let err = &sim["result"]["value"]["err"];
    if !err.is_null() {
        println!("⛔ simulation failed, NOT submitting: {err}");
        for l in sim["result"]["value"]["logs"].as_array().into_iter().flatten() {
            println!("  {}", l.as_str().unwrap_or_default());
        }
        std::process::exit(1);
    }
    println!("✅ simulates clean ({} CU)", sim["result"]["value"]["unitsConsumed"]);

    if !live {
        println!("dry run (set LIVE=1 to submit for real — costs ~{} lamports)", tip_lamports + 10_000);
        return;
    }

    let mut bundle_id = String::new();
    match mode.as_str() {
        "jitotx" => {
            // Jito transactions endpoint, bundleOnly=true → single-tx bundle
            // WITH revert protection; documented low-latency send path.
            let url = format!("{block_engine}/api/v1/transactions?bundleOnly=true");
            let v: serde_json::Value = ureq::post(&url)
                .send_json(serde_json::json!({"jsonrpc":"2.0","id":1,"method":"sendTransaction",
                    "params":[b64,{"encoding":"base64"}]}))
                .map(|r| r.into_json().unwrap_or_default())
                .unwrap_or_else(|e| match e {
                    ureq::Error::Status(code, r) => serde_json::json!({"error": format!("HTTP {code}: {}", r.into_string().unwrap_or_default())}),
                    e => serde_json::json!({"error": e.to_string()}),
                });
            if let Some(e) = v.get("error").filter(|e| !e.is_null()) {
                println!("⛔ jito sendTransaction rejected: {e}");
                std::process::exit(1);
            }
            println!("⚡ sent via jito transactions endpoint (bundleOnly): {}", v["result"]);
        }
        "rpc" => {
            // Plain sendTransaction — no Jito. If THIS lands, the tx is valid
            // and any Jito non-landing is a bundle-path problem, not ours.
            let v = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"sendTransaction",
                "params":[b64,{"encoding":"base64","skipPreflight":false,"preflightCommitment":"confirmed","maxRetries":5}]}))
                .expect("sendTransaction");
            if let Some(e) = v.get("error").filter(|e| !e.is_null()) {
                println!("⛔ sendTransaction rejected: {e}");
                std::process::exit(1);
            }
            println!("⚡ sent via plain RPC: {}", v["result"]);
        }
        _ => {
            // The unauth lane 429s often — retry with backoff for up to ~60s.
            let mut attempt = 0;
            bundle_id = loop {
                attempt += 1;
                match send_bundle(&block_engine, &[b64.clone()]) {
                    Ok(id) => break id,
                    Err(e) if e.to_string().contains("429") && attempt < 12 => {
                        println!("  [attempt {attempt}] rate limited, retrying in 5s…");
                        std::thread::sleep(Duration::from_secs(5));
                    }
                    Err(e) => panic!("send bundle: {e}"),
                }
            };
            println!("⚡ submitted bundle {bundle_id} (attempt {attempt})");
        }
    }

    // Poll until landed (or give up after ~90s).
    for i in 1..=18 {
        std::thread::sleep(Duration::from_secs(5));
        let status = if mode == "jito" {
            bundle_status(&block_engine, &bundle_id).unwrap_or_else(|| "unknown".into())
        } else {
            "n/a".into()
        };
        let tx_meta = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getTransaction",
            "params":[sig,{"encoding":"json","maxSupportedTransactionVersion":0,"commitment":"confirmed"}]}));
        let landed = tx_meta.as_ref().map(|v| !v["result"].is_null()).unwrap_or(false);
        println!("[{}s] jito_status={status} on_chain={landed}", i * 5);
        if landed {
            let meta = &tx_meta.unwrap()["result"];
            println!("\n🎉 LANDED — slot {} fee {} lamports err {}", meta["slot"], meta["meta"]["fee"], meta["meta"]["err"]);
            println!("https://solscan.io/tx/{sig}");
            return;
        }
    }
    println!("\n⚠️ not seen on-chain after 90s — if MODE=rpc also fails, the tx itself is the problem; if only jito fails, raise TIP_LAMPORTS or the bundle path is at fault");
}
