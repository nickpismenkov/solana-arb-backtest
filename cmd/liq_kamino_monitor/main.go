// Kamino continuous liquidation-opportunity monitor (read-only, long-run).
//
// Full-scans obligations occasionally to build a watch-set of near-liquidation
// positions, then polls only that set + its reserves frequently — recomputing
// health at each reserve's latest cached price. Logs when a position crosses
// liquidatable (APPEARED) and when it's taken/recovers (RESOLVED, with survival
// seconds). Over hours this measures real opportunity flow + how fast the
// competition takes them = the go/no-go for a Kamino liquidation executor.
//
// Usage: HELIUS_RPC=<url> [POLL_SECS=15] [FULL_SCAN_SECS=300] [WATCH_RATIO=0.92]
//
//	[MIN_COLLATERAL_USD=100] go run ./cmd/liq_kamino_monitor
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/kamino"
)

func nowTs() uint64 { return uint64(time.Now().Unix()) }

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

func getMultiple(endpoint string, keys []solana.PublicKey, sliceLen uint64) map[solana.PublicKey][]byte {
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
		v, ok := rpc(endpoint, map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "getMultipleAccounts",
			"params": []any{strs, map[string]any{"encoding": "base64", "dataSlice": map[string]any{"offset": 0, "length": sliceLen}}},
		})
		if !ok {
			continue
		}
		values := asArray(asMap(v["result"])["value"])
		for i, accV := range values {
			acc := asMap(accV)
			if acc == nil {
				continue
			}
			if raw, ok := b64(acc["data"]); ok {
				out[chunk[i]] = raw
			}
		}
		time.Sleep(40 * time.Millisecond)
	}
	return out
}

type obligationEntry struct {
	pk solana.PublicKey
	ob *kamino.Obligation
}

// fetchReserves fetches reserves referenced by a set of obligations, decode → map.
func fetchReserves(endpoint string, obs []obligationEntry) map[solana.PublicKey]*kamino.Reserve {
	seen := map[solana.PublicKey]bool{}
	var pks []solana.PublicKey
	for _, e := range obs {
		for _, d := range e.ob.Deposits {
			if !seen[d.Reserve] {
				seen[d.Reserve] = true
				pks = append(pks, d.Reserve)
			}
		}
		for _, b := range e.ob.Borrows {
			if !seen[b.Reserve] {
				seen[b.Reserve] = true
				pks = append(pks, b.Reserve)
			}
		}
	}
	raw := getMultiple(endpoint, pks, 5016)
	out := map[solana.PublicKey]*kamino.Reserve{}
	for pk, data := range raw {
		if r, ok := kamino.DecodeReserve(data); ok {
			out[pk] = r
		}
	}
	return out
}

// fullScan → all debt obligations (with their account pubkey).
func fullScan(endpoint string) []obligationEntry {
	resp, _ := rpc(endpoint, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{kamino.KlendProgram, map[string]any{"encoding": "base64", "dataSlice": map[string]any{"offset": 0, "length": 2288},
			"filters": []any{map[string]any{"dataSize": kamino.ObligationSize}}}},
	})
	entries := asArray(resp["result"])
	var out []obligationEntry
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
		o, ok := kamino.DecodeObligation(raw)
		if !ok || len(o.Borrows) == 0 {
			continue
		}
		out = append(out, obligationEntry{pk, o})
	}
	return out
}

