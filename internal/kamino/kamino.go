// Package kamino implements the Kamino Lend (klend) liquidation finder,
// instruction builders, atomic fire path, and the Lazer-driven watch engine.
//
// Unlike marginfi (where we decode Pyth oracles and compute health), Kamino's
// Obligation STORES pre-computed USD health values as Fraction fixed-point
// (u128, 60 fractional bits). So we read liquidatability straight from the
// obligation — no oracle needed:
//
//	liquidatable  ⟺  borrow_factor_adjusted_debt_value ≥ unhealthy_borrow_value
package kamino

import (
	"encoding/binary"
	"math"
	"math/big"

	"github.com/gagliardetto/solana-go"
)

const (
	KlendProgram = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"
	MainMarket   = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"
)

// account:Obligation Anchor discriminator, and total account size.
var ObligationDisc = [8]byte{168, 206, 141, 106, 88, 76, 172, 167}

const ObligationSize = 3344

// account:Reserve Anchor discriminator, and total account size.
var ReserveDisc = [8]byte{43, 242, 204, 202, 26, 247, 59, 127}

const ReserveSize = 8624

// Kamino `Fraction` = u128 with 60 fractional bits (value / 2^60).
const fractionBits = 60

// VERIFIED offsets (offset 0 = 8-byte discriminator).
const (
	oLastUpdateSlot     = 16
	oStale              = 24
	oLendingMarket      = 32
	oOwner              = 64
	oDeposits           = 96 // [ObligationCollateral; 8], 136B each
	oDepositedValue     = 1192
	oBorrows            = 1208 // [ObligationLiquidity; 5], 200B each
	oBfAdjDebtValue     = 2208
	oBorrowedValue      = 2224
	oAllowedBorrowValue = 2240
	oUnhealthyBorrowVal = 2256
	oElevationGroup     = 2285
	oHasDebt            = 2287
	collateralStride    = 136
	collDepositedAmount = 32 // u64 cTokens
	liquidityStride     = 200
	liqBorrowedAmountSF = 88 // u128 Fraction, native smallest units
)

func fracAt(d []byte, off int) float64 {
	be := make([]byte, 16)
	for i := 0; i < 16; i++ {
		be[15-i] = d[off+i]
	}
	f := new(big.Float).SetInt(new(big.Int).SetBytes(be))
	v, _ := f.Float64()
	return v / math.Pow(2, fractionBits)
}

func u64At(d []byte, off int) uint64 { return binary.LittleEndian.Uint64(d[off : off+8]) }
func pubkeyAt(d []byte, off int) solana.PublicKey {
	return solana.PublicKeyFromBytes(d[off : off+32])
}

// Position is a raw collateral or debt position.
type Position struct {
	Reserve solana.PublicKey
	Amount  uint64  // deposit: cToken amount
	AmountF float64 // borrow: native smallest units (Fraction)
}

// Obligation is a decoded Kamino borrower position with its stored USD
// health values.
type Obligation struct {
	Owner                solana.PublicKey
	LendingMarket        solana.PublicKey
	LastUpdateSlot       uint64
	Stale                bool
	DepositedValue       float64
	BfAdjustedDebt       float64
	BorrowedValue        float64
	AllowedBorrowValue   float64
	UnhealthyBorrowValue float64
	ElevationGroup       uint8
	Deposits             []DepositPos
	Borrows              []BorrowPos
}

type DepositPos struct {
	Reserve solana.PublicKey
	Amount  uint64
}
type BorrowPos struct {
	Reserve solana.PublicKey
	Amount  float64
}

func DecodeObligation(data []byte) (*Obligation, bool) {
	if len(data) < oHasDebt+1 || !hasDisc(data, ObligationDisc) {
		return nil, false
	}
	var deposits []DepositPos
	for i := 0; i < 8; i++ {
		base := oDeposits + i*collateralStride
		reserve := pubkeyAt(data, base)
		if reserve.IsZero() {
			continue
		}
		amt := u64At(data, base+collDepositedAmount)
		if amt == 0 {
			continue
		}
		deposits = append(deposits, DepositPos{Reserve: reserve, Amount: amt})
	}
	var borrows []BorrowPos
	for i := 0; i < 5; i++ {
		base := oBorrows + i*liquidityStride
		reserve := pubkeyAt(data, base)
		if reserve.IsZero() {
			continue
		}
		amt := fracAt(data, base+liqBorrowedAmountSF)
		if amt == 0.0 {
			continue
		}
		borrows = append(borrows, BorrowPos{Reserve: reserve, Amount: amt})
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
		UnhealthyBorrowValue: fracAt(data, oUnhealthyBorrowVal),
		ElevationGroup:       data[oElevationGroup],
		Deposits:             deposits,
		Borrows:              borrows,
	}, true
}

func hasDisc(d []byte, disc [8]byte) bool {
	var got [8]byte
	copy(got[:], d[:8])
	return got == disc
}

// Liquidatable is Kamino's own liquidation gate.
func (o *Obligation) Liquidatable() bool {
	return o.BfAdjustedDebt >= o.UnhealthyBorrowValue && o.UnhealthyBorrowValue > 0.0
}

