//! Deploy an address-lookup-table (ALT) on-chain from a list of accounts on
//! stdin — so the box doesn't need the `solana` CLI to create SAVE_ALT / JUP_ALT.
//!
//! Reads pubkeys (one per line) from stdin, creates a fresh lookup table owned by
//! the KEYPAIR_PATH wallet, extends it with those addresses (batched to fit the
//! 1232B tx limit), and prints the resulting table address.
//!
//! Pipe it from the *_alt_print bins:
//!   LIVE=1 cargo run --release --bin save_alt_print | cargo run --release --bin alt_deploy
//!   LIVE=1 cargo run --release --bin jup_alt_print  | cargo run --release --bin alt_deploy
//! then `export SAVE_ALT=<printed table>` (or JUP_ALT=…).
//!
//! SAFETY: DRY-RUN by default — it only prints the plan. Set LIVE=1 to submit
//! real txs (creates on-chain state + spends a little SOL for rent + fees, signed
//! by KEYPAIR_PATH). Uses the official solana-address-lookup-table-interface
//! instruction builders, not a hand-rolled format.
//!
//! Env: HELIUS_RPC|RPC_HTTP|RPC_ENDPOINT, KEYPAIR_PATH, [LIVE=1], [CU_PRICE=<micro-lamports>]

use arb_engine::arb::{cu_limit_ix, cu_price_ix};
use base64::Engine;
use solana_address_lookup_table_interface::instruction::{create_lookup_table, extend_lookup_table};
use solana_hash::Hash;
use solana_keypair::Keypair;
use solana_message::{v0, VersionedMessage};
use solana_pubkey::Pubkey;
use solana_signer::Signer;
use solana_transaction::versioned::VersionedTransaction;
use std::io::Read;
use std::str::FromStr;
use std::time::{Duration, Instant};

/// Addresses per extend tx. Each pubkey is 32B of ix data; 20 keeps the tx well
/// under the 1232B single-packet limit with room for the header + signature.
const EXTEND_BATCH: usize = 20;

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

fn latest_blockhash(endpoint: &str) -> Hash {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getLatestBlockhash",
        "params":[{"commitment":"finalized"}]})).expect("getLatestBlockhash");
    Hash::from_str(v["result"]["value"]["blockhash"].as_str().expect("blockhash")).unwrap()
}

