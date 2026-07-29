// Package detector is a feed-agnostic fee-adjusted arb detector — a direct
// port of the TS version. Feed it price ticks from any source (gRPC now,
// ShredStream later) and it emits arb open/close with lifetime (slots + ms)
// = the reaction budget. A "real" arb requires the cross-venue gap to
// exceed the SUM of both fees.
package detector

import "sort"

// Tick.TsMs and ArbEvent.LifetimeMs are u128 in the Rust source, but every
// call site only ever stores millisecond epoch timestamps / deltas, which
// comfortably fit in uint64 (good until year ~584 billion). Mapped to
// uint64 here rather than carrying math/big.Int through the hot path.

type Tick struct {
	Venue string
	Price float64 // quote per base (USDC per SOL)
	Slot  uint64
	TsMs  uint64
}

type ArbEvent struct {
	BuyVenue      string
	SellVenue     string
	OpenSlot      uint64
	CloseSlot     uint64
	LifetimeSlots uint64
	LifetimeMs    uint64
	PeakNetBps    float64
}

// TickResultKind discriminates the TickResult sum type (Rust: None | Open |
// Close).
type TickResultKind int

const (
	TickResultNone TickResultKind = iota
	TickResultOpen
	TickResultClose
)

// TickResult mirrors Rust's TickResult enum. Only the field(s) relevant to
// Kind are populated: NetBps for TickResultOpen, Event for TickResultClose.
type TickResult struct {
	Kind   TickResultKind
	NetBps float64
	Event  ArbEvent
}

type openState struct {
	buy        string
	sell       string
	openSlot   uint64
	openTsMs   uint64
	peakNetBps float64
	lastSlot   uint64
	lastTsMs   uint64
}

type Detector struct {
	venueA, venueB string
	ThresholdBps   float64
	lastA, lastB   float64
	haveA, haveB   bool
	open           *openState
}

func New(a, b string, feeA, feeB float64) *Detector {
	return &Detector{
		venueA:       a,
		venueB:       b,
		ThresholdBps: feeA + feeB,
	}
}

func (d *Detector) OnTick(t *Tick) TickResult {
	a, b := d.venueA, d.venueB
	if t.Venue == a {
		d.lastA, d.haveA = t.Price, true
	} else if t.Venue == b {
		d.lastB, d.haveB = t.Price, true
	}
	if !d.haveA || !d.haveB {
		return TickResult{Kind: TickResultNone}
	}
	pa, pb := d.lastA, d.lastB

	signedGapBps := ((pb - pa) / pa) * 10_000.0 // + => B dearer
	var net float64
	var buy, sell string
	if signedGapBps > d.ThresholdBps {
		net, buy, sell = signedGapBps-d.ThresholdBps, a, b
	} else if signedGapBps < -d.ThresholdBps {
		net, buy, sell = -signedGapBps-d.ThresholdBps, b, a
	}

	if net > 0.0 {
		if d.open == nil {
			d.open = &openState{
				buy:        buy,
				sell:       sell,
				openSlot:   t.Slot,
				openTsMs:   t.TsMs,
				peakNetBps: net,
				lastSlot:   t.Slot,
				lastTsMs:   t.TsMs,
			}
			return TickResult{Kind: TickResultOpen, NetBps: net}
		}
		o := d.open
		if net > o.peakNetBps {
			o.peakNetBps = net
		}
		o.lastSlot = t.Slot
		o.lastTsMs = t.TsMs
		return TickResult{Kind: TickResultNone}
	}

	if o := d.open; o != nil {
		d.open = nil
		closeSlot := o.lastSlot
		if t.Slot != 0 {
			closeSlot = t.Slot
		}
		return TickResult{
			Kind: TickResultClose,
			Event: ArbEvent{
				BuyVenue:      o.buy,
				SellVenue:     o.sell,
				OpenSlot:      o.openSlot,
				CloseSlot:     closeSlot,
				LifetimeSlots: satSub(closeSlot, o.openSlot),
				LifetimeMs:    satSub(t.TsMs, o.openTsMs),
				PeakNetBps:    o.peakNetBps,
			},
		}
	}
	return TickResult{Kind: TickResultNone}
}

func satSub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

func MedianF64(v []float64) float64 {
	if len(v) == 0 {
		return 0.0
	}
	sorted := append([]float64(nil), v...)
	sort.Float64s(sorted)
	return sorted[len(sorted)/2]
}

func MedianU128(v []uint64) uint64 {
	if len(v) == 0 {
		return 0
	}
	sorted := append([]uint64(nil), v...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}
