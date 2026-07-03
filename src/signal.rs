//! Hot-path signal layer. A lock-free price cache (updated by a background gRPC
//! task) and a pure local edge calc — so the reaction path reads memory and
//! does arithmetic only, never RPC. The on-chain exact-out guard is the real
//! safety; this is just the go/no-go heuristic that keeps us from blind-firing
//! (and picks the direction). Prices here are gRPC/Turbine-lagged by design —
//! acceptable for a heuristic, backstopped by the guard.

use std::sync::atomic::{AtomicU64, Ordering};

/// Lock-free latest prices (quote per base) for both venues. f64 stored as bits;
/// reads are a relaxed atomic load (nanoseconds), safe in the hot path.
pub struct PriceCache {
    orca_bits: AtomicU64,
    orca_slot: AtomicU64,
    ray_bits: AtomicU64,
    ray_slot: AtomicU64,
}

impl Default for PriceCache {
    fn default() -> Self {
        Self {
            orca_bits: AtomicU64::new(0),
            orca_slot: AtomicU64::new(0),
            ray_bits: AtomicU64::new(0),
            ray_slot: AtomicU64::new(0),
        }
    }
}

impl PriceCache {
    pub fn set_orca(&self, price: f64, slot: u64) {
        self.orca_bits.store(price.to_bits(), Ordering::Relaxed);
        self.orca_slot.store(slot, Ordering::Relaxed);
    }
    pub fn set_ray(&self, price: f64, slot: u64) {
        self.ray_bits.store(price.to_bits(), Ordering::Relaxed);
        self.ray_slot.store(slot, Ordering::Relaxed);
    }
    /// (orca_price, ray_price, orca_slot, ray_slot). Prices are NaN until seeded.
    pub fn get(&self) -> (f64, f64, u64, u64) {
        (
            f64::from_bits(self.orca_bits.load(Ordering::Relaxed)),
            f64::from_bits(self.ray_bits.load(Ordering::Relaxed)),
            self.orca_slot.load(Ordering::Relaxed),
            self.ray_slot.load(Ordering::Relaxed),
        )
    }
}

/// Local round-trip edge estimate. Returns (orca_first, edge_bps) for the more
/// profitable direction. orca_first=true means buy base on Orca, sell on Ray.
/// First-order (ignores price impact) — a go/no-go heuristic; the guard handles
/// the exact economics on chain.
pub fn local_edge(orca_price: f64, ray_price: f64, orca_fee_bps: f64, ray_fee_bps: f64) -> (bool, f64) {
    if !(orca_price.is_finite() && ray_price.is_finite()) || orca_price <= 0.0 || ray_price <= 0.0 {
        return (true, f64::NEG_INFINITY);
    }
    let keep = (1.0 - orca_fee_bps / 10_000.0) * (1.0 - ray_fee_bps / 10_000.0);
    // orca_first: buy base on Orca (cost orca_price), sell on Ray (recv ray_price).
    let edge_of = (ray_price / orca_price * keep - 1.0) * 10_000.0;
    // ray_first: buy base on Ray, sell on Orca.
    let edge_rf = (orca_price / ray_price * keep - 1.0) * 10_000.0;
    if edge_of >= edge_rf {
        (true, edge_of)
    } else {
        (false, edge_rf)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn no_spread_is_negative_after_fees() {
        // Equal prices, 4+4bp fees → round trip loses ~8bp.
        let (_, edge) = local_edge(100.0, 100.0, 4.0, 4.0);
        assert!(edge < 0.0 && edge > -10.0, "edge {edge}");
    }

    #[test]
    fn ray_higher_favors_orca_first() {
        // Ray pays more for base → buy Orca, sell Ray.
        let (orca_first, edge) = local_edge(100.0, 100.5, 1.0, 1.0);
        assert!(orca_first);
        assert!(edge > 0.0, "edge {edge}");
    }

    #[test]
    fn orca_higher_favors_ray_first() {
        let (orca_first, edge) = local_edge(100.5, 100.0, 1.0, 1.0);
        assert!(!orca_first);
        assert!(edge > 0.0, "edge {edge}");
    }

    #[test]
    fn cache_roundtrips() {
        let c = PriceCache::default();
        c.set_orca(123.45, 10);
        c.set_ray(543.21, 11);
        let (o, r, os, rs) = c.get();
        assert_eq!(o, 123.45);
        assert_eq!(r, 543.21);
        assert_eq!((os, rs), (10, 11));
    }
}
