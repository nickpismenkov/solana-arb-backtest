package liqengine

import (
	"testing"

	"arbengine/internal/liquidation"
	"arbengine/internal/solana"
)

var uniqueCounter uint64

// newUniquePubkey mirrors Rust's Pubkey::new_unique() for test fixtures: a
// distinct, deterministic pubkey each call.
func newUniquePubkey() solana.Pubkey {
	uniqueCounter++
	var b [32]byte
	v := uniqueCounter
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * i))
	}
	pk, _ := solana.PubkeyFromBytes(b[:])
	return pk
}

func mkBank(mint solana.Pubkey, dec uint8, wa, wl float64) liquidation.Bank {
	return liquidation.Bank{
		Mint: mint, MintDecimals: dec,
		AssetShareValue: 1.0, LiabilityShareValue: 1.0,
		AssetWeightInit: wa, AssetWeightMaint: wa,
		LiabilityWeightInit: wl, LiabilityWeightMaint: wl,
		OracleSetup: 3, OracleKey: solana.Pubkey{}, OracleMaxAge: 0,
		EmodeTag: 0, EmodeEntries: nil,
	}
}

func directOf(m map[solana.Pubkey]uint32) map[solana.Pubkey]struct{} {
	out := make(map[solana.Pubkey]struct{}, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out
}

// fixture builds an account with SOL collateral (Lazer feed 6) and USDC debt (baseline).
func fixture() (liquidation.MarginfiAccount, liquidation.BankMap, liquidation.PriceMap, map[solana.Pubkey]uint32) {
	solBank := newUniquePubkey()
	usdcBank := newUniquePubkey()
	solMint := newUniquePubkey()
	usdcMint := newUniquePubkey()
	banks := liquidation.BankMap{}
	solB := mkBank(solMint, 9, 0.8, 1.0)
	usdcB := mkBank(usdcMint, 6, 1.0, 1.1)
	banks[solBank] = &solB
	banks[usdcBank] = &usdcB
	acct := liquidation.MarginfiAccount{
		Group: solana.Pubkey{}, Authority: newUniquePubkey(),
		Balances: []liquidation.Balance{
			{BankPk: solBank, AssetShares: 10.0 * 1e9, LiabilityShares: 0.0},
			{BankPk: usdcBank, AssetShares: 0.0, LiabilityShares: 700.0 * 1e6},
		},
	}
	baseline := liquidation.PriceMap{}
	baseline[solBank] = 100.0 // will be overridden by Lazer feed 6
	baseline[usdcBank] = 1.0
	mintFeed := map[solana.Pubkey]uint32{solMint: 6}
	return acct, banks, baseline, mintFeed
}

func TestEngineHealthMatchesMaintenanceHealth(t *testing.T) {
	acct, banks, baseline, mintFeed := fixture()
	// Reference: MaintenanceHealth with SOL @ $92.
	prices := liquidation.PriceMap{}
	for k, v := range baseline {
		prices[k] = v
	}
	var solBank solana.Pubkey
	for pk, b := range banks {
		if b.MintDecimals == 9 {
			solBank = pk
		}
	}
	prices[solBank] = 92.0
	reference := liquidation.MaintenanceHealth(&acct, banks, prices).Health

	lazer := map[uint32]float64{6: 92.0}
	wa := BuildWatchAccount(&acct, banks, baseline, mintFeed, directOf(mintFeed), lazer)
	engineH := wa.Health(lazer)
	if diff := engineH.WeightedAssets - reference.WeightedAssets; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("assets %v vs %v", engineH.WeightedAssets, reference.WeightedAssets)
	}
	if diff := engineH.WeightedLiabilities - reference.WeightedLiabilities; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("liabilities %v vs %v", engineH.WeightedLiabilities, reference.WeightedLiabilities)
	}
	if diff := engineH.Ratio() - reference.Ratio(); diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("ratio %v vs %v", engineH.Ratio(), reference.Ratio())
	}
}

