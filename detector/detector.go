package detector

import (
	"math"
	"sort"
)

type Tick struct {
	Venue string
	Price float64 // quote per base (USDC per SOL)
	Slot  u64
	TsMs  uint64
}

type u64 = uint64

type ArbEvent struct {
	BuyVenue      string
	SellVenue     string
	OpenSlot      u64
	CloseSlot     u64
	LifetimeSlots u64
	LifetimeMs    uint64
	PeakNetBps    float64
}

type TickResultType int

const (
	ResultNone TickResultType = iota
	ResultOpen
	ResultClose
)

type TickResult struct {
	Type   TickResultType
	NetBps float64
	Event  *ArbEvent
}

type openState struct {
	buy        string
	sell       string
	openSlot   u64
	openTsMs   uint64
	peakNetBps float64
	lastSlot   u64
	lastTsMs   uint64
}

type Detector struct {
	venues       [2]string
	ThresholdBps float64
	lastA        *float64
	lastB        *float64
	open         *openState
}

func NewDetector(a, b string, feeA, feeB float64) *Detector {
	return &Detector{
		venues:       [2]string{a, b},
		ThresholdBps: feeA + feeB,
	}
}

func (d *Detector) OnTick(t *Tick) TickResult {
	venueA, venueB := d.venues[0], d.venues[1]
	if t.Venue == venueA {
		price := t.Price
		d.lastA = &price
	} else if t.Venue == venueB {
		price := t.Price
		d.lastB = &price
	}

	if d.lastA == nil || d.lastB == nil {
		return TickResult{Type: ResultNone}
	}

	pa, pb := *d.lastA, *d.lastB
	signedGapBps := ((pb - pa) / math.Min(pa, pb)) * 10000.0 // + => B dearer

	var net float64
	var buy, sell string
	if signedGapBps > d.ThresholdBps {
		net = signedGapBps - d.ThresholdBps
		buy, sell = venueA, venueB
	} else if signedGapBps < -d.ThresholdBps {
		net = -signedGapBps - d.ThresholdBps
		buy, sell = venueB, venueA
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
			return TickResult{
				Type:   ResultOpen,
				NetBps: net,
			}
		} else {
			if net > d.open.peakNetBps {
				d.open.peakNetBps = net
			}
			d.open.lastSlot = t.Slot
			d.open.lastTsMs = t.TsMs
			return TickResult{Type: ResultNone}
		}
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

		return TickResult{
			Type: ResultClose,
			Event: &ArbEvent{
				BuyVenue:      o.buy,
				SellVenue:     o.sell,
				OpenSlot:      o.openSlot,
				CloseSlot:     closeSlot,
				LifetimeSlots: lifetimeSlots,
				LifetimeMs:    lifetimeMs,
				PeakNetBps:    o.peakNetBps,
			},
		}
	}

	return TickResult{Type: ResultNone}
}

func MedianF64(v []float64) float64 {
	if len(v) == 0 {
		return 0.0
	}
	sort.Float64s(v)
	return v[len(v)/2]
}

func MedianU64(v []uint64) uint64 {
	if len(v) == 0 {
		return 0
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	return v[len(v)/2]
}
