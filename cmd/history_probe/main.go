// Backward-looking residual scan: replay the last N hours of landed swaps on
// both pools from chain history and reconstruct the cross-venue gap timeline.
// Complements backrun_probe (live, sub-slot) with slot-level coverage over a
// full day — including hours we weren't listening.
//
// Method: getSignaturesForAddress on each pool → getTransaction (jsonParsed)
// → the pool's vault balance deltas give each swap's execution price. A CLMM
// price only moves on swaps, so the last execution price on a venue ≈ its
// current price until the next swap. Caveat: execution price is the swap's
// average (mid of pre/post marginal price), so large swaps read ~half their
// own price impact as "gap" — treat counts near the floor as upper bounds.
//
// Usage: RPC_ENDPOINT=<url> HOURS=24 [pair env vars] \
//
//	go run ./cmd/history_probe
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
	"strconv"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/pools"
)

const tipCushionBps = 1.0

// Backstop for very active pools (liquid controls). When hit, the window is
// truncated and the report says so — never silently.
const maxTxPerPool = 8000

var httpClient = &http.Client{Timeout: 15 * time.Second}

// rpcCall posts a JSON-RPC request, retrying with backoff. Backs off hard on
// 429s — a rate-limit storm is slower than pacing.
func rpcCall(rpc string, body map[string]any) map[string]any {
	for attempt := 0; attempt < 5; attempt++ {
		b, _ := json.Marshal(body)
		resp, err := httpClient.Post(rpc, "application/json", bytes.NewReader(b))
		if err != nil {
			if attempt < 4 {
				time.Sleep(time.Duration(400<<attempt) * time.Millisecond)
				continue
			}
			fmt.Fprintf(os.Stderr, "rpc error (giving up): %v\n", err)
			return nil
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			time.Sleep(time.Duration(1500*(attempt+1)) * time.Millisecond)
			continue
		}
		var v map[string]any
		decErr := json.NewDecoder(resp.Body).Decode(&v)
		resp.Body.Close()
		if decErr != nil {
			if attempt < 4 {
				time.Sleep(time.Duration(400<<attempt) * time.Millisecond)
				continue
			}
			fmt.Fprintf(os.Stderr, "rpc error (giving up): %v\n", decErr)
			return nil
		}
		return v
	}
	return nil
}

func accountData(rpc, addr string) []byte {
	v := rpcCall(rpc, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{addr, map[string]any{"encoding": "base64"}}})
	if v == nil {
		return nil
	}
	result, _ := v["result"].(map[string]any)
	value, _ := result["value"].(map[string]any)
	if value == nil {
		return nil
	}
	dataArr, _ := value["data"].([]any)
	if len(dataArr) == 0 {
		return nil
	}
	s, ok := dataArr[0].(string)
	if !ok {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	return raw
}

func pkAt(d []byte, o int) string {
	return solana.PublicKeyFromBytes(d[o : o+32]).String()
}

type swap struct {
	slot      uint64
	blockTime int64
	venue     string
	price     float64 // quote per base (execution price)
	baseUI    float64 // |base delta| of the swap
}

// signaturesSince returns all pool signatures newer than cutoff (unix secs),
// oldest capped by maxTxPerPool. Returns (sigs, truncated).
func signaturesSince(rpc, pool string, cutoff int64) ([]string, bool) {
	var sigs []string
	var before string
	for {
		params := map[string]any{"limit": 1000}
		if before != "" {
			params["before"] = before
		}
		v := rpcCall(rpc, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getSignaturesForAddress",
			"params": []any{pool, params}})
		if v == nil {
			break
		}
		arr, _ := v["result"].([]any)
		if len(arr) == 0 {
			break
		}
		reachedCutoff := false
		for _, ev := range arr {
			em, _ := ev.(map[string]any)
			if em == nil {
				continue
			}
			bt, _ := em["blockTime"].(float64)
			if bt != 0 && int64(bt) < cutoff {
				reachedCutoff = true
				break
			}
			if em["err"] == nil {
				if s, ok := em["signature"].(string); ok {
					sigs = append(sigs, s)
				}
			}
		}
		if reachedCutoff {
			return sigs, false
		}
		if len(sigs) >= maxTxPerPool {
			return sigs, true
		}
		last, _ := arr[len(arr)-1].(map[string]any)
		if last == nil {
			break
		}
		nextBefore, _ := last["signature"].(string)
		if nextBefore == "" {
			break
		}
		before = nextBefore
	}
	return sigs, false
}

