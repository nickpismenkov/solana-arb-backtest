package saveengine

import (
	"math"
	"testing"

	"arbengine/internal/save"
	"arbengine/internal/solana"
)

var reserveSeq int

func newUniquePubkey() solana.Pubkey {
	reserveSeq++
	var b [32]byte
	b[0] = byte(reserveSeq)
	b[1] = byte(reserveSeq >> 8)
	pk, _ := solana.PubkeyFromBytes(b[:])
	return pk
}

func mkReserve(mint string, price float64) save.Reserve {
	return save.Reserve{
		Reserve:       newUniquePubkey(),
		LendingMarket: solana.Pubkey{},
		LiquidityMint: solana.MustPubkeyFromBase58(mint),
		MintDecimals:  9,

		LiquiditySupply:   solana.Pubkey{},
		PythOracle:        solana.Pubkey{},
		SwitchboardOracle: solana.Pubkey{},
		CollateralMint:    solana.Pubkey{},
		CollateralSupply:  solana.Pubkey{},
		FeeReceiver:       solana.Pubkey{},

		MarketPrice:             price,
		LiquidationThresholdPct: 80,
		LiquidationBonusPct:     5,
		LoanToValuePct:          75,

		// cToken exchange rate 1.0 (no cTokens minted -> INITIAL_COLLATERAL_RATE),
		// borrow cum rate 1.0 (matches the borrows' snapshot -> accrual 1.0), so
		// FreshHealth values are exactly deposited_amount*price*thr / wads*price.
		AvailableAmount:           0,
		BorrowedAmount:            0.0,
		CumulativeBorrowRate:      1.0,
		AccumulatedProtocolFees:   0.0,
		CollateralMintTotalSupply: 0,
	}
}

// dep is a deposit whose FRESH value at price (rate 1.0, 9-dp) is usd.
func dep(reserve solana.Pubkey, usd, price float64) save.Deposit {
	return save.Deposit{Reserve: reserve, DepositedAmount: uint64(usd / price * 1e9), MarketValue: usd}
}

// bor is a borrow whose FRESH value (rate 1.0, 9-dp, $1) is usd.
func bor(reserve solana.Pubkey, usd float64) save.Borrow {
	return save.Borrow{Reserve: reserve, CumulativeBorrowRate: 1.0, BorrowedAmountWads: usd * 1e9, MarketValue: usd}
}

func fixture() (save.Obligation, map[solana.Pubkey]save.Reserve, map[solana.Pubkey]uint32) {
	// SOL collateral (feed 6), USDC debt (feed 7). Healthy at build.
	sol := mkReserve("So11111111111111111111111111111111111111112", 100.0)
	usdc := mkReserve("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", 1.0)
	reserves := map[solana.Pubkey]save.Reserve{
		sol.Reserve:  sol,
		usdc.Reserve: usdc,
	}
	obl := save.Obligation{
		LendingMarket: solana.Pubkey{}, Owner: solana.Pubkey{},
		DepositedValue: 1000.0, BorrowedValue: 700.0, UnhealthyBorrowValue: 800.0,
		Deposits: []save.Deposit{dep(sol.Reserve, 1000.0, 100.0)},
		Borrows:  []save.Borrow{bor(usdc.Reserve, 700.0)},
	}
	mintFeed := map[solana.Pubkey]uint32{
		sol.LiquidityMint:  6,
		usdc.LiquidityMint: 7,
	}
	return obl, reserves, mintFeed
}

func TestReproducesStoredHealthAtRescan(t *testing.T) {
	o, reserves, mintFeed := fixture()
	anchor := map[uint32]float64{6: 100.0, 7: 1.0}
	w, ok := BuildSolendWatch(&o, newUniquePubkey(), reserves, mintFeed, anchor)
	if !ok {
		t.Fatal("build failed")
	}
	if math.Abs(w.Borrowed(anchor)-700.0) >= 1e-9 {
		t.Errorf("borrowed = %v, want 700", w.Borrowed(anchor))
	}
	if math.Abs(w.Unhealthy(anchor)-800.0) >= 1e-9 {
		t.Errorf("unhealthy = %v, want 800", w.Unhealthy(anchor))
	}
	if w.Liquidatable(anchor) {
		t.Error("should not be liquidatable: 700 < 800")
	}
}

