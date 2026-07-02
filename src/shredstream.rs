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

use crate::pools::{ORCA_POOL, RAY_CLMM_POOL};

#[derive(Clone, Debug)]
pub struct Trigger {
    pub venue: &'static str,
    pub slot: u64,
    pub ts_ms: u128, // stamped at receipt, before downstream work
    pub sig: String,
}

fn now_ms() -> u128 {
    SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_millis()
}

/// Spawns the blocking ShredStream listener on its own thread. Returns the
/// join handle. Logs a `txns seen / pool-hits` heartbeat every ~10s.
pub fn run_shredstream_feed(port: u16, tx: UnboundedSender<Trigger>) -> std::thread::JoinHandle<()> {
    let orca = bs58::decode(ORCA_POOL).into_vec().unwrap();
    let ray = bs58::decode(RAY_CLMM_POOL).into_vec().unwrap();

    std::thread::spawn(move || {
        let mut listener = match shredstream::ShredListener::bind(port) {
            Ok(l) => l,
            Err(e) => {
                eprintln!("[shredstream] bind udp/{port} failed: {e}");
                return;
            }
        };
        eprintln!("[shredstream] listening on udp/{port}");
        let (mut seen, mut hits) = (0u64, 0u64);
        let mut last_hb = Instant::now();

        for (slot, txns) in listener.transactions() {
            let ts_ms = now_ms();
            for txn in &txns {
                seen += 1;
                let keys = txn.message.static_account_keys();
                let venue = if keys.iter().any(|k| k.as_ref() == orca.as_slice()) {
                    "Orca"
                } else if keys.iter().any(|k| k.as_ref() == ray.as_slice()) {
                    "Raydium"
                } else {
                    continue;
                };
                hits += 1;
                let sig = txn
                    .signatures
                    .first()
                    .map(|s| s.to_string())
                    .unwrap_or_default();
                let _ = tx.send(Trigger {
                    venue,
                    slot,
                    ts_ms,
                    sig,
                });
            }
            if last_hb.elapsed().as_secs() >= 10 {
                eprintln!("[shredstream] txns seen={seen} pool-hits={hits}");
                last_hb = Instant::now();
            }
        }
    })
}
