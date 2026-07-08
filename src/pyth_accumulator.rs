//! Parse Pyth's Hermes "accumulator update" blob into the pieces the on-chain
//! crank consumes: the Wormhole VAA (guardian-signed merkle root) and, per
//! feed, the price MESSAGE + its merkle PROOF. This is the data layer of the
//! self-crank pipeline (step 3): Hermes gives us the signed update; the VAA
//! goes to the Wormhole-verify program, and each {message, proof} goes to the
//! Pyth push wrapper which writes the sponsored feed marginfi reads.
//!
//! Format (AccumulatorUpdateData, VERIFIED against a live SOL update — the split
//! matches the ~247B VAA / ~396B per-update seen in a real mainnet crank tx):
//!   magic "PNAU" (0x504e4155) · major u8 · minor u8 ·
//!   trailing_hdr_len u8 + that many bytes ·
//!   update_type u8 (0 = WormholeMerkle) ·
//!   vaa_len u16 (BE) + vaa bytes ·
//!   num_updates u8 ·
//!   [ message_len u16 (BE) + message · proof (merkle path: u8 count + count×20B) ] × num_updates
//!
//! The 32-byte feed id lives inside each message (a PriceFeedMessage: variant
//! u8=0, then feed_id[32], …) so we can match the update to a bank's feed.

use anyhow::{anyhow, bail, Result};

const MAGIC: u32 = 0x504e_4155; // "PNAU"

#[derive(Clone, Debug)]
pub struct MerkleUpdate {
    /// The price message (PriceFeedMessage bytes) — variant byte then feed_id@1.
    pub message: Vec<u8>,
    /// Merkle proof path: a length-prefixed list of 20-byte hashes, kept as the
    /// raw wire bytes (count u8 + count×20) so it re-serializes verbatim.
    pub proof: Vec<u8>,
}

impl MerkleUpdate {
    /// 32-byte Pyth feed id (offset 1: after the 1-byte message variant).
    pub fn feed_id(&self) -> Option<[u8; 32]> {
        self.message.get(1..33)?.try_into().ok()
    }
}

#[derive(Clone, Debug)]
pub struct AccumulatorUpdate {
    /// Guardian-signed Wormhole VAA (goes to the verify program).
    pub vaa: Vec<u8>,
    pub updates: Vec<MerkleUpdate>,
}

struct Cur<'a> { b: &'a [u8], i: usize }
impl<'a> Cur<'a> {
    fn u8(&mut self) -> Result<u8> {
        let v = *self.b.get(self.i).ok_or_else(|| anyhow!("eof u8"))?; self.i += 1; Ok(v)
    }
    fn u16be(&mut self) -> Result<usize> {
        let hi = self.u8()? as usize; let lo = self.u8()? as usize; Ok((hi << 8) | lo)
    }
    fn take(&mut self, n: usize) -> Result<&'a [u8]> {
        let s = self.b.get(self.i..self.i + n).ok_or_else(|| anyhow!("eof take {n}"))?;
        self.i += n; Ok(s)
    }
}

/// Parse one base64 accumulator blob (as Hermes returns in binary.data[0]).
pub fn parse_base64(b64: &str) -> Result<AccumulatorUpdate> {
    use base64::Engine;
    let bytes = base64::engine::general_purpose::STANDARD.decode(b64)?;
    parse(&bytes)
}