func TestSolDropFlipsLiquidatable(t *testing.T) {
	o, reserves, mintFeed := fixture()
	anchor := map[uint32]float64{6: 100.0, 7: 1.0}
	w, ok := BuildSolendWatch(&o, newUniquePubkey(), reserves, mintFeed, anchor)
	if !ok {
		t.Fatal("build failed")
	}
	// SOL (collateral) drops 20% -> unhealthy 800->640 < borrowed 700 -> liquidatable.
	moved := map[uint32]float64{6: 80.0, 7: 1.0}
	if math.Abs(w.Unhealthy(moved)-640.0) >= 1e-6 {
		t.Errorf("unhealthy = %v, want 640", w.Unhealthy(moved))
	}
	if !w.Liquidatable(moved) {
		t.Error("should be liquidatable after SOL drop")
	}
}

func TestRatioCapExcludesMispricedDust(t *testing.T) {
	// A dust obligation: $500 debt, ~$0 collateral -> unhealthy ~$1, ratio ~500.
	// RatioCap keeps it OUT of the watch-set so it can't starve real ones.
	sol := mkReserve("So11111111111111111111111111111111111111112", 100.0)
	usdc := mkReserve("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", 1.0)
	reserves := map[solana.Pubkey]save.Reserve{sol.Reserve: sol, usdc.Reserve: usdc}

	dustPk := newUniquePubkey()
	dust := ObligationRef{Pubkey: dustPk, Obligation: save.Obligation{
		LendingMarket: solana.Pubkey{}, Owner: solana.Pubkey{},
		DepositedValue: 1.0, BorrowedValue: 500.0, UnhealthyBorrowValue: 1.0,
		Deposits: []save.Deposit{dep(sol.Reserve, 1.0, 100.0)},
		Borrows:  []save.Borrow{bor(usdc.Reserve, 500.0)},
	}}
	realPk := newUniquePubkey()
	real := ObligationRef{Pubkey: realPk, Obligation: save.Obligation{
		LendingMarket: solana.Pubkey{}, Owner: solana.Pubkey{},
		DepositedValue: 1000.0, BorrowedValue: 810.0, UnhealthyBorrowValue: 800.0,
		Deposits: []save.Deposit{dep(sol.Reserve, 1000.0, 100.0)},
		Borrows:  []save.Borrow{bor(usdc.Reserve, 810.0)},
	}}
	mintFeed := map[solana.Pubkey]uint32{sol.LiquidityMint: 6, usdc.LiquidityMint: 7}
	anchor := map[uint32]float64{6: 100.0, 7: 1.0}
	engine := NewEngine(100.0, 3.0)
	engine.Rebuild([]ObligationRef{dust, real}, reserves, mintFeed, 0.85, anchor)
	crossed := engine.Crossed(anchor, 1.0)
	if len(crossed) != 1 || crossed[0] != realPk {
		t.Errorf("crossed = %v, want only the real near-threshold obligation, not the mis-priced dust", crossed)
	}
}

