// Package save implements Save (formerly Solend) liquidation data layer +
// instruction builders.
//
// Save is the original SPL token-lending model — a NATIVE program, so each
// instruction is a one-byte tag (not an Anchor discriminator). Every layout
// below is derived from CAPTURED mainnet truth (the marginfi/Kamino lesson):
// the Reserve/Obligation packed layouts are cross-checked against the
// canonical Solend source AND real on-chain accounts, and the
// liquidate/refresh account orders are taken verbatim from real liquidation
// txs (4tQm9zcd… and 2inNexup…, both identical).
//
// ★ Save's USDC reserve reads the SAME Pyth sponsored feed (Dpw1EAVr…) that
// our self-crank pipeline already refreshes — so the crank front-run edge
// applies here too on Pyth-priced collateral, with no extra crank work.
package save

import (
	"encoding/binary"
	"math/big"

	"github.com/gagliardetto/solana-go"
)

const (
	SolendProgram = "So1endDq2YkqhipRh3WViPa8hdiSpxWy6z3Z6tMCpAo"
	MainPool      = "4UpD2fh7xH3VP9QQaXtsS1YY3bxzWhtfpks7FatyKvdY"
)

// Main-pool debt reserves the fire path repays. All three have a wired
// JupLend flash market (internal/flashloan) and are classic-SPL mints.
// Reserve pubkeys discovered live (getProgramAccounts, dataSize 619, memcmp
// lending_market = MainPool) and cross-checked against the reserve's decoded
// liquidity_mint.
const (
	USDCReserve = "BgxfHJDzm44T7XG68MYKx7YisTjZu73tVovyZSjJMpmw" // 6dp
	USDTReserve = "8K9WC8xoh2rtQNY7iEGXtPvfbDCi563SdWhCAhuMP2xE" // 6dp
	WSOLReserve = "8PbodeaosQP19SjYFx855UMqWxH2HynZLdBXmsrbac36" // 9dp
)

// Isolated Solend pool with LIVE liquidation flow. Census 2026-07-15: over
// 48h this pool carried $321 of $341 (94%) of Solend's liquidation value
// while the main pool went effectively dead ($3). The fire path is
// pool-agnostic (lending_market_authority is a PDA of the market, and the tx
// uses the obligation's own lending_market), so covering a pool = scanning
// its obligations + registering its accepted-debt reserves. Only USDC/USDT
// here (same mints → same JupLend flash markets as the main pool; no wSOL
// reserve).
const (
	IsoPool1            = "24FVbp6yRxP7qNNiVXHjAjwUabdvVfbJtDb3aJ5zCWwy"
	IsoPool1USDCReserve = "56v2DrnHB7kp5KkM4UboGymm8SxUJ8xRR9uYnU62uw4R"
	IsoPool1USDTReserve = "AQTzHsJ5AHk1PN89o4mjPm7FHkMR7a7BxmY5jXgqnBTP"
)

// SCANPools is every Solend lending market we scan for liquidatable
// obligations. Add active pools here (per an all-pools census) — the fire
// path needs no other change.
var ScanPools = []string{MainPool, IsoPool1}

// DebtReserves is the accepted-debt reserves across all ScanPools
// (USDC/USDT/wSOL mints, each wired to a JupLend flash market). An
// obligation borrowing one of these is fireable.
var DebtReserves = []string{
	USDCReserve, USDTReserve, WSOLReserve,
	IsoPool1USDCReserve, IsoPool1USDTReserve,
}

const (
	USDCMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	USDTMint = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"
	WSOLMint = "So11111111111111111111111111111111111111112"
)

// IsAcceptedDebtMint reports whether mint is a debt mint the widened fire
// path accepts (each has a JupLend flash market).
func IsAcceptedDebtMint(mint solana.PublicKey) bool {
	switch mint.String() {
	case USDCMint, USDTMint, WSOLMint:
		return true
	default:
		return false
	}
}

const tokenProgram = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"

