//! marginfi flash-loan GO/NO-GO. Does an empty marginfi flashloan (borrow 1
//! USDC → deposit it straight back → end, net-zero balances) LAND in a Jito
//! bundle — the thing Jupiter Lend can't do? One-time: creates a MarginfiAccount
//! (plain keypair, persisted). Always simulates first; LIVE=1 submits.
//! MODE=jito (default, the real test) or MODE=rpc (control — proves the tx is
//! valid regardless of Jito).
//!
//! Usage: RPC_ENDPOINT=<url> KEYPAIR_PATH=<path> \
//!   MARGINFI_ACCOUNT_KEYPAIR=<path, created if absent> \
//!   [LIVE=1] [MODE=jito|rpc] [TIP_LAMPORTS=1000000] \
//!   [MARGINFI_USDC_VAULT=<pk> MARGINFI_USDC_VAULT_AUTH=<pk>] [MARGINFI_DEPOSIT_OPT=1] \
//!   cargo run --release --bin marginfi_probe

use arb_engine::arb::{cu_limit_ix, cu_price_ix, transfer_ix};
use arb_engine::flashloan::{ata, create_ata_idempotent};
use arb_engine::jito::{default_block_engine, get_tip_accounts, send_bundle};
use arb_engine::marginfi::{account_initialize, borrow_usdc, end_flashloan, payback_usdc, start_flashloan, usdc_vault, usdc_vault_authority, USDC_MINT};
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

fn finalized_blockhash(endpoint: &str) -> Hash {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getLatestBlockhash","params":[{"commitment":"finalized"}]})).expect("blockhash");
    Hash::from_str(v["result"]["value"]["blockhash"].as_str().expect("bh str")).unwrap()
}

fn account_exists(endpoint: &str, pk: &Pubkey) -> bool {
    rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":[pk.to_string(),{"encoding":"base64"}]}))
        .map(|v| !v["result"]["value"].is_null())
        .unwrap_or(false)
}

fn landed(endpoint: &str, sig: &str) -> Option<serde_json::Value> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getTransaction",
        "params":[sig,{"encoding":"json","maxSupportedTransactionVersion":0,"commitment":"confirmed"}]}))?;
    if v["result"].is_null() { None } else { Some(v["result"].clone()) }
}

fn load_or_make_keypair(path: &str) -> (Keypair, bool) {
    if let Ok(s) = std::fs::read_to_string(path) {
        let bytes: Vec<u8> = serde_json::from_str(&s).expect("parse keypair");
        return (Keypair::try_from(&bytes[..]).expect("keypair"), false);
    }
    let kp = Keypair::new();
    std::fs::write(path, serde_json::to_string(&kp.to_bytes().to_vec()).unwrap()).expect("write keypair");
    (kp, true)
}

