// Package signal is the hot-path signal layer. A lock-free price cache
// (updated by a background gRPC task) and a pure local edge calc — so the
// reaction path reads memory and does arithmetic only, never RPC. The
// on-chain exact-out guard is the real safety; this is just the go/no-go
// heuristic that keeps us from blind-firing (and picks the direction).
// Prices here are gRPC/Turbine-lagged by design — acceptable for a
// heuristic, backstopped by the guard.
package signal

import (
	"math"
	"sync/atomic"
)

// PriceCache holds lock-free latest prices (quote per base) for both
// venues. f64 stored as bits; reads are a relaxed atomic load
// (nanoseconds), safe in the hot path.
type PriceCache struct {
	orcaBits atomic.Uint64
	orcaSlot atomic.Uint64
	rayBits  atomic.Uint64
	raySlot  atomic.Uint64
}

func (c *PriceCache) SetOrca(price float64, slot uint64) {
	c.orcaBits.Store(math.Float64bits(price))
	c.orcaSlot.Store(slot)
}

func (c *PriceCache) SetRay(price float64, slot uint64) {
	c.rayBits.Store(math.Float64bits(price))
	c.raySlot.Store(slot)
}

// Get returns (orca_price, ray_price, orca_slot, ray_slot). Prices are NaN
// until seeded.
func (c *PriceCache) Get() (float64, float64, uint64, uint64) {
	return math.Float64frombits(c.orcaBits.Load()),
		math.Float64frombits(c.rayBits.Load()),
		c.orcaSlot.Load(),
		c.raySlot.Load()
}

// LocalEdge is a local round-trip edge estimate. Returns (orcaFirst,
// edgeBps) for the more profitable direction. orcaFirst=true means buy
// base on Orca, sell on Ray. First-order (ignores price impact) — a
// go/no-go heuristic; the guard handles the exact economics on chain.
func LocalEdge(orcaPrice, rayPrice, orcaFeeBps, rayFeeBps float64) (bool, float64) {
	if !isFinite(orcaPrice) || !isFinite(rayPrice) || orcaPrice <= 0.0 || rayPrice <= 0.0 {
		return true, math.Inf(-1)
	}
	keep := (1.0 - orcaFeeBps/10_000.0) * (1.0 - rayFeeBps/10_000.0)
	// orcaFirst: buy base on Orca (cost orcaPrice), sell on Ray (recv rayPrice).
	edgeOf := (rayPrice/orcaPrice*keep - 1.0) * 10_000.0
	// rayFirst: buy base on Ray, sell on Orca.
	edgeRf := (orcaPrice/rayPrice*keep - 1.0) * 10_000.0
	if edgeOf >= edgeRf {
		return true, edgeOf
	}
	return false, edgeRf
}

func isFinite(x float64) bool {
	return !math.IsNaN(x) && !math.IsInf(x, 0)
}
