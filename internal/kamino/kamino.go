// Package kamino implements Kamino Lend (klend) liquidation finding —
// Stage 1, read-only.
//
// Unlike marginfi (where we decode Pyth oracles and compute health),
// Kamino's Obligation STORES pre-computed USD health values as a Fraction
// fixed-point (u128, 60 fractional bits). So we read liquidatability
// straight from the obligation — no oracle needed:
//
//	liquidatable  <=>  borrow_factor_adjusted_debt_value >= unhealthy_borrow_value
//
// Caveat: these values are as of the obligation's last on-chain refresh; the
// Stale flag + LastUpdateSlot tell us how fresh they are. Good enough for a
// finder; a live trigger would re-price via Scope (Kamino's oracle, not
// Pyth).
//
// All offsets VERIFIED against a real 3344-byte main-market obligation: the
// stored allowed/unhealthy values equal deposited_value x the init/liq LTVs
// (0.80 / 0.90), which only holds if every offset is correct.
package kamino

import (
	"math"

	"arbengine/internal/solana"
)

const (
	KlendProgram     = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"
	KaminoMainMarket = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"
)

// ObligationDisc is the account:Obligation Anchor discriminator.
var ObligationDisc = [8]byte{168, 206, 141, 106, 88, 76, 172, 167}

// ObligationSize is the total Obligation account size.
const ObligationSize = 3344

// ReserveDisc is the account:Reserve Anchor discriminator.
var ReserveDisc = [8]byte{43, 242, 204, 202, 26, 247, 59, 127}

// ReserveSize is the total Reserve account size.
const ReserveSize = 8624

// fractionBits: Kamino `Fraction` = u128 with 60 fractional bits (value / 2^60).
const fractionBits = 60

// VERIFIED offsets (offset 0 = 8-byte discriminator).
const (
	oLastUpdateSlot       = 16
	oStale                = 24
	oLendingMarket        = 32
	oOwner                = 64
	oDeposits             = 96 // [ObligationCollateral; 8], 136B each
	oDepositedValue       = 1192
	oBorrows              = 1208 // [ObligationLiquidity; 5], 200B each
	oBfAdjDebtValue       = 2208
	oBorrowedValue        = 2224
	oAllowedBorrowValue   = 2240
	oUnhealthyBorrowValue = 2256
	oElevationGroup       = 2285
	oHasDebt              = 2287
	collateralStride      = 136
	collDepositedAmount   = 32 // u64 cTokens
	liquidityStride       = 200
	liqBorrowedAmountSf   = 88 // u128 Fraction, native smallest units
)

func fracAt(data []byte, off int) float64 {
	var b [16]byte
	copy(b[:], data[off:off+16])
	var lo, hi uint64
	for i := 0; i < 8; i++ {
		lo |= uint64(b[i]) << (8 * i)
	}
	for i := 0; i < 8; i++ {
		hi |= uint64(b[8+i]) << (8 * i)
	}
	// u128 -> float64, then divide by 2^60.
	v := float64(hi)*18446744073709551616.0 + float64(lo)
	return v / math.Pow(2, fractionBits)
}

