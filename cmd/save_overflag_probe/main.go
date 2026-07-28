// Quantify the Save two-tier gating fix + calibrate the on-chain fire gate on
// LIVE mainnet data — read-only.
//
// The overflag bug: the executor flagged obligations "liquidatable" off the
// LAZER-projected ratio, then ran a full simulateTransaction/Bundle on each.
// But Solend settles at the ON-CHAIN oracle price, and Lazer leads/diverges —
// so the flagged set was dominated by phantoms (healthy on-chain), a
// per-cycle sim flood that starves a real opportunity's sim budget.
//
// The fix: Lazer NARROWS the watch-set; the ON-CHAIN price GATES the sim.
// Only obligations liquidatable at the on-chain oracle price earn a sim,
// ranked by USD deficit and capped top-K (MAX_FIRE_PER_CYCLE).
//
// CALIBRATION (task point 4): an obligation's STORED borrowed/unhealthy
// values are lazily updated by Solend (only when someone refresh_obligation's
// it), so a marginally-over-threshold obligation can sit "stored-liquidatable"
// while a fresh refresh_reserve (fresh Pyth price) shows it healthy — the
// "healthy at fresh price" sim rejects. This probe RE-COMPUTES each
// obligation's health from the freshly-fetched reserve prices + amounts
// (cToken exchange rate from the reserve bytes) and reports, for the
// stored-liquidatable set: (a) how many stay liquidatable at the fresh
// RESERVE price (the calibrated fire gate), and (b) the per-cycle sim
// reduction. If (a) is still hundreds, the residual phantoms are live-Pyth-
// vs-cranked-reserve drift the top-K cap must absorb.
//
// Usage: HELIUS_RPC=<url> [MIN_DEBT=100] [WATCH_RATIO=0.85] [ARM_RATIO=0.97]
//
//	[RATIO_CAP=3.0] [MAX_FIRE=4] go run ./cmd/save_overflag_probe
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/lazer"
	"solana-arb-backtest-go/internal/save"
)

const lazerUSDT uint32 = 8

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
		values := asArray(asMap(asMap(v["result"]))["value"])
		for idx, accV := range values {
			acc := asMap(accV)
			if acc == nil {
				continue
			}
			if b, ok := b64(acc["data"]); ok {
				out[chunk[idx]] = b
			}
		}
	}
	return out
}

// The cToken exchange rate + fresh-price health now live on save.Reserve /
// save.Obligation (Reserve.CtokenExchangeRate, Obligation.FreshHealth), so
// this probe just exercises those directly — no local layout duplication.

