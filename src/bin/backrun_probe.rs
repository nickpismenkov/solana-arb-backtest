//! The go/no-go measurement. On each ShredStream trigger (a swap hitting one of
//! our pools), simulate that victim tx against current state and read the
//! POST-victim pool prices — the residual cross-venue gap a backrun placed
//! right after it could capture. Real chain math (CPI and all), no tx
//! construction, no money at risk.
//!
//! Reverts (the victim's own slippage guard) are skipped — those wouldn't have
//! moved the pool anyway. Sampling: while a simulate is in flight, queued
//! triggers are drained and counted as skipped (we measure at the RPC's pace).
//!
//! Usage (on the box):
//!   RPC_ENDPOINT=<helius-url> SHREDSTREAM_PORT=20000 RUN_MS=600000 \
//!     cargo run --release --bin backrun_probe

use arb_engine::pools::{orca_price, pair, ray_clmm_price};
use base64::Engine;
use std::time::Duration;
use tokio::sync::mpsc;

const TIP_CUSHION_BPS: f64 = 2.0; // rough gas+tip headroom

fn simulate_victim(rpc: &str, tx_b64: &str) -> Option<(f64, f64)> {
    let body = serde_json::json!({
        "jsonrpc":"2.0","id":1,"method":"simulateTransaction",
        "params":[tx_b64, {
            "encoding":"base64","sigVerify":false,"replaceRecentBlockhash":true,
            "accounts":{"encoding":"base64","addresses":[pair().orca_pool, pair().ray_pool]}
        }]
    });
    let resp: serde_json::Value = ureq::post(rpc).send_json(body).ok()?.into_json().ok()?;
    let v = &resp["result"]["value"];
    if !v["err"].is_null() {
        return None; // victim reverted (slippage) — wouldn't move the pool
    }
    let accs = v["accounts"].as_array()?;
    let dec = |i: usize| -> Option<Vec<u8>> {
        let b64 = accs.get(i)?["data"][0].as_str()?;
        base64::engine::general_purpose::STANDARD.decode(b64).ok()
    };
    let orca = orca_price(&dec(0)?)?;
    let ray = ray_clmm_price(&dec(1)?)?;
    Some((orca, ray))
}

fn median(mut v: Vec<f64>) -> f64 {
    if v.is_empty() {
        return 0.0;
    }
    v.sort_by(|a, b| a.partial_cmp(b).unwrap());
    v[v.len() / 2]
}

#[tokio::main]
async fn main() {
    let _ = dotenvy::dotenv();
    let rpc = std::env::var("RPC_ENDPOINT").expect("set RPC_ENDPOINT (Helius) for simulate + ALT");
    let port: u16 = std::env::var("SHREDSTREAM_PORT")
        .ok()
        .and_then(|s| s.parse().ok())
        .unwrap_or(20000);
    let run_ms: u64 = std::env::var("RUN_MS")
        .ok()
        .and_then(|s| s.parse().ok())
        .unwrap_or(600_000);

    let fee_bps = pair().round_trip_fee_bps();
    println!(
        "backrun-probe — simulate victims → residual gap. pair {}, threshold: fee {fee_bps}bp (+{TIP_CUSHION_BPS}bp cushion). Running {}s…\n",
        pair().label,
        run_ms / 1000
    );

    let (tx, mut rx) = mpsc::unbounded_channel();
    let _feed = arb_engine::shredstream::run_shredstream_feed(port, Some(rpc.clone()), tx);

    let (mut triggers, mut skipped, mut simmed, mut reverted, mut opps, mut opps_net) =
        (0u64, 0u64, 0u64, 0u64, 0u64, 0u64);
    let mut gaps: Vec<f64> = Vec::new();
    let mut nets: Vec<f64> = Vec::new();

    let deadline = tokio::time::sleep(Duration::from_millis(run_ms));
    tokio::pin!(deadline);

    loop {
        tokio::select! {
            _ = &mut deadline => break,
            Some(t) = rx.recv() => {
                triggers += 1;
                // Drain backlog accumulated while the last sim ran → sample at RPC pace.
                while rx.try_recv().is_ok() { triggers += 1; skipped += 1; }
                if t.raw.is_empty() { continue; }
                let tx_b64 = base64::engine::general_purpose::STANDARD.encode(&t.raw);
                let rpc2 = rpc.clone();
                let res = tokio::task::spawn_blocking(move || simulate_victim(&rpc2, &tx_b64))
                    .await
                    .ok()
                    .flatten();
                match res {
                    None => reverted += 1,
                    Some((orca, ray)) => {
                        simmed += 1;
                        let gap = ((ray - orca) / orca.min(ray) * 10_000.0).abs();
                        gaps.push(gap);
                        if gap > fee_bps {
                            opps += 1;
                            let net = gap - fee_bps;
                            nets.push(net);
                            if gap > fee_bps + TIP_CUSHION_BPS { opps_net += 1; }
                            println!(
                                "⚡ backrunnable via {} slot {} — gap {:.1}bp, net {:.1}bp (post-victim Orca ${:.4} / Ray ${:.4})",
                                t.venue, t.slot, gap, net, orca, ray
                            );
                        }
                    }
                }
            }
        }
    }

    println!("\n──────── backrun-probe report ({}s) ────────", run_ms / 1000);
    println!("pool triggers seen:        {triggers}");
    println!("  simulated (sampled):     {}", simmed + reverted);
    println!("  skipped (RPC-paced):     {skipped}");
    println!("victim sims applied ok:    {simmed}");
    println!("victim sims reverted:      {reverted}  (own slippage — no pool move)");
    println!("── residual cross-venue gap after a real swap ──");
    if simmed > 0 {
        println!("  median gap: {:.1} bp   max gap: {:.1} bp", median(gaps.clone()), gaps.iter().cloned().fold(0.0, f64::max));
        println!("  fee-clearing (>{fee_bps}bp):        {opps}/{simmed} ({:.0}%)", opps as f64 / simmed as f64 * 100.0);
        println!("  after tip cushion (>{:.0}bp): {opps_net}/{simmed} ({:.0}%)", fee_bps + TIP_CUSHION_BPS, opps_net as f64 / simmed as f64 * 100.0);
        if !nets.is_empty() {
            println!("  net edge when present: median {:.1} bp, max {:.1} bp", median(nets.clone()), nets.iter().cloned().fold(0.0, f64::max));
        }
    } else {
        println!("  no successful victim sims — check RPC / freshness.");
    }
    println!();
}
