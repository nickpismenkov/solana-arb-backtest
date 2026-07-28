// Package detector implements a feed-agnostic fee-adjusted arb detector — a
// direct port of the original TS/Rust version. Feed it price ticks from any
// source (gRPC now, ShredStream later) and it emits arb open/close with
// lifetime (slots + ms) = the reaction budget. A "real" arb requires the
// cross-venue gap to exceed the SUM of both fees.
package detector

import "sort"

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

type ResultKind int

const (
	ResultNone ResultKind = iota
	ResultOpen
	ResultClose
)

type TickResult struct {
	Kind   ResultKind
	NetBps float64  // valid when Kind == ResultOpen
	Event  ArbEvent // valid when Kind == ResultClose
}

type openState struct {
	buy, sell  string
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
	return &Detector{venueA: a, venueB: b, ThresholdBps: feeA + feeB}
}

func (d *Detector) OnTick(t Tick) TickResult {
	if t.Venue == d.venueA {
		d.lastA, d.haveA = t.Price, true
	} else if t.Venue == d.venueB {
		d.lastB, d.haveB = t.Price, true
	}
	if !d.haveA || !d.haveB {
		return TickResult{Kind: ResultNone}
	}
	pa, pb := d.lastA, d.lastB

	signedGapBps := ((pb - pa) / pa) * 10000.0 // + => B dearer
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
			d.open = &openState{
				buy: buy, sell: sell,
				openSlot: t.Slot, openTsMs: t.TsMs,
				peakNetBps: net,
				lastSlot:   t.Slot, lastTsMs: t.TsMs,
			}
			return TickResult{Kind: ResultOpen, NetBps: net}
		}
		if net > d.open.peakNetBps {
			d.open.peakNetBps = net
		}
		d.open.lastSlot = t.Slot
		d.open.lastTsMs = t.TsMs
		return TickResult{Kind: ResultNone}
	}

	if d.open != nil {
		o := d.open
		d.open = nil
		closeSlot := t.Slot
		if closeSlot == 0 {
			closeSlot = o.lastSlot
		}
		lifetimeSlots := uint64(0)
		if closeSlot > o.openSlot {
			lifetimeSlots = closeSlot - o.openSlot
		}
		lifetimeMs := uint64(0)
		if t.TsMs > o.openTsMs {
			lifetimeMs = t.TsMs - o.openTsMs
		}
		return TickResult{Kind: ResultClose, Event: ArbEvent{
			BuyVenue: o.buy, SellVenue: o.sell,
			OpenSlot: o.openSlot, CloseSlot: closeSlot,
			LifetimeSlots: lifetimeSlots, LifetimeMs: lifetimeMs,
			PeakNetBps: o.peakNetBps,
		}}
	}
	return TickResult{Kind: ResultNone}
}

func MedianF64(v []float64) float64 {
	if len(v) == 0 {
		return 0.0
	}
	s := append([]float64{}, v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

func MedianU64(v []uint64) uint64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]uint64{}, v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}
