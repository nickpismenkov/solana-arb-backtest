// Verify the Save fire path composes across debt assets: find real
// liquidatable v1 obligations (1 collateral deposit + 1 borrow, debt ∈
// {USDC,USDT,wSOL}), build the flash-loan-wrapped liquidate+redeem+swap+repay
// tx, and simulateTransaction. Success = a live profitable liquidation (CLEAN
// sim); a revert at the Solend liquidate/health gate (custom err 29
// LiquidationTooSmall = healthy at the fresh price) proves every upstream leg
// (JupLend flash borrow, refresh, liquidate wiring, Jupiter swap, payback)
// composes. Reports tx byte size (flags if a SAVE_ALT is needed to fit
// 1232B). Read-only — never submits.
//
// Usage: HELIUS_RPC=<url> [DEBT=all|usdc|usdt|wsol] [TRIES=25] [MIN_DEBT=50]
//
//	[REPAY_FRAC=0.2] [RATIO_CAP=3.0] [MAX_SWAP_ACCOUNTS=18]
//	go run ./cmd/save_fire_probe
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/save"
)

const classicTokenProgram = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"

var httpClient = &http.Client{Timeout: 20 * time.Second}

func rpc(endpoint string, body map[string]any) (map[string]any, bool) {
	for attempt := 0; attempt < 4; attempt++ {
		b, _ := json.Marshal(body)
		resp, err := httpClient.Post(endpoint, "application/json", bytes.NewReader(b))
		if err == nil {
			raw, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr == nil {
				var v map[string]any
				if json.Unmarshal(raw, &v) == nil {
					return v, true
				}
			}
		}
		time.Sleep(time.Duration(400<<attempt) * time.Millisecond)
	}
	return nil, false
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func asArray(v any) []any        { a, _ := v.([]any); return a }
func asStr(v any) string         { s, _ := v.(string); return s }

func b64(d any) ([]byte, bool) {
	arr := asArray(d)
	if len(arr) == 0 {
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

func getAcct(endpoint string, pk solana.PublicKey) ([]byte, bool) {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{pk.String(), map[string]any{"encoding": "base64"}}})
	if !ok {
		return nil, false
	}
	return b64(asMap(asMap(v["result"])["value"])["data"])
}

func mintOwner(endpoint string, mint solana.PublicKey) (solana.PublicKey, bool) {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{mint.String(), map[string]any{"encoding": "base64"}}})
	if !ok {
		return solana.PublicKey{}, false
	}
	owner := asStr(asMap(asMap(v["result"])["value"])["owner"])
	if owner == "" {
		return solana.PublicKey{}, false
	}
	pk, err := solana.PublicKeyFromBase58(owner)
	if err != nil {
		return solana.PublicKey{}, false
	}
	return pk, true
}

func load(endpoint string, reserves map[solana.PublicKey]*save.Reserve, pk solana.PublicKey) (*save.Reserve, bool) {
	if r, ok := reserves[pk]; ok {
		return r, true
	}
	raw, ok := getAcct(endpoint, pk)
	if !ok {
		return nil, false
	}
	r, ok := save.DecodeReserve(pk, raw)
	if !ok {
		return nil, false
	}
	reserves[pk] = r
	return r, true
}

type candidate struct {
	ratio float64
	pk    solana.PublicKey
	o     *save.Obligation
}