func TestOnchainTierIsTheOnchainVerdictNotTheLazerProjection(t *testing.T) {
	// Three obligations, two healthy on-chain (stored borrowed < unhealthy),
	// one underwater. A 5% SOL drop makes the Lazer projection flag ALL THREE
	// as crossed, but the FIRE tier (Solend's authoritative on-chain health)
	// only holds the genuinely-underwater one — the other two must not earn a
	// sim off a mere Lazer divergence.
	sol := mkReserve("So11111111111111111111111111111111111111112", 100.0)
	usdc := mkReserve("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", 1.0)
	reserves := map[solana.Pubkey]save.Reserve{sol.Reserve: sol, usdc.Reserve: usdc}

	// Collateral sized so the FRESH unhealthy (deposit*price*thr) equals the
	// stored unhealthy; borrow sized so FRESH borrowed equals borrowed.
	mkObl := func(borrowed, unhealthy float64) save.Obligation {
		return save.Obligation{
			LendingMarket: solana.Pubkey{}, Owner: solana.Pubkey{},
			DepositedValue: 1000.0, BorrowedValue: borrowed, UnhealthyBorrowValue: unhealthy,
			Deposits: []save.Deposit{dep(sol.Reserve, unhealthy/0.80, 100.0)},
			Borrows:  []save.Borrow{bor(usdc.Reserve, borrowed)},
		}
	}
	healthyA := ObligationRef{Pubkey: newUniquePubkey(), Obligation: mkObl(790.0, 800.0)}
	healthyB := ObligationRef{Pubkey: newUniquePubkey(), Obligation: mkObl(795.0, 800.0)}
	realPk := newUniquePubkey()
	real := ObligationRef{Pubkey: realPk, Obligation: mkObl(820.0, 800.0)}
	mintFeed := map[solana.Pubkey]uint32{sol.LiquidityMint: 6, usdc.LiquidityMint: 7}
	anchor := map[uint32]float64{6: 100.0, 7: 1.0}
	engine := NewEngine(100.0, 3.0)
	engine.Rebuild([]ObligationRef{healthyA, healthyB, real}, reserves, mintFeed, 0.85, anchor)

	// SOL drops 5% -> Lazer projection flags all three (projected unhealthy 760).
	moved := map[uint32]float64{6: 95.0, 7: 1.0}
	if len(engine.Crossed(moved, 1.0)) != 3 {
		t.Error("Lazer projection should flag all three")
	}
	// ...but the on-chain fire tier only has the truly-underwater one.
	fire := engine.OnchainLiquidatableRanked()
	if len(fire) != 1 {
		t.Fatalf("fire = %v, want only the on-chain-underwater obligation", fire)
	}
	if fire[0].Obligation != realPk {
		t.Errorf("fire[0].Obligation = %v, want %v", fire[0].Obligation, realPk)
	}
	if engine.OnchainLiquidatableCount() != 1 {
		t.Errorf("OnchainLiquidatableCount = %d, want 1", engine.OnchainLiquidatableCount())
	}
	if math.Abs(fire[0].Deficit-20.0) >= 1e-9 {
		t.Errorf("deficit = %v, want 20 (820 - 800)", fire[0].Deficit)
	}
}

func TestFireTierDropsStoredLiquidatablePhantom(t *testing.T) {
	// The core fix: an obligation the STORED verdict calls liquidatable
	// (borrowed 810 > stored-unhealthy 800) but whose collateral, valued at
	// FRESH reserve prices, is worth more than the lazy refresh captured
	// (unhealthy 960 > borrowed 810) -> genuinely healthy. It stays WATCHED
	// (stored ratio ~= 1.01 in [0.85, 3.0]) but the fire tier must NOT flag it.
	sol := mkReserve("So11111111111111111111111111111111111111112", 100.0)
	usdc := mkReserve("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", 1.0)
	reserves := map[solana.Pubkey]save.Reserve{sol.Reserve: sol, usdc.Reserve: usdc}

	o := save.Obligation{
		LendingMarket: solana.Pubkey{}, Owner: solana.Pubkey{},
		DepositedValue: 1000.0, BorrowedValue: 810.0, UnhealthyBorrowValue: 800.0,
		Deposits: []save.Deposit{dep(sol.Reserve, 1200.0, 100.0)}, // fresh coll $1200 -> unhealthy $960
		Borrows:  []save.Borrow{bor(usdc.Reserve, 810.0)},
	}
	if !o.Liquidatable() {
		t.Error("stored verdict should over-report it as liquidatable")
	}
	if o.FreshLiquidatable(reserves) {
		t.Error("fresh price should prove it healthy (810 < 960)")
	}
	mintFeed := map[solana.Pubkey]uint32{sol.LiquidityMint: 6, usdc.LiquidityMint: 7}
	anchor := map[uint32]float64{6: 100.0, 7: 1.0}
	engine := NewEngine(100.0, 3.0)
	engine.Rebuild([]ObligationRef{{Pubkey: newUniquePubkey(), Obligation: o}}, reserves, mintFeed, 0.85, anchor)
	if len(engine.Accounts) != 1 {
		t.Errorf("Accounts = %d, want 1 (still WATCHED, stored ratio in range)", len(engine.Accounts))
	}
	if engine.OnchainLiquidatableCount() != 0 {
		t.Errorf("OnchainLiquidatableCount = %d, want 0 (phantom removed)", engine.OnchainLiquidatableCount())
	}
	if len(engine.OnchainLiquidatableRanked()) != 0 {
		t.Error("OnchainLiquidatableRanked should be empty")
	}
}

