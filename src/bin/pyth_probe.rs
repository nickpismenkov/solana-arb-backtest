//! Pyth Lazer feed probe — connects with PYTH_LAZER_TOKEN, subscribes to a few
//! feeds, and prints live prices from the shared table for ~10s. Confirms the
//! Rust feed module works end-to-end (auth, subscribe, parse, scale).
//!
//! Usage: PYTH_LAZER_TOKEN=<key> [FEED_IDS=6,7] cargo run --release --bin pyth_probe

use arb_engine::pyth;
use std::time::Duration;

#[tokio::main]
async fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();

    let token = std::env::var("PYTH_LAZER_TOKEN").expect("PYTH_LAZER_TOKEN (.env)");
    let feed_ids: Vec<u32> = std::env::var("FEED_IDS")
        .unwrap_or_else(|_| "6,7".into())
        .split(',')
        .filter_map(|s| s.trim().parse().ok())
        .collect();

    let names: std::collections::HashMap<u32, &str> =
        [(1u32, "BTC"), (2, "ETH"), (6, "SOL"), (7, "USDC")].into_iter().collect();

    eprintln!("[pyth_probe] subscribing to feeds {feed_ids:?} …");
    let table = pyth::new_table();
    pyth::spawn_lazer(token, feed_ids.clone(), table.clone());

    for tick in 0..10 {
        tokio::time::sleep(Duration::from_millis(1000)).await;
        let mut line = format!("t+{tick}s  ");
        for id in &feed_ids {
            match pyth::get(&table, *id) {
                Some(p) => line.push_str(&format!(
                    "{}({id})=${:.4} [{}µs]  ",
                    names.get(id).unwrap_or(&"?"),
                    p.price,
                    p.ts_us
                )),
                None => line.push_str(&format!("{}({id})=…  ", names.get(id).unwrap_or(&"?"))),
            }
        }
        println!("{line}");
    }
    eprintln!("[pyth_probe] done");
}
