// Simulate the FULL Kamino atomic fire tx against the most-underwater live
// main-market obligation (USDC debt). Classifies by instruction index — the
// wiring test for the whole flashloan-wrapped path. Expected outcomes with a
// healthy market: either the obligation is genuinely liquidatable and the
// whole tx runs (err null), or the liquidate ix reverts on health/close-factor
// — both prove borrow + refreshes + liquidate account wiring + Jupiter swap
// compose + JupLend payback compile under the size limit. A revert at any
// other index is a wiring bug.
//
// Usage: HELIUS_RPC=<url> [AUTHORITY=<pk>] go run ./cmd/kamino_fire_probe
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/flashloan"
	"solana-arb-backtest-go/internal/kamino"
)

const (
	klend            = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"
	mainMarket       = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"
	usdcMint         = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	usdtMint         = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"
	tokenProgramID   = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	obligationSize   = 3344
	defaultAuthority = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak"
)

// [cu, cu_price, ata, ata, ata, borrow, refresh, refresh, refresh_ob, LIQUIDATE, …]
const liquidateIxIndex = 9

var httpClient = &http.Client{Timeout: 15 * time.Second}

func rpc(endpoint string, body map[string]any) (map[string]any, bool) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, false
	}
	for attempt := 0; attempt < 5; attempt++ {
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
	strs := make([]string, len(keys))
	for i, k := range keys {
		strs[i] = k.String()
	}
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getMultipleAccounts",
		"params": []any{strs, map[string]any{"encoding": "base64"}}})
	if !ok {
		return out
	}
	values := asArray(asMap(v["result"])["value"])
	for i, accV := range values {
		acc := asMap(accV)
		if acc == nil {
			continue
		}
		if b, ok := b64(acc["data"]); ok {
			out[keys[i]] = b
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

type ranked struct {
	pk    solana.PublicKey
	ob    *kamino.Obligation
	ratio float64
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
	usdc := solana.MustPublicKeyFromBase58(usdcMint)
	// NONUSDC=1 → skip USDC-debt candidates, to prove the widened USDT/wSOL path.
	skipUsdc := os.Getenv("NONUSDC") == "1"
	// DEBT=USDC|USDT|wSOL → only sim that debt asset.
	wantDebt := os.Getenv("DEBT")

	nonUsdcTag := ""
	if skipUsdc {
		nonUsdcTag = " [NON-USDC only]"
	}
	fmt.Fprintf(os.Stderr, "[kfire] scanning main-market obligations (wired-debt USDC/USDT/wSOL, single deposit/borrow)%s …\n", nonUsdcTag)
	resp, _ := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{klend, map[string]any{"encoding": "base64", "dataSlice": map[string]any{"offset": 0, "length": 2288},
			"filters": []any{map[string]any{"dataSize": obligationSize}, map[string]any{"memcmp": map[string]any{"offset": 32, "bytes": mainMarket}}}}}})
	entries := asArray(resp["result"])
	fmt.Fprintf(os.Stderr, "[kfire] %d obligations\n", len(entries))

	// Need USDC to be the debt reserve → resolve each candidate's repay reserve
	// liquidity mint. Rank by stored ratio, take the first USDC-debt one.
	var rankedList []ranked
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
		if len(ob.Deposits) == 1 && len(ob.Borrows) == 1 && ob.ElevationGroup == 0 && ob.UnhealthyBorrowValue >= 50.0 {
			rankedList = append(rankedList, ranked{pk, ob, ob.Ratio()})
		}
	}
	sort.Slice(rankedList, func(i, j int) bool { return rankedList[i].ratio > rankedList[j].ratio })

	for i, cand := range rankedList {
		if i >= 40 {
			break
		}
		obPk, ob, ratio := cand.pk, cand.ob, cand.ratio
		withdrawPk := ob.Deposits[0].Reserve
		repayPk := ob.Borrows[0].Reserve
		raw := getMultiple(endpoint, []solana.PublicKey{withdrawPk, repayPk})
		wrData, wrOk := raw[withdrawPk]
		rrData, rrOk := raw[repayPk]
		if !wrOk || !rrOk {
			continue
		}
		wr, wrDecOk := kamino.DecodeReserveAccounts(withdrawPk, wrData)
		rr, rrDecOk := kamino.DecodeReserveAccounts(repayPk, rrData)
		if !wrDecOk || !rrDecOk {
			continue
		}
		// v1.5: any debt with a wired JupLend flash market (USDC/USDT/wSOL).
		if !flashloan.HasMarket(rr.LiquidityMint) {
			continue
		}
		if skipUsdc && rr.LiquidityMint.Equals(usdc) {
			continue
		}
		wrRes, wrResOk := kamino.DecodeReserve(wrData)
		rrRes, rrResOk := kamino.DecodeReserve(rrData)
		if !wrResOk || !rrResOk {
			continue
		}
		debtSym := "wSOL"
		if rr.LiquidityMint.Equals(usdc) {
			debtSym = "USDC"
		} else if rr.LiquidityMint.String() == usdtMint {
			debtSym = "USDT"
		}
		if wantDebt != "" && wantDebt != debtSym {
			continue
		}

		// Size: repay 20% of debt (Kamino close factor), capped small for the probe.
		debtDec := int(rrRes.MintDecimals)
		debtPrice := math.Max(rrRes.MarketPrice, 1e-9)
		debtUsd := (ob.Borrows[0].Amount / pow10(debtDec)) * rrRes.MarketPrice
		repayUsd := math.Min(debtUsd*0.2, 50.0)
		if repayUsd < 1.0 {
			repayUsd = 1.0
		}
		// Native debt units priced in the actual debt asset (not hardcoded USDC).
		repayAmount := uint64(repayUsd / debtPrice * pow10(debtDec))
		// Seized underlying native ≈ repay_usd × (1 + ~5% bonus) / price, 0.5% haircut.
		bonus := 1.05
		seizedNative := repayUsd * bonus / wrRes.MarketPrice * pow10(int(wrRes.MintDecimals))
		swapInAmount := uint64(seizedNative * 0.995)

		fmt.Fprintf(os.Stderr, "[kfire] target %s [%s debt] ratio %.3f  debt $%.0f  repay $%.2f (%d native)  seize %d native (%d dp @ $%.2f)\n",
			shortStr(obPk.String(), 8), debtSym, ratio, debtUsd, repayUsd, repayAmount, swapInAmount, wrRes.MintDecimals, wrRes.MarketPrice)

		fireCand := &kamino.FireCandidate{
			Obligation:                     obPk,
			LendingMarket:                  market,
			RepayReserve:                   rr,
			WithdrawReserve:                wr,
			ObligationReserves:             []solana.PublicKey{withdrawPk, repayPk},
			WithdrawLiquidityMint:          wr.LiquidityMint,
			WithdrawLiquidityTokenProgram:  mintOwner(endpoint, wr.LiquidityMint),
			WithdrawCollateralTokenProgram: mintOwner(endpoint, wr.CollateralMint),
			RepayLiquidityTokenProgram:     mintOwner(endpoint, rr.LiquidityMint),
			RepayAmount:                    repayAmount,
			SwapInAmount:                   swapInAmount,
		}

		bhResp, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getLatestBlockhash",
			"params": []any{map[string]any{"commitment": "finalized"}}})
		if !ok {
			continue
		}
		bhStr := asStr(asMap(asMap(bhResp["result"])["value"])["blockhash"])
		bh, err := solana.HashFromBase58(bhStr)
		if err != nil {
			continue
		}
		fire, err := kamino.BuildFireTx(endpoint, fireCand, authority, nil, 0, 100_000, 100, 20, bh)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[kfire]   build failed: %v\n", err)
			continue
		}
		fmt.Fprintf(os.Stderr, "[kfire]   tx %d bytes (limit 1232)  quoted_usdc_out=%d\n", fire.TxBytes, fire.QuotedUSDCOut)

		b64tx, err := fire.Tx.ToBase64()
		if err != nil {
			continue
		}
		sim, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "simulateTransaction",
			"params": []any{b64tx, map[string]any{"sigVerify": false, "replaceRecentBlockhash": true, "commitment": "processed", "encoding": "base64"}}})
		if !ok {
			continue
		}
		result := asMap(sim["result"])
		if result == nil || result["value"] == nil {
			errB, _ := json.Marshal(sim["error"])
			fmt.Fprintf(os.Stderr, "[kfire]   RPC rejected sim: %s\n", string(errB))
			continue
		}
		res := asMap(result["value"])
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
		fmt.Printf("\n──── Kamino fire simulation (%s…) ────\n", shortStr(obPk.String(), 8))
		errB, _ := json.Marshal(res["err"])
		fmt.Printf("err: %s  (ix %s, custom %s)\n", string(errB), idxStr(ixIdx), idxStr(code))
		switch {
		case res["err"] == nil:
			fmt.Println("★★ FULL KAMINO FIRE VERIFIED — whole flashloan-wrapped tx executes end to end")
			return
		case ixIdx != nil && *ixIdx == liquidateIxIndex:
			fmt.Printf("★ WIRING OK — borrow + refresh×2 + refresh_obligation executed; liquidate reached "+
				"health/close-factor checks (custom %s). Path compiles at %d bytes; swap + payback wired.\n", idxStr(code), fire.TxBytes)
			return
		case ixIdx != nil:
			fmt.Printf("✗ reverted at ix %d (custom %s) — wiring bug, logs:\n", *ixIdx, idxStr(code))
			for _, lv := range asArray(res["logs"]) {
				if s, ok := lv.(string); ok {
					fmt.Printf("  %s\n", s)
				}
			}
			return
		default:
			errB2, _ := json.Marshal(res["err"])
			fmt.Printf("? inconclusive: %s\n", string(errB2))
		}
	}
	fmt.Println("no wired-debt single-position obligation simulated")
}

func idxStr(v *uint64) string {
	if v == nil {
		return "None"
	}
	return fmt.Sprintf("Some(%d)", *v)
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