func TestTickFlipsToLiquidatable(t *testing.T) {
	acct, banks, baseline, mintFeed := fixture()
	engine := NewEngine(50.0)
	// Healthy at $100 (10 SOL x 0.8 x 100 = $800 assets vs 700x1.1=$770 -> ratio .96).
	engine.Rebuild([]struct {
		Pubkey  solana.Pubkey
		Account liquidation.MarginfiAccount
	}{{newUniquePubkey(), acct}}, banks, baseline, mintFeed,
		directOf(mintFeed), map[uint32]float64{6: 100.0}, 0.85)
	if got := len(engine.Crossed(map[uint32]float64{6: 100.0}, 1.0)); got != 0 {
		t.Fatalf("expected 0 crossed, got %d", got)
	}
	// SOL drops to $90 -> assets 10x0.8x90=$720 < $770 -> liquidatable.
	if got := len(engine.Crossed(map[uint32]float64{6: 90.0}, 1.0)); got != 1 {
		t.Fatalf("expected 1 crossed, got %d", got)
	}
}

func TestLSTBankIsAnchoredToBaselineNotRawFeed(t *testing.T) {
	// LST collateral (mapped to feed 6 but NOT in direct): its oracle values
	// it at $130 while raw SOL trades at $100. Health must match
	// MaintenanceHealth at the $130 baseline (not read 23% under), and a SOL
	// tick must move it proportionally.
	lstBank := newUniquePubkey()
	usdcBank := newUniquePubkey()
	lstMint := newUniquePubkey()
	usdcMint := newUniquePubkey()
	banks := liquidation.BankMap{}
	lstB := mkBank(lstMint, 9, 0.8, 1.0)
	usdcB := mkBank(usdcMint, 6, 1.0, 1.1)
	banks[lstBank] = &lstB
	banks[usdcBank] = &usdcB
	acct := liquidation.MarginfiAccount{
		Group: solana.Pubkey{}, Authority: newUniquePubkey(),
		Balances: []liquidation.Balance{
			{BankPk: lstBank, AssetShares: 10.0 * 1e9, LiabilityShares: 0.0},
			{BankPk: usdcBank, AssetShares: 0.0, LiabilityShares: 900.0 * 1e6},
		},
	}
	baseline := liquidation.PriceMap{}
	baseline[lstBank] = 130.0
	baseline[usdcBank] = 1.0
	mintFeed := map[solana.Pubkey]uint32{lstMint: 6}
	direct := map[solana.Pubkey]struct{}{} // LST is not 1:1

	lazer := map[uint32]float64{6: 100.0}
	wa := BuildWatchAccount(&acct, banks, baseline, mintFeed, direct, lazer)
	// At build-time prices the engine must agree with MaintenanceHealth at
	// the ORACLE's $130 (assets 10x0.8x130 = $1040 vs liabs $990 ->
	// healthy), not the raw feed's $100 (assets $800 -> phantom-underwater).
	reference := liquidation.MaintenanceHealth(&acct, banks, baseline).Health
	h := wa.Health(lazer)
	if diff := h.WeightedAssets - reference.WeightedAssets; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("assets %v vs %v", h.WeightedAssets, reference.WeightedAssets)
	}
	if h.Liquidatable() {
		t.Fatalf("healthy LST account must not be flagged")
	}
	// SOL -10% -> LST tracks proportionally (130 x 0.9 = 117): 10x0.8x117 =
	// $936 < $990 -> now genuinely liquidatable.
	dropped := wa.Health(map[uint32]float64{6: 90.0})
	if diff := dropped.WeightedAssets - 936.0; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("dropped assets %v vs 936.0", dropped.WeightedAssets)
	}
	if !dropped.Liquidatable() {
		t.Fatalf("expected liquidatable after drop")
	}
}

func TestIncompleteNeverCrosses(t *testing.T) {
	acct, banks, baseline, mintFeed := fixture()
	// Add a balance on an unknown bank -> incomplete.
	acct.Balances = append(acct.Balances, liquidation.Balance{BankPk: newUniquePubkey(), AssetShares: 0.0, LiabilityShares: 1e6})
	wa := BuildWatchAccount(&acct, banks, baseline, mintFeed, directOf(mintFeed), map[uint32]float64{6: 50.0})
	if wa.Complete {
		t.Fatalf("expected incomplete")
	}
	engine := NewEngine(50.0)
	engine.Rebuild([]struct {
		Pubkey  solana.Pubkey
		Account liquidation.MarginfiAccount
	}{{newUniquePubkey(), acct}}, banks, baseline, mintFeed,
		directOf(mintFeed), map[uint32]float64{6: 50.0}, 0.0)
	if engine.Len() != 0 {
		t.Fatalf("expected incomplete filtered out, got %d", engine.Len())
	}
}
