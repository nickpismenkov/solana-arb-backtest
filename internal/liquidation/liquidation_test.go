package liquidation

import (
	"encoding/binary"
	"math"
	"testing"

	"arbengine/internal/solana"
)

func TestI80F48One(t *testing.T) {
	var buf [16]byte
	// (1i128 << 48).to_le_bytes()
	binary.LittleEndian.PutUint64(buf[0:8], uint64(1)<<48)
	binary.LittleEndian.PutUint64(buf[8:16], 0)
	if math.Abs(I80F48ToF64(buf[:])-1.0) >= 1e-12 {
		t.Fatalf("I80F48ToF64(1<<48) = %v, want 1.0", I80F48ToF64(buf[:]))
	}
}

func TestHealthLiquidatableWhenLiabsExceedAssets(t *testing.T) {
	h := Health{WeightedAssets: 100.0, WeightedLiabilities: 101.0}
	if !h.Liquidatable() {
		t.Fatal("expected liquidatable")
	}
	if !(h.Value() < 0.0) {
		t.Fatal("expected value < 0")
	}
	h2 := Health{WeightedAssets: 100.0, WeightedLiabilities: 90.0}
	if h2.Liquidatable() {
		t.Fatal("expected not liquidatable")
	}
}

func testBank(tag uint16, baseMaint float64, entries []EmodeEntry) *Bank {
	return &Bank{
		Mint: solana.Pubkey{}, MintDecimals: 6,
		AssetShareValue: 1.0, LiabilityShareValue: 1.0,
		AssetWeightInit: baseMaint, AssetWeightMaint: baseMaint,
		LiabilityWeightInit: 1.05, LiabilityWeightMaint: 1.05,
		OracleSetup: 3, OracleKey: solana.Pubkey{}, OracleMaxAge: 0,
		EmodeTag: tag, EmodeEntries: entries,
	}
}

// Reproduces the verified mainnet case: collateral tag 619, base maint
// 0.65, borrowing USDC which grants tag 619 -> 0.99. The boost must apply.
func TestEmodeBoostAppliesWithMatchingLiability(t *testing.T) {
	collat := testBank(619, 0.65, nil)
	usdc := testBank(57481, 1.0, []EmodeEntry{{CollateralTag: 619, AssetWeightInit: 0.94, AssetWeightMaint: 0.99}})
	got := EffectiveAssetWeightMaint(collat, []*Bank{usdc})
	if math.Abs(got-0.99) >= 1e-9 {
		t.Fatalf("got %v, want ~0.99", got)
	}
}

func TestEmodeNoBoostWithoutMatchingEntry(t *testing.T) {
	collat := testBank(871, 0.65, nil) // tag not offered by this liability
	usdc := testBank(57481, 1.0, []EmodeEntry{{CollateralTag: 619, AssetWeightInit: 0.94, AssetWeightMaint: 0.99}})
	got := EffectiveAssetWeightMaint(collat, []*Bank{usdc})
	if got != 0.65 {
		t.Fatalf("got %v, want 0.65", got)
	}
}

// Intersection rule: emode applies only if EVERY borrowed liability grants it.
func TestEmodeRequiresAllLiabilitiesToGrant(t *testing.T) {
	collat := testBank(619, 0.65, nil)
	usdc := testBank(57481, 1.0, []EmodeEntry{{CollateralTag: 619, AssetWeightInit: 0.94, AssetWeightMaint: 0.99}})
	other := testBank(42, 1.0, nil) // second borrow with no emode -> disqualifies
	got := EffectiveAssetWeightMaint(collat, []*Bank{usdc, other})
	if got != 0.65 {
		t.Fatalf("got %v, want 0.65", got)
	}
}

func TestEmodeUntaggedCollateralNeverBoosts(t *testing.T) {
	collat := testBank(0, 0.65, nil)
	usdc := testBank(57481, 1.0, []EmodeEntry{{CollateralTag: 0, AssetWeightInit: 0.94, AssetWeightMaint: 0.99}})
	got := EffectiveAssetWeightMaint(collat, []*Bank{usdc})
	if got != 0.65 {
		t.Fatalf("got %v, want 0.65", got)
	}
}

// A synthetic Switchboard PullFeed account: disc + value@56 + result-slot@40.
func sbFeed(priceE18 int64, resultSlot uint64) []byte {
	d := make([]byte, sbPullResult+16)
	copy(d[:8], SwitchboardPullDisc[:])
	binary.LittleEndian.PutUint64(d[sbPullResultSlot:sbPullResultSlot+8], resultSlot)
	// price_e18 as i128 le bytes (sign-extended into hi word).
	lo := uint64(priceE18)
	var hi uint64
	if priceE18 < 0 {
		hi = math.MaxUint64
	}
	binary.LittleEndian.PutUint64(d[sbPullResult:sbPullResult+8], lo)
	binary.LittleEndian.PutUint64(d[sbPullResult+8:sbPullResult+16], hi)
	return d
}

func TestSwitchboardStalePriceIsDroppedButFreshSurvives(t *testing.T) {
	price := int64(5 * pow10(18)) // $5.00 * 1e18
	feedFresh := sbFeed(price, 1_000_000)
	feedStale := sbFeed(price, 900_000)
	now := uint64(1_001_000) // fresh is 1k slots behind, stale is 101k behind

	// Fresh feed: within the ceiling -> price flows.
	got, ok := DecodeOraclePriceFresh(feedFresh, now, DefaultMaxSBStaleSlots)
	if !ok || got != 5.0 {
		t.Fatalf("fresh: got (%v, %v), want (5.0, true)", got, ok)
	}
	// Stale feed: beyond the ceiling -> not ok, so the account reads
	// missing and is never trusted (mirrors the chain's 6049 gate).
	_, ok = DecodeOraclePriceFresh(feedStale, now, DefaultMaxSBStaleSlots)
	if ok {
		t.Fatal("stale: expected not ok")
	}
	// Gate disabled (slot 0): price flows regardless of age (back-compat).
	got, ok = DecodeOraclePriceFresh(feedStale, 0, DefaultMaxSBStaleSlots)
	if !ok || got != 5.0 {
		t.Fatalf("gate disabled: got (%v, %v), want (5.0, true)", got, ok)
	}
}

func pow10(n int) int64 {
	v := int64(1)
	for i := 0; i < n; i++ {
		v *= 10
	}
	return v
}