func TestFreshCollateralDropFlipsFireTier(t *testing.T) {
	// Healthy at build ($100 SOL -> unhealthy $800 > borrowed $750). Rebuild with
	// the collateral reserve repriced to $90 -> unhealthy $720 < $750 -> the FRESH
	// fire tier flags it, exactly as Solend's own liquidate would at that price.
	usdc := mkReserve("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", 1.0)
	mk := func(solPx float64) (map[solana.Pubkey]save.Reserve, map[solana.Pubkey]uint32, save.Obligation) {
		sol := mkReserve("So11111111111111111111111111111111111111112", solPx)
		reserves := map[solana.Pubkey]save.Reserve{sol.Reserve: sol, usdc.Reserve: usdc}
		o := save.Obligation{
			LendingMarket: solana.Pubkey{}, Owner: solana.Pubkey{},
			DepositedValue: 1000.0, BorrowedValue: 750.0, UnhealthyBorrowValue: 800.0,
			Deposits: []save.Deposit{dep(sol.Reserve, 1000.0, 100.0)}, // 10 SOL, valued at solPx on rebuild
			Borrows:  []save.Borrow{bor(usdc.Reserve, 750.0)},
		}
		mf := map[solana.Pubkey]uint32{sol.LiquidityMint: 6, usdc.LiquidityMint: 7}
		return reserves, mf, o
	}
	anchor := map[uint32]float64{6: 100.0, 7: 1.0}
	// Build price $100 -> healthy.
	r0, mf0, o0 := mk(100.0)
	engine := NewEngine(100.0, 3.0)
	engine.Rebuild([]ObligationRef{{Pubkey: newUniquePubkey(), Obligation: o0}}, r0, mf0, 0.85, anchor)
	if engine.OnchainLiquidatableCount() != 0 {
		t.Errorf("should be healthy at $100, got count %d", engine.OnchainLiquidatableCount())
	}
	// Collateral drops to $90 -> fresh unhealthy $720 < borrowed $750 -> liquidatable.
	r1, mf1, o1 := mk(90.0)
	pk := newUniquePubkey()
	engine.Rebuild([]ObligationRef{{Pubkey: pk, Obligation: o1}}, r1, mf1, 0.85, anchor)
	if engine.OnchainLiquidatableCount() != 1 {
		t.Errorf("fire tier should flag it at $90, got count %d", engine.OnchainLiquidatableCount())
	}
	fire := engine.OnchainLiquidatableRanked()
	if len(fire) != 1 || fire[0].Obligation != pk {
		t.Fatalf("fire = %v, want [%v]", fire, pk)
	}
	if math.Abs(fire[0].Deficit-30.0) >= 1e-6 {
		t.Errorf("deficit = %v, want 30 (750 - 720)", fire[0].Deficit)
	}
}

func TestLstAnchorIsTheFeedNotReservePrice(t *testing.T) {
	// jitoSOL collateral (reserve price $115, but maps to SOL feed @ $100).
	// Anchoring on the FEED (100) means ratio=1 at rescan despite the $115
	// reserve price — no false liquidation.
	jito := mkReserve("J1toso1uCk3RLmjorhTtrVwY9HJ7X8V9yYac6Y7kGCPn", 115.0)
	usdc := mkReserve("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", 1.0)
	reserves := map[solana.Pubkey]save.Reserve{jito.Reserve: jito, usdc.Reserve: usdc}

	o := save.Obligation{
		LendingMarket: solana.Pubkey{}, Owner: solana.Pubkey{},
		DepositedValue: 1150.0, BorrowedValue: 700.0, UnhealthyBorrowValue: 800.0,
		Deposits: []save.Deposit{dep(jito.Reserve, 1150.0, 115.0)},
		Borrows:  []save.Borrow{bor(usdc.Reserve, 700.0)},
	}
	mintFeed := map[solana.Pubkey]uint32{jito.LiquidityMint: 6, usdc.LiquidityMint: 7}
	anchor := map[uint32]float64{6: 100.0, 7: 1.0}
	w, ok := BuildSolendWatch(&o, newUniquePubkey(), reserves, mintFeed, anchor)
	if !ok {
		t.Fatal("build failed")
	}
	if math.Abs(w.Unhealthy(anchor)-800.0) >= 1e-9 {
		t.Errorf("unhealthy = %v, want 800 (ratio must be 1.0 at rescan despite $115 reserve price)", w.Unhealthy(anchor))
	}
	if w.Liquidatable(anchor) {
		t.Error("should not be liquidatable")
	}
}
