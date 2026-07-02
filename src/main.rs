//! shadow (Rust) — reproduces the TS `shadow` harness: gRPC prices → the
//! fee-adjusted Detector → reaction-budget report, with a live price heartbeat.
//! Testable locally against Tatum; the ShredStream feed + shred-time pricing
//! land in later PRs.

use anyhow::Result;
use arb_engine::detector::{median_f64, median_u128, ArbEvent, Detector, Tick, TickResult};
use arb_engine::grpc;
use std::time::Duration;
use tokio::sync::mpsc;

#[tokio::main]
async fn main() -> Result<()> {
    let _ = dotenvy::dotenv();
    // rustls 0.23 needs an explicit process-level crypto provider (TLS to Tatum).
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("GRPC_ENDPOINT")
        .unwrap_or_else(|_| "https://solana-mainnet-grpc.gateway.tatum.io".to_string());
    let x_token = std::env::var("GRPC_X_TOKEN").expect("set GRPC_X_TOKEN in .env");
    let run_ms: u64 = std::env::var("RUN_MS")
        .ok()
        .and_then(|s| s.parse().ok())
        .unwrap_or(120_000);

    // Orca whirlpool 4bp, Raydium CLMM 4bp → 8bp round-trip threshold.
    let mut detector = Detector::new("Orca", "Raydium", 4.0, 4.0);
    println!(
        "\nshadow (Rust) — gRPC prices. Threshold {} bps. Running {}s…\n",
        detector.threshold_bps,
        run_ms / 1000
    );

    let (tx, mut rx) = mpsc::unbounded_channel::<Tick>();
    tokio::spawn(async move {
        if let Err(e) = grpc::run_grpc_feed(endpoint, x_token, tx).await {
            eprintln!("gRPC feed error: {e:#}");
        }
    });

    let mut events: Vec<ArbEvent> = Vec::new();
    let mut heartbeat = tokio::time::interval(Duration::from_secs(10));
    heartbeat.tick().await; // consume the immediate first tick
    let deadline = tokio::time::sleep(Duration::from_millis(run_ms));
    tokio::pin!(deadline);
    let (mut last_orca, mut last_ray, mut ticks) = (f64::NAN, f64::NAN, 0u64);

    loop {
        tokio::select! {
            _ = &mut deadline => break,
            _ = heartbeat.tick() => {
                let spread = if last_orca.is_finite() && last_ray.is_finite() {
                    format!("{:.1} bps", (last_ray - last_orca) / last_orca.min(last_ray) * 10_000.0)
                } else {
                    "n/a".to_string()
                };
                println!(
                    "[gRPC] ticks={ticks} Orca=${last_orca:.4} Raydium=${last_ray:.4} spread={spread} (arb>{}bps)",
                    detector.threshold_bps
                );
            }
            Some(t) = rx.recv() => {
                ticks += 1;
                if t.venue == "Orca" { last_orca = t.price; } else { last_ray = t.price; }
                match detector.on_tick(&t) {
                    TickResult::Open { net_bps } => {
                        println!("⚡ arb OPEN slot {} net {net_bps:.1}bps", t.slot);
                    }
                    TickResult::Close(ev) => {
                        println!(
                            "   closed {} slots / {}ms · peak {:.1}bps",
                            ev.lifetime_slots, ev.lifetime_ms, ev.peak_net_bps
                        );
                        events.push(ev);
                    }
                    TickResult::None => {}
                }
            }
        }
    }

    println!("\n──────── shadow report ({}s) ────────", run_ms / 1000);
    println!("gRPC ticks: {ticks}");
    println!("Real fee-adjusted arbs: {}", events.len());
    if events.is_empty() {
        println!("  none: pair arbed to within fees at this feed resolution.");
    } else {
        let slots: Vec<u128> = events.iter().map(|e| e.lifetime_slots as u128).collect();
        let ms: Vec<u128> = events.iter().map(|e| e.lifetime_ms).collect();
        let nets: Vec<f64> = events.iter().map(|e| e.peak_net_bps).collect();
        let max_net = nets.iter().cloned().fold(f64::MIN, f64::max);
        println!(
            "  peak net edge: median {:.1} bps, max {:.1} bps",
            median_f64(nets),
            max_net
        );
        println!(
            "  lifetime (reaction budget): median {} slots / {} ms",
            median_u128(slots),
            median_u128(ms)
        );
    }
    Ok(())
}
