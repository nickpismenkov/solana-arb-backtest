// Package detector is a feed-agnostic fee-adjusted arb detector — ported
// directly from the Rust/TS versions. Feed it price ticks from any source
// (gRPC-poll now, ShredStream later) and it emits arb open/close events with
// lifetime (slots + ms) = the reaction budget. A "real" arb requires the
// cross-venue gap to exceed the SUM of both fees.
package detector

import "sort"

// Tick is one price observation for a venue.
type Tick struct {
	Venue string
	Price float64 // quote per base (USDC per SOL)
	Slot  uint64
	TsMs  uint64
}

// ArbEvent is a closed (buy_venue, sell_venue) opportunity window.
type ArbEvent struct {
	BuyVenue      string
	SellVenue     string
	OpenSlot      uint64
	CloseSlot     uint64
	LifetimeSlots uint64
	LifetimeMs    uint64
	PeakNetBps    float64
}

// TickResultKind discriminates the outcome of Detector.OnTick.
type TickResultKind int

const (
	TickNone TickResultKind = iota
	TickOpen
	TickClose
)

// TickResult is the outcome of feeding one Tick to the detector.
type TickResult struct {
	Kind   TickResultKind
	NetBps float64  // valid when Kind == TickOpen
	Event  ArbEvent // valid when Kind == TickClose
}

type openState struct {
	buy, sell  string
	openSlot   uint64
	openTsMs   uint64
	peakNetBps float64
	lastSlot   uint64
	lastTsMs   uint64
}

// Detector tracks two venues' latest prices and reports fee-adjusted arb
// windows as ticks arrive.
type Detector struct {
	venueA, venueB string
	ThresholdBps   float64
	lastA, lastB   *float64
	open           *openState
}

// New creates a Detector for venues a/b with per-venue fees (their sum is
// the profitability threshold).
func New(a, b string, feeA, feeB float64) *Detector {
	return &Detector{venueA: a, venueB: b, ThresholdBps: feeA + feeB}
}

// OnTick feeds one price tick and returns the resulting event, if any.
func (d *Detector) OnTick(t Tick) TickResult {
	if t.Venue == d.venueA {
		p := t.Price
		d.lastA = &p
	} else if t.Venue == d.venueB {
		p := t.Price
		d.lastB = &p
	}
	if d.lastA == nil || d.lastB == nil {
		return TickResult{Kind: TickNone}
	}
	pa, pb := *d.lastA, *d.lastB

	signedGapBps := ((pb - pa) / pa) * 10_000.0 // + => B dearer
	var net float64
	var buy, sell string
	switch {
	case signedGapBps > d.ThresholdBps:
		net, buy, sell = signedGapBps-d.ThresholdBps, d.venueA, d.venueB
	case signedGapBps < -d.ThresholdBps:
		net, buy, sell = -signedGapBps-d.ThresholdBps, d.venueB, d.venueA
	}

	if net > 0.0 {
		if d.open == nil {
			d.open = &openState{buy: buy, sell: sell, openSlot: t.Slot, openTsMs: t.TsMs, peakNetBps: net, lastSlot: t.Slot, lastTsMs: t.TsMs}
			return TickResult{Kind: TickOpen, NetBps: net}
		}
		if net > d.open.peakNetBps {
			d.open.peakNetBps = net
		}
		d.open.lastSlot = t.Slot
		d.open.lastTsMs = t.TsMs
		return TickResult{Kind: TickNone}
	}

	if d.open != nil {
		o := d.open
		d.open = nil
		closeSlot := o.lastSlot
		if t.Slot != 0 {
			closeSlot = t.Slot
		}
		return TickResult{Kind: TickClose, Event: ArbEvent{
			BuyVenue:      o.buy,
			SellVenue:     o.sell,
			OpenSlot:      o.openSlot,
			CloseSlot:     closeSlot,
			LifetimeSlots: satSub(closeSlot, o.openSlot),
			LifetimeMs:    satSub(t.TsMs, o.openTsMs),
			PeakNetBps:    o.peakNetBps,
		}}
	}
	return TickResult{Kind: TickNone}
}

func satSub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

// MedianF64 returns the median of a float64 slice (0 for empty input). The
// input is sorted in place, matching the Rust helper's semantics.
func MedianF64(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sort.Float64s(v)
	return v[len(v)/2]
}

// MedianU64 returns the median of a uint64 slice (0 for empty input).
func MedianU64(v []uint64) uint64 {
	if len(v) == 0 {
		return 0
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	return v[len(v)/2]
}