// runAsset runs one debt asset: ranks its liquidatable v1 candidates near
// threshold, builds + sims the top tries, and tallies CLEAN / too-small-at-
// fresh / other.
func runAsset(
	endpoint, label string, debtMint solana.PublicKey, entries []any,
	reserves map[solana.PublicKey]*save.Reserve, authority solana.PublicKey,
	tries int, minDebt, repayFrac, ratioCap float64, maxSwapAccounts int,
	sameMintOnly bool,
) {
	// Candidates: v1, this debt mint, liquidatable, ≥ min_debt, ratio ≤
	// ratio_cap (the cap drops mis-priced-dust obligations — huge borrowed /
	// ~0 unhealthy — that would otherwise sort first and waste every try,
	// matching the engine).
	var cands []candidate
	for _, e := range entries {
		em := asMap(e)
		pk, err := solana.PublicKeyFromBase58(asStr(em["pubkey"]))
		if err != nil {
			continue
		}
		bts, ok := b64(asMap(em["account"])["data"])
		if !ok {
			continue
		}
		o, ok := save.DecodeObligation(bts)
		if !ok {
			continue
		}
		if len(o.Deposits) != 1 || len(o.Borrows) != 1 {
			continue
		}
		if !o.Liquidatable() || o.BorrowedValue < minDebt {
			continue
		}
		r := o.HealthRatio()
		if r > ratioCap {
			continue
		}
		// Keep only this debt mint — its reserve is pre-loaded, so this is free.
		rv, ok := reserves[o.Borrows[0].Reserve]
		if !ok || !rv.LiquidityMint.Equals(debtMint) {
			continue
		}
		cands = append(cands, candidate{r, pk, o})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].ratio > cands[j].ratio })
	fmt.Printf("\n== %s debt: %d liquidatable v1 candidates (≥ $%v, ratio ≤ %v); trying top %d ==\n",
		label, len(cands), minDebt, ratioCap, tries)

	clean, tooSmall, other, tried, maxBytes := 0, 0, 0, 0, 0
	debtTP := solana.MustPublicKeyFromBase58(classicTokenProgram)
	for _, c := range cands {
		if tried >= tries {
			break
		}
		repayReserve, ok := load(endpoint, reserves, c.o.Borrows[0].Reserve)
		if !ok {
			continue
		}
		withdrawReserve, ok := load(endpoint, reserves, c.o.Deposits[0].Reserve)
		if !ok {
			continue
		}
		// sameMintOnly targets the sub-1232B path (no swap leg): collateral
		// underlying == debt mint. Skip others without spending a try.
		if sameMintOnly && !withdrawReserve.LiquidityMint.Equals(debtMint) {
			continue
		}
		ctp, ok := mintOwner(endpoint, withdrawReserve.LiquidityMint)
		if !ok {
			continue
		}
		tried++
		debtDec := pow10(int(repayReserve.MintDecimals))
		repayUSD := c.o.BorrowedValue * repayFrac
		mp := repayReserve.MarketPrice
		if mp < 1e-9 {
			mp = 1e-9
		}
		repayAmountF := repayUSD / mp * debtDec
		if repayAmountF < 1.0 {
			repayAmountF = 1.0
		}
		repayAmount := uint64(repayAmountF)
		seizedUSD := repayUSD * (1.0 + float64(withdrawReserve.LiquidationBonusPct)/100.0)
		wmp := withdrawReserve.MarketPrice
		if wmp < 1e-9 {
			wmp = 1e-9
		}
		seizeUnderlying := uint64(seizedUSD / wmp * pow10(int(withdrawReserve.MintDecimals)))

		cand := &save.FireCandidate{
			Obligation:             c.pk,
			RepayReserve:           repayReserve,
			WithdrawReserve:        withdrawReserve,
			CollateralTokenProgram: ctp,
			DebtTokenProgram:       debtTP,
			RepayAmount:            repayAmount,
			SeizeUnderlying:        seizeUnderlying,
			DepositReserves:        []solana.PublicKey{withdrawReserve.Reserve},
			BorrowReserves:         []solana.PublicKey{repayReserve.Reserve},
		}
		sameMint := withdrawReserve.LiquidityMint.Equals(repayReserve.LiquidityMint)
		fire, err := save.BuildFireTx(endpoint, cand, authority, nil, 0, 50_000, 100, maxSwapAccounts, solana.Hash{})
		if err != nil {
			fmt.Printf("  %s ratio %.3f $%.0f: build failed: %v\n", c.pk, c.ratio, c.o.BorrowedValue, err)
			other++
			continue
		}
		if fire.TxBytes > maxBytes {
			maxBytes = fire.TxBytes
		}
		txBin, err := fire.Tx.MarshalBinary()
		if err != nil {
			fmt.Printf("  %s ratio %.3f $%.0f: marshal failed: %v\n", c.pk, c.ratio, c.o.BorrowedValue, err)
			other++
			continue
		}
		b64tx := base64.StdEncoding.EncodeToString(txBin)
		sim, _ := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "simulateTransaction",
			"params": []any{b64tx, map[string]any{"sigVerify": false, "replaceRecentBlockhash": true, "commitment": "processed", "encoding": "base64"}}})
		sm := ""
		if sameMint {
			sm = " same-mint(no-swap)"
		}
		var val map[string]any
		if sim != nil {
			val = asMap(asMap(sim["result"])["value"])
		}
		if val != nil {
			if val["err"] == nil {
				clean++
				unitsConsumed := val["unitsConsumed"]
				fmt.Printf("  ★★ %s ratio %.3f $%.0f: SIMULATES CLEAN — WOULD FIRE (%dB%s, out %d, %v CU)\n",
					c.pk, c.ratio, c.o.BorrowedValue, fire.TxBytes, sm, fire.QuotedDebtOut, unitsConsumed)
			} else {
				errB, _ := json.Marshal(val["err"])
				eStr := string(errB)
				if strings.Contains(eStr, "29") {
					tooSmall++
					fmt.Printf("  ·  %s ratio %.3f $%.0f: GATED at Solend liquidate (err 29 = healthy/too-small at fresh price) (%dB%s) — wiring composes\n",
						c.pk, c.ratio, c.o.BorrowedValue, fire.TxBytes, sm)
				} else {
					other++
					fmt.Printf("  %s ratio %.3f $%.0f: OTHER err %s (%dB%s)\n", c.pk, c.ratio, c.o.BorrowedValue, eStr, fire.TxBytes, sm)
					logs := asArray(val["logs"])
					start := len(logs) - 5
					if start < 0 {
						start = 0
					}
					for _, l := range logs[start:] {
						fmt.Printf("       %s\n", asStr(l))
					}
				}
			}
		} else {
			other++
			// No result.value → the RPC rejected the tx pre-execution (most
			// commonly "too large" when > 1232B without a SAVE_ALT).
			errMsg := "no sim value"
			if sim != nil {
				if e := asMap(sim["error"]); e != nil {
					if m, ok := e["message"].(string); ok {
						errMsg = m
					}
				}
			}
			fmt.Printf("  %s ratio %.3f $%.0f: sim rejected (%dB%s): %s\n", c.pk, c.ratio, c.o.BorrowedValue, fire.TxBytes, sm, errMsg)
		}
	}
	fmt.Printf("── %s: tried %d · CLEAN(would-fire) %d · gated-at-liquidate(composes) %d · other %d · max tx %dB ──\n",
		label, tried, clean, tooSmall, other, maxBytes)
	if maxBytes > 1232 {
		fmt.Println("   ⚠ tx exceeds 1232B — a SAVE_ALT is required for live submission (set SAVE_ALT to a deployed ALT).")
	}
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