// Ratio is debt / threshold — ≥ 1.0 means liquidatable; how close otherwise.
func (o *Obligation) Ratio() float64 {
	if o.UnhealthyBorrowValue == 0.0 {
		return 0.0
	}
	return o.BfAdjustedDebt / o.UnhealthyBorrowValue
}

// ── Reserve (8624 bytes) — cached price + params to recompute CURRENT health ──
const (
	rLastUpdateSlot        = 16
	rStale                 = 24
	rAvailableAmount       = 224 // u64 native
	rBorrowedAmountSF      = 232 // u128 Fraction native
	rMarketPriceSF         = 248 // u128 Fraction USD/whole-token
	rMintDecimals          = 272 // u64
	rAccProtocolFeesSF     = 344
	rAccReferrerFeesSF     = 360
	rPendingReferrerFeesSF = 376
	rCollMintTotalSupply   = 2592 // u64 cToken supply
	rLtvPct                = 4872 // u8
	rLiqThresholdPct       = 4873 // u8
	rBorrowFactorPct       = 5008 // u64
)

// Reserve is a reserve's cached price + params needed to value obligation positions.
type Reserve struct {
	MintDecimals    uint32
	MarketPrice     float64
	PriceSlot       uint64
	PriceStale      bool
	ExchangeRate    float64
	LtvPct          uint8
	LiqThresholdPct uint8
	BorrowFactorPct uint64
}

func DecodeReserve(data []byte) (*Reserve, bool) {
	if len(data) < rBorrowFactorPct+8 || !hasDisc(data, ReserveDisc) {
		return nil, false
	}
	totalSupply := float64(u64At(data, rAvailableAmount)) +
		fracAt(data, rBorrowedAmountSF) -
		fracAt(data, rAccProtocolFeesSF) -
		fracAt(data, rAccReferrerFeesSF) -
		fracAt(data, rPendingReferrerFeesSF)
	ctokenSupply := float64(u64At(data, rCollMintTotalSupply))
	exchangeRate := 1.0 // INITIAL_COLLATERAL_RATE
	if ctokenSupply > 0.0 && totalSupply > 0.0 {
		exchangeRate = totalSupply / ctokenSupply
	}
	return &Reserve{
		MintDecimals:    uint32(u64At(data, rMintDecimals)),
		MarketPrice:     fracAt(data, rMarketPriceSF),
		PriceSlot:       u64At(data, rLastUpdateSlot),
		PriceStale:      data[rStale] != 0,
		ExchangeRate:    exchangeRate,
		LtvPct:          data[rLtvPct],
		LiqThresholdPct: data[rLiqThresholdPct],
		BorrowFactorPct: u64At(data, rBorrowFactorPct),
	}, true
}

// Recomputed is health recomputed from CURRENT reserve prices (replicates refresh_obligation).
type Recomputed struct {
	DepositedValue       float64
	AllowedBorrowValue   float64
	UnhealthyBorrowValue float64
	BfAdjustedDebt       float64
	Missing              int  // positions whose reserve we couldn't resolve — result is INCOMPLETE if > 0.
	Elevation            bool // obligation uses an elevation group → reserve-config LTV/liq/bf are WRONG here.
	OldestPriceSlot      uint64
}

func (r *Recomputed) Liquidatable() bool {
	return r.BfAdjustedDebt >= r.UnhealthyBorrowValue && r.UnhealthyBorrowValue > 0.0
}
func (r *Recomputed) Ratio() float64 {
	if r.UnhealthyBorrowValue == 0.0 {
		return 0.0
	}
	return r.BfAdjustedDebt / r.UnhealthyBorrowValue
}

// Trustworthy is true only when fully priced and not elevation-group-dependent.
func (r *Recomputed) Trustworthy() bool { return r.Missing == 0 && !r.Elevation }

// Recompute recomputes an obligation's health at current reserve prices.
// Caveat: uses the stored borrowed_amount without re-accruing interest to the
// current slot (slightly under-counts debt → conservative, won't
// false-positive from this).
func Recompute(ob *Obligation, reserves map[solana.PublicKey]*Reserve) *Recomputed {
	var deposited, allowed, unhealthy, bfDebt float64
	missing := 0
	oldest := uint64(1<<64 - 1)
	for _, dep := range ob.Deposits {
		r, ok := reserves[dep.Reserve]
		if !ok {
			missing++
			continue
		}
		if r.PriceSlot < oldest {
			oldest = r.PriceSlot
		}
		underlying := float64(dep.Amount) * r.ExchangeRate
		val := underlying * r.MarketPrice / pow10(int(r.MintDecimals))
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
		val := (b.Amount / pow10(int(r.MintDecimals))) * r.MarketPrice
		bfDebt += val * float64(r.BorrowFactorPct) / 100.0
	}
	if oldest == 1<<64-1 {
		oldest = 0
	}
	return &Recomputed{
		DepositedValue: deposited, AllowedBorrowValue: allowed,
		UnhealthyBorrowValue: unhealthy, BfAdjustedDebt: bfDebt,
		Missing: missing, Elevation: ob.ElevationGroup != 0, OldestPriceSlot: oldest,
	}
}

func pow10(n int) float64 {
	r := 1.0
	if n >= 0 {
		for i := 0; i < n; i++ {
			r *= 10
		}
	} else {
		for i := 0; i < -n; i++ {
			r /= 10
		}
	}
	return r
}
