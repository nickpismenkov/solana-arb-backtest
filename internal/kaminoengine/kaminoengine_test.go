package kaminoengine

import (
	"math"
	"testing"

	"arbengine/internal/kamino"
	"arbengine/internal/solana"
)

var testPkCounter byte

// newUniquePubkey mirrors Rust's Pubkey::new_unique() for test fixtures.
func newUniquePubkey() solana.Pubkey {
	testPkCounter++
	var b [32]byte
	b[0] = testPkCounter
	pk, _ := solana.PubkeyFromBytes(b[:])
	return pk
}

// mkObligation builds a v1 obligation with the given stored health, one
// collateral reserve and one debt reserve. Feed ids are wired by the
// returned reserveFeed map.
func mkObligation(collReserve, debtReserve solana.Pubkey, borrowedValue, bfAdjustedDebt, unhealthyBorrowValue float64) kamino.Obligation {
	return kamino.Obligation{
		Owner:                solana.Pubkey{},
		LendingMarket:        solana.Pubkey{},
		LastUpdateSlot:       0,
		Stale:                false,
		DepositedValue:       1000.0,
		BfAdjustedDebt:       bfAdjustedDebt,
		BorrowedValue:        borrowedValue,
		AllowedBorrowValue:   0.0,
		UnhealthyBorrowValue: unhealthyBorrowValue,
		ElevationGroup:       0,
		Deposits:             []kamino.DepositPosition{{Reserve: collReserve, Amount: 10}},
		Borrows:              []kamino.BorrowPosition{{Reserve: debtReserve, Amount: borrowedValue}},
	}
}

// fixture returns a SOL collateral (feed 6), USDC debt (feed 7) obligation
// healthy at build: bf_debt 700 < unhealthy 800.
func fixture() (kamino.Obligation, map[solana.Pubkey]uint32, solana.Pubkey, solana.Pubkey) {
	coll := newUniquePubkey()
	debt := newUniquePubkey()
	o := mkObligation(coll, debt, 700.0, 700.0, 800.0)
	reserveFeed := map[solana.Pubkey]uint32{coll: 6, debt: 7}
	return o, reserveFeed, coll, debt
}

func TestReproducesStoredHealthAtRescan(t *testing.T) {
	o, reserveFeed, _, _ := fixture()
	anchor := map[uint32]float64{6: 100.0, 7: 1.0}
	w, ok := BuildKaminoWatch(&o, newUniquePubkey(), reserveFeed, anchor)
	if !ok {
		t.Fatal("expected BuildKaminoWatch to succeed")
	}
	if math.Abs(w.BfDebt(anchor)-700.0) >= 1e-9 {
		t.Errorf("BfDebt = %v, want ~700.0", w.BfDebt(anchor))
	}
	if math.Abs(w.Unhealthy(anchor)-800.0) >= 1e-9 {
		t.Errorf("Unhealthy = %v, want ~800.0", w.Unhealthy(anchor))
	}
	if w.Liquidatable(anchor) {
		t.Error("expected not liquidatable (700 < 800)")
	}
}

func TestSolDropFlipsLiquidatable(t *testing.T) {
	o, reserveFeed, _, _ := fixture()
	anchor := map[uint32]float64{6: 100.0, 7: 1.0}
	w, ok := BuildKaminoWatch(&o, newUniquePubkey(), reserveFeed, anchor)
	if !ok {
		t.Fatal("expected BuildKaminoWatch to succeed")
	}
	// SOL (collateral) drops 20% -> unhealthy 800->640 < bf_debt 700 -> liquidatable.
	moved := map[uint32]float64{6: 80.0, 7: 1.0}
	if math.Abs(w.Unhealthy(moved)-640.0) >= 1e-6 {
		t.Errorf("Unhealthy(moved) = %v, want ~640.0", w.Unhealthy(moved))
	}
	if !w.Liquidatable(moved) {
		t.Error("expected liquidatable after SOL drop")
	}
}