fn sign(msg: v0::Message, signers: &[&Keypair]) -> VersionedTransaction {
    VersionedTransaction::try_new(VersionedMessage::V0(msg), signers).expect("sign")
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("RPC_ENDPOINT").expect("RPC_ENDPOINT");
    let keypair_path = std::env::var("KEYPAIR_PATH").expect("KEYPAIR_PATH");
    let mfi_acc_path = std::env::var("MARGINFI_ACCOUNT_KEYPAIR").expect("MARGINFI_ACCOUNT_KEYPAIR (path; created if absent)");
    let live = std::env::var("LIVE").map(|v| v == "1").unwrap_or(false);
    let mode = std::env::var("MODE").unwrap_or_else(|_| "jito".into());
    let tip_lamports: u64 = std::env::var("TIP_LAMPORTS").ok().and_then(|s| s.parse().ok()).unwrap_or(1_000_000);
    let block_engine = default_block_engine();

    let bytes: Vec<u8> = serde_json::from_str(&std::fs::read_to_string(&keypair_path).expect("read keypair")).expect("parse");
    let authority = Keypair::try_from(&bytes[..]).expect("keypair");
    let signer = authority.pubkey();
    let usdc = Pubkey::from_str(USDC_MINT).unwrap();
    let usdc_ata = ata(&signer, &usdc);

    println!("authority={signer}");
    println!("usdc vault={} auth={}", usdc_vault(), usdc_vault_authority());

    let (mfi_acc_kp, freshly_made) = load_or_make_keypair(&mfi_acc_path);
    let mfi_acc = mfi_acc_kp.pubkey();
    println!("marginfi account={mfi_acc}{}", if freshly_made { " (NEW keypair generated)" } else { "" });

    // ── one-time: create the MarginfiAccount on-chain ──
    if !account_exists(&endpoint, &mfi_acc) {
        if !live {
            println!("marginfi account does not exist yet — rerun with LIVE=1 to create it (one-time, ~0.016 SOL rent)");
            return;
        }
        println!("creating MarginfiAccount…");
        let bh = finalized_blockhash(&endpoint);
        let ixs = [
            cu_limit_ix(60_000),
            cu_price_ix(10_000),
            account_initialize(&mfi_acc, &signer, &signer),
        ];
        let msg = v0::Message::try_compile(&signer, &ixs, &[], bh).expect("compile init");
        let tx = sign(msg, &[&authority, &mfi_acc_kp]);
        let b64 = base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&tx).unwrap());
        let v = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"sendTransaction",
            "params":[b64,{"encoding":"base64","skipPreflight":false,"preflightCommitment":"confirmed","maxRetries":5}]})).expect("send init");
        if let Some(e) = v.get("error").filter(|e| !e.is_null()) {
            println!("⛔ MarginfiAccount init rejected: {e}");
            std::process::exit(1);
        }
        let sig = v["result"].as_str().unwrap_or_default().to_string();
        println!("  init sig {sig} — waiting for confirmation…");
        let mut ok = false;
        for _ in 0..20 {
            std::thread::sleep(Duration::from_secs(3));
            if let Some(meta) = landed(&endpoint, &sig) {
                println!("  ✅ MarginfiAccount created (slot {}, err {})", meta["slot"], meta["meta"]["err"]);
                ok = true;
                break;
            }
        }
        if !ok {
            println!("⚠️ init not confirmed after 60s — check {sig} and rerun (keypair saved at {mfi_acc_path})");
            return;
        }
    } else {
        println!("MarginfiAccount already exists — reusing");
    }

    // ── the flashloan test tx ──
    // ix layout (end_index = 6): 0 cu_limit, 1 cu_price, 2 create-ATA,
    // 3 start_flashloan(6), 4 borrow 1 USDC, 5 deposit 1 USDC, 6 end_flashloan, 7 tip.
    let tip_to = {
        let mut t = None;
        for _ in 0..12 {
            if let Ok(v) = get_tip_accounts(&block_engine) {
                if let Some(a) = v.first().copied() { t = Some(a); break; }
            }
            std::thread::sleep(Duration::from_secs(3));
        }
        t.expect("tip accounts (rate limited)")
    };
    let bh = finalized_blockhash(&endpoint);
    println!("blockhash {bh}");
    let ixs = vec![
        cu_limit_ix(400_000),
        cu_price_ix(10_000),
        create_ata_idempotent(&signer, &usdc),
        start_flashloan(&mfi_acc, &signer, 6),
        borrow_usdc(&mfi_acc, &signer, &usdc_ata, 1_000_000),
        payback_usdc(&mfi_acc, &signer, &usdc_ata, 1_000_000, true),
        end_flashloan(&mfi_acc, &signer, &[]), // net-zero → empty remaining
        transfer_ix(signer, tip_to, tip_lamports),
    ];
    let msg = v0::Message::try_compile(&signer, &ixs, &[], bh).expect("compile flashloan");
    let tx = sign(msg, &[&authority]);
    let sig = tx.signatures[0].to_string();
    let raw = bincode::serialize(&tx).unwrap();
    let b64 = base64::engine::general_purpose::STANDARD.encode(&raw);
    println!("marginfi flashloan tx {}B sig={sig} tip={tip_lamports}", raw.len());

    // simulate first
    let sim = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
        "params":[b64,{"encoding":"base64","sigVerify":false,"replaceRecentBlockhash":true}]})).expect("simulate");
    let err = &sim["result"]["value"]["err"];
    if !err.is_null() {
        println!("⛔ simulation FAILED: {err}");
        for l in sim["result"]["value"]["logs"].as_array().into_iter().flatten() {
            println!("  {}", l.as_str().unwrap_or_default());
        }
        println!("\n(fix accounts/args from the logs above — likely vault PDA or deposit arg; see env overrides)");
        std::process::exit(1);
    }
    println!("✅ simulates clean ({} CU)", sim["result"]["value"]["unitsConsumed"]);

    // MODE=simbundle: run Jito's simulateBundle — executes the bundle exactly as
    // the block engine would (needs a simulateBundle-capable RPC, e.g. Helius).
    // Read-only, no cost. Rules out a filter/execution problem: if this succeeds
    // but the live bundle never lands, the barrier is the AUCTION (tip/profit),
    // not Jito rejecting the bundle.
    if mode == "simbundle" {
        let v = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateBundle",
            "params":[{"encodedTransactions":[b64]}]}));
        match v {
            Some(v) if v.get("error").filter(|e| !e.is_null()).is_some() => {
                println!("simulateBundle error: {}", v["error"]);
            }
            Some(v) => {
                let val = &v["result"]["value"];
                println!("simulateBundle summary: {}", val["summary"]);
                for (i, r) in val["transactionResults"].as_array().into_iter().flatten().enumerate() {
                    println!("  tx[{i}] err={} cu={}", r["err"], r["unitsConsumed"]);
                    for l in r["logs"].as_array().into_iter().flatten().rev().take(3).collect::<Vec<_>>().into_iter().rev() {
                        println!("    {}", l.as_str().unwrap_or_default());
                    }
                }
            }
            None => println!("simulateBundle: no response (does this RPC support it? use Helius)"),
        }
        return;
    }

    if !live {
        println!("dry run — rerun with LIVE=1 to submit via {mode} (~{} lamports if it lands)", tip_lamports + 10_000);
        return;
    }

    // submit
    match mode.as_str() {
        "rpc" => {
            let v = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"sendTransaction",
                "params":[b64,{"encoding":"base64","skipPreflight":false,"preflightCommitment":"confirmed","maxRetries":5}]})).expect("send");
            if let Some(e) = v.get("error").filter(|e| !e.is_null()) {
                println!("⛔ rpc rejected: {e}");
                std::process::exit(1);
            }
            println!("⚡ sent via plain RPC: {}", v["result"]);
        }
        _ => {
            let mut attempt = 0;
            let id = loop {
                attempt += 1;
                match send_bundle(&block_engine, &[b64.clone()]) {
                    Ok(id) => break id,
                    Err(e) if e.to_string().contains("429") && attempt < 12 => {
                        println!("  [attempt {attempt}] rate limited, retry in 5s…");
                        std::thread::sleep(Duration::from_secs(5));
                    }
                    Err(e) => panic!("send bundle: {e}"),
                }
            };
            println!("⚡ submitted Jito bundle {id} (attempt {attempt})");
        }
    }

    for i in 1..=18 {
        std::thread::sleep(Duration::from_secs(5));
        if let Some(meta) = landed(&endpoint, &sig) {
            println!("\n🎉 LANDED via {mode} — slot {} fee {} err {}", meta["slot"], meta["meta"]["fee"], meta["meta"]["err"]);
            println!("https://solscan.io/tx/{sig}");
            return;
        }
        println!("[{}s] on_chain=false", i * 5);
    }
    println!("\n⚠️ not landed after 90s via {mode}. If MODE=rpc landed but jito didn't → flash loans are filtered on Jito generally → pivot to inventory mode.");
}
