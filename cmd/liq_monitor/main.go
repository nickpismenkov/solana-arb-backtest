// marginfi continuous liquidation monitor (read-only, real-data test).
//
// The go/no-go experiment: does a *profitable* liquidation opportunity ever
// sit around long enough for us to take it, or is it gone in milliseconds?
//
// Architecture (getProgramAccounts over 500k accounts is ~850MB, far too
// heavy to loop): scan ONCE to build a watch-set of accounts near
// liquidation, then on a fast loop poll only (a) oracle prices and (b) the
// watch-set's fresh account data — a few cheap getMultipleAccounts calls. We
// detect the instant a watched account crosses underwater (APPEARED) and
// when it recovers / gets liquidated (RESOLVED), logging how long it stayed
// takeable. Full re-scan every FULL_REFRESH_SECS to catch new entrants.
//
// Usage: HELIUS_RPC=<url> [POLL_SECS=5] [FULL_REFRESH_SECS=600]
//
//	[WATCH_RATIO=0.85] [MIN_COLLATERAL_USD=50] go run ./cmd/liq_monitor
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/liquidation"
)

const (
	marginfiProgram = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA"
	marginfiGroup   = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8"
)

func nowTS() uint64 {
	return uint64(time.Now().Unix())
}

func rpc(endpoint string, body map[string]any) (map[string]any, bool) {
	for attempt := 0; attempt < 4; attempt++ {
		b, _ := json.Marshal(body)
		resp, err := http.Post(endpoint, "application/json", bytes.NewReader(b))
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

func b64(data []any) ([]byte, bool) {
	if len(data) == 0 {
		return nil, false
	}
	s, ok := data[0].(string)
	if !ok {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	return raw, true
}

// getMultiple batches getMultipleAccounts (100/call) → pubkey → raw bytes.
func getMultiple(endpoint string, keys []solana.PublicKey) map[solana.PublicKey][]byte {
	out := map[solana.PublicKey][]byte{}
	for i := 0; i < len(keys); i += 100 {
		end := i + 100
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[i:end]
		strs := make([]string, len(chunk))
		for j, k := range chunk {
			strs[j] = k.String()
		}
		v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getMultipleAccounts",
			"params": []any{strs, map[string]any{"encoding": "base64"}}})
		if !ok {
			continue
		}
		result, _ := v["result"].(map[string]any)
		value, _ := result["value"].([]any)
		for j, accAny := range value {
			acc, ok := accAny.(map[string]any)
			if !ok || acc == nil {
				continue
			}
			dataArr, _ := acc["data"].([]any)
			if bytes, ok := b64(dataArr); ok {
				out[chunk[j]] = bytes
			}
		}
		time.Sleep(40 * time.Millisecond)
	}
	return out
}

// watched is a watched account: its authority + last-known balances.
type watched struct {
	authority solana.PublicKey
	account   *liquidation.MarginfiAccount
}

// fullScan → banks, bank→oracle map, and the near-liquidation watch-set
// (keyed by account pubkey). Heavy; run rarely.
func fullScan(endpoint string, watchRatio, minCollateral float64) (liquidation.BankMap, map[solana.PublicKey]solana.PublicKey, map[solana.PublicKey]*watched) {
	fmt.Fprintf(os.Stderr, "[monitor] full scan: getProgramAccounts (group %s) …\n", marginfiGroup[:8])
	resp, _ := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{marginfiProgram, map[string]any{
			"encoding":  "base64",
			"dataSlice": map[string]any{"offset": 0, "length": 1736},
			"filters": []any{
				map[string]any{"dataSize": liquidation.MASize},
				map[string]any{"memcmp": map[string]any{"offset": 8, "bytes": marginfiGroup}},
			},
		}},
	})
	var entries []any
	if resp != nil {
		entries, _ = resp["result"].([]any)
	}
	fmt.Fprintf(os.Stderr, "[monitor]   %d accounts\n", len(entries))

	// Decode borrowers, remembering the account pubkey.
	type borrower struct {
		pk   solana.PublicKey
		acct *liquidation.MarginfiAccount
	}
	var borrowers []borrower
	for _, eAny := range entries {
		e, ok := eAny.(map[string]any)
		if !ok {
			continue
		}
		pkStr, _ := e["pubkey"].(string)
		pk, err := solana.PublicKeyFromBase58(pkStr)
		if err != nil {
			continue
		}
		acc, _ := e["account"].(map[string]any)
		dataArr, _ := acc["data"].([]any)
		raw, ok := b64(dataArr)
		if !ok {
			continue
		}
		acct, ok := liquidation.DecodeMarginfiAccount(raw)
		if !ok {
			continue
		}
		for _, b := range acct.Balances {
			if b.LiabilityShares > 0.0 {
				borrowers = append(borrowers, borrower{pk, acct})
				break
			}
		}
	}

	// Banks + oracle prices (once) to score the watch-set.
	bankSet := map[solana.PublicKey]struct{}{}
	for _, b := range borrowers {
		for _, bal := range b.acct.Balances {
			bankSet[bal.BankPk] = struct{}{}
		}
	}
	bankPks := make([]solana.PublicKey, 0, len(bankSet))
	for pk := range bankSet {
		bankPks = append(bankPks, pk)
	}
	bankRaw := getMultiple(endpoint, bankPks)
	banks := liquidation.BankMap{}
	bankOracle := map[solana.PublicKey]solana.PublicKey{}
	for pk, raw := range bankRaw {
		if bank, ok := liquidation.DecodeBank(raw); ok {
			bankOracle[pk] = bank.OracleKey
			banks[pk] = bank
		}
	}
	prices := refreshPrices(endpoint, banks, bankOracle)

	// Watch-set: fully-priced borrowers within striking distance.
	watch := map[solana.PublicKey]*watched{}
	for _, b := range borrowers {
		r := liquidation.MaintenanceHealth(b.acct, banks, prices)
		if r.Missing > 0 {
			continue
		}
		if r.Health.WeightedAssets < minCollateral {
			continue
		}
		if r.Health.Ratio() >= watchRatio {
			watch[b.pk] = &watched{authority: b.acct.Authority, account: b.acct}
		}
	}
	fmt.Fprintf(os.Stderr, "[monitor]   watch-set: %d accounts (liab/asset ≥ %.2f, collateral ≥ $%.0f)\n",
		len(watch), watchRatio, minCollateral)
	return banks, bankOracle, watch
}

