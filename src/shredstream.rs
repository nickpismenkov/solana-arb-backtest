//! Leg-1 ShredStream trigger feed (shredstream.com pure-Rust SDK). Emits a
//! Trigger the instant a swap touches one of our pools = how fast WE'd see the
//! dislocating swap. The `.transactions()` iterator is blocking, so it runs on
//! its own OS thread and sends Triggers over a channel; prices/arb come from
//! the gRPC (later: shred-sourced) feed and the harness correlates the two.
//!
//! NOTE: matches pools via static account keys — v0 txns that reference a pool
//! only through an Address Lookup Table won't match until ALT resolution lands
//! (watch the pool-hit heartbeat vs txns-seen to gauge the gap).

use std::time::{Instant, SystemTime, UNIX_EPOCH};
use tokio::sync::mpsc::UnboundedSender;

use crate::decode::AltCache;
use crate::pools::pair;
use solana_pubkey::Pubkey;
use std::str::FromStr;

#[derive(Clone, Debug)]
pub struct Trigger {
    pub venue: &'static str,
    pub slot: u64,
    pub ts_ms: u128, // stamped at receipt, before downstream work
    pub sig: String,
    pub raw: Vec<u8>, // serialized victim tx (for simulate/backrun); empty if unused
}

fn now_ms() -> u128 {
    SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_millis()
}

/// Spawns the blocking ShredStream listener on its own thread. Returns the
/// join handle. Logs a `txns seen / pool-hits` heartbeat every ~10s.
///
/// If `rpc` is Some, ALTs are resolved so pool touches referenced via lookup
/// tables are caught too (essential — ~all routed swaps use ALTs). If None,
/// falls back to a static-key match (misses routed swaps).
pub fn run_shredstream_feed(
    port: u16,
    rpc: Option<String>,
    tx: UnboundedSender<Trigger>,
) -> std::thread::JoinHandle<()> {
    let pools = [
        (Pubkey::from_str(&pair().orca_pool).expect("bad ORCA_POOL"), "Orca"),
        (Pubkey::from_str(&pair().ray_pool).expect("bad RAY_CLMM_POOL"), "Raydium"),
    ];

    std::thread::spawn(move || {
        let mut alt = rpc.map(AltCache::new);
        let mut listener = match shredstream::ShredListener::bind(port) {
            Ok(l) => l,
            Err(e) => {
                eprintln!("[shredstream] bind udp/{port} failed: {e}");
                return;
            }
        };
        eprintln!(
            "[shredstream] listening on udp/{port} (ALT resolution: {})",
            if alt.is_some() { "on" } else { "off (static-key only)" }
        );
        let (mut seen, mut hits) = (0u64, 0u64);
        let mut last_hb = Instant::now();

        for (slot, txns) in listener.transactions() {
            let ts_ms = now_ms();
            for txn in &txns {
                seen += 1;
                let venue = match alt.as_mut() {
                    Some(cache) => cache.touches_pool(&txn.message, &pools),
                    None => {
                        let keys = txn.message.static_account_keys();
                        pools
                            .iter()
                            .find(|(p, _)| keys.iter().any(|k| k == p))
                            .map(|(_, v)| *v)
                    }
                };
                let Some(venue) = venue else {
                    continue;
                };
                hits += 1;
                let sig = txn
                    .signatures
                    .first()
                    .map(|s| s.to_string())
                    .unwrap_or_default();
                let raw = bincode::serialize(txn).unwrap_or_default();
                let _ = tx.send(Trigger {
                    venue,
                    slot,
                    ts_ms,
                    sig,
                    raw,
                });
            }
            if last_hb.elapsed().as_secs() >= 10 {
                eprintln!("[shredstream] txns seen={seen} pool-hits={hits}");
                last_hb = Instant::now();
            }
        }
    })
}
