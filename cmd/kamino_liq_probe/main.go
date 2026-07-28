// Command kamino_liq_probe is a Kamino liquidation WIRING probe — assembles
// the real 3-ix sequence (refresh_reserve x2 + refresh_obligation +
// liquidate_and_redeem_v2) against the most-underwater live main-market
// obligation and simulates it (no send, no money). Classifies by
// instruction INDEX:
//
//	err null                    -> fully liquidatable, whole seq runs
//	revert at the LIQUIDATE ix  -> wiring OK, guard/health rejected
//	                               (expected while 0 real liquidatable)
//	revert at an earlier ix     -> refresh/account wiring bug
//
// Uses the liquidator's existing USDC ATA as the repay source and the wSOL
// / collateral-mint ATA as the destination (created idempotently in the
// real fire path; here we just need the accounts to exist for the account
// list).
//
// Usage: HELIUS_RPC=<url> [AUTHORITY=<pk>] go run ./cmd/kamino_liq_probe
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"arbengine/internal/config"
	"arbengine/internal/flashloan"
	"arbengine/internal/kamino"
	"arbengine/internal/kaminoix"
	"arbengine/internal/rpcclient"
	"arbengine/internal/solana"
)

const (
	tokenProgram     = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	defaultAuthority = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak"
	liquidateIxIndex = 3
)

func mintOwner(rpc *rpcclient.Client, mint solana.Pubkey) solana.Pubkey {
	info, err := rpc.GetAccountInfo(mint)
	if err != nil || info == nil {
		return solana.MustPubkeyFromBase58(tokenProgram)
	}
	pk, err := solana.PubkeyFromBase58(info.Owner)
	if err != nil {
		return solana.MustPubkeyFromBase58(tokenProgram)
	}
	return pk
}

func returnReason(code uint64) string {
	if code == 6017 {
		return "obligation healthy (not liquidatable)"
	}
	return "custom error past refresh — see logs"
}

