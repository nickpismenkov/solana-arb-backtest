// Save (Solend) liquidation instructions — tags + account orders taken
// verbatim from the captured mainnet liquidation txs (4tQm9zcd… and
// 2inNexup…, both identical). Save is a NATIVE program (not Anchor), so each
// instruction is a one-byte tag rather than an 8-byte discriminator.
package save

import (
	"encoding/binary"

	"github.com/gagliardetto/solana-go"
)

// RefreshReserve builds refresh_reserve (tag 3):
// [reserve(w), pyth_oracle, switchboard_oracle].
func RefreshReserve(r *Reserve) solana.Instruction {
	metas := solana.AccountMetaSlice{
		solana.NewAccountMeta(r.Reserve, true, false),
		solana.NewAccountMeta(r.PythOracle, false, false),
		solana.NewAccountMeta(r.SwitchboardOracle, false, false),
	}
	return solana.NewInstruction(pk(SolendProgram), metas, []byte{tagRefreshReserve})
}

// RefreshObligation builds refresh_obligation (tag 7):
// [obligation(w), then each deposit reserve, then each borrow reserve — in
// obligation order].
func RefreshObligation(obligation solana.PublicKey, depositReserves, borrowReserves []solana.PublicKey) solana.Instruction {
	metas := solana.AccountMetaSlice{
		solana.NewAccountMeta(obligation, true, false),
	}
	for _, r := range depositReserves {
		metas = append(metas, solana.NewAccountMeta(r, true, false))
	}
	for _, r := range borrowReserves {
		metas = append(metas, solana.NewAccountMeta(r, true, false))
	}
	return solana.NewInstruction(pk(SolendProgram), metas, []byte{tagRefreshObligation})
}

// LiquidateAndRedeem builds
// liquidate_obligation_and_redeem_reserve_collateral (tag 17). Repays
// liquidityAmount of the borrow (the debt asset — USDC/USDT/wSOL) and
// seizes+redeems the withdraw reserve's collateral to underlying, into the
// liquidator's accounts. The account order is asset-agnostic and verbatim
// from the captured txs (15 accounts).
func LiquidateAndRedeem(
	liquidityAmount uint64,
	sourceLiquidity solana.PublicKey, // liquidator debt-asset ATA (repay)
	destCollateral solana.PublicKey, // liquidator cToken ATA
	destLiquidity solana.PublicKey, // liquidator underlying ATA (redeemed collateral)
	repayReserve *Reserve, // the borrow (debt) reserve
	withdrawReserve *Reserve, // the collateral reserve being seized
	obligation solana.PublicKey,
	lendingMarket solana.PublicKey,
	userTransferAuthority solana.PublicKey, // signer
) solana.Instruction {
	data := make([]byte, 0, 9)
	data = append(data, tagLiquidateAndRedeem)
	data = binary.LittleEndian.AppendUint64(data, liquidityAmount)
	metas := solana.AccountMetaSlice{
		solana.NewAccountMeta(sourceLiquidity, true, false),                        // 0
		solana.NewAccountMeta(destCollateral, true, false),                         // 1
		solana.NewAccountMeta(destLiquidity, true, false),                          // 2
		solana.NewAccountMeta(repayReserve.Reserve, true, false),                   // 3
		solana.NewAccountMeta(repayReserve.LiquiditySupply, true, false),           // 4
		solana.NewAccountMeta(withdrawReserve.Reserve, true, false),                // 5
		solana.NewAccountMeta(withdrawReserve.CollateralMint, true, false),         // 6
		solana.NewAccountMeta(withdrawReserve.CollateralSupply, true, false),       // 7
		solana.NewAccountMeta(withdrawReserve.LiquiditySupply, true, false),        // 8
		solana.NewAccountMeta(withdrawReserve.FeeReceiver, true, false),            // 9
		solana.NewAccountMeta(obligation, true, false),                             // 10
		solana.NewAccountMeta(lendingMarket, false, false),                         // 11
		solana.NewAccountMeta(LendingMarketAuthority(lendingMarket), false, false), // 12
		solana.NewAccountMeta(userTransferAuthority, false, true),                  // 13 signer
		solana.NewAccountMeta(pk(tokenProgram), false, false),                      // 14
	}
	return solana.NewInstruction(pk(SolendProgram), metas, data)
}
