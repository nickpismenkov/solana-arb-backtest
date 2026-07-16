//! Definitive Triton liveness test: subscribe to SLOTS + a few specific accounts
//! on one connection and count each separately. Slots tick ~2.5×/s unconditionally
//! — so this discriminates:
//!   • slots > 0, accounts = 0  → connection & stream fine; account sub is the issue
//!   • slots = 0, accounts = 0  → whole stream throttled/banned (transport is up but
//!                                 no data flows) → it's the rate-limit penalty box
//! Usage: GRPC_ENDPOINT=<url> GRPC_X_TOKEN=<tok> [SECS=15] cargo run --release --bin grpc_ping

use anyhow::Result;
use futures::StreamExt;
use std::collections::HashMap;
use std::time::{Duration, Instant};
use yellowstone_grpc_client::{ClientTlsConfig, GeyserGrpcClient};
use yellowstone_grpc_proto::prelude::{
    subscribe_update::UpdateOneof, CommitmentLevel, SubscribeRequest,
    SubscribeRequestFilterAccounts, SubscribeRequestFilterSlots,
};

#[tokio::main]
async fn main() -> Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("GRPC_ENDPOINT").expect("GRPC_ENDPOINT");
    let x_token = std::env::var("GRPC_X_TOKEN").expect("GRPC_X_TOKEN");
    let secs: u64 = std::env::var("SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(15);

    eprintln!("[ping] connecting …");
    let t = Instant::now();
    let mut client = GeyserGrpcClient::build_from_shared(endpoint)?
        .x_token(Some(x_token))?
        .tls_config(ClientTlsConfig::new().with_native_roots())?
        .connect().await?;
    eprintln!("[ping] connected in {:?}", t.elapsed());

    let mut slots = HashMap::new();
    slots.insert("s".to_string(), SubscribeRequestFilterSlots { filter_by_commitment: Some(false), ..Default::default() });
    // Default includes the Clock sysvar — it updates EVERY slot, so if even Clock
    // yields 0 account updates, account subscriptions are broadly not delivering.
    let acct_list: Vec<String> = std::env::var("ACCOUNTS").ok()
        .map(|s| s.split(',').map(|x| x.trim().to_string()).collect())
        .unwrap_or_else(|| vec![
            "SysvarC1ock11111111111111111111111111111111".into(), // Clock — ticks every slot
            "2s37akK2eyBbp8DZgCm7RtsaEz8eJP3Nxd4urLHQv7yB".into(), // marginfi USDC bank
            "DeyH7QxWvnbbaVB4zFrf4hoq7Q8z1ZT14co42BGwGtfM".into(), // marginfi BONK bank
            "CCKtUs6Cgwo4aaQUmBPmyoApH2gUDErxNZCAntD6LYGh".into(), // marginfi wSOL bank
        ]);
    let commitment = match std::env::var("COMMITMENT").as_deref() {
        Ok("confirmed") => CommitmentLevel::Confirmed,
        Ok("finalized") => CommitmentLevel::Finalized,
        _ => CommitmentLevel::Processed,
    };
    eprintln!("[ping] {} accounts, commitment={commitment:?}", acct_list.len());
    let mut accounts = HashMap::new();
    accounts.insert("a".to_string(), SubscribeRequestFilterAccounts {
        account: acct_list, owner: vec![], filters: vec![], ..Default::default() });
    let req = SubscribeRequest {
        slots, accounts, commitment: Some(commitment as i32), ..Default::default() };

    let (mut _sink, mut stream) = client.subscribe_with_request(Some(req)).await?;
    eprintln!("[ping] subscribed (slots + 3 accounts, processed). listening {secs}s …\n");

    let (mut n_slot, mut n_acct, mut n_other) = (0u64, 0u64, 0u64);
    let deadline = Instant::now() + Duration::from_secs(secs);
    loop {
        if Instant::now() >= deadline { break; }
        match tokio::time::timeout(Duration::from_millis(500), stream.next()).await {
            Ok(Some(Ok(msg))) => match msg.update_oneof {
                Some(UpdateOneof::Slot(_)) => n_slot += 1,
                Some(UpdateOneof::Account(_)) => n_acct += 1,
                Some(UpdateOneof::Ping(_)) | None => {}
                Some(_) => n_other += 1,
            },
            Ok(Some(Err(e))) => { eprintln!("[ping] stream error: {e}"); break; }
            Ok(None) => { eprintln!("[ping] stream closed by server"); break; }
            Err(_) => {}
        }
    }

    println!("\n═══ Triton liveness ({secs}s) ═══");
    println!("  SLOT updates:    {n_slot}  ({:.1}/s)", n_slot as f64 / secs as f64);
    println!("  ACCOUNT updates: {n_acct}");
    println!("  other:           {n_other}");
    if n_slot > 0 && n_acct > 0 {
        println!("  VERDICT: ✅ FULLY LIVE — stream + account subscription both delivering.");
    } else if n_slot > 0 {
        println!("  VERDICT: ⚠ stream is LIVE (slots flow) but ACCOUNT updates = 0 → subscription/filter issue, NOT a ban.");
    } else {
        println!("  VERDICT: ❌ stream SILENT (0 slots too) — connection up but no data → rate-limit penalty box still active.");
    }
    Ok(())
}