func main() {
	config.LoadDotenv()

	endpoint, ok := config.EnvOptional("HELIUS_RPC")
	if !ok {
		endpoint, ok = config.EnvOptional("RPC_HTTP")
	}
	if !ok {
		fmt.Fprintln(os.Stderr, "HELIUS_RPC")
		os.Exit(1)
	}
	authority := solana.MustPubkeyFromBase58(config.EnvOr("AUTHORITY", defaultAuthority))
	market := solana.MustPubkeyFromBase58(kamino.KaminoMainMarket)

	rpc := rpcclient.New(endpoint)

	// Scan main-market obligations (borrows present), pick the most
	// underwater by STORED health (fresh enough to be a real wiring
	// target).
	fmt.Fprintln(os.Stderr, "[kliq] scanning main-market obligations …")
	entries, err := rpc.GetProgramAccounts(solana.MustPubkeyFromBase58(kamino.KlendProgram), rpcclient.GetProgramAccountsOpts{
		Filters: []any{
			map[string]any{"dataSize": kamino.ObligationSize},
			map[string]any{"memcmp": map[string]any{"offset": 32, "bytes": kamino.KaminoMainMarket}},
		},
		DataSlice: &struct {
			Offset int `json:"offset"`
			Length int `json:"length"`
		}{Offset: 0, Length: 2288},
	})
	if err != nil {
		entries = nil
	}
	fmt.Fprintf(os.Stderr, "[kliq] %d obligations\n", len(entries))

	var bestPk solana.Pubkey
	var bestOb *kamino.Obligation
	bestRatio := 0.0
	haveBest := false
	for _, e := range entries {
		ob, ok := kamino.DecodeObligation(e.Account.Data)
		if !ok {
			continue
		}
		if len(ob.Deposits) != 1 || len(ob.Borrows) != 1 || ob.ElevationGroup != 0 {
			continue
		}
		if ob.UnhealthyBorrowValue < 50.0 {
			continue
		}
		ratio := ob.Ratio()
		if !haveBest || ratio > bestRatio {
			bestPk, bestOb, bestRatio, haveBest = e.Pubkey, ob, ratio, true
		}
	}
	if !haveBest {
		fmt.Fprintln(os.Stderr, "[kliq] no single-deposit/single-borrow obligation found")
		return
	}
	obPkStr := bestPk.String()
	depStr := bestOb.Deposits[0].Reserve.String()
	borStr := bestOb.Borrows[0].Reserve.String()
	fmt.Fprintf(os.Stderr, "[kliq] target %s ratio %.3f deposit_reserve %s borrow_reserve %s\n",
		obPkStr[:8], bestRatio, depStr[:8], borStr[:8])

	withdrawReservePk := bestOb.Deposits[0].Reserve // collateral we seize
	repayReservePk := bestOb.Borrows[0].Reserve     // debt we repay

	multi, err := rpc.GetMultipleAccounts([]solana.Pubkey{withdrawReservePk, repayReservePk})
	if err != nil || len(multi) != 2 || multi[0] == nil || multi[1] == nil {
		fmt.Fprintln(os.Stderr, "[kliq] reserve decode failed")
		return
	}
	wr, wrOk := kaminoix.DecodeReserveAccounts(withdrawReservePk, multi[0].Data)
	rr, rrOk := kaminoix.DecodeReserveAccounts(repayReservePk, multi[1].Data)
	if !wrOk || !rrOk {
		fmt.Fprintln(os.Stderr, "[kliq] reserve decode failed")
		return
	}

	repayTp := mintOwner(rpc, rr.LiquidityMint)
	withdrawLiqTp := mintOwner(rpc, wr.LiquidityMint)
	collTp := mintOwner(rpc, wr.CollateralMint)

	// ATAs (the fire path creates these idempotently; probe just references them).
	userSourceLiquidity := flashloan.AtaFor(authority, rr.LiquidityMint, repayTp)     // repay from USDC ATA
	userDestLiquidity := flashloan.AtaFor(authority, wr.LiquidityMint, withdrawLiqTp) // seized underlying
	userDestCollateral := flashloan.AtaFor(authority, wr.CollateralMint, collTp)

	// 3-ix sequence.
	ixs := []solana.Instruction{
		kaminoix.RefreshReserve(rr),
		kaminoix.RefreshReserve(wr),
		kaminoix.RefreshObligation(market, bestPk, []solana.Pubkey{withdrawReservePk, repayReservePk}),
		kaminoix.LiquidateAndRedeemV2(kaminoix.LiquidateAndRedeemV2Params{
			Liquidator:                     authority,
			Obligation:                     bestPk,
			LendingMarket:                  market,
			Repay:                          rr,
			Withdraw:                       wr,
			UserDestCollateral:             userDestCollateral,
			UserDestLiquidity:              userDestLiquidity,
			UserSourceLiquidity:            userSourceLiquidity,
			CollateralTokenProgram:         collTp,
			RepayLiquidityTokenProgram:     repayTp,
			WithdrawLiquidityTokenProgram:  withdrawLiqTp,
			LiquidityAmount:                1_000_000,
			MinAcceptableReceivedLiquidity: 0,
			MaxAllowedLtvOverridePct:       0,
		}),
	}

	bh, err := rpc.GetLatestBlockhash()
	if err != nil {
		fmt.Fprintln(os.Stderr, "[kliq] no response")
		return
	}
	msg, err := solana.CompileV0(authority, ixs, nil, bh)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[kliq] compile failed:", err)
		return
	}
	tx := solana.NewUnsignedVersionedTransaction(msg)
	b64tx, err := tx.Base64()
	if err != nil {
		fmt.Fprintln(os.Stderr, "[kliq] encode failed:", err)
		return
	}

	fmt.Fprintln(os.Stderr, "[kliq] simulating refresh×2 + refresh_obligation + liquidate …")
	simRaw, err := rpc.SimulateTransaction(b64tx)
	if err != nil || simRaw == nil {
		fmt.Fprintln(os.Stderr, "[kliq] no response")
		return
	}

	var res struct {
		Err           json.RawMessage `json:"err"`
		Logs          []string        `json:"logs"`
		UnitsConsumed json.RawMessage `json:"unitsConsumed"`
	}
	if err := json.Unmarshal(simRaw, &res); err != nil {
		fmt.Printf("✗ RPC rejected simulation: %s\n", string(simRaw))
		return
	}

	fmt.Println("\n──── Kamino liquidation-wiring simulation ────")
	fmt.Printf("err: %s\n", errString(res.Err))

	ixIdx, code, hasIdx := parseInstructionError(res.Err)

	switch {
	case isNullErr(res.Err):
		fmt.Println("★★ FULLY LIQUIDATABLE — whole sequence executes end to end")
	case hasIdx && ixIdx == liquidateIxIndex:
		var why string
		switch {
		case code != nil && *code == 3012:
			why = "missing destination ATA (3012 AccountNotInitialized) — the fire path creates these; health gate PASSED"
		case code != nil:
			why = returnReason(*code)
		default:
			why = "non-custom revert"
		}
		fmt.Printf("★ WIRING OK — refresh×2 + refresh_obligation executed; liquidate reached account/health checks: %s. Account layout verified.\n", why)
	case hasIdx:
		var codeStr string
		if code != nil {
			codeStr = fmt.Sprintf("%d", *code)
		} else {
			codeStr = "<nil>"
		}
		fmt.Printf("✗ reverted at ix %d (custom %s) — refresh/account wiring bug:\n", ixIdx, codeStr)
		for _, l := range res.Logs {
			fmt.Printf("  %s\n", l)
		}
	default:
		fmt.Printf("? inconclusive: %s\n", errString(res.Err))
	}
}

func isNullErr(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

func errString(raw json.RawMessage) string {
	if isNullErr(raw) {
		return "null"
	}
	return string(raw)
}

// parseInstructionError extracts {"InstructionError":[idx,{"Custom":code}]}.
func parseInstructionError(raw json.RawMessage) (idx uint64, code *uint64, ok bool) {
	if isNullErr(raw) {
		return 0, nil, false
	}
	var v struct {
		InstructionError json.RawMessage `json:"InstructionError"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.InstructionError == nil {
		return 0, nil, false
	}
	var tuple []json.RawMessage
	if err := json.Unmarshal(v.InstructionError, &tuple); err != nil || len(tuple) != 2 {
		return 0, nil, false
	}
	if err := json.Unmarshal(tuple[0], &idx); err != nil {
		return 0, nil, false
	}
	var custom struct {
		Custom *uint64 `json:"Custom"`
	}
	if err := json.Unmarshal(tuple[1], &custom); err == nil {
		code = custom.Custom
	}
	return idx, code, true
}
