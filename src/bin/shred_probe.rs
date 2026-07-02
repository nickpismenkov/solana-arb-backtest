//! Standalone ShredStream feed probe — run on the co-located box to confirm
//! the fast feed is live and hitting our pools before wiring it into the
//! shadow harness. `RUN_MS=60000 cargo run --release --bin shred_probe`.

use std::time::Duration;
use tokio::sync::mpsc;

#[tokio::main]
async fn main() {
    let _ = dotenvy::dotenv();
    let port: u16 = std::env::var("SHREDSTREAM_PORT")
        .ok()
        .and_then(|s| s.parse().ok())
        .unwrap_or(20000);
    let run_ms: u64 = std::env::var("RUN_MS")
        .ok()
        .and_then(|s| s.parse().ok())
        .unwrap_or(60_000);
    println!("shred-probe — listening udp/{port} for {}s…\n", run_ms / 1000);

    let (tx, mut rx) = mpsc::unbounded_channel();
    let _handle = arb_engine::shredstream::run_shredstream_feed(port, tx);

    let mut count: u64 = 0;
    let deadline = tokio::time::sleep(Duration::from_millis(run_ms));
    tokio::pin!(deadline);
    loop {
        tokio::select! {
            _ = &mut deadline => break,
            Some(t) = rx.recv() => {
                count += 1;
                if count <= 20 || count % 100 == 0 {
                    let sig = &t.sig[..t.sig.len().min(8)];
                    println!("trigger #{count} {} slot {} sig {sig}…", t.venue, t.slot);
                }
            }
        }
    }
    println!("\nshred-probe: {count} pool triggers in {}s", run_ms / 1000);
}
