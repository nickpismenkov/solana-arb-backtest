//! pump_collect — real-time, read-only recorder of every pump.fun bonding-curve
//! event (token create / buy / sell / migrate) to `runs/pump/events.jsonl`.
//!
//! ── Transport ────────────────────────────────────────────────────────────────
//! **Helius WebSocket `logsSubscribe`** with a `mentions:[pump_program]` filter,
//! at `processed` commitment. Chosen because:
//!   * It is real-time (sub-second) and works with the standard Helius API key in
//!     `.env` (`HELIUS_RPC` → derive the `wss://` host). No gRPC/Laserstream add-on
//!     or separate credential is required. (The repo's `GRPC_ENDPOINT` is a Tatum
//!     gateway, not a Helius Yellowstone stream, so we do not depend on it here.)
//!   * pump emits every event as an anchor self-CPI **`Program data:` log blob**,
//!     so the log stream alone carries the full structured payload (mint, amounts,
//!     reserves, dev, …) — no second `getTransaction` round-trip, hence lowest
//!     latency. See `arb_engine::pump` for the verified layouts.
//!
//! Trade-off / caveat: `logsSubscribe` can, under load, drop or lag; it is the
//! standard-tier tool though, and for a measurement collector completeness is
//! "best effort, high coverage" rather than "every single tx". If a run needs
//! guaranteed completeness, back-fill later with getSignaturesForAddress. This
//! binary favours latency + simplicity, which is what Phase-1 recon needs.
//!
//! Robustness: reconnects with capped backoff on any drop, flushes each event to
//! disk immediately, prints a heartbeat every 10s (events/sec, launches seen,
//! migrations seen). It NEVER signs or submits a transaction.
//!
//! Usage: `HELIUS_RPC=<url> cargo run --release --bin pump_collect`
//!        env: PUMP_WS (override ws url), PUMP_OUT (override output path).

use std::io::Write;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use arb_engine::pump::{bonding_curve_pda, PumpEvent, PUMP_PROGRAM};
use futures::{SinkExt, StreamExt};
use tokio_tungstenite::tungstenite::{client::IntoClientRequest, Message};

fn now_ms() -> u128 {
    SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_millis()
}

/// Derive the Helius `wss://` endpoint from the `https://` RPC url (keeps the
/// `?api-key=…`). Override entirely with `PUMP_WS`.
fn ws_url() -> String {
    if let Ok(u) = std::env::var("PUMP_WS") {
        return u;
    }
    let http = std::env::var("HELIUS_RPC")
        .or_else(|_| std::env::var("RPC_HTTP"))
        .expect("set HELIUS_RPC (or RPC_HTTP)");
    http.replacen("https://", "wss://", 1)
        .replacen("http://", "ws://", 1)
}

#[derive(Default)]
struct Counts {
    total: u64,
    creates: u64,
    buys: u64,
    sells: u64,
    migrates: u64,
}

#[tokio::main]
async fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();

    let out_path = std::env::var("PUMP_OUT").unwrap_or_else(|_| "runs/pump/events.jsonl".into());
    if let Some(dir) = std::path::Path::new(&out_path).parent() {
        std::fs::create_dir_all(dir).expect("create runs/pump");
    }
    let mut file = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(&out_path)
        .expect("open events.jsonl");

    eprintln!("[pump_collect] program {PUMP_PROGRAM}");
    eprintln!("[pump_collect] appending events to {out_path}");
    eprintln!("[pump_collect] transport: Helius WebSocket logsSubscribe (read-only)");

    let mut counts = Counts::default();
    let mut backoff = Duration::from_millis(500);

    loop {
        match run_once(&mut file, &mut counts).await {
            Ok(()) => {
                eprintln!("[pump_collect] stream closed cleanly; reconnecting");
                backoff = Duration::from_millis(500);
            }
            Err(e) => {
                eprintln!("[pump_collect] error: {e}; reconnecting in {:?}", backoff);
                tokio::time::sleep(backoff).await;
                backoff = (backoff * 2).min(Duration::from_secs(15));
            }
        }
    }
}

/// One connection lifecycle: connect, subscribe, drain notifications until the
/// socket drops. Returns Ok on clean close, Err on any failure (→ reconnect).
async fn run_once(
    file: &mut std::fs::File,
    counts: &mut Counts,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let req = ws_url().into_client_request()?;
    let (mut ws, _) = tokio_tungstenite::connect_async(req).await?;

    let sub = serde_json::json!({
        "jsonrpc": "2.0",
        "id": 1,
        "method": "logsSubscribe",
        "params": [
            { "mentions": [ PUMP_PROGRAM ] },
            { "commitment": "processed" }
        ]
    });
    ws.send(Message::Text(sub.to_string())).await?;
    eprintln!("[pump_collect] subscribed; waiting for events…");

    let mut hb = tokio::time::interval(Duration::from_secs(10));
    hb.tick().await; // consume the immediate first tick
    let mut last_total = counts.total;
    let mut last_at = Instant::now();

    loop {
        tokio::select! {
            _ = hb.tick() => {
                let dt = last_at.elapsed().as_secs_f64().max(1e-9);
                let rate = (counts.total - last_total) as f64 / dt;
                eprintln!(
                    "[pump_collect] hb: {:.0} ev/s | total {} (create {}, buy {}, sell {}, migrate {})",
                    rate, counts.total, counts.creates, counts.buys, counts.sells, counts.migrates
                );
                last_total = counts.total;
                last_at = Instant::now();
            }
            msg = ws.next() => {
                let Some(msg) = msg else { return Ok(()) };
                match msg? {
                    Message::Text(t) => handle_notification(&t, file, counts),
                    Message::Ping(p) => { ws.send(Message::Pong(p)).await?; }
                    Message::Close(_) => return Ok(()),
                    _ => {}
                }
            }
        }
    }
}