func u64At(data []byte, off int) uint64 {
	var b [8]byte
	copy(b[:], data[off:off+8])
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

func pubkeyAt(data []byte, off int) solana.Pubkey {
	pk, _ := solana.PubkeyFromBytes(data[off : off+32])
	return pk
}

// DepositPosition is a raw collateral position: (deposit reserve, deposited
// amount in cTokens).
type DepositPosition struct {
	Reserve solana.Pubkey
	Amount  uint64
}

// BorrowPosition is a raw debt position: (borrow reserve, borrowed amount in
// native smallest units).
type BorrowPosition struct {
	Reserve solana.Pubkey
	Amount  float64
}

// Obligation is a decoded Kamino borrower position with its stored USD
// health values.
type Obligation struct {
	Owner          solana.Pubkey
	LendingMarket  solana.Pubkey
	LastUpdateSlot uint64
	Stale          bool
	// DepositedValue is the total deposited collateral value (USD) — what's
	// seizable.
	DepositedValue float64
	// BfAdjustedDebt is the borrow-factor-adjusted debt — the value compared
	// against the threshold.
	BfAdjustedDebt float64
	// BorrowedValue is the raw borrowed market value (USD).
	BorrowedValue float64
	// AllowedBorrowValue is the borrow allowed at init LTV (USD).
	AllowedBorrowValue float64
	// UnhealthyBorrowValue is the liquidation threshold (USD) — cross it and
	// you're liquidatable.
	UnhealthyBorrowValue float64
	// ElevationGroup: 0 = none; nonzero overrides reserve LTV/liq/bf params.
	ElevationGroup uint8
	// Deposits are the raw collateral positions.
	Deposits []DepositPosition
	// Borrows are the raw debt positions.
	Borrows []BorrowPosition
}

// DecodeObligation decodes a raw Kamino Obligation account.
func DecodeObligation(data []byte) (*Obligation, bool) {
	if len(data) < oHasDebt+1 || [8]byte(data[:8]) != ObligationDisc {
		return nil, false
	}
	// Raw positions (skip empty slots = zeroed reserve pubkey).
	var deposits []DepositPosition
	for i := 0; i < 8; i++ {
		base := oDeposits + i*collateralStride
		reserve := pubkeyAt(data, base)
		if reserve == solana.ZeroPubkey {
			continue
		}
		amt := u64At(data, base+collDepositedAmount)
		if amt == 0 {
			continue
		}
		deposits = append(deposits, DepositPosition{Reserve: reserve, Amount: amt})
	}
	var borrows []BorrowPosition
	for i := 0; i < 5; i++ {
		base := oBorrows + i*liquidityStride
		reserve := pubkeyAt(data, base)
		if reserve == solana.ZeroPubkey {
			continue
		}
		amt := fracAt(data, base+liqBorrowedAmountSf) // native smallest units
		if amt == 0.0 {
			continue
		}
		borrows = append(borrows, BorrowPosition{Reserve: reserve, Amount: amt})
	}
	return &Obligation{
		Owner:                pubkeyAt(data, oOwner),
		LendingMarket:        pubkeyAt(data, oLendingMarket),
		LastUpdateSlot:       u64At(data, oLastUpdateSlot),
		Stale:                data[oStale] != 0,
		DepositedValue:       fracAt(data, oDepositedValue),
		BfAdjustedDebt:       fracAt(data, oBfAdjDebtValue),
		BorrowedValue:        fracAt(data, oBorrowedValue),
		AllowedBorrowValue:   fracAt(data, oAllowedBorrowValue),
		UnhealthyBorrowValue: fracAt(data, oUnhealthyBorrowValue),
		ElevationGroup:       data[oElevationGroup],
		Deposits:             deposits,
		Borrows:              borrows,
	}, true
}

// Liquidatable is Kamino's own liquidation gate.
func (o *Obligation) Liquidatable() bool {
	return o.BfAdjustedDebt >= o.UnhealthyBorrowValue && o.UnhealthyBorrowValue > 0.0
}

// Ratio is debt / threshold — >= 1.0 means liquidatable; how close otherwise.
func (o *Obligation) Ratio() float64 {
	if o.UnhealthyBorrowValue == 0.0 {
		return 0.0
	}
	return o.BfAdjustedDebt / o.UnhealthyBorrowValue
}

// ── Reserve (8624 bytes) — cached price + params to recompute CURRENT health ──
// Offsets VERIFIED against the mainnet SOL reserve (price $82.20, dec 9).
const (
	rLastUpdateSlot        = 16
	rStale                 = 24
	rAvailableAmount       = 224 // u64 native
	rBorrowedAmountSf      = 232 // u128 Fraction native
	rMarketPriceSf         = 248 // u128 Fraction USD/whole-token
	rMintDecimals          = 272 // u64
	rAccProtocolFeesSf     = 344
	rAccReferrerFeesSf     = 360
	rPendingReferrerFeesSf = 376
	rCollMintTotalSupply   = 2592 // u64 cToken supply
	rLtvPct                = 4872 // u8
	rLiqThresholdPct       = 4873 // u8
	rBorrowFactorPct       = 5008 // u64
)

// Reserve is a reserve's cached price + params needed to value obligation
// positions.
type Reserve struct {
	MintDecimals uint32
	// MarketPrice is USD per whole token (from the reserve's cached,
	// refresh_reserve'd oracle).
	MarketPrice float64
	PriceSlot   uint64
	PriceStale  bool
	// ExchangeRate is underlying liquidity tokens per cToken (>= 1, grows
	// with interest).
	ExchangeRate    float64
	LtvPct          uint8
	LiqThresholdPct uint8
	BorrowFactorPct uint64
}

// DecodeReserve decodes a raw Kamino Reserve account.
func DecodeReserve(data []byte) (*Reserve, bool) {
	if len(data) < rBorrowFactorPct+8 || [8]byte(data[:8]) != ReserveDisc {
		return nil, false
	}
	// total_supply() (native units): available + borrowed - fees.
	totalSupply := float64(u64At(data, rAvailableAmount)) +
		fracAt(data, rBorrowedAmountSf) -
		fracAt(data, rAccProtocolFeesSf) -
		fracAt(data, rAccReferrerFeesSf) -
		fracAt(data, rPendingReferrerFeesSf)
	ctokenSupply := float64(u64At(data, rCollMintTotalSupply))
	exchangeRate := 1.0 // INITIAL_COLLATERAL_RATE
	if ctokenSupply > 0.0 && totalSupply > 0.0 {
		exchangeRate = totalSupply / ctokenSupply
	}
	return &Reserve{
		MintDecimals:    uint32(u64At(data, rMintDecimals)),
		MarketPrice:     fracAt(data, rMarketPriceSf),
		PriceSlot:       u64At(data, rLastUpdateSlot),
		PriceStale:      data[rStale] != 0,
		ExchangeRate:    exchangeRate,
		LtvPct:          data[rLtvPct],
		LiqThresholdPct: data[rLiqThresholdPct],
		BorrowFactorPct: u64At(data, rBorrowFactorPct),
	}, true
}

// Recomputed is health recomputed from CURRENT reserve prices (replicates
// refresh_obligation).
type Recomputed struct {
	DepositedValue       float64
	AllowedBorrowValue   float64
	UnhealthyBorrowValue float64
	BfAdjustedDebt       float64
	// Missing is the count of positions whose reserve we couldn't resolve —
	// result is INCOMPLETE if > 0.
	Missing int
	// Elevation: obligation uses an elevation group -> reserve-config
	// LTV/liq/bf are WRONG here.
	Elevation bool
	// OldestPriceSlot is the worst (oldest) reserve-price slot used — how
	// fresh the recompute is.
	OldestPriceSlot uint64
}

// Liquidatable reports Kamino's liquidation gate on the recomputed health.
func (r *Recomputed) Liquidatable() bool {
	return r.BfAdjustedDebt >= r.UnhealthyBorrowValue && r.UnhealthyBorrowValue > 0.0
}

// Ratio is debt / threshold — >= 1.0 means liquidatable; how close otherwise.
func (r *Recomputed) Ratio() float64 {
	if r.UnhealthyBorrowValue == 0.0 {
		return 0.0
	}
	return r.BfAdjustedDebt / r.UnhealthyBorrowValue
}

// Trustworthy: trust only when fully priced and not elevation-group-dependent.
func (r *Recomputed) Trustworthy() bool {
	return r.Missing == 0 && !r.Elevation
}

// Recompute recomputes an obligation's health at current reserve prices.
// Caveat: uses the stored borrowed_amount without re-accruing interest to
// the current slot (slightly under-counts debt -> conservative, won't
// false-positive from this).
func Recompute(ob *Obligation, reserves map[solana.Pubkey]*Reserve) Recomputed {
	var deposited, allowed, unhealthy, bfDebt float64
	missing := 0
	oldest := uint64(math.MaxUint64)
	for _, d := range ob.Deposits {
		r, ok := reserves[d.Reserve]
		if !ok {
			missing++
			continue
		}
		if r.PriceSlot < oldest {
			oldest = r.PriceSlot
		}
		underlying := float64(d.Amount) * r.ExchangeRate
		val := underlying * r.MarketPrice / math.Pow(10, float64(r.MintDecimals))
		deposited += val
		allowed += val * float64(r.LtvPct) / 100.0
		unhealthy += val * float64(r.LiqThresholdPct) / 100.0
	}
	for _, b := range ob.Borrows {
		r, ok := reserves[b.Reserve]
		if !ok {
			missing++
			continue
		}
		if r.PriceSlot < oldest {
			oldest = r.PriceSlot
		}
		val := (b.Amount / math.Pow(10, float64(r.MintDecimals))) * r.MarketPrice
		bfDebt += val * float64(r.BorrowFactorPct) / 100.0
	}
	if oldest == uint64(math.MaxUint64) {
		oldest = 0
	}
	return Recomputed{
		DepositedValue:       deposited,
		AllowedBorrowValue:   allowed,
		UnhealthyBorrowValue: unhealthy,
		BfAdjustedDebt:       bfDebt,
		Missing:              missing,
		Elevation:            ob.ElevationGroup != 0,
		OldestPriceSlot:      oldest,
	}
}
