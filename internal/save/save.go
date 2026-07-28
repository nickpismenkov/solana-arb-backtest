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
	"math"

	"arbengine/internal/solana"
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
var SCANPools = []string{MainPool, IsoPool1}

// DebtReserves is the set of accepted-debt reserves across all SCANPools
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
func IsAcceptedDebtMint(mint solana.Pubkey) bool {
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

func pk(s string) solana.Pubkey { return solana.MustPubkeyFromBase58(s) }

func readPk(d []byte, o int) (solana.Pubkey, bool) {
	if o < 0 || o+32 > len(d) {
		return solana.Pubkey{}, false
	}
	p, err := solana.PubkeyFromBytes(d[o : o+32])
	if err != nil {
		return solana.Pubkey{}, false
	}
	return p, true
}

func u64le(d []byte, o int) (uint64, bool) {
	if o < 0 || o+8 > len(d) {
		return 0, false
	}
	return binary.LittleEndian.Uint64(d[o : o+8]), true
}

// wad decodes a Solend Decimal / scaled value: a u128 with WAD = 1e18.
func wad(d []byte, o int) (float64, bool) {
	if o < 0 || o+16 > len(d) {
		return 0, false
	}
	// u128 little-endian: low 8 bytes then high 8 bytes.
	lo := binary.LittleEndian.Uint64(d[o : o+8])
	hi := binary.LittleEndian.Uint64(d[o+8 : o+16])
	return u128ToFloat64(lo, hi) / 1e18, true
}

// u128ToFloat64 converts a little-endian-decoded u128 (given as low/high
// uint64 halves) to float64, matching Rust's `u128::from_le_bytes(..) as f64`.
func u128ToFloat64(lo, hi uint64) float64 {
	if hi == 0 {
		return float64(lo)
	}
	return float64(hi)*18446744073709551616.0 + float64(lo) // hi * 2^64 + lo
}

// ── Reserve (619 bytes, layout from Solend reserve.rs Pack + verified on the
//
//	USDC reserve). ─────────────────────────────────────────────────────────
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
	rLTV             = 300
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

// Reserve holds every account a refresh/liquidate touches for one reserve,
// pulled from the reserve bytes — mirrors Kamino's ReserveAccounts pattern.
type Reserve struct {
	Reserve                 solana.Pubkey
	LendingMarket           solana.Pubkey
	LiquidityMint           solana.Pubkey
	MintDecimals            uint8
	LiquiditySupply         solana.Pubkey
	PythOracle              solana.Pubkey
	SwitchboardOracle       solana.Pubkey
	CollateralMint          solana.Pubkey
	CollateralSupply        solana.Pubkey
	FeeReceiver             solana.Pubkey
	MarketPrice             float64
	LiquidationThresholdPct uint8
	LiquidationBonusPct     uint8
	LoanToValuePct          uint8

	// ── cToken exchange-rate inputs (fresh-price collateral valuation) ──────
	// AvailableAmount is liquidity available in the reserve (base units).
	AvailableAmount uint64
	// BorrowedAmount is liquidity borrowed (whole native units; WAD
	// decoded), accrues interest.
	BorrowedAmount float64
	// CumulativeBorrowRate is the cumulative borrow rate (WAD decoded),
	// monotone ↑ — accrues each borrow's principal to "now" when
	// reproducing Solend's refresh_obligation.
	CumulativeBorrowRate float64
	// AccumulatedProtocolFees are protocol fees accrued but not yet claimed
	// (whole units; WAD decoded). Netted out of total_supply, exactly like
	// Solend's own math.
	AccumulatedProtocolFees float64
	// CollateralMintTotalSupply is the cToken mint total supply (base
	// units) — denominator of the exchange rate.
	CollateralMintTotalSupply uint64
}

// DecodeReserve decodes a Save/Solend Reserve account. Returns false if the
// data is too short or the Pack version byte isn't 1.
func DecodeReserve(reserve solana.Pubkey, d []byte) (Reserve, bool) {
	if len(d) < 619 || d[0] != 1 {
		return Reserve{}, false
	}
	var r Reserve
	var ok bool
	r.Reserve = reserve
	if r.LendingMarket, ok = readPk(d, rLendingMarket); !ok {
		return Reserve{}, false
	}
	if r.LiquidityMint, ok = readPk(d, rLiqMint); !ok {
		return Reserve{}, false
	}
	r.MintDecimals = d[rMintDecimals]
	if r.LiquiditySupply, ok = readPk(d, rLiqSupply); !ok {
		return Reserve{}, false
	}
	if r.PythOracle, ok = readPk(d, rPythOracle); !ok {
		return Reserve{}, false
	}
	if r.SwitchboardOracle, ok = readPk(d, rSbOracle); !ok {
		return Reserve{}, false
	}
	if r.CollateralMint, ok = readPk(d, rCollMint); !ok {
		return Reserve{}, false
	}
	if r.CollateralSupply, ok = readPk(d, rCollSupply); !ok {
		return Reserve{}, false
	}
	if r.FeeReceiver, ok = readPk(d, rFeeReceiver); !ok {
		return Reserve{}, false
	}
	if r.MarketPrice, ok = wad(d, rMarketPrice); !ok {
		return Reserve{}, false
	}
	r.LoanToValuePct = d[rLTV]
	r.LiquidationBonusPct = d[rLiqBonus]
	r.LiquidationThresholdPct = d[rLiqThreshold]
	if r.AvailableAmount, ok = u64le(d, rAvailableAmount); !ok {
		return Reserve{}, false
	}
	if r.BorrowedAmount, ok = wad(d, rBorrowedWads); !ok {
		return Reserve{}, false
	}
	if r.CumulativeBorrowRate, ok = wad(d, rCumBorrowRate); !ok {
		return Reserve{}, false
	}
	if r.AccumulatedProtocolFees, ok = wad(d, rAccumProtocolFeesWads); !ok {
		return Reserve{}, false
	}
	if r.CollateralMintTotalSupply, ok = u64le(d, rCollMintSupply); !ok {
		return Reserve{}, false
	}
	return r, true
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
func (r Reserve) CtokenExchangeRate() float64 {
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

// ── Obligation (1300 bytes, layout from Solend obligation.rs Pack + verified
//
//	on a real main-pool obligation). ──────────────────────────────────────
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

// Deposit is one collateral deposit within an Obligation.
type Deposit struct {
	Reserve solana.Pubkey
	// DepositedAmount is the cToken (collateral) amount deposited.
	DepositedAmount uint64
	MarketValue     float64
}

// Borrow is one debt position within an Obligation.
type Borrow struct {
	Reserve solana.Pubkey
	// CumulativeBorrowRate is the borrow's cumulative_borrow_rate snapshot
	// at the obligation's last refresh; dividing the reserve's current rate
	// by this accrues interest to "now".
	CumulativeBorrowRate float64
	BorrowedAmountWads   float64
	MarketValue          float64
}

// Obligation is a decoded Save/Solend Obligation account.
type Obligation struct {
	LendingMarket        solana.Pubkey
	Owner                solana.Pubkey
	DepositedValue       float64
	BorrowedValue        float64
	UnhealthyBorrowValue float64
	Deposits             []Deposit
	Borrows              []Borrow
}

// DecodeObligation decodes a Save/Solend Obligation account. Returns false
// if the data is too short or the Pack version byte isn't 1.
func DecodeObligation(d []byte) (Obligation, bool) {
	if len(d) < 1300 || d[0] != 1 {
		return Obligation{}, false
	}
	nDep := int(d[oDepositsLen])
	nBor := int(d[oBorrowsLen])
	deposits := make([]Deposit, 0, nDep)
	off := oDataFlat
	for i := 0; i < nDep; i++ {
		reserve, ok := readPk(d, off)
		if !ok {
			return Obligation{}, false
		}
		depositedAmount, ok := u64le(d, off+32)
		if !ok {
			return Obligation{}, false
		}
		marketValue, ok := wad(d, off+40)
		if !ok {
			return Obligation{}, false
		}
		deposits = append(deposits, Deposit{
			Reserve:         reserve,
			DepositedAmount: depositedAmount,
			MarketValue:     marketValue,
		})
		off += collateralLen
	}
	borrows := make([]Borrow, 0, nBor)
	for i := 0; i < nBor; i++ {
		reserve, ok := readPk(d, off)
		if !ok {
			return Obligation{}, false
		}
		cumRate, ok := wad(d, off+32)
		if !ok {
			return Obligation{}, false
		}
		borrowedWads, ok := wad(d, off+48)
		if !ok {
			return Obligation{}, false
		}
		marketValue, ok := wad(d, off+64)
		if !ok {
			return Obligation{}, false
		}
		borrows = append(borrows, Borrow{
			Reserve:              reserve,
			CumulativeBorrowRate: cumRate,
			BorrowedAmountWads:   borrowedWads,
			MarketValue:          marketValue,
		})
		off += liquidityLen
	}

	lendingMarket, ok := readPk(d, oLendingMarket)
	if !ok {
		return Obligation{}, false
	}
	owner, ok := readPk(d, oOwner)
	if !ok {
		return Obligation{}, false
	}
	depositedValue, ok := wad(d, oDepositedValue)
	if !ok {
		return Obligation{}, false
	}
	borrowedValue, ok := wad(d, oBorrowedValue)
	if !ok {
		return Obligation{}, false
	}
	unhealthyBorrowValue, ok := wad(d, oUnhealthyBorrowValue)
	if !ok {
		return Obligation{}, false
	}

	return Obligation{
		LendingMarket:        lendingMarket,
		Owner:                owner,
		DepositedValue:       depositedValue,
		BorrowedValue:        borrowedValue,
		UnhealthyBorrowValue: unhealthyBorrowValue,
		Deposits:             deposits,
		Borrows:              borrows,
	}, true
}

// Liquidatable reports whether the obligation is liquidatable per Solend's
// own on-chain math: borrowed value has crossed the (deposit-weighted)
// unhealthy threshold. Both fields are refreshed on-chain, so this is the
// protocol's verdict — the fire is still sim-gated.
func (o *Obligation) Liquidatable() bool {
	return o.UnhealthyBorrowValue > 0.0 && o.BorrowedValue > o.UnhealthyBorrowValue
}

// HealthRatio reports how far over the threshold the obligation is (>1.0 =
// underwater), for ranking.
func (o *Obligation) HealthRatio() float64 {
	if o.UnhealthyBorrowValue == 0.0 {
		return 0.0
	}
	return o.BorrowedValue / o.UnhealthyBorrowValue
}

// FreshHealth recomputes (borrowedValue, unhealthyBorrowValue) at the
// reserves' CURRENT on-chain prices — a faithful reproduction of Solend's
// own `refresh_obligation` math. Returns ok=false if any referenced reserve
// is missing from reserves.
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
//	collateral: `deposited_ctokens × exchange_rate × price × liq_threshold`
//	            where exchange_rate = Reserve.CtokenExchangeRate().
//	debt:       `borrowed_wads × (reserve_cum_rate / borrow_cum_rate) / 10^dec
//	            × price` — the ratio accrues interest since the last refresh
//	            (accrual ≥ 1, so debt is never understated; clamped for
//	            safety).
//
// Solend applies a per-asset borrow weight, but for the accepted debt set
// (USDC/USDT/wSOL) it is 1.0 (verified against freshly-refreshed
// obligations).
func (o *Obligation) FreshHealth(reserves map[solana.Pubkey]Reserve) (borrowed, unhealthy float64, ok bool) {
	for _, d := range o.Deposits {
		r, found := reserves[d.Reserve]
		if !found {
			return 0, 0, false
		}
		underlying := float64(d.DepositedAmount) * r.CtokenExchangeRate() / math.Pow(10, float64(r.MintDecimals))
		unhealthy += underlying * r.MarketPrice * float64(r.LiquidationThresholdPct) / 100.0
	}
	for _, b := range o.Borrows {
		r, found := reserves[b.Reserve]
		if !found {
			return 0, 0, false
		}
		accrual := 1.0
		if b.CumulativeBorrowRate > 0.0 {
			accrual = r.CumulativeBorrowRate / b.CumulativeBorrowRate
			if accrual < 1.0 {
				accrual = 1.0
			}
		}
		native := b.BorrowedAmountWads * accrual / math.Pow(10, float64(r.MintDecimals))
		borrowed += native * r.MarketPrice
	}
	return borrowed, unhealthy, true
}

// FreshLiquidatable reports whether the obligation is liquidatable at
// CURRENT on-chain prices (FreshHealth), the value Solend's `liquidate`
// recomputes at settle time — the phantom-free fire-tier verdict.
func (o *Obligation) FreshLiquidatable(reserves map[solana.Pubkey]Reserve) bool {
	borrowed, unhealthy, ok := o.FreshHealth(reserves)
	return ok && unhealthy > 0.0 && borrowed > unhealthy
}

// LendingMarketAuthority derives the lending_market_authority PDA — seed =
// the lending market pubkey (VERIFIED: derives DdZR6zR… for the main pool,
// matching the captured liquidation tx).
func LendingMarketAuthority(lendingMarket solana.Pubkey) solana.Pubkey {
	addr, _ := solana.FindProgramAddress([][]byte{lendingMarket.Bytes()}, pk(SolendProgram))
	return addr
}

// ── Instruction builders (tags + account orders from the captured txs) ─────

// RefreshReserve builds refresh_reserve (tag 3):
// [reserve(w), pyth_oracle, switchboard_oracle].
func RefreshReserve(r *Reserve) solana.Instruction {
	return solana.Instruction{
		ProgramID: pk(SolendProgram),
		Accounts: []solana.AccountMeta{
			solana.Writable(r.Reserve),
			solana.ReadonlyMeta(r.PythOracle),
			solana.ReadonlyMeta(r.SwitchboardOracle),
		},
		Data: []byte{tagRefreshReserve},
	}
}

// RefreshObligation builds refresh_obligation (tag 7):
// [obligation(w), then each deposit reserve, then each borrow reserve — in
// obligation order].
func RefreshObligation(obligation solana.Pubkey, depositReserves, borrowReserves []solana.Pubkey) solana.Instruction {
	accounts := make([]solana.AccountMeta, 0, 1+len(depositReserves)+len(borrowReserves))
	accounts = append(accounts, solana.Writable(obligation))
	for _, r := range depositReserves {
		accounts = append(accounts, solana.Writable(r))
	}
	for _, r := range borrowReserves {
		accounts = append(accounts, solana.Writable(r))
	}
	return solana.Instruction{ProgramID: pk(SolendProgram), Accounts: accounts, Data: []byte{tagRefreshObligation}}
}

// LiquidateAndRedeem builds liquidate_obligation_and_redeem_reserve_collateral
// (tag 17). Repays liquidityAmount of the borrow (the debt asset —
// USDC/USDT/wSOL) and seizes+redeems the withdraw reserve's collateral to
// underlying, into the liquidator's accounts. The account order is
// asset-agnostic and verbatim from the captured txs (15 accounts).
func LiquidateAndRedeem(
	liquidityAmount uint64,
	sourceLiquidity solana.Pubkey, // liquidator debt-asset ATA (repay)
	destCollateral solana.Pubkey, // liquidator cToken ATA
	destLiquidity solana.Pubkey, // liquidator underlying ATA (redeemed collateral)
	repayReserve *Reserve, // the borrow (debt) reserve
	withdrawReserve *Reserve, // the collateral reserve being seized
	obligation solana.Pubkey,
	lendingMarket solana.Pubkey,
	userTransferAuthority solana.Pubkey, // signer
) solana.Instruction {
	data := make([]byte, 0, 9)
	data = append(data, tagLiquidateAndRedeem)
	var amt [8]byte
	binary.LittleEndian.PutUint64(amt[:], liquidityAmount)
	data = append(data, amt[:]...)

	return solana.Instruction{
		ProgramID: pk(SolendProgram),
		Accounts: []solana.AccountMeta{
			solana.Writable(sourceLiquidity),                           // 0
			solana.Writable(destCollateral),                            // 1
			solana.Writable(destLiquidity),                             // 2
			solana.Writable(repayReserve.Reserve),                      // 3
			solana.Writable(repayReserve.LiquiditySupply),              // 4
			solana.Writable(withdrawReserve.Reserve),                   // 5
			solana.Writable(withdrawReserve.CollateralMint),            // 6
			solana.Writable(withdrawReserve.CollateralSupply),          // 7
			solana.Writable(withdrawReserve.LiquiditySupply),           // 8
			solana.Writable(withdrawReserve.FeeReceiver),               // 9
			solana.Writable(obligation),                                // 10
			solana.ReadonlyMeta(lendingMarket),                         // 11
			solana.ReadonlyMeta(LendingMarketAuthority(lendingMarket)), // 12
			solana.WritableSigner(userTransferAuthority),               // 13 signer
			solana.ReadonlyMeta(pk(tokenProgram)),                      // 14
		},
		Data: data,
	}
}
