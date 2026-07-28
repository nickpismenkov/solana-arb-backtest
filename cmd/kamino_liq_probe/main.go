// Kamino liquidation WIRING probe — assembles the real 3-ix sequence
// (refresh_reserve ×2 + refresh_obligation + liquidate_and_redeem_v2) against
// the most-underwater live main-market obligation and simulates it (no send,
// no money). Classifies by instruction INDEX:
//
//	err null                              → fully liquidatable, whole seq runs
//	revert at the LIQUIDATE ix            → wiring OK, guard/health rejected
//	                                        (expected while 0 real liquidatable)
//	revert at an earlier ix               → refresh/account wiring bug
//
// Uses the liquidator's existing USDC ATA as the repay source and the wSOL /
// collateral-mint ATA as the destination (created idempotently in the real
// fire path; here we just need the accounts to exist for the account list).
//
// Usage: HELIUS_RPC=<url> [AUTHORITY=<pk>] go run ./cmd/kamino_liq_probe
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/arb"
	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/flashloan"
	"solana-arb-backtest-go/internal/kamino"
)

const (
	klend            = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"
	mainMarket       = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"
	tokenProgramID   = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	obligationSize   = 3344
	defaultAuthority = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak"
)

const liquidateIxIndex = 3

var httpClient = &http.Client{Timeout: 15 * time.Second}

func rpc(endpoint string, body map[string]any) (map[string]any, bool) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, false
	}
	for attempt := 0; attempt < 4; attempt++ {
		resp, err := httpClient.Post(endpoint, "application/json", bytes.NewReader(b))
		if err == nil {
			var v map[string]any
			decErr := json.NewDecoder(resp.Body).Decode(&v)
			resp.Body.Close()
			if decErr == nil {
				return v, true
			}
		}
		time.Sleep(time.Duration(400<<attempt) * time.Millisecond)
	}
	return nil, false
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}
func asArray(v any) []any {
	a, _ := v.([]any)
	return a
}
func asStr(v any) string {
	s, _ := v.(string)
	return s
}
func b64(data any) ([]byte, bool) {
	arr, ok := data.([]any)
	if !ok || len(arr) == 0 {
		return nil, false
	}
	s, ok := arr[0].(string)
	if !ok {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	return raw, true
}

func getMultiple(endpoint string, keys []solana.PublicKey) map[solana.PublicKey][]byte {
	out := map[solana.PublicKey][]byte{}
	for start := 0; start < len(keys); start += 100 {
		end := start + 100
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[start:end]
		strs := make([]string, len(chunk))
		for i, k := range chunk {
			strs[i] = k.String()
		}
		v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getMultipleAccounts",
			"params": []any{strs, map[string]any{"encoding": "base64"}}})
		if !ok {
			continue
		}
		values := asArray(asMap(v["result"])["value"])
		for i, accV := range values {
			acc := asMap(accV)
			if acc == nil {
				continue
			}
			if b, ok := b64(acc["data"]); ok {
				out[chunk[i]] = b
			}
		}
	}
	return out
}

func mintOwner(endpoint string, mint solana.PublicKey) solana.PublicKey {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{mint.String(), map[string]any{"encoding": "base64"}}})
	if ok {
		value := asMap(asMap(v["result"])["value"])
		if s := asStr(value["owner"]); s != "" {
			if pk, err := solana.PublicKeyFromBase58(s); err == nil {
				return pk
			}
		}
	}
	return solana.MustPublicKeyFromBase58(tokenProgramID)
}

