//! Pyth Lazer ("Pyth Pro") price feed — a reconnecting WebSocket client that
//! streams live prices into a shared in-memory table. This is the fast trigger
//! + fair-value source for the liquidation engine: Lazer delivers ms-grade
//! updates so we can PRE-BUILD a liquidation the instant a price approaches the
//! threshold, then fire the moment it crosses.
//!
//! Auth: Bearer token via PYTH_LAZER_TOKEN (never hardcode; .env only).
//! Endpoint + subscribe shape are VERIFIED live against the SOL/USDC feeds.
//!
//! Feeds are numeric IDs (SOL=6, USDC=7, BTC=1, ETH=2). The parsed message
//! carries the raw integer price; the exponent is per-feed static (all the
//! crypto majors are -8). We request "exponent" as a property and fall back to
//! -8 if the server omits it.

use std::collections::HashMap;
use std::sync::{Arc, RwLock};
use std::time::Duration;

use futures::{SinkExt, StreamExt};
use tokio_tungstenite::tungstenite::{client::IntoClientRequest, Message};

/// Pyth Lazer WebSocket endpoint. Default is the Cloudflare-anycast host
/// (pyth-lazer.dourolabs.app) which routes to the nearest edge — measured ~2.5×
/// faster to connect than the pinned pyth-lazer-0 origin. Override with
/// LAZER_URL; measure `curl -w %{time_connect}` to each candidate from the box
/// and pick the lowest. (Also available: pyth-lazer-0/-1 direct origins.)
pub fn lazer_url() -> String {
    std::env::var("LAZER_URL").unwrap_or_else(|_| "wss://pyth-lazer.dourolabs.app/v1/stream".into())
}
const DEFAULT_EXPONENT: i32 = -8;

/// A single price observation, already scaled to a real number (price × 10^exp).
#[derive(Clone, Copy, Debug)]
pub struct PricePoint {
    pub price: f64,
    /// Lazer publish timestamp, microseconds since epoch (0 if absent).
    pub ts_us: u64,
}

/// Shared, lock-guarded map feed_id → latest price. Cheap to clone (Arc).
pub type PriceTable = Arc<RwLock<HashMap<u32, PricePoint>>>;

pub fn new_table() -> PriceTable {
    Arc::new(RwLock::new(HashMap::new()))
}

/// Read the latest price for a feed, if we've received one.
pub fn get(table: &PriceTable, feed_id: u32) -> Option<PricePoint> {
    table.read().ok()?.get(&feed_id).copied()
}

/// Spawn the Lazer feed as a background task. Reconnects forever on drop/error.
/// The returned table is updated in place as prices arrive.
pub fn spawn_lazer(token: String, feed_ids: Vec<u32>, table: PriceTable) {
    tokio::spawn(async move {
        let mut backoff_ms = 500u64;
        loop {
            match run(&token, &feed_ids, &table).await {
                Ok(()) => {
                    // Clean close — reconnect promptly.
                    backoff_ms = 500;
                }
                Err(e) => {
                    eprintln!("[pyth-lazer] {e}; reconnecting in {}ms", backoff_ms);
                }
            }
            tokio::time::sleep(Duration::from_millis(backoff_ms)).await;
            backoff_ms = (backoff_ms * 2).min(10_000);
        }
    });
}

/// One connection lifecycle: connect, subscribe, pump updates into the table.
/// Returns Ok on a clean server close, Err on any failure (→ reconnect).
async fn run(
    token: &str,
    feed_ids: &[u32],
    table: &PriceTable,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let mut req = lazer_url().into_client_request()?;
    req.headers_mut()
        .insert("Authorization", format!("Bearer {token}").parse()?);

    let (mut ws, _) = tokio_tungstenite::connect_async(req).await?;

    let sub = serde_json::json!({
        "type": "subscribe",
        "subscriptionId": 1,
        "priceFeedIds": feed_ids,
        "properties": ["price", "exponent"],
        "formats": [],
        "channel": "fixed_rate@50ms",
        "deliveryFormat": "json",
    });
    ws.send(Message::Text(sub.to_string())).await?;

    while let Some(msg) = ws.next().await {
        match msg? {
            Message::Text(t) => apply_update(&t, table),
            Message::Ping(p) => {
                ws.send(Message::Pong(p)).await?;
            }
            Message::Close(_) => return Ok(()),
            _ => {}
        }
    }
    Ok(())
}

/// Parse a Lazer text frame and update the table. Ignores non-price frames
/// (e.g. the initial {"type":"subscribed"} ack).
fn apply_update(text: &str, table: &PriceTable) {
    let Ok(v) = serde_json::from_str::<serde_json::Value>(text) else { return };
    let parsed = &v["parsed"];
    let Some(feeds) = parsed["priceFeeds"].as_array() else { return };

    // timestampUs may arrive as a string or a number depending on delivery.
    let ts_us = parsed["timestampUs"]
        .as_str()
        .and_then(|s| s.parse::<u64>().ok())
        .or_else(|| parsed["timestampUs"].as_u64())
        .unwrap_or(0);

    let Ok(mut w) = table.write() else { return };
    for f in feeds {
        let Some(id) = f["priceFeedId"].as_u64() else { continue };
        // price arrives as a string integer (may also be numeric).
        let raw = f["price"]
            .as_str()
            .and_then(|s| s.parse::<f64>().ok())
            .or_else(|| f["price"].as_f64());
        let Some(raw) = raw else { continue };
        let exp = f["exponent"].as_i64().map(|e| e as i32).unwrap_or(DEFAULT_EXPONENT);
        let price = raw * 10f64.powi(exp);
        w.insert(id as u32, PricePoint { price, ts_us });
    }
}