func envFloat(name string, def float64) float64 {
	if v := os.Getenv(name); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
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
	minDebt := envFloat("MIN_DEBT", 100.0)
	watchRatio := envFloat("WATCH_RATIO", 0.85)
	armRatio := envFloat("ARM_RATIO", 0.97)
	ratioCap := envFloat("RATIO_CAP", 3.0)
	maxFire := 4
	if v := os.Getenv("MAX_FIRE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			maxFire = n
		}
	}

	// Debt reserves (USDC/USDT/wSOL) — the accepted debt set.
	reserves := map[solana.PublicKey]*save.Reserve{}
	for _, res := range []string{save.USDCReserve, save.USDTReserve, save.WSOLReserve} {
		pk := solana.MustPublicKeyFromBase58(res)
		if d, ok := getAcct(endpoint, pk); ok {
			if r, ok := save.DecodeReserve(pk, d); ok {
				reserves[pk] = r
			}
		}
	}
	debtReserves := map[solana.PublicKey]bool{}
	for pk := range reserves {
		debtReserves[pk] = true
	}

	fmt.Fprintln(os.Stderr, "[overflag] scanning main-pool obligations …")
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

	type obEntry struct {
		pk solana.PublicKey
		o  *save.Obligation
	}
	var obls []obEntry
	for _, e := range entries {
		em := asMap(e)
		pk, err := solana.PublicKeyFromBase58(asStr(em["pubkey"]))
		if err != nil {
			continue
		}
		d, ok := b64(asMap(em["account"])["data"])
		if !ok {
			continue
		}
		o, ok := save.DecodeObligation(d)
		if !ok {
			continue
		}
		if len(o.Deposits) != 1 || len(o.Borrows) != 1 {
			continue
		}
		if !debtReserves[o.Borrows[0].Reserve] {
			continue
		}
		if o.BorrowedValue < minDebt {
			continue
		}
		obls = append(obls, obEntry{pk, o})
	}

	// Load collateral reserves.
	collSet := map[solana.PublicKey]bool{}
	for _, e := range obls {
		collSet[e.o.Deposits[0].Reserve] = true
	}
	var collPks []solana.PublicKey
	for pk := range collSet {
		collPks = append(collPks, pk)
	}
	for pk, raw := range getMultiple(endpoint, collPks) {
		if r, ok := save.DecodeReserve(pk, raw); ok {
			reserves[pk] = r
		}
	}

	mintFeed := lazer.MintFeedMap()
	mintFeed[solana.MustPublicKeyFromBase58(save.USDTMint)] = lazerUSDT

	snap := map[uint32]float64{}
	engine := save.NewEngine(minDebt, ratioCap)
	var engineEntries []save.ObligationEntry
	for _, e := range obls {
		engineEntries = append(engineEntries, save.ObligationEntry{Pubkey: e.pk, Obligation: e.o})
	}
	engine.Rebuild(engineEntries, reserves, mintFeed, watchRatio, snap)

	armTier := len(engine.Crossed(snap, armRatio))
	// BEFORE (old gate): the obligation's own STORED borrowed_value >
	// unhealthy_borrow_value (Solend's lazily-refreshed verdict, which the
	// executor used to flag + sim). AFTER (new gate): the engine's FRESH-price
	// fire tier — borrowed/unhealthy recomputed at the current reserve prices
	// via the cToken exchange rate (Obligation.FreshHealth), the value
	// Solend's `liquidate` recomputes at settle time.
	oblsByPk := map[solana.PublicKey]*save.Obligation{}
	for _, e := range obls {
		oblsByPk[e.pk] = e.o
	}
	type storedEntry struct {
		pk      solana.PublicKey
		deficit float64
		ratio   float64
	}
	var storedLiq []storedEntry
	for _, e := range obls {
		r := e.o.HealthRatio()
		if e.o.Liquidatable() && r <= ratioCap {
			storedLiq = append(storedLiq, storedEntry{e.pk, e.o.BorrowedValue - e.o.UnhealthyBorrowValue, r})
		}
	}
	freshFire := engine.OnChainLiquidatableRanked()

	// How many of the STORED-liquidatable set are phantoms (healthy at fresh price)?
	phantom := 0
	for _, se := range storedLiq {
		o := oblsByPk[se.pk]
		b, u, ok := o.FreshHealth(reserves)
		if !(ok && !(u > 0.0 && b > u)) {
			continue
		}
		phantom++
	}

	fmt.Println("\n=== Save fire-tier gate: STORED verdict vs FRESH cToken health — live mainnet ===")
	fmt.Printf("scanned obligations (main-pool, 1300B) ........ %d\n", len(entries))
	fmt.Printf("v1 / accepted-debt / ≥ $%.0f ............... %d\n", minDebt, len(obls))
	fmt.Printf("engine watch-set (%v ≤ ratio ≤ %v) ...... %d  (NEVER simulated)\n", watchRatio, ratioCap, len(engine.Accounts))
	fmt.Printf("within arm(%v) — Lazer near-threshold ...... %d\n", armRatio, armTier)
	fmt.Printf("BEFORE — on-chain liquidatable (STORED verdict) . %d  ← the phantom flood\n", len(storedLiq))
	fmt.Printf("AFTER  — on-chain liquidatable (FRESH cToken)  .. %d  ← NEW fire gate\n", len(freshFire))
	fmt.Printf("  stored-liquidatable that are phantoms @ fresh . %d  (dropped by the fresh gate)\n", phantom)
	fmt.Printf("fire cap (MAX_FIRE_PER_CYCLE) ................. %d\n", maxFire)

	fmt.Println("\nDIAGNOSTIC — stored deposit/borrow market_value vs FRESH recompute @ current reserve px")
	fmt.Println("(the collateral gap is the staleness that left the stored health stale-high):")
	for i, se := range storedLiq {
		if i >= 6 {
			break
		}
		o := oblsByPk[se.pk]
		d := o.Deposits[0]
		b := o.Borrows[0]
		coll := reserves[d.Reserve]
		debt := reserves[b.Reserve]
		rate := coll.CtokenExchangeRate()
		freshBor, freshUnh, ok := o.FreshHealth(reserves)
		if !ok {
			freshBor, freshUnh = 0.0, 0.0
		}
		freshDep := float64(d.DepositedAmount) * rate / pow10(int(coll.MintDecimals)) * coll.MarketPrice
		fmt.Printf("  %s\n", se.pk)
		fmt.Printf("     borrow  stored mv $%.2f  fresh $%.2f  (debt px $%.4f)\n", b.MarketValue, freshBor, debt.MarketPrice)
		fmt.Printf("     deposit stored mv $%.2f  fresh $%.2f  (coll px $%.4f, cToken rate %.5f, liq_thr %d%% → fresh unhealthy $%.2f)\n",
			d.MarketValue, freshDep, coll.MarketPrice, rate, coll.LiquidationThresholdPct, freshUnh)
	}

	fmt.Println("\ntop fresh fire-tier candidates (deficit desc), fresh ratio:")
	for i, re := range freshFire {
		if i >= 10 {
			break
		}
		fr, _ := engine.OnChainRatioOf(re.Obligation)
		fmt.Printf("  %s  fresh deficit $%.0f  fresh r%.4f\n", re.Obligation, re.Deficit, fr)
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
