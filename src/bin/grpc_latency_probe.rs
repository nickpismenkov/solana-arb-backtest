//! Measure the REAL freshness of the Yellowstone gRPC account stream — the thing
//! that decides whether streaming can replace hot-path RPC polling for the
//! liquidation fire loop. Subscribes to marginfi program account updates + slot
//! updates, and for each account update at slot S computes the lag against the
//! latest tip slot we've seen (lag 0-1 = we get updates as blocks are produced;
//! lag 3+ ≈ >1s behind = too slow to fire competitively).
//!
//! Usage: GRPC_ENDPOINT=<url> GRPC_X_TOKEN=<tok> [SECS=30] cargo run --release --bin grpc_latency_probe

use anyhow::Result;
use futures::StreamExt;
use std::collections::HashMap;
use std::time::{Duration, Instant};
use yellowstone_grpc_client::{ClientTlsConfig, GeyserGrpcClient};
use yellowstone_grpc_proto::prelude::{
    subscribe_update::UpdateOneof, CommitmentLevel, SubscribeRequest,
    SubscribeRequestFilterAccounts, SubscribeRequestFilterSlots,
};

const MARGINFI_PROGRAM: &str = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA";

#[tokio::main]
async fn main() -> Result<()> {
    let endpoint = std::env::var("GRPC_ENDPOINT").expect("GRPC_ENDPOINT");
    let x_token = std::env::var("GRPC_X_TOKEN").expect("GRPC_X_TOKEN");
    let secs: u64 = std::env::var("SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(30);

    eprintln!("[grpc] connecting to {} …", endpoint.split('/').nth(2).unwrap_or("?"));
    let t_connect = Instant::now();
    let mut client = GeyserGrpcClient::build_from_shared(endpoint)?
        .x_token(Some(x_token))?
        .tls_config(ClientTlsConfig::new().with_native_roots())?
        .connect()
        .await?;
    eprintln!("[grpc] connected in {:?}", t_connect.elapsed());

    let mut accounts = HashMap::new();
    accounts.insert("mfi".to_string(), SubscribeRequestFilterAccounts {
        account: vec![], owner: vec![MARGINFI_PROGRAM.to_string()], filters: vec![], ..Default::default()
    });
    let mut slots = HashMap::new();
    slots.insert("slots".to_string(), SubscribeRequestFilterSlots { ..Default::default() });

    let request = SubscribeRequest {
        accounts, slots,
        commitment: Some(CommitmentLevel::Processed as i32),
        ..Default::default()
    };

    let (mut _sink, mut stream) = client.subscribe_with_request(Some(request)).await?;
    eprintln!("[grpc] subscribed (marginfi accounts + slots, processed). measuring {secs}s …\n");

    let mut tip_slot: u64 = 0;
    let mut acct_updates: u64 = 0;
    let mut slot_updates: u64 = 0;
    let mut lags: Vec<i64> = Vec::new();
    let mut first_update: Option<Instant> = None;
    let deadline = Instant::now() + Duration::from_secs(secs);

    while let Some(message) = stream.next().await {
        if Instant::now() >= deadline { break; }
        let msg = message?;
        match msg.update_oneof {
            Some(UpdateOneof::Slot(s)) => {
                slot_updates += 1;
                if s.slot > tip_slot { tip_slot = s.slot; }
            }
            Some(UpdateOneof::Account(acc)) => {
                acct_updates += 1;
                first_update.get_or_insert_with(Instant::now);
                if tip_slot > 0 {
                    lags.push(tip_slot as i64 - acc.slot as i64);
                }
            }
            _ => {}
        }
    }

    lags.sort_unstable();
    let n = lags.len();
    let med = if n > 0 { lags[n/2] } else { 0 };
    let p90 = if n > 0 { lags[(n*9/10).min(n-1)] } else { 0 };
    println!("═══ gRPC stream freshness (Tatum, {secs}s) ═══");
    println!("  account updates: {acct_updates}  ({:.0}/s)", acct_updates as f64 / secs as f64);
    println!("  slot updates:    {slot_updates}");
    println!("  slot lag (tip_slot − account_update_slot): median {med}, p90 {p90}  [0-1=fresh, 3+=slow]");
    println!("  → ~{:.0}ms median staleness (at ~400ms/slot)", med as f64 * 400.0);
    if med <= 1 { println!("  VERDICT: FRESH — stream keeps pace with block production. Good enough to fire on."); }
    else if med <= 2 { println!("  VERDICT: OK — ~1 slot behind, usable."); }
    else { println!("  VERDICT: SLOW — {med} slots behind, too stale to fire competitively; need a better provider."); }
    Ok(())
}