pub fn parse(bytes: &[u8]) -> Result<AccumulatorUpdate> {
    let mut c = Cur { b: bytes, i: 0 };
    let magic = u32::from_be_bytes(c.take(4)?.try_into().unwrap());
    if magic != MAGIC { bail!("bad magic {magic:#x}"); }
    let _major = c.u8()?;
    let _minor = c.u8()?;
    let trailing = c.u8()? as usize;
    c.take(trailing)?; // skip trailing header
    let update_type = c.u8()?;
    if update_type != 0 { bail!("unsupported update_type {update_type}"); }
    let vaa_len = c.u16be()?;
    let vaa = c.take(vaa_len)?.to_vec();
    let num = c.u8()? as usize;
    let mut updates = Vec::with_capacity(num);
    for _ in 0..num {
        let msg_len = c.u16be()?;
        let message = c.take(msg_len)?.to_vec();
        // proof = count u8 + count × 20-byte hashes (kept as raw wire bytes).
        let proof_start = c.i;
        let count = c.u8()? as usize;
        c.take(count * 20)?;
        let proof = bytes[proof_start..c.i].to_vec();
        updates.push(MerkleUpdate { message, proof });
    }
    Ok(AccumulatorUpdate { vaa, updates })
}

/// Fetch the latest signed update for a set of hex feed ids from Hermes.
pub fn fetch_hermes(hermes: &str, feed_ids_hex: &[&str]) -> Result<AccumulatorUpdate> {
    let ids: String = feed_ids_hex.iter().map(|f| format!("&ids[]={f}")).collect();
    let url = format!("{hermes}/v2/updates/price/latest?encoding=base64{ids}");
    let v: serde_json::Value = ureq::get(&url).call()?.into_json()?;
    let b64 = v["binary"]["data"][0].as_str().ok_or_else(|| anyhow!("no binary.data: {v}"))?;
    parse_base64(b64)
}

// ── Hermes cache: keep the latest signed blob hot for the fire path ─────────

use std::sync::{Arc, RwLock};
use std::time::{Duration, Instant};

/// Latest Hermes blob for a (settable) feed set, refreshed by a background
/// thread so the crank fire path never waits on an HTTP round-trip. One blob
/// covers the whole set: a single VAA proves every update in it.
#[derive(Clone)]
pub struct HermesCache {
    latest: Arc<RwLock<Option<(AccumulatorUpdate, Instant)>>>,
    feeds: Arc<RwLock<Vec<String>>>,
}

impl HermesCache {
    /// Latest parsed blob and its age. None until the first successful fetch.
    pub fn latest(&self) -> Option<(AccumulatorUpdate, Duration)> {
        self.latest.read().ok()?.as_ref().map(|(u, t)| (u.clone(), t.elapsed()))
    }

    /// The update for one feed from the latest blob, with the blob's age.
    pub fn update_for(&self, feed_id: &[u8; 32]) -> Option<(MerkleUpdate, Vec<u8>, Duration)> {
        let (blob, age) = self.latest()?;
        let mu = blob.updates.iter().find(|u| u.feed_id().as_ref() == Some(feed_id))?.clone();
        Some((mu, blob.vaa, age))
    }

    /// Replace the polled feed set (hex ids); takes effect next poll.
    pub fn set_feeds(&self, feed_ids_hex: Vec<String>) {
        if let Ok(mut f) = self.feeds.write() { *f = feed_ids_hex; }
    }
}

/// Spawn the poll thread. Errors are retried on the next tick; the cache keeps
/// the last good blob (callers gate on age).
pub fn spawn_hermes_cache(hermes: String, feed_ids_hex: Vec<String>, interval: Duration) -> HermesCache {
    let cache = HermesCache {
        latest: Arc::new(RwLock::new(None)),
        feeds: Arc::new(RwLock::new(feed_ids_hex)),
    };
    let c = cache.clone();
    std::thread::spawn(move || loop {
        let feeds: Vec<String> = c.feeds.read().map(|f| f.clone()).unwrap_or_default();
        if !feeds.is_empty() {
            let refs: Vec<&str> = feeds.iter().map(|s| s.as_str()).collect();
            match fetch_hermes(&hermes, &refs) {
                Ok(u) => { if let Ok(mut w) = c.latest.write() { *w = Some((u, Instant::now())); } }
                Err(e) => eprintln!("[hermes] fetch failed (will retry): {e}"),
            }
        }
        std::thread::sleep(interval);
    });
    cache
}