// refreshPrices fetches + decodes the oracle accounts, returning bank_pk → USD price.
func refreshPrices(endpoint string, banks liquidation.BankMap, bankOracle map[solana.PublicKey]solana.PublicKey) liquidation.PriceMap {
	oracleSet := map[solana.PublicKey]struct{}{}
	for _, o := range bankOracle {
		oracleSet[o] = struct{}{}
	}
	oraclePks := make([]solana.PublicKey, 0, len(oracleSet))
	for pk := range oracleSet {
		oraclePks = append(oraclePks, pk)
	}
	oracleRaw := getMultiple(endpoint, oraclePks)
	oraclePrice := map[solana.PublicKey]float64{}
	for pk, raw := range oracleRaw {
		if _, usd, _, ok := liquidation.DecodePriceUpdateV2(raw); ok {
			oraclePrice[pk] = usd
		}
	}
	prices := liquidation.PriceMap{}
	for bankPk, oraclePk := range bankOracle {
		if _, ok := banks[bankPk]; !ok {
			continue
		}
		if p, ok := oraclePrice[oraclePk]; ok {
			prices[bankPk] = p
		}
	}
	return prices
}

func short(pk solana.PublicKey) string {
	s := pk.String()
	if len(s) > 8 {
		return s[:8]
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
		fmt.Fprintln(os.Stderr, "HELIUS_RPC in .env")
		os.Exit(1)
	}
	pollSecs := 5
	if v := os.Getenv("POLL_SECS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			pollSecs = n
		}
	}
	poll := time.Duration(pollSecs) * time.Second
	fullRefresh := uint64(600)
	if v := os.Getenv("FULL_REFRESH_SECS"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			fullRefresh = n
		}
	}
	watchRatio := 0.85
	if v := os.Getenv("WATCH_RATIO"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			watchRatio = f
		}
	}
	minCollateral := 50.0
	if v := os.Getenv("MIN_COLLATERAL_USD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			minCollateral = f
		}
	}

	logPath := "runs/liq_events.jsonl"
	_ = os.MkdirAll("runs", 0o755)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open liq_events.jsonl: %v\n", err)
		os.Exit(1)
	}
	defer logFile.Close()
	emit := func(v map[string]any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		_, _ = logFile.Write(append(b, '\n'))
		_ = logFile.Sync()
	}

	fmt.Fprintf(os.Stderr, "[monitor] poll=%ds full_refresh=%ds watch_ratio=%g min_collateral=$%g\n",
		pollSecs, fullRefresh, watchRatio, minCollateral)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	banks, bankOracle, watch := fullScan(endpoint, watchRatio, minCollateral)
	lastFull := nowTS()
	// account → (ts first seen liquidatable, peak collateral, peak deficit)
	type openEntry struct {
		t0      uint64
		peakCol float64
		peakDef float64
	}
	open := map[solana.PublicKey]*openEntry{}
	var appeared, resolved uint64
	var resolveDurations []uint64

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		// Periodic full re-scan for new entrants + fresh positions.
		if nowTS()-lastFull >= fullRefresh {
			b, bo, w := fullScan(endpoint, watchRatio, minCollateral)
			banks, bankOracle = b, bo
			// Keep open events even if an account drops out of the watch-set.
			watch = w
			lastFull = nowTS()
		}

		prices := refreshPrices(endpoint, banks, bankOracle)

		// Fresh account data for the watch-set (positions change too, not just price).
		watchPks := make([]solana.PublicKey, 0, len(watch))
		for pk := range watch {
			watchPks = append(watchPks, pk)
		}
		fresh := getMultiple(endpoint, watchPks)
		for pk, raw := range fresh {
			if a, ok := liquidation.DecodeMarginfiAccount(raw); ok {
				if w, exists := watch[pk]; exists {
					w.account = a
				}
			}
		}

		ts := nowTS()
		curLiq := 0
		tightestRatio := math.Inf(-1)
		var tightestAuthority solana.PublicKey
		var tightestCollateral float64
		for pk, w := range watch {
			r := liquidation.MaintenanceHealth(w.account, banks, prices)
			if r.Missing > 0 {
				continue
			}
			ratio := r.Health.Ratio()
			deficit := r.Health.Value()
			collateral := r.Health.WeightedAssets
			if ratio > tightestRatio {
				tightestRatio = ratio
				tightestAuthority = w.authority
				tightestCollateral = collateral
			}

			isLiq := deficit < 0.0 && collateral >= minCollateral
			if isLiq {
				curLiq++
				e, exists := open[pk]
				if !exists {
					appeared++
					emit(map[string]any{
						"ts": ts, "event": "appeared", "account": pk.String(),
						"authority":      w.authority.String(),
						"collateral_usd": round2(collateral),
						"deficit_usd":    round2(deficit),
						"ratio":          round4(ratio),
					})
					fmt.Fprintf(os.Stderr, "[APPEARED %d] %s… collateral=$%.0f deficit=%+.2f\n",
						ts, short(w.authority), collateral, deficit)
					e = &openEntry{t0: ts, peakCol: collateral, peakDef: deficit}
					open[pk] = e
				}
				if collateral > e.peakCol {
					e.peakCol = collateral
				}
				if deficit < e.peakDef {
					e.peakDef = deficit
				}
			} else if e, exists := open[pk]; exists {
				delete(open, pk)
				resolved++
				dur := ts - e.t0
				resolveDurations = append(resolveDurations, dur)
				emit(map[string]any{
					"ts": ts, "event": "resolved", "account": pk.String(),
					"authority": w.authority.String(), "seen_secs": dur,
					"peak_collateral_usd": round2(e.peakCol),
					"peak_deficit_usd":    round2(e.peakDef),
				})
				fmt.Fprintf(os.Stderr, "[RESOLVED %d] %s… after %ds (peak collateral $%.0f, peak deficit %+.2f)\n",
					ts, short(w.authority), dur, e.peakCol, e.peakDef)
			}
		}

		sort.Slice(resolveDurations, func(i, j int) bool { return resolveDurations[i] < resolveDurations[j] })
		var med uint64
		if len(resolveDurations) > 0 {
			med = resolveDurations[len(resolveDurations)/2]
		}
		tightestShort := ""
		if tightestAuthority != (solana.PublicKey{}) {
			tightestShort = short(tightestAuthority)
		}
		fmt.Fprintf(os.Stderr, "[monitor %d] watch=%d liq_now=%d open=%d | total appeared=%d resolved=%d med_lifetime=%ds | tightest %.4f (%s…)\n",
			ts, len(watch), curLiq, len(open), appeared, resolved, med, tightestRatio, tightestShort)
		_ = tightestCollateral

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func round2(f float64) float64 { return math.Round(f*100.0) / 100.0 }
func round4(f float64) float64 { return math.Round(f*10000.0) / 10000.0 }
