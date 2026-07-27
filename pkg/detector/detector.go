package detector

import (
	"sort"
)

type Tick struct {
	Venue string
	Price float64
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

type TickResultType int

const (
	TickResultNone TickResultType = iota
	TickResultOpen
	TickResultClose
)

type TickResult struct {
	Type   TickResultType
	NetBps float64
	Event  ArbEvent
}

type OpenState struct {
	Buy        string
	Sell       string
	OpenSlot   uint64
	OpenTsMs   uint64
	PeakNetBps float64
	LastSlot   uint64
	LastTsMs   uint64
}

type Detector struct {
	VenueA       string
	VenueB       string
	ThresholdBps float64
	LastA        *float64
	LastB        *float64
	Open         *OpenState
}

func NewDetector(a, b string, feeA, feeB float64) *Detector {
	return &Detector{
		VenueA:       a,
		VenueB:       b,
		ThresholdBps: feeA + feeB,
	}
}

func (d *Detector) OnTick(t Tick) TickResult {
	if t.Venue == d.VenueA {
		p := t.Price
		d.LastA = &p
	} else if t.Venue == d.VenueB {
		p := t.Price
		d.LastB = &p
	}

	if d.LastA == nil || d.LastB == nil {
		return TickResult{Type: TickResultNone}
	}

	pa := *d.LastA
	pb := *d.LastB

	signedGapBps := ((pb - pa) / pa) * 10000.0 // + => B dearer
	var net float64
	var buy, sell string

	if signedGapBps > d.ThresholdBps {
		net = signedGapBps - d.ThresholdBps
		buy, sell = d.VenueA, d.VenueB
	} else if signedGapBps < -d.ThresholdBps {
		net = -signedGapBps - d.ThresholdBps
		buy, sell = d.VenueB, d.VenueA
	} else {
		net = 0.0
	}

	if net > 0.0 {
		if d.Open == nil {
			d.Open = &OpenState{
				Buy:        buy,
				Sell:       sell,
				OpenSlot:   t.Slot,
				OpenTsMs:   t.TsMs,
				PeakNetBps: net,
				LastSlot:   t.Slot,
				LastTsMs:   t.TsMs,
			}
			return TickResult{Type: TickResultOpen, NetBps: net}
		} else {
			if net > d.Open.PeakNetBps {
				d.Open.PeakNetBps = net
			}
			d.Open.LastSlot = t.Slot
			d.Open.LastTsMs = t.TsMs
			return TickResult{Type: TickResultNone}
		}
	}

	if d.Open != nil {
		o := d.Open
		d.Open = nil
		closeSlot := t.Slot
		if closeSlot == 0 {
			closeSlot = o.LastSlot
		}
		var diffSlots uint64
		if closeSlot > o.OpenSlot {
			diffSlots = closeSlot - o.OpenSlot
		}
		var diffMs uint64
		if t.TsMs > o.OpenTsMs {
			diffMs = t.TsMs - o.OpenTsMs
		}
		return TickResult{
			Type: TickResultClose,
			Event: ArbEvent{
				BuyVenue:      o.Buy,
				SellVenue:     o.Sell,
				OpenSlot:      o.OpenSlot,
				CloseSlot:     closeSlot,
				LifetimeSlots: diffSlots,
				LifetimeMs:    diffMs,
				PeakNetBps:    o.PeakNetBps,
			},
		}
	}

	return TickResult{Type: TickResultNone}
}

func MedianF64(v []float64) float64 {
	if len(v) == 0 {
		return 0.0
	}
	sort.Float64s(v)
	return v[len(v)/2]
}

func MedianUint64(v []uint64) uint64 {
	if len(v) == 0 {
		return 0
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	return v[len(v)/2]
}
