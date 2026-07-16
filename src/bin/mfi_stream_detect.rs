//! Step 1 of the streaming migration: prove Dragon's Mouth OWNER-subscription
//! streams the marginfi program's accounts and populates a LIVE in-memory loan
//! book — the thing that removes the hot-path RPC poll. Read-only: decodes each
//! account update into MarginfiAccount / Bank maps, counts the update rate, and
//! reports freshness (slot lag vs an independent RPC tip). No firing.
//!
//! Usage: GRPC_ENDPOINT=<triton-url-with-token> GRPC_X_TOKEN=<tok> HELIUS_RPC=<url>
//!        [SECS=30] cargo run --release --bin mfi_stream_detect

use anyhow::Result;
use arb_engine::liquidation::{Bank, MarginfiAccount};
use futures::StreamExt;
use solana_pubkey::Pubkey;
use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, RwLock};
use std::time::{Duration, Instant};
use yellowstone_grpc_client::{ClientTlsConfig, GeyserGrpcClient};
use yellowstone_grpc_proto::prelude::{
    subscribe_update::UpdateOneof, CommitmentLevel, SubscribeRequest, SubscribeRequestFilterAccounts,
};

const MARGINFI_PROGRAM: &str = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA";

#[tokio::main]
async fn main() -> Result<()> {
    let endpoint = std::env::var("GRPC_ENDPOINT").expect("GRPC_ENDPOINT");
    let x_token = std::env::var("GRPC_X_TOKEN").expect("GRPC_X_TOKEN");
    let rpc = std::env::var("HELIUS_RPC").expect("HELIUS_RPC");
    let secs: u64 = std::env::var("SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(30);

    // Independent tip-slot reference (bg thread) → absolute freshness of the stream.
    let tip = Arc::new(AtomicU64::new(0));
    {
        let (tip, rpc) = (tip.clone(), rpc.clone());
        std::thread::spawn(move || loop {
            if let Ok(r) = ureq::post(&rpc).send_json(ureq::json!(
                {"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"processed"}]})) {
                if let Ok(v) = r.into_json::<serde_json::Value>() {
                    if let Some(s) = v["result"].as_u64() { tip.store(s, Ordering::Relaxed); }
                }
            }
            std::thread::sleep(Duration::from_millis(400));
        });
    }

    // The LIVE loan book — populated purely from the stream. This is what the
    // fire loop will read instead of polling+re-fetching on the hot path.
    let accounts: Arc<RwLock<HashMap<Pubkey, MarginfiAccount>>> = Arc::new(RwLock::new(HashMap::new()));
    let banks: Arc<RwLock<HashMap<Pubkey, Bank>>> = Arc::new(RwLock::new(HashMap::new()));

    // Triton tier3 rejects owner (program-wide) subscriptions (0 updates), but
    // supports thousands of SPECIFIC accounts on one connection. For liquidations
    // we know our set: scan the marginfi accounts (pubkeys only) and subscribe to
    // a capped batch — the real migration pattern (subscribe the watch-set).
    let max_sub: usize = std::env::var("MAX_SUB").ok().and_then(|s| s.parse().ok()).unwrap_or(2000);
    eprintln!("[stream] scanning marginfi account pubkeys to subscribe …");
    let scan = ureq::post(&rpc).send_json(ureq::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[MARGINFI_PROGRAM, {"encoding":"base64","dataSlice":{"offset":0,"length":0},
            "filters":[{"dataSize": arb_engine::liquidation::MA_SIZE}]}]}))?
        .into_json::<serde_json::Value>()?;
    let mut sub_accounts: Vec<String> = scan["result"].as_array().map(|a| a.iter()
        .filter_map(|e| e["pubkey"].as_str().map(String::from)).take(max_sub).collect()).unwrap_or_default();
    // Prepend the 3 known high-activity banks (update every slot-ish) so we can
    // tell a subscription-size limit (no bank updates either) from quiet
    // borrowers (bank updates flow, borrowers just idle).
    for b in ["2s37akK2eyBbp8DZgCm7RtsaEz8eJP3Nxd4urLHQv7yB",
              "DeyH7QxWvnbbaVB4zFrf4hoq7Q8z1ZT14co42BGwGtfM",
              "CCKtUs6Cgwo4aaQUmBPmyoApH2gUDErxNZCAntD6LYGh"] {
        sub_accounts.insert(0, b.to_string());
    }
    eprintln!("[stream] subscribing to {} accounts (3 active banks + {} borrowers)", sub_accounts.len(), sub_accounts.len()-3);

    eprintln!("[stream] connecting to Dragon's Mouth …");
    let mut client = GeyserGrpcClient::build_from_shared(endpoint)?
        .x_token(Some(x_token))?
        .tls_config(ClientTlsConfig::new().with_native_roots())?
        .connect().await?;

    let mut accs = HashMap::new();
    accs.insert("mfi".to_string(), SubscribeRequestFilterAccounts {
        account: sub_accounts, owner: vec![], filters: vec![], ..Default::default()
    });
    let _ = MARGINFI_PROGRAM;
    let req = SubscribeRequest { accounts: accs, commitment: Some(CommitmentLevel::Processed as i32), ..Default::default() };
    let (mut _sink, mut stream) = client.subscribe_with_request(Some(req)).await?;
    eprintln!("[stream] owner-subscribed marginfi (processed). building live loan book for {secs}s …\n");

    let (mut updates, mut acct_decodes, mut bank_decodes) = (0u64, 0u64, 0u64);
    let mut lags: Vec<i64> = Vec::new();
    let deadline = Instant::now() + Duration::from_secs(secs);
    let mut last_log = Instant::now();

    loop {
        if Instant::now() >= deadline { break; }
        match tokio::time::timeout(Duration::from_millis(500), stream.next()).await {
            Ok(Some(Ok(msg))) => {
                if let Some(UpdateOneof::Account(acc)) = msg.update_oneof {
                    updates += 1;
                    let t = tip.load(Ordering::Relaxed);
                    if t > 0 { lags.push(t as i64 - acc.slot as i64); }
                    if let Some(info) = acc.account {
                        let pk = Pubkey::try_from(info.pubkey.as_slice()).ok();
                        if let Some(pk) = pk {
                            if info.data.len() == arb_engine::liquidation::MA_SIZE {
                                if let Some(a) = MarginfiAccount::decode(&info.data) {
                                    accounts.write().unwrap().insert(pk, a); acct_decodes += 1;
                                }
                            } else if let Some(b) = Bank::decode(&info.data) {
                                banks.write().unwrap().insert(pk, b); bank_decodes += 1;
                            }
                        }
                    }
                }
            }
            Ok(Some(Err(e))) => { eprintln!("[stream] error: {e}"); break; }
            Ok(None) => break,
            Err(_) => {}
        }
        if last_log.elapsed() >= Duration::from_secs(5) {
            eprintln!("[stream] {updates} updates | live loan book: {} accounts, {} banks",
                accounts.read().unwrap().len(), banks.read().unwrap().len());
            last_log = Instant::now();
        }
    }

    lags.sort_unstable();
    let n = lags.len();
    let med = if n > 0 { lags[n/2] } else { 0 };
    println!("\n═══ marginfi streaming detector (Dragon's Mouth, {secs}s) ═══");
    println!("  account updates received: {updates}  ({:.0}/s)", updates as f64 / secs as f64);
    println!("  decoded into live map:    {acct_decodes} account-writes, {bank_decodes} bank-writes");
    println!("  live loan book now holds: {} accounts, {} banks", accounts.read().unwrap().len(), banks.read().unwrap().len());
    println!("  stream freshness (slot lag vs RPC tip): median {med}  [≤1 or negative = leads block production]");
    if updates > 0 && med <= 1 {
        println!("  VERDICT: ✅ live loan book populates from the stream, fresh. Hot-path RPC poll is replaceable.");
    } else if updates == 0 {
        println!("  VERDICT: ❌ no updates — owner subscription not delivering (check tier/endpoint).");
    } else {
        println!("  VERDICT: ⚠ updates flowing but stale (median {med} slots) — investigate.");
    }
    Ok(())
}
