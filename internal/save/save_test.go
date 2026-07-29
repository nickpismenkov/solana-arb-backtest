package save

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestLendingMarketAuthorityMatchesCapturedTx(t *testing.T) {
	// Captured liquidation txs had lending_market_authority = DdZR6zR… for
	// the main pool. This pins the PDA seed derivation.
	got := LendingMarketAuthority(pk(MainPool)).String()
	want := "DdZR6zRFiUt4S5mg7AV1uKB2z1f1WzcNYCaTEEWPAuby"
	if got != want {
		t.Fatalf("lending_market_authority mismatch: got %s want %s", got, want)
	}
}

func mkReserve(avail uint64, borrowed, fees float64, mint uint64) Reserve {
	return Reserve{
		Reserve:                   pk(USDCReserve),
		LendingMarket:             pk(MainPool),
		LiquidityMint:             pk(USDCMint),
		MintDecimals:              6,
		LiquiditySupply:           pk(USDCMint),
		PythOracle:                pk(USDCMint),
		SwitchboardOracle:         pk(USDCMint),
		CollateralMint:            pk(USDCMint),
		CollateralSupply:          pk(USDCMint),
		FeeReceiver:               pk(USDCMint),
		MarketPrice:               1.0,
		LiquidationThresholdPct:   77,
		LiquidationBonusPct:       3,
		LoanToValuePct:            70,
		AvailableAmount:           avail,
		BorrowedAmount:            borrowed,
		CumulativeBorrowRate:      1.5,
		AccumulatedProtocolFees:   fees,
		CollateralMintTotalSupply: mint,
	}
}

func TestCtokenExchangeRateReproducesCapturedReserves(t *testing.T) {
	// Captured live (main pool) at slot ~431.88M. total_supply = available +
	// borrowed − accumulated_protocol_fees; rate = total_supply / mint_supply.

	// USDC reserve.
	usdc := mkReserve(6_771_743_899_093, 16_058_963_885_683.2, 11_390_014.30, 17_550_169_954_986)
	if got := usdc.CtokenExchangeRate(); math.Abs(got-1.300881784) >= 1e-6 {
		t.Fatalf("USDC cToken rate: got %v", got)
	}
	// wSOL reserve.
	wsol := mkReserve(49_716_822_211_353, 153_950_042_692_808.8, 128_785_618.17, 171_721_525_817_724)
	if got := wsol.CtokenExchangeRate(); math.Abs(got-1.186029155) >= 1e-6 {
		t.Fatalf("wSOL cToken rate: got %v", got)
	}
	// Degenerate: no cTokens minted → INITIAL_COLLATERAL_RATE (1.0).
	if got := mkReserve(1_000, 0.0, 0.0, 0).CtokenExchangeRate(); got != 1.0 {
		t.Fatalf("degenerate cToken rate: got %v want 1.0", got)
	}
}

func TestAcceptedDebtMints(t *testing.T) {
	if !IsAcceptedDebtMint(pk(USDCMint)) {
		t.Fatal("USDC should be accepted")
	}
	if !IsAcceptedDebtMint(pk(USDTMint)) {
		t.Fatal("USDT should be accepted")
	}
	if !IsAcceptedDebtMint(pk(WSOLMint)) {
		t.Fatal("wSOL should be accepted")
	}
	// A random collateral-only mint (mSOL) is not an accepted debt asset.
	if IsAcceptedDebtMint(pk("mSoLzYCxHdYgdzU16g5QSh3i5K3z3KZK7ytfqcJm7So")) {
		t.Fatal("mSOL should not be accepted")
	}
}

func TestLiquidateIxShape(t *testing.T) {
	dummy := func(seed string) Reserve {
		return Reserve{
			Reserve:                   pk(seed),
			LendingMarket:             pk(MainPool),
			LiquidityMint:             pk(USDCMint),
			MintDecimals:              6,
			LiquiditySupply:           pk(USDCMint),
			PythOracle:                pk(USDCMint),
			SwitchboardOracle:         pk(USDCMint),
			CollateralMint:            pk(USDCMint),
			CollateralSupply:          pk(USDCMint),
			FeeReceiver:               pk(USDCMint),
			MarketPrice:               1.0,
			LiquidationThresholdPct:   77,
			LiquidationBonusPct:       3,
			LoanToValuePct:            70,
			AvailableAmount:           0,
			BorrowedAmount:            0.0,
			CumulativeBorrowRate:      1.0,
			AccumulatedProtocolFees:   0.0,
			CollateralMintTotalSupply: 0,
		}
	}
	repay := dummy(USDCReserve)
	withdraw := dummy(USDCReserve)
	ix := LiquidateAndRedeem(
		1_000_000, pk(USDCMint), pk(USDCMint), pk(USDCMint),
		&repay, &withdraw, pk(MainPool), pk(MainPool), pk(USDCMint))

	if len(ix.Accounts) != 15 {
		t.Fatalf("expected 15 accounts, got %d", len(ix.Accounts))
	}
	if ix.Data[0] != tagLiquidateAndRedeem {
		t.Fatalf("unexpected tag: %d", ix.Data[0])
	}
	if got := binary.LittleEndian.Uint64(ix.Data[1:9]); got != 1_000_000 {
		t.Fatalf("unexpected amount: %d", got)
	}
	if !ix.Accounts[13].IsSigner {
		t.Fatal("account 13 should be signer")
	}
}