// Instruction tags (Solend LendingInstruction enum).
const (
	tagRefreshReserve     byte = 3
	tagRefreshObligation  byte = 7
	tagLiquidateAndRedeem byte = 17 // LiquidateObligationAndRedeemReserveCollateral
)

func pk(s string) solana.PublicKey { return solana.MustPublicKeyFromBase58(s) }

func readPk(d []byte, o int) (solana.PublicKey, bool) {
	if o < 0 || o+32 > len(d) {
		return solana.PublicKey{}, false
	}
	return solana.PublicKeyFromBytes(d[o : o+32]), true
}

func u64le(d []byte, o int) (uint64, bool) {
	if o < 0 || o+8 > len(d) {
		return 0, false
	}
	return binary.LittleEndian.Uint64(d[o : o+8]), true
}

// wad decodes a Solend Decimal / scaled value: u128 with WAD = 1e18.
func wad(d []byte, o int) (float64, bool) {
	if o < 0 || o+16 > len(d) {
		return 0, false
	}
	// u128 little-endian → big.Int by reversing to big-endian.
	be := make([]byte, 16)
	for i := 0; i < 16; i++ {
		be[15-i] = d[o+i]
	}
	i := new(big.Int).SetBytes(be)
	f := new(big.Float).SetInt(i)
	v, _ := f.Float64()
	return v / 1e18, true
}

// ── Reserve (619 bytes, layout from Solend reserve.rs Pack + verified on the
//
//	USDC reserve). ───────────────────────────────────────────────────────
const (
	rLendingMarket   = 10
	rLiqMint         = 42
	rMintDecimals    = 74
	rLiqSupply       = 75
	rPythOracle      = 107
	rSbOracle        = 139
	rAvailableAmount = 171 // u64: liquidity available in the reserve (base units)
	rBorrowedWads    = 179 // Decimal (WAD): liquidity borrowed, accrues interest
	rCumBorrowRate   = 195 // Decimal (WAD): cumulative borrow rate (monotone ↑)
	rMarketPrice     = 211 // Decimal (WAD): USD price per whole token
	rCollMint        = 227
	rCollMintSupply  = 259 // u64: cToken mint total supply (base units)
	rCollSupply      = 267
	rLtv             = 300
	rLiqBonus        = 301
	rLiqThreshold    = 302
	rFeeReceiver     = 339
	// accumulated_protocol_fees_wads lives in the reserve's trailing padding
	// region (added after the original 619-byte layout froze), NOT inline
	// after cumulative_borrow_rate. Verified live: subtracting it moves the
	// exchange rate by <1e-4 %, but total_supply() nets it out, so we
	// reproduce it exactly.
	rAccumProtocolFeesWads = 373 // Decimal (WAD)
)

// Reserve is every account a refresh/liquidate touches for one reserve,
// pulled from the reserve bytes — mirrors Kamino's ReserveAccounts pattern.
type Reserve struct {
	Reserve                 solana.PublicKey
	LendingMarket           solana.PublicKey
	LiquidityMint           solana.PublicKey
	MintDecimals            uint8
	LiquiditySupply         solana.PublicKey
	PythOracle              solana.PublicKey
	SwitchboardOracle       solana.PublicKey
	CollateralMint          solana.PublicKey
	CollateralSupply        solana.PublicKey
	FeeReceiver             solana.PublicKey
	MarketPrice             float64
	LiquidationThresholdPct uint8
	LiquidationBonusPct     uint8
	LoanToValuePct          uint8
	// ── cToken exchange-rate inputs (fresh-price collateral valuation) ─────
	// AvailableAmount is liquidity available in the reserve (base units).
	AvailableAmount uint64
	// BorrowedAmount is liquidity borrowed (whole native units; WAD
	// decoded), accrues interest.
	BorrowedAmount float64
	// CumulativeBorrowRate (WAD decoded), monotone ↑ — accrues each
	// borrow's principal to "now" when reproducing Solend's
	// refresh_obligation.
	CumulativeBorrowRate float64
	// AccumulatedProtocolFees accrued but not yet claimed (whole units; WAD
	// decoded). Netted out of total_supply, exactly like Solend's own math.
	AccumulatedProtocolFees float64
	// CollateralMintTotalSupply is the cToken mint total supply (base
	// units) — denominator of the exchange rate.
	CollateralMintTotalSupply uint64
}