/// Parse one `logsNotification` frame, decode any pump event blobs in it, and
/// append a JSONL record per decoded event.
fn handle_notification(text: &str, file: &mut std::fs::File, counts: &mut Counts) {
    let Ok(v) = serde_json::from_str::<serde_json::Value>(text) else { return };
    let result = &v["params"]["result"];
    if result.is_null() {
        return; // subscription ack or unrelated frame
    }
    let slot = result["context"]["slot"].as_u64().unwrap_or(0);
    let value = &result["value"];
    // Skip failed txs — a reverted tx's event logs would be misleading.
    if !value["err"].is_null() {
        return;
    }
    let signature = value["signature"].as_str().unwrap_or("").to_string();
    let Some(logs) = value["logs"].as_array() else { return };

    let ts = now_ms();
    for line in logs {
        let Some(s) = line.as_str() else { continue };
        let Some(b64) = s.strip_prefix("Program data: ") else { continue };
        let Some(ev) = arb_engine::pump::parse_program_data_b64(b64) else { continue };
        let rec = to_record(&ev, ts, slot, &signature);
        counts.total += 1;
        match ev.kind() {
            "create" => counts.creates += 1,
            "buy" => counts.buys += 1,
            "sell" => counts.sells += 1,
            "migrate" => counts.migrates += 1,
            _ => {}
        }
        let _ = writeln!(file, "{}", rec);
    }
    let _ = file.flush();
}

/// Build the JSONL record for one event. Fields common to all: unix_ms, slot,
/// signature, event_type, mint, bonding_curve, actor, sol_amount, token_amount.
fn to_record(ev: &PumpEvent, ts: u128, slot: u64, sig: &str) -> serde_json::Value {
    use serde_json::json;
    let mut rec = json!({
        "unix_ms": ts,
        "slot": slot,
        "signature": sig,
        "event_type": ev.kind(),
    });
    let obj = rec.as_object_mut().unwrap();
    match ev {
        PumpEvent::Create(c) => {
            obj.insert("mint".into(), json!(c.mint.to_string()));
            obj.insert("bonding_curve".into(), json!(c.bonding_curve.to_string()));
            obj.insert("actor".into(), json!(c.user.to_string()));
            obj.insert("dev".into(), json!(c.user.to_string()));
            obj.insert("creator".into(), json!(c.creator.to_string()));
            obj.insert("block_time".into(), json!(c.timestamp));
            obj.insert("name".into(), json!(c.name));
            obj.insert("symbol".into(), json!(c.symbol));
            obj.insert("uri".into(), json!(c.uri));
            obj.insert("sol_amount".into(), json!(0));
            obj.insert("token_amount".into(), json!(0));
            obj.insert("init_virtual_sol_reserves".into(), json!(c.virtual_sol_reserves));
            obj.insert("init_virtual_token_reserves".into(), json!(c.virtual_token_reserves));
            obj.insert("init_real_token_reserves".into(), json!(c.real_token_reserves));
            obj.insert("token_total_supply".into(), json!(c.token_total_supply));
        }
        PumpEvent::Trade(t) => {
            obj.insert("mint".into(), json!(t.mint.to_string()));
            obj.insert("bonding_curve".into(), json!(bonding_curve_pda(&t.mint).to_string()));
            obj.insert("actor".into(), json!(t.user.to_string()));
            if let Some(c) = t.creator {
                obj.insert("creator".into(), json!(c.to_string()));
            }
            obj.insert("block_time".into(), json!(t.timestamp));
            obj.insert("sol_amount".into(), json!(t.sol_amount));
            obj.insert("token_amount".into(), json!(t.token_amount));
            obj.insert("virtual_sol_reserves".into(), json!(t.virtual_sol_reserves));
            obj.insert("virtual_token_reserves".into(), json!(t.virtual_token_reserves));
            obj.insert("real_sol_reserves".into(), json!(t.real_sol_reserves));
            obj.insert("real_token_reserves".into(), json!(t.real_token_reserves));
            obj.insert("price_in_sol".into(), json!(t.price_in_sol()));
        }
        PumpEvent::Migrate(m) => {
            obj.insert("mint".into(), json!(m.mint.to_string()));
            obj.insert("bonding_curve".into(), json!(bonding_curve_pda(&m.mint).to_string()));
            obj.insert("actor".into(), json!(arb_engine::pump::MIGRATION_AUTHORITY));
            obj.insert("sol_amount".into(), json!(0));
            obj.insert("token_amount".into(), json!(0));
        }
    }
    rec
}
