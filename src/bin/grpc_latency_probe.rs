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
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};
use yellowstone_grpc_client::{ClientTlsConfig, GeyserGrpcClient};
use yellowstone_grpc_proto::prelude::{
    subscribe_update::UpdateOneof, CommitmentLevel, SubscribeRequest,
    SubscribeRequestFilterAccounts,
};

const MARGINFI_PROGRAM: &str = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA";

#[tokio::main]
async fn main() -> Result<()> {
    let endpoint = std::env::var("GRPC_ENDPOINT").expect("GRPC_ENDPOINT");
    let x_token = std::env::var("GRPC_X_TOKEN").expect("GRPC_X_TOKEN");
    let rpc = std::env::var("HELIUS_RPC").expect("HELIUS_RPC");
    let secs: u64 = std::env::var("SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(30);

    // Independent tip-slot reference: poll getSlot(processed) via RPC on a bg
    // thread so lag = rpc_tip − gRPC_account_slot is an ABSOLUTE latency (not a
    // self-referential max-seen proxy). A stream FRESHER than RPC yields lag ≤ 0.
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

    eprintln!("[grpc] connecting to {} …", endpoint.split('/').nth(2).unwrap_or("?"));
    let t_connect = Instant::now();
    let mut client = GeyserGrpcClient::build_from_shared(endpoint)?
        .x_token(Some(x_token))?
        .tls_config(ClientTlsConfig::new().with_native_roots())?
        .connect()
        .await?;
    eprintln!("[grpc] connected in {:?}", t_connect.elapsed());

    // Tatum's gateway tier appears to reject owner (program-wide) subscriptions,
    // so subscribe to specific high-activity accounts — marginfi USDC + BONK
    // banks (update on every deposit/borrow/interest tick). ACCOUNTS env
    // overrides with a comma-separated list.
    let watch: Vec<String> = std::env::var("ACCOUNTS").ok()
        .map(|s| s.split(',').map(|x| x.trim().to_string()).collect())
        .unwrap_or_else(|| vec![
            "2s37akK2eyBbp8DZgCm7RtsaEz8eJP3Nxd4urLHQv7yB".into(), // marginfi USDC bank
            "DeyH7QxWvnbbaVB4zFrf4hoq7Q8z1ZT14co42BGwGtfM".into(), // marginfi BONK bank
            "CCKtUs6Cgwo4aaQUmBPmyoApH2gUDErxNZCAntD6LYGh".into(), // marginfi wSOL bank
        ]);
    let _ = MARGINFI_PROGRAM;
    let mut accounts = HashMap::new();
    accounts.insert("watch".to_string(), SubscribeRequestFilterAccounts {
        account: watch.clone(), owner: vec![], filters: vec![], ..Default::default()
    });
    eprintln!("[grpc] watching {} specific accounts", watch.len());

    let request = SubscribeRequest {
        accounts,
        commitment: Some(CommitmentLevel::Processed as i32),
        ..Default::default()
    };

    let (mut _sink, mut stream) = client.subscribe_with_request(Some(request)).await?;
    eprintln!("[grpc] subscribed (marginfi accounts, processed). measuring {secs}s …\n");

    let mut acct_updates: u64 = 0;
    let mut lags: Vec<i64> = Vec::new();
    let deadline = Instant::now() + Duration::from_secs(secs);

    while let Some(message) = stream.next().await {
        if Instant::now() >= deadline { break; }
        let msg = message?;
        if let Some(UpdateOneof::Account(acc)) = msg.update_oneof {
            acct_updates += 1;
            let t = tip.load(Ordering::Relaxed);
            if t > 0 { lags.push(t as i64 - acc.slot as i64); }
        }
    }

    lags.sort_unstable();
    let n = lags.len();
    let med = if n > 0 { lags[n/2] } else { 0 };
    let p90 = if n > 0 { lags[(n*9/10).min(n-1)] } else { 0 };
    let best = lags.first().copied().unwrap_or(0);
    println!("═══ gRPC stream freshness (Tatum, {secs}s) ═══");
    println!("  account updates: {acct_updates}  ({:.0}/s)", acct_updates as f64 / secs as f64);
    println!("  slot lag (RPC_tip − gRPC_account_slot): median {med}, p90 {p90}, best {best}  [≤1=fresh, 3+=slow]");
    println!("  (note: RPC tip itself lags ~1 slot, so lag ~0-1 means gRPC keeps pace with the chain)");
    println!("  → ~{:.0}ms median staleness (at ~400ms/slot)", med as f64 * 400.0);
    if med <= 1 { println!("  VERDICT: FRESH — stream keeps pace with block production. Good enough to fire on."); }
    else if med <= 2 { println!("  VERDICT: OK — ~1 slot behind, usable."); }
    else { println!("  VERDICT: SLOW — {med} slots behind, too stale to fire competitively; need a better provider."); }
    Ok(())
}