// DecodeReserve decodes a Save Reserve account (619 bytes, version tag 1).
func DecodeReserve(reserve solana.PublicKey, d []byte) (*Reserve, bool) {
	if len(d) < 619 || d[0] != 1 {
		return nil, false
	}
	lendingMarket, ok := readPk(d, rLendingMarket)
	if !ok {
		return nil, false
	}
	liqMint, ok := readPk(d, rLiqMint)
	if !ok {
		return nil, false
	}
	liqSupply, ok := readPk(d, rLiqSupply)
	if !ok {
		return nil, false
	}
	pythOracle, ok := readPk(d, rPythOracle)
	if !ok {
		return nil, false
	}
	sbOracle, ok := readPk(d, rSbOracle)
	if !ok {
		return nil, false
	}
	collMint, ok := readPk(d, rCollMint)
	if !ok {
		return nil, false
	}
	collSupply, ok := readPk(d, rCollSupply)
	if !ok {
		return nil, false
	}
	feeReceiver, ok := readPk(d, rFeeReceiver)
	if !ok {
		return nil, false
	}
	marketPrice, ok := wad(d, rMarketPrice)
	if !ok {
		return nil, false
	}
	availableAmount, ok := u64le(d, rAvailableAmount)
	if !ok {
		return nil, false
	}
	borrowedAmount, ok := wad(d, rBorrowedWads)
	if !ok {
		return nil, false
	}
	cumBorrowRate, ok := wad(d, rCumBorrowRate)
	if !ok {
		return nil, false
	}
	accumFees, ok := wad(d, rAccumProtocolFeesWads)
	if !ok {
		return nil, false
	}
	collMintSupply, ok := u64le(d, rCollMintSupply)
	if !ok {
		return nil, false
	}
	return &Reserve{
		Reserve:                   reserve,
		LendingMarket:             lendingMarket,
		LiquidityMint:             liqMint,
		MintDecimals:              d[rMintDecimals],
		LiquiditySupply:           liqSupply,
		PythOracle:                pythOracle,
		SwitchboardOracle:         sbOracle,
		CollateralMint:            collMint,
		CollateralSupply:          collSupply,
		FeeReceiver:               feeReceiver,
		MarketPrice:               marketPrice,
		LoanToValuePct:            d[rLtv],
		LiquidationBonusPct:       d[rLiqBonus],
		LiquidationThresholdPct:   d[rLiqThreshold],
		AvailableAmount:           availableAmount,
		BorrowedAmount:            borrowedAmount,
		CumulativeBorrowRate:      cumBorrowRate,
		AccumulatedProtocolFees:   accumFees,
		CollateralMintTotalSupply: collMintSupply,
	}, true
}

// CtokenExchangeRate is the cToken → underlying multiplier (Solend
// `collateral_exchange_rate`, inverted to liquidity-per-cToken so a deposit
// valuation is `ctokens × rate × price`).
//
//	total_supply = available + borrowed − accumulated_protocol_fees
//	rate         = total_supply / cToken_mint_total_supply
//
// Solend's `CollateralExchangeRate` is cTokens-per-liquidity; the liquidity
// a deposit is worth is `collateral_to_liquidity(amount) = amount / rate`,
// i.e. `amount × total_supply / mint_supply` — what we return here. Starts
// at 1.0 (INITIAL_COLLATERAL_RATE) and only grows with interest; a reserve
// with no cTokens minted (or degenerate liquidity) falls back to 1.0.
func (r *Reserve) CtokenExchangeRate() float64 {
	if r.CollateralMintTotalSupply == 0 {
		return 1.0
	}
	totalSupply := float64(r.AvailableAmount) + r.BorrowedAmount - r.AccumulatedProtocolFees
	mint := float64(r.CollateralMintTotalSupply)
	if totalSupply <= 0.0 || mint <= 0.0 {
		return 1.0
	}
	return totalSupply / mint
}