func envFloat(name string, def float64) float64 {
	if v := os.Getenv(name); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func main() {
	envfile.LoadDotEnv()

	endpoint := os.Getenv("HELIUS_RPC")
	if endpoint == "" {
		endpoint = os.Getenv("RPC_HTTP")
	}
	if endpoint == "" {
		fmt.Fprintln(os.Stderr, "HELIUS_RPC")
		os.Exit(1)
	}
	debt := strings.ToLower(os.Getenv("DEBT"))
	if debt == "" {
		debt = "all"
	}
	tries := envInt("TRIES", 25)
	minDebt := envFloat("MIN_DEBT", 50.0)
	repayFrac := envFloat("REPAY_FRAC", 0.2)
	ratioCap := envFloat("RATIO_CAP", 3.0)
	maxSwapAccounts := envInt("MAX_SWAP_ACCOUNTS", 18)
	sameMintOnly := false
	if v := os.Getenv("SAMEMINT"); v != "" {
		sameMintOnly = v != "0"
	}
	authorityStr := os.Getenv("AUTHORITY")
	if authorityStr == "" {
		authorityStr = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak"
	}
	authority := solana.MustPublicKeyFromBase58(authorityStr)

	// Pre-load the three debt reserves so the mint match is free.
	reserves := map[solana.PublicKey]*save.Reserve{}
	for _, res := range []string{save.USDCReserve, save.USDTReserve, save.WSOLReserve} {
		pk := solana.MustPublicKeyFromBase58(res)
		load(endpoint, reserves, pk)
	}

	fmt.Fprintln(os.Stderr, "[save-fire] scanning main-pool obligations …")
	resp, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{save.SolendProgram, map[string]any{"encoding": "base64", "dataSize": 1300,
			"filters": []any{
				map[string]any{"dataSize": 1300},
				map[string]any{"memcmp": map[string]any{"offset": 10, "bytes": save.MainPool}},
			}}}})
	if !ok {
		fmt.Fprintln(os.Stderr, "gPA failed")
		os.Exit(1)
	}
	entries := asArray(resp["result"])
	fmt.Fprintf(os.Stderr, "[save-fire] %d obligations; debt filter = %s\n", len(entries), debt)

	type asset struct {
		label, mint string
	}
	var assets []asset
	switch debt {
	case "usdc":
		assets = []asset{{"USDC", save.USDCMint}}
	case "usdt":
		assets = []asset{{"USDT", save.USDTMint}}
	case "wsol", "sol":
		assets = []asset{{"wSOL", save.WSOLMint}}
	default:
		assets = []asset{{"USDC", save.USDCMint}, {"USDT", save.USDTMint}, {"wSOL", save.WSOLMint}}
	}
	for _, a := range assets {
		m := solana.MustPublicKeyFromBase58(a.mint)
		runAsset(endpoint, a.label, m, entries, reserves, authority, tries, minDebt, repayFrac, ratioCap, maxSwapAccounts, sameMintOnly)
	}
}