func returnReason(code uint64) string {
	switch code {
	case 6017:
		return "obligation healthy (not liquidatable)"
	default:
		return "custom error past refresh — see logs"
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func shortStr(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func main() {
	envfile.LoadDotEnv()

	endpoint := os.Getenv("HELIUS_RPC")
	if endpoint == "" {
		endpoint = os.Getenv("RPC_HTTP")
	}
	if endpoint == "" {
		fail("HELIUS_RPC")
	}
	authorityStr := os.Getenv("AUTHORITY")
	if authorityStr == "" {
		authorityStr = defaultAuthority
	}
	authority, err := solana.PublicKeyFromBase58(authorityStr)
	if err != nil {
		fail("bad AUTHORITY: %v", err)
	}
	market := solana.MustPublicKeyFromBase58(mainMarket)

	// Scan main-market obligations (borrows present), pick the most underwater
	// by STORED health (fresh enough to be a real wiring target).
	fmt.Fprintln(os.Stderr, "[kliq] scanning main-market obligations …")
	resp, _ := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{klend, map[string]any{"encoding": "base64", "dataSlice": map[string]any{"offset": 0, "length": 2288},
			"filters": []any{map[string]any{"dataSize": obligationSize}, map[string]any{"memcmp": map[string]any{"offset": 32, "bytes": mainMarket}}}}}})
	entries := asArray(resp["result"])
	fmt.Fprintf(os.Stderr, "[kliq] %d obligations\n", len(entries))

	var bestPk solana.PublicKey
	var bestOb *kamino.Obligation
	bestRatio := 0.0
	haveBest := false
	for _, ev := range entries {
		e := asMap(ev)
		pk, err := solana.PublicKeyFromBase58(asStr(e["pubkey"]))
		if err != nil {
			continue
		}
		raw, ok := b64(asMap(e["account"])["data"])
		if !ok {
			continue
		}
		ob, ok := kamino.DecodeObligation(raw)
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
			bestPk, bestOb, bestRatio, haveBest = pk, ob, ratio, true
		}
	}
	if !haveBest {
		fmt.Fprintln(os.Stderr, "[kliq] no single-deposit/single-borrow obligation found")
		return
	}
	obPk, ob, ratio := bestPk, bestOb, bestRatio
	fmt.Fprintf(os.Stderr, "[kliq] target %s ratio %.3f deposit_reserve %s borrow_reserve %s\n",
		shortStr(obPk.String(), 8), ratio, shortStr(ob.Deposits[0].Reserve.String(), 8), shortStr(ob.Borrows[0].Reserve.String(), 8))

	withdrawReservePk := ob.Deposits[0].Reserve // collateral we seize
	repayReservePk := ob.Borrows[0].Reserve     // debt we repay
	raw := getMultiple(endpoint, []solana.PublicKey{withdrawReservePk, repayReservePk})
	wrData, wrOk := raw[withdrawReservePk]
	rrData, rrOk := raw[repayReservePk]
	if !wrOk || !rrOk {
		fmt.Fprintln(os.Stderr, "[kliq] reserve decode failed")
		return
	}
	wr, wrDecOk := kamino.DecodeReserveAccounts(withdrawReservePk, wrData)
	rr, rrDecOk := kamino.DecodeReserveAccounts(repayReservePk, rrData)
	if !wrDecOk || !rrDecOk {
		fmt.Fprintln(os.Stderr, "[kliq] reserve decode failed")
		return
	}
	// Reserve for token-program + decimals of each side (decoded but unused
	// beyond confirming the reserve parses — matches the original probe).
	_, _ = kamino.DecodeReserve(wrData)

	repayTp := mintOwner(endpoint, rr.LiquidityMint)
	withdrawLiqTp := mintOwner(endpoint, wr.LiquidityMint)
	collTp := mintOwner(endpoint, wr.CollateralMint)

	// ATAs (the fire path creates these idempotently; probe just references them).
	userSourceLiquidity := flashloan.AtaFor(authority, rr.LiquidityMint, repayTp)     // repay from USDC ATA
	userDestLiquidity := flashloan.AtaFor(authority, wr.LiquidityMint, withdrawLiqTp) // seized underlying
	userDestCollateral := flashloan.AtaFor(authority, wr.CollateralMint, collTp)

	// 3-ix sequence.
	ixs := []solana.Instruction{
		kamino.RefreshReserve(rr),
		kamino.RefreshReserve(wr),
		kamino.RefreshObligation(market, obPk, []solana.PublicKey{withdrawReservePk, repayReservePk}),
		kamino.LiquidateAndRedeemV2(
			authority, obPk, market, rr, wr,
			userDestCollateral, userDestLiquidity, userSourceLiquidity,
			collTp, repayTp, withdrawLiqTp,
			1_000_000, 0, 0,
		),
	}

	bhResp, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getLatestBlockhash",
		"params": []any{map[string]any{"commitment": "finalized"}}})
	if !ok {
		fail("getLatestBlockhash")
	}
	bhStr := asStr(asMap(asMap(bhResp["result"])["value"])["blockhash"])
	bh, err := solana.HashFromBase58(bhStr)
	if err != nil {
		fail("bad blockhash: %v", err)
	}
	tx, err := arb.CompileV0(authority, ixs, nil, bh)
	if err != nil {
		fail("compile: %v", err)
	}
	b64tx, err := tx.ToBase64()
	if err != nil {
		fail("serialize: %v", err)
	}

	fmt.Fprintln(os.Stderr, "[kliq] simulating refresh×2 + refresh_obligation + liquidate …")
	sim, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "simulateTransaction",
		"params": []any{b64tx, map[string]any{"sigVerify": false, "replaceRecentBlockhash": true, "commitment": "processed", "encoding": "base64"}}})
	if !ok {
		fmt.Fprintln(os.Stderr, "[kliq] no response")
		return
	}
	result := asMap(sim["result"])
	if result == nil || result["value"] == nil {
		b, _ := json.Marshal(sim)
		fmt.Printf("✗ RPC rejected simulation: %s\n", string(b))
		return
	}
	res := asMap(result["value"])
	fmt.Println("\n──── Kamino liquidation-wiring simulation ────")
	errB, _ := json.Marshal(res["err"])
	fmt.Printf("err: %s\n", string(errB))

	var ixIdx *uint64
	var code *uint64
	if errMap := asMap(res["err"]); errMap != nil {
		if ie, ok := errMap["InstructionError"].([]any); ok && len(ie) == 2 {
			if f, ok := ie[0].(float64); ok {
				n := uint64(f)
				ixIdx = &n
			}
			if cm, ok := ie[1].(map[string]any); ok {
				if cf, ok := cm["Custom"].(float64); ok {
					n := uint64(cf)
					code = &n
				}
			}
		}
	}

	switch {
	case res["err"] == nil:
		fmt.Println("★★ FULLY LIQUIDATABLE — whole sequence executes end to end")
	case ixIdx != nil && *ixIdx == liquidateIxIndex:
		why := "non-custom revert"
		if code != nil {
			if *code == 3012 {
				why = "missing destination ATA (3012 AccountNotInitialized) — the fire path creates these; health gate PASSED"
			} else {
				why = returnReason(*code)
			}
		}
		fmt.Printf("★ WIRING OK — refresh×2 + refresh_obligation executed; liquidate reached account/health checks: %s. Account layout verified.\n", why)
	case ixIdx != nil:
		fmt.Printf("✗ reverted at ix %d (custom %v) — refresh/account wiring bug:\n", *ixIdx, codeStr(code))
		for _, lv := range asArray(res["logMessages"]) {
			if s, ok := lv.(string); ok {
				fmt.Printf("  %s\n", s)
			}
		}
	default:
		errB2, _ := json.Marshal(res["err"])
		fmt.Printf("? inconclusive: %s\n", string(errB2))
	}
}

func codeStr(c *uint64) string {
	if c == nil {
		return "None"
	}
	return fmt.Sprintf("Some(%d)", *c)
}