// ── Obligation (1300 bytes, layout from Solend obligation.rs Pack +
//
//	verified on a real main-pool obligation). ─────────────────────────────
const (
	oLendingMarket        = 10
	oOwner                = 42
	oDepositedValue       = 74
	oBorrowedValue        = 90
	oUnhealthyBorrowValue = 122
	oDepositsLen          = 202
	oBorrowsLen           = 203
	oDataFlat             = 204
	collateralLen         = 88  // reserve(32) + deposited_amount u64(8) + market_value(16) + pad(32)
	liquidityLen          = 112 // reserve(32) + cum_rate(16) + borrowed_wads(16) + market_value(16) + pad(32)
)

// Deposit is one obligation collateral position.
type Deposit struct {
	Reserve solana.PublicKey
	// DepositedAmount is the cToken (collateral) amount deposited.
	DepositedAmount uint64
	MarketValue     float64
}

// Borrow is one obligation debt position.
type Borrow struct {
	Reserve solana.PublicKey
	// CumulativeBorrowRate is the borrow's cumulative_borrow_rate snapshot
	// at the obligation's last refresh; dividing the reserve's current rate
	// by this accrues interest to "now".
	CumulativeBorrowRate float64
	BorrowedAmountWads   float64
	MarketValue          float64
}

// Obligation is a decoded Save borrower position.
type Obligation struct {
	LendingMarket        solana.PublicKey
	Owner                solana.PublicKey
	DepositedValue       float64
	BorrowedValue        float64
	UnhealthyBorrowValue float64
	Deposits             []Deposit
	Borrows              []Borrow
}

// DecodeObligation decodes a Save Obligation account (1300 bytes, version tag 1).
func DecodeObligation(d []byte) (*Obligation, bool) {
	if len(d) < 1300 || d[0] != 1 {
		return nil, false
	}
	nDep := int(d[oDepositsLen])
	nBor := int(d[oBorrowsLen])
	deposits := make([]Deposit, 0, nDep)
	off := oDataFlat
	for i := 0; i < nDep; i++ {
		reserve, ok := readPk(d, off)
		if !ok {
			return nil, false
		}
		amt, ok := u64le(d, off+32)
		if !ok {
			return nil, false
		}
		mv, ok := wad(d, off+40)
		if !ok {
			return nil, false
		}
		deposits = append(deposits, Deposit{Reserve: reserve, DepositedAmount: amt, MarketValue: mv})
		off += collateralLen
	}
	borrows := make([]Borrow, 0, nBor)
	for i := 0; i < nBor; i++ {
		reserve, ok := readPk(d, off)
		if !ok {
			return nil, false
		}
		cumRate, ok := wad(d, off+32)
		if !ok {
			return nil, false
		}
		borrowedWads, ok := wad(d, off+48)
		if !ok {
			return nil, false
		}
		mv, ok := wad(d, off+64)
		if !ok {
			return nil, false
		}
		borrows = append(borrows, Borrow{
			Reserve:              reserve,
			CumulativeBorrowRate: cumRate,
			BorrowedAmountWads:   borrowedWads,
			MarketValue:          mv,
		})
		off += liquidityLen
	}
	lendingMarket, ok := readPk(d, oLendingMarket)
	if !ok {
		return nil, false
	}
	owner, ok := readPk(d, oOwner)
	if !ok {
		return nil, false
	}
	depositedValue, ok := wad(d, oDepositedValue)
	if !ok {
		return nil, false
	}
	borrowedValue, ok := wad(d, oBorrowedValue)
	if !ok {
		return nil, false
	}
	unhealthyValue, ok := wad(d, oUnhealthyBorrowValue)
	if !ok {
		return nil, false
	}
	return &Obligation{
		LendingMarket:        lendingMarket,
		Owner:                owner,
		DepositedValue:       depositedValue,
		BorrowedValue:        borrowedValue,
		UnhealthyBorrowValue: unhealthyValue,
		Deposits:             deposits,
		Borrows:              borrows,
	}, true
}