/// Build a v0 tx from `ixs`, sign with `kp`, submit, and poll until confirmed.
/// Returns the signature. Exits the process on rejection or confirm timeout.
fn send_and_confirm(
    endpoint: &str,
    kp: &Keypair,
    ixs: &[solana_instruction::Instruction],
    label: &str,
) -> String {
    let bh = latest_blockhash(endpoint);
    let msg = v0::Message::try_compile(&kp.pubkey(), ixs, &[], bh).expect("compile");
    let mut tx = VersionedTransaction {
        signatures: vec![solana_signature::Signature::default()],
        message: VersionedMessage::V0(msg),
    };
    tx.signatures[0] = kp.sign_message(&tx.message.serialize());
    let sig = tx.signatures[0].to_string();
    let b64 = base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&tx).unwrap());

    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"sendTransaction",
        "params":[b64,{"encoding":"base64","skipPreflight":false,"preflightCommitment":"confirmed","maxRetries":5}]}))
        .expect("sendTransaction");
    if let Some(e) = v.get("error").filter(|e| !e.is_null()) {
        eprintln!("⛔ {label}: sendTransaction rejected: {e}");
        std::process::exit(1);
    }
    println!("  {label}: submitted {sig} — confirming…");

    let start = Instant::now();
    while start.elapsed() < Duration::from_secs(45) {
        std::thread::sleep(Duration::from_millis(1500));
        let s = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getSignatureStatuses",
            "params":[[sig], {"searchTransactionHistory":false}]}));
        let Some(s) = s else { continue };
        let st = &s["result"]["value"][0];
        if st.is_null() {
            continue;
        }
        if !st["err"].is_null() {
            eprintln!("⛔ {label}: tx failed on-chain: {}", st["err"]);
            std::process::exit(1);
        }
        let cs = st["confirmationStatus"].as_str().unwrap_or("");
        if cs == "confirmed" || cs == "finalized" {
            println!("  {label}: ✅ {cs}");
            return sig;
        }
    }
    eprintln!("⛔ {label}: not confirmed within 45s (sig {sig}); check explorer before retrying to avoid a duplicate table");
    std::process::exit(1);
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC")
        .or_else(|_| std::env::var("RPC_HTTP"))
        .or_else(|_| std::env::var("RPC_ENDPOINT"))
        .expect("set HELIUS_RPC / RPC_HTTP / RPC_ENDPOINT");
    let keypair_path = std::env::var("KEYPAIR_PATH").expect("set KEYPAIR_PATH");
    let live = std::env::var("LIVE").map(|v| v == "1").unwrap_or(false);
    let cu_price: u64 = std::env::var("CU_PRICE").ok().and_then(|s| s.parse().ok()).unwrap_or(100_000);

    let bytes: Vec<u8> = serde_json::from_str(&std::fs::read_to_string(&keypair_path).expect("read keypair"))
        .expect("parse keypair (expects a JSON byte array)");
    let kp = Keypair::try_from(&bytes[..]).expect("keypair");
    let authority = kp.pubkey();

    // Read addresses from stdin (one per line). Non-parseable lines are skipped
    // so piping a print bin's stdout (addresses only; notes go to stderr) is clean.
    let mut input = String::new();
    std::io::stdin().read_to_string(&mut input).expect("read stdin");
    let mut seen = std::collections::HashSet::new();
    let addrs: Vec<Pubkey> = input
        .lines()
        .filter_map(|l| Pubkey::from_str(l.trim()).ok())
        .filter(|p| seen.insert(*p))
        .collect();
    if addrs.is_empty() {
        eprintln!("⛔ no valid pubkeys on stdin — pipe from save_alt_print / jup_alt_print");
        std::process::exit(1);
    }

    // Recent (finalized) slot for the CreateLookupTable derivation.
    let slot = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getSlot",
        "params":[{"commitment":"finalized"}]})).expect("getSlot")["result"]
        .as_u64()
        .expect("slot");
    let (create_ix, table) = create_lookup_table(authority, authority, slot);
    let batches = addrs.len().div_ceil(EXTEND_BATCH);

    println!("ALT deploy plan:");
    println!("  authority/payer : {authority}");
    println!("  addresses       : {}", addrs.len());
    println!("  recent slot     : {slot}");
    println!("  table address   : {table}");
    println!("  txs             : 1 create + {batches} extend (batches of {EXTEND_BATCH})");

    if !live {
        println!("\nDRY RUN — nothing submitted. Set LIVE=1 to deploy for real.");
        println!("NOTE: the table address above is derived from the current slot and will DIFFER on the live run;");
        println!("      the real address is printed at the end of the LIVE run.");
        return;
    }

    println!("\nLIVE — submitting…");
    send_and_confirm(&endpoint, &kp, &[cu_limit_ix(60_000), cu_price_ix(cu_price), create_ix], "create");
    for (i, chunk) in addrs.chunks(EXTEND_BATCH).enumerate() {
        let ix = extend_lookup_table(table, authority, Some(authority), chunk.to_vec());
        send_and_confirm(&endpoint, &kp, &[cu_limit_ix(60_000), cu_price_ix(cu_price), ix], &format!("extend {}/{batches}", i + 1));
    }

    println!("\n✅ ALT deployed with {} addresses.", addrs.len());
    println!("   table = {table}");
    println!("   → export the matching var, e.g.  export SAVE_ALT={table}   (or JUP_ALT={table})");
    println!("   (an ALT needs ~1 slot to warm up before it can be used in a tx.)");
}