// tokenBalance reads the raw token amount (as a signed int64 — SPL token
// amounts are u64, well within range) of vault from a pre/post token
// balances array by matching accountIndex against the transaction's account
// keys.
func tokenBalance(bals []any, keys []string, vault string) int64 {
	for _, bv := range bals {
		bm, _ := bv.(map[string]any)
		if bm == nil {
			continue
		}
		idx, ok := bm["accountIndex"].(float64)
		if !ok || int(idx) >= len(keys) || keys[int(idx)] != vault {
			continue
		}
		uiAmt, _ := bm["uiTokenAmount"].(map[string]any)
		if uiAmt == nil {
			continue
		}
		s, _ := uiAmt["amount"].(string)
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

// swapFromTx derives the execution price of one landed tx from its pool
// vault deltas.
func swapFromTx(rpc, sig, venue, baseVault, quoteVault string, baseDec, quoteDec int32) *swap {
	v := rpcCall(rpc, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getTransaction",
		"params": []any{sig, map[string]any{"encoding": "jsonParsed", "maxSupportedTransactionVersion": 0,
			"commitment": "confirmed"}}})
	if v == nil {
		return nil
	}
	r, _ := v["result"].(map[string]any)
	if r == nil {
		return nil
	}
	meta, _ := r["meta"].(map[string]any)
	if meta == nil || meta["err"] != nil {
		return nil
	}
	txn, _ := r["transaction"].(map[string]any)
	msg, _ := txn["message"].(map[string]any)
	keysArr, _ := msg["accountKeys"].([]any)
	keys := make([]string, 0, len(keysArr))
	for _, k := range keysArr {
		km, _ := k.(map[string]any)
		if km == nil {
			continue
		}
		pk, _ := km["pubkey"].(string)
		keys = append(keys, pk)
	}
	postBals, _ := meta["postTokenBalances"].([]any)
	preBals, _ := meta["preTokenBalances"].([]any)

	dBase := tokenBalance(postBals, keys, baseVault) - tokenBalance(preBals, keys, baseVault)
	dQuote := tokenBalance(postBals, keys, quoteVault) - tokenBalance(preBals, keys, quoteVault)
	if dBase == 0 || dQuote == 0 {
		return nil // not a swap on this pool (liquidity op, or vault untouched)
	}
	baseUI := absFloat(dBase) / pow10(baseDec)
	quoteUI := absFloat(dQuote) / pow10(quoteDec)

	slotF, _ := r["slot"].(float64)
	btF, _ := r["blockTime"].(float64)
	return &swap{
		slot:      uint64(slotF),
		blockTime: int64(btF),
		venue:     venue,
		price:     quoteUI / baseUI,
		baseUI:    baseUI,
	}
}

func pow10(n int32) float64 {
	return math.Pow(10, float64(n))
}

func absFloat(n int64) float64 {
	if n < 0 {
		return float64(-n)
	}
	return float64(n)
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sorted := append([]float64(nil), v...)
	sort.Float64s(sorted)
	return sorted[len(sorted)/2]
}

func maxOf(v []float64) float64 {
	m := 0.0
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}

// chronoLite is a tiny UTC formatter (no external dep): unix secs → "HH:MM:SSZ".
func chronoLite(secs int64) string {
	s := secs % 86400
	if s < 0 {
		s += 86400
	}
	return fmt.Sprintf("%02d:%02d:%02dZ", s/3600, (s%3600)/60, s%60)
}

type clearEpisode struct {
	slot      uint64
	blockTime int64
	gap       float64
	size      float64
}

func main() {
	envfile.LoadDotEnv()
	rpc := os.Getenv("RPC_ENDPOINT")
	if rpc == "" {
		fmt.Fprintln(os.Stderr, "set RPC_ENDPOINT")
		os.Exit(1)
	}
	hours := 24.0
	if v := os.Getenv("HOURS"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			hours = f
		}
	}
	cfg := pools.Pair()
	feeBps := cfg.RoundTripFeeBps()
	cutoff := time.Now().Unix() - int64(hours*3600.0)

	// Vault addresses from the pool accounts (offsets verified on mainnet).
	orca := accountData(rpc, cfg.OrcaPool)
	if orca == nil {
		fmt.Fprintln(os.Stderr, "fetch orca pool failed")
		os.Exit(1)
	}
	ray := accountData(rpc, cfg.RayPool)
	if ray == nil {
		fmt.Fprintln(os.Stderr, "fetch ray pool failed")
		os.Exit(1)
	}
	orcaBaseIsA := pkAt(orca, 101) == cfg.BaseMint
	var orcaBaseV, orcaQuoteV string
	if orcaBaseIsA {
		orcaBaseV, orcaQuoteV = pkAt(orca, 133), pkAt(orca, 213)
	} else {
		orcaBaseV, orcaQuoteV = pkAt(orca, 213), pkAt(orca, 133)
	}
	rayBaseIs0 := pkAt(ray, 73) == cfg.BaseMint
	var rayBaseV, rayQuoteV string
	if rayBaseIs0 {
		rayBaseV, rayQuoteV = pkAt(ray, 137), pkAt(ray, 169)
	} else {
		rayBaseV, rayQuoteV = pkAt(ray, 169), pkAt(ray, 137)
	}

	fmt.Printf("history-probe — pair %s, floor %gbp (+%gbp cushion), last %gh\n\n",
		cfg.Label, feeBps, tipCushionBps, hours)

	type venueCfg struct {
		venue      string
		pool       string
		baseVault  string
		quoteVault string
	}
	venues := []venueCfg{
		{"Orca", cfg.OrcaPool, orcaBaseV, orcaQuoteV},
		{"Raydium", cfg.RayPool, rayBaseV, rayQuoteV},
	}

	var swaps []swap
	for _, vc := range venues {
		sigs, truncated := signaturesSince(rpc, vc.pool, cutoff)
		if truncated {
			fmt.Printf("⚠ %s: hit the %d-tx cap — window truncated, report covers less than %gh on this venue.\n",
				vc.venue, maxTxPerPool, hours)
		}
		fmt.Printf("%s: %d landed txs in window, fetching…\n", vc.venue, len(sigs))
		var n uint32
		for i, sig := range sigs {
			if s := swapFromTx(rpc, sig, vc.venue, vc.baseVault, vc.quoteVault, cfg.BaseDec, cfg.QuoteDec); s != nil {
				swaps = append(swaps, *s)
				n++
			}
			if (i+1)%200 == 0 {
				fmt.Fprintf(os.Stderr, "  %s: %d/%d fetched…\n", vc.venue, i+1, len(sigs))
			}
			time.Sleep(120 * time.Millisecond) // ~8 rps, under RPC rate limits
		}
		fmt.Printf("%s: %d swaps decoded\n", vc.venue, n)
	}

	sort.Slice(swaps, func(i, j int) bool { return swaps[i].slot < swaps[j].slot })

	// Replay: last exec price per venue is that venue's standing price (CLMM
	// price only moves on swaps). On every swap, measure the cross-venue gap.
	lastOrca, lastRay := math.NaN(), math.NaN()
	var gaps []float64
	var clears []clearEpisode
	var openSlot uint64
	haveOpen := false
	var lifetimes []uint64
	byHour := map[int64]uint32{}
	for _, s := range swaps {
		if s.venue == "Orca" {
			lastOrca = s.price
		} else {
			lastRay = s.price
		}
		if math.IsNaN(lastOrca) || math.IsInf(lastOrca, 0) || math.IsNaN(lastRay) || math.IsInf(lastRay, 0) {
			continue
		}
		m := lastOrca
		if lastRay < m {
			m = lastRay
		}
		gap := (lastRay - lastOrca) / m * 10_000.0
		if gap < 0 {
			gap = -gap
		}
		gaps = append(gaps, gap)
		if gap > feeBps {
			if !haveOpen {
				haveOpen = true
				openSlot = s.slot
				clears = append(clears, clearEpisode{s.slot, s.blockTime, gap, s.baseUI})
				byHour[(s.blockTime/3600)%24]++
			}
		} else if haveOpen {
			lifetimes = append(lifetimes, s.slot-openSlot)
			haveOpen = false
		}
	}

	fmt.Printf("\n──────── history-probe report (%gh) ────────\n", hours)
	fmt.Printf("swaps decoded: %d (both venues)\n", len(swaps))
	if len(gaps) == 0 {
		fmt.Println("no overlapping price data — one venue had no swaps in the window.")
		return
	}
	fmt.Printf("cross-venue gap at each swap: median %.1f bp, max %.1f bp\n", median(gaps), maxOf(gaps))
	fmt.Printf("fee-clearing episodes (>%gbp): %d\n", feeBps, len(clears))
	var strong int
	for _, c := range clears {
		if c.gap > feeBps+tipCushionBps {
			strong++
		}
	}
	fmt.Printf("  above floor+cushion (>%.0fbp): %d\n", feeBps+tipCushionBps, strong)
	if len(clears) > 0 {
		clearGaps := make([]float64, len(clears))
		for i, c := range clears {
			clearGaps[i] = c.gap
		}
		fmt.Printf("  gap at open: median %.1f bp, max %.1f bp\n", median(clearGaps), maxOf(clearGaps))
		if len(lifetimes) > 0 {
			lt := append([]uint64(nil), lifetimes...)
			sort.Slice(lt, func(i, j int) bool { return lt[i] < lt[j] })
			fmt.Printf("  episode lifetime: median %d slots (~%.1fs), max %d slots\n",
				lt[len(lt)/2], float64(lt[len(lt)/2])*0.4, lt[len(lt)-1])
		}
		hoursSorted := make([]int64, 0, len(byHour))
		for h := range byHour {
			hoursSorted = append(hoursSorted, h)
		}
		sort.Slice(hoursSorted, func(i, j int) bool { return hoursSorted[i] < hoursSorted[j] })
		hist := make([]string, 0, len(hoursSorted))
		for _, h := range hoursSorted {
			hist = append(hist, fmt.Sprintf("%02dh:%d", h, byHour[h]))
		}
		fmt.Printf("  episodes by UTC hour: %s\n", strings.Join(hist, " "))
		fmt.Println("\nlast 10 episodes:")
		start := len(clears) - 10
		if start < 0 {
			start = 0
		}
		for i := len(clears) - 1; i >= start; i-- {
			c := clears[i]
			fmt.Printf("  slot %d %s gap %.1fbp (trigger swap %.3f base)\n",
				c.slot, chronoLite(c.blockTime), c.gap, c.size)
		}
	}
}