// Liquidatable is liquidatable per Solend's own on-chain math: borrowed
// value has crossed the (deposit-weighted) unhealthy threshold. Both fields
// are refreshed on-chain, so this is the protocol's verdict — the fire is
// still sim-gated.
func (o *Obligation) Liquidatable() bool {
	return o.UnhealthyBorrowValue > 0.0 && o.BorrowedValue > o.UnhealthyBorrowValue
}

// HealthRatio is how far over the threshold (>1.0 = underwater), for ranking.
func (o *Obligation) HealthRatio() float64 {
	if o.UnhealthyBorrowValue == 0.0 {
		return 0.0
	}
	return o.BorrowedValue / o.UnhealthyBorrowValue
}

// FreshHealth recomputes (borrowed_value, unhealthy_borrow_value) at the
// reserves' CURRENT on-chain prices — a faithful reproduction of Solend's
// own `refresh_obligation` math. ok=false if any referenced reserve is
// missing from reserves.
//
// WHY: the STORED borrowed/unhealthy are refreshed LAZILY (only when
// someone touches the obligation), so hundreds read stale — usually
// stale-HIGH on the collateral side (priced when the collateral was worth
// more), which makes a genuinely-healthy position look liquidatable
// ("phantom"). The fire tier must gate on the value Solend's own
// `liquidate` will recompute at settle time, which is exactly this.
//
// EXACTNESS (validated live): for obligations refreshed in the same slot as
// their reserves (fresh price == the price Solend last refreshed at) this
// reproduces the stored values to 0.0000%. The pieces:
//
//	collateral: deposited_ctokens × exchange_rate × price × liq_threshold
//	            where exchange_rate = Reserve.CtokenExchangeRate().
//	debt:       borrowed_wads × (reserve_cum_rate / borrow_cum_rate) / 10^dec
//	            × price — the ratio accrues interest since the last refresh
//	            (accrual ≥ 1, so debt is never understated; clamped for
//	            safety).
//
// Solend applies a per-asset borrow weight, but for the accepted debt set
// (USDC/USDT/wSOL) it is 1.0 (verified against freshly-refreshed
// obligations).
func (o *Obligation) FreshHealth(reserves map[solana.PublicKey]*Reserve) (borrowed, unhealthy float64, ok bool) {
	for _, d := range o.Deposits {
		r, present := reserves[d.Reserve]
		if !present {
			return 0, 0, false
		}
		underlying := float64(d.DepositedAmount) * r.CtokenExchangeRate() / pow10(int(r.MintDecimals))
		unhealthy += underlying * r.MarketPrice * float64(r.LiquidationThresholdPct) / 100.0
	}
	for _, b := range o.Borrows {
		r, present := reserves[b.Reserve]
		if !present {
			return 0, 0, false
		}
		accrual := 1.0
		if b.CumulativeBorrowRate > 0.0 {
			accrual = r.CumulativeBorrowRate / b.CumulativeBorrowRate
			if accrual < 1.0 {
				accrual = 1.0
			}
		}
		native := b.BorrowedAmountWads * accrual / pow10(int(r.MintDecimals))
		borrowed += native * r.MarketPrice
	}
	return borrowed, unhealthy, true
}

// FreshLiquidatable is liquidatable at CURRENT on-chain prices (FreshHealth),
// the value Solend's `liquidate` recomputes at settle time — the
// phantom-free fire-tier verdict.
func (o *Obligation) FreshLiquidatable(reserves map[solana.PublicKey]*Reserve) bool {
	borrowed, unhealthy, ok := o.FreshHealth(reserves)
	return ok && unhealthy > 0.0 && borrowed > unhealthy
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

// LendingMarketAuthority derives the lending_market_authority PDA — seed =
// the lending market pubkey (VERIFIED: derives DdZR6zR… for the main pool,
// matching the captured liquidation tx).
func LendingMarketAuthority(lendingMarket solana.PublicKey) solana.PublicKey {
	addr, _, _ := solana.FindProgramAddress([][]byte{lendingMarket.Bytes()}, pk(SolendProgram))
	return addr
}