func TestRatioCapExcludesMispricedDust(t *testing.T) {
	// A dust obligation: $500 bf_debt, ~$1 threshold -> ratio ~500. RatioCap
	// keeps it OUT of the watch-set so it can't starve real near-threshold ones.
	coll := newUniquePubkey()
	debt := newUniquePubkey()
	reserveFeed := map[solana.Pubkey]uint32{coll: 6, debt: 7}
	dustPk := newUniquePubkey()
	dust := ObligationEntry{Pubkey: dustPk, Obligation: mkObligation(coll, debt, 500.0, 500.0, 1.0)}
	realPk := newUniquePubkey()
	real := ObligationEntry{Pubkey: realPk, Obligation: mkObligation(coll, debt, 810.0, 810.0, 800.0)}
	anchor := map[uint32]float64{6: 100.0, 7: 1.0}
	engine := NewEngine(100.0, 3.0)
	engine.Rebuild([]ObligationEntry{dust, real}, reserveFeed, 0.85, anchor)
	crossed := engine.Crossed(anchor, 1.0)
	if len(crossed) != 1 || crossed[0] != real.Pubkey {
		t.Errorf("crossed = %v, want only the real near-threshold obligation, not the mis-priced dust", crossed)
	}
}

func TestOnChainTierExcludesLazerPhantoms(t *testing.T) {
	// The core overflag bug: Lazer LEADS Scope, so an obligation healthy at
	// the on-chain (stored) price can read as crossed on the Lazer
	// projection. The fire tier must gate on the ON-CHAIN price and stay
	// empty here.
	coll := newUniquePubkey()
	debt := newUniquePubkey()
	reserveFeed := map[solana.Pubkey]uint32{coll: 6, debt: 7}
	// Healthy on-chain: bf_debt 780 < unhealthy 800 (ratio 0.975, in watch band).
	phantomPk := newUniquePubkey()
	phantom := ObligationEntry{Pubkey: phantomPk, Obligation: mkObligation(coll, debt, 780.0, 780.0, 800.0)}
	// Genuinely underwater on-chain: bf_debt 820 > unhealthy 800.
	realPk := newUniquePubkey()
	real := ObligationEntry{Pubkey: realPk, Obligation: mkObligation(coll, debt, 820.0, 820.0, 800.0)}
	anchor := map[uint32]float64{6: 100.0, 7: 1.0}
	engine := NewEngine(100.0, 3.0)
	engine.Rebuild([]ObligationEntry{phantom, real}, reserveFeed, 0.9, anchor)
	// Lazer drops collateral 5% -> both project as crossed (unhealthy 800->760).
	lazerMoved := map[uint32]float64{6: 95.0, 7: 1.0}
	if got := engine.Crossed(lazerMoved, 1.0); len(got) != 2 {
		t.Errorf("Crossed (Lazer-projected) = %d entries, want 2 (over-flag)", len(got))
	}
	// But the on-chain tier — anchored on stored health — fires ONLY the real one.
	fire := engine.OnChainLiquidatableRanked()
	if len(fire) != 1 {
		t.Fatalf("OnChainLiquidatableRanked = %d entries, want 1 (on-chain tier drops the phantom)", len(fire))
	}
	if fire[0].Obligation != real.Pubkey {
		t.Errorf("fire[0].Obligation = %v, want real obligation", fire[0].Obligation)
	}
	if got := engine.OnChainLiquidatableCount(); got != 1 {
		t.Errorf("OnChainLiquidatableCount = %d, want 1", got)
	}
}

func TestLstAnchorIsTheFeedNotReservePrice(t *testing.T) {
	// jitoSOL collateral maps to the SOL feed (id 6). The stored
	// unhealthy_borrow_value was computed on-chain from jitoSOL's Scope
	// price (which carries the staking premium), but the ratio anchors on
	// the FEED price — so ratio = 1.0 at rescan regardless of the premium,
	// no false liquidation.
	coll := newUniquePubkey()
	debt := newUniquePubkey()
	reserveFeed := map[solana.Pubkey]uint32{coll: 6, debt: 7}
	o := mkObligation(coll, debt, 700.0, 700.0, 800.0)
	anchor := map[uint32]float64{6: 100.0, 7: 1.0}
	w, ok := BuildKaminoWatch(&o, newUniquePubkey(), reserveFeed, anchor)
	if !ok {
		t.Fatal("expected BuildKaminoWatch to succeed")
	}
	if math.Abs(w.Unhealthy(anchor)-800.0) >= 1e-9 {
		t.Errorf("Unhealthy(anchor) = %v, want ~800.0 (ratio must be 1.0 at rescan, feed-anchored)", w.Unhealthy(anchor))
	}
	if w.Liquidatable(anchor) {
		t.Error("expected not liquidatable at rescan")
	}
	// A 10% SOL-feed drop tracks the collateral even though the reserve
	// price is a premium jitoSOL price we never touch.
	moved := map[uint32]float64{6: 90.0, 7: 1.0}
	if math.Abs(w.Unhealthy(moved)-720.0) >= 1e-6 {
		t.Errorf("Unhealthy(moved) = %v, want ~720.0", w.Unhealthy(moved))
	}
}