func envF64(name string, def float64) float64 {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n
		}
	}
	return def
}
func envU64(name string, def uint64) uint64 {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func satSub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
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

type openEntry struct {
	firstTs uint64
	peak    float64
	owner   string
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
	poll := time.Duration(envU64("POLL_SECS", 15)) * time.Second
	fullScanSecs := envU64("FULL_SCAN_SECS", 300)
	watchRatio := envF64("WATCH_RATIO", 0.92)
	minCollateral := envF64("MIN_COLLATERAL_USD", 100.0)

	if err := os.MkdirAll("runs", 0o755); err != nil {
		fail("mkdir runs: %v", err)
	}
	logFile, err := os.OpenFile("runs/kamino_opportunities.jsonl", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fail("open log: %v", err)
	}
	defer logFile.Close()
	emit := func(v map[string]any) {
		b, _ := json.Marshal(v)
		fmt.Fprintln(logFile, string(b))
	}

	fmt.Fprintf(os.Stderr, "[kmon] poll=%ds full_scan=%ds watch_ratio=%g min_collateral=$%g\n",
		int(poll.Seconds()), fullScanSecs, watchRatio, minCollateral)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// watch-set: account pubkey → obligation (positions refreshed each poll)
	watch := map[solana.PublicKey]*kamino.Obligation{}
	var lastFull uint64
	open := map[solana.PublicKey]*openEntry{} // acct → (first_ts, peak_collateral, owner)
	var appeared, resolved uint64
	var durations []uint64

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Periodic full scan → refresh watch-set (near-liquidation + trustworthy).
		if satSub(nowTs(), lastFull) >= fullScanSecs {
			fmt.Fprintf(os.Stderr, "[kmon %d] full scan …\n", nowTs())
			all := fullScan(endpoint)
			reserves := fetchReserves(endpoint, all)
			newWatch := map[solana.PublicKey]*kamino.Obligation{}
			for _, e := range all {
				r := kamino.Recompute(e.ob, reserves)
				if r.Trustworthy() && r.DepositedValue >= minCollateral && r.Ratio() >= watchRatio {
					newWatch[e.pk] = e.ob
				}
			}
			fmt.Fprintf(os.Stderr, "[kmon %d] watch-set: %d obligations (ratio ≥ %g, collateral ≥ $%g)\n",
				nowTs(), len(newWatch), watchRatio, minCollateral)
			watch = newWatch
			lastFull = nowTs()
		}

		// Poll: fresh obligation data + fresh reserve prices → recompute.
		var watchPks []solana.PublicKey
		for pk := range watch {
			watchPks = append(watchPks, pk)
		}
		for pk, raw := range getMultiple(endpoint, watchPks, 2288) {
			if o, ok := kamino.DecodeObligation(raw); ok {
				watch[pk] = o
			}
		}
		var obs []obligationEntry
		for pk, o := range watch {
			obs = append(obs, obligationEntry{pk, o})
		}
		reserves := fetchReserves(endpoint, obs)

		ts := nowTs()
		curLiq := 0
		tightest := 0.0
		for _, e := range obs {
			pk, o := e.pk, e.ob
			r := kamino.Recompute(o, reserves)
			if !r.Trustworthy() {
				continue
			}
			if r.Ratio() > tightest {
				tightest = r.Ratio()
			}
			if r.Liquidatable() && r.DepositedValue >= minCollateral {
				curLiq++
				entry, existed := open[pk]
				if !existed {
					appeared++
					emit(map[string]any{"ts": ts, "event": "appeared", "protocol": "kamino",
						"account": pk.String(), "owner": o.Owner.String(),
						"collateral_usd": round2(r.DepositedValue),
						"debt_usd":       round2(r.BfAdjustedDebt),
						"ratio":          round4(r.Ratio())})
					fmt.Fprintf(os.Stderr, "[APPEARED %d] %s… collateral=$%.0f ratio=%.4f\n", ts, shortStr(o.Owner.String(), 8), r.DepositedValue, r.Ratio())
					entry = &openEntry{firstTs: ts, peak: r.DepositedValue, owner: o.Owner.String()}
					open[pk] = entry
				}
				if r.DepositedValue > entry.peak {
					entry.peak = r.DepositedValue
				}
			} else if entry, existed := open[pk]; existed {
				delete(open, pk)
				resolved++
				dur := satSub(ts, entry.firstTs)
				durations = append(durations, dur)
				emit(map[string]any{"ts": ts, "event": "resolved", "protocol": "kamino",
					"account": pk.String(), "owner": entry.owner, "seen_secs": dur,
					"peak_collateral_usd": round2(entry.peak)})
				fmt.Fprintf(os.Stderr, "[RESOLVED %d] %s… after %ds (peak $%.0f)\n", ts, shortStr(entry.owner, 8), dur, entry.peak)
			}
		}
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		med := uint64(0)
		if len(durations) > 0 {
			med = durations[len(durations)/2]
		}
		fmt.Fprintf(os.Stderr, "[kmon %d] watch=%d liq_now=%d open=%d | appeared=%d resolved=%d med_survival=%ds | tightest %.4f\n",
			ts, len(watch), curLiq, len(open), appeared, resolved, med, tightest)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func round2(v float64) float64 { return roundN(v, 100.0) }
func round4(v float64) float64 { return roundN(v, 10000.0) }
func roundN(v, scale float64) float64 {
	return float64(int64(v*scale+sign(v)*0.5)) / scale
}
func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}
