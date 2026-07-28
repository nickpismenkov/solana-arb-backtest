// Profitability watcher — proves the WINNING path with zero cost. Loops
// over live mainnet: refresh both pools, build the guarded arb (both
// directions) from fresh state, simulateTransaction. Most iterations show
// the guard reverting at leg 2 (no edge). The instant a real edge appears —
// standing or transient — the sim comes back clean (err=null), meaning a
// profitable arb exists right now and our tx would land. Logs every clean
// hit + the spot edge to profit_watch.jsonl. No money, no submission — pure
// measurement.
//
// Usage: RPC_ENDPOINT=<url> ALT_ADDRESS=<alt> [BORROW_USDC=500] [POLL_MS=800]
//
//	[RUN_DIR=runs] go run ./cmd/profit_watch
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/arb"
	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/pools"
)

func rpc(endpoint string, body map[string]any) (map[string]any, bool) {
	for attempt := 0; attempt < 3; attempt++ {
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
		time.Sleep(time.Duration(200<<attempt) * time.Millisecond)
	}
	return nil, false
}

func accountData(endpoint, addr string) ([]byte, bool) {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{addr, map[string]string{"encoding": "base64"}}})
	if !ok {
		return nil, false
	}
	result, _ := v["result"].(map[string]any)
	value, _ := result["value"].(map[string]any)
	dataArr, _ := value["data"].([]any)
	if len(dataArr) == 0 {
		return nil, false
	}
	s, ok := dataArr[0].(string)
	if !ok {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	return raw, true
}

func now() uint64 {
	return uint64(time.Now().Unix())
}

func main() {
	envfile.LoadDotEnv()

	endpoint := os.Getenv("RPC_ENDPOINT")
	if endpoint == "" {
		fmt.Fprintln(os.Stderr, "RPC_ENDPOINT")
		os.Exit(1)
	}
	altAddr := os.Getenv("ALT_ADDRESS")
	if altAddr == "" {
		fmt.Fprintln(os.Stderr, "ALT_ADDRESS")
		os.Exit(1)
	}
	borrowUI := 500.0
	if v := os.Getenv("BORROW_USDC"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			borrowUI = f
		}
	}
	pollMs := uint64(800)
	if v := os.Getenv("POLL_MS"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			pollMs = n
		}
	}
	runDir := os.Getenv("RUN_DIR")
	if runDir == "" {
		runDir = "runs"
	}
	borrowAmount := uint64(borrowUI * 1e6)
	cfg := pools.Pair()
	// Placeholder signer — simulate with sigVerify=false, so no keypair needed.
	signer := solana.MustPublicKeyFromBase58("Anu6Awu4kxaEDrg1nkpcikx6tJ2xhfVci5TvDrZBsZEB")

	altData, ok := accountData(endpoint, altAddr)
	if !ok {
		fmt.Fprintln(os.Stderr, "ALT: fetch failed")
		os.Exit(1)
	}
	alt := arb.LoadAlt(altAddr, altData)
	_ = os.MkdirAll(runDir, 0o755)
	out := runDir + "/profit_watch.jsonl"

	fmt.Fprintf(os.Stderr, "profit-watch %s borrow %v USDC poll %dms — simulating both dirs; logs clean hits → %s\n",
		cfg.Label, borrowUI, pollMs, out)
	var iters, clean uint64
	bestEdgeBps := math.Inf(-1)

	pollDur := time.Duration(pollMs) * time.Millisecond
	for {
		iters++
		o, okO := accountData(endpoint, cfg.OrcaPool)
		r, okR := accountData(endpoint, cfg.RayPool)
		if !okO || !okR {
			time.Sleep(pollDur)
			continue
		}
		// Spot edge for context (stale-free here: just-fetched pools).
		edgeBps := math.NaN()
		po, okPO := pools.OrcaPrice(o)
		pr, okPR := pools.RayClmmPrice(r)
		if okPO && okPR && po > 0.0 && pr > 0.0 {
			minP := po
			if pr < minP {
				minP = pr
			}
			edgeBps = (math.Abs(pr-po)/minP)*1e4 - cfg.RoundTripFeeBps()
		}
		if !math.IsNaN(edgeBps) && !math.IsInf(edgeBps, 0) && edgeBps > bestEdgeBps {
			bestEdgeBps = edgeBps
		}
		poolData := &arb.PoolData{Orca: o, Ray: r}
		bh := solana.Hash{}

		for _, orcaFirst := range []bool{false, true} {
			tx, err := arb.BuildArbTx(poolData, signer, alt, borrowAmount, orcaFirst, nil, 0, 10_000, bh, 0)
			if err != nil {
				continue
			}
			tx.Signatures = []solana.Signature{{}}
			raw, err := tx.MarshalBinary()
			if err != nil {
				continue
			}
			txB64 := base64.StdEncoding.EncodeToString(raw)
			v, _ := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "simulateTransaction",
				"params": []any{txB64, map[string]any{"encoding": "base64", "sigVerify": false, "replaceRecentBlockhash": true}}})
			var val map[string]any
			if v != nil {
				if result, ok := v["result"].(map[string]any); ok {
					if value, ok := result["value"].(map[string]any); ok {
						val = value
					}
				}
			}
			if val != nil && val["err"] == nil {
				clean++
				dir := "ray→orca"
				if orcaFirst {
					dir = "orca→ray"
				}
				cu := val["unitsConsumed"]
				fmt.Fprintf(os.Stderr, "🎉 CLEAN SIM [%s] — profitable arb exists NOW, tx would land (edge≈%.2fbp, cu=%v)\n", dir, edgeBps, cu)
				row := map[string]any{"t": now(), "dir": dir, "edge_bps": edgeBps, "cu": cu, "borrow_usdc": borrowUI}
				if f, err := os.OpenFile(out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
					if b, err := json.Marshal(row); err == nil {
						_, _ = f.Write(append(b, '\n'))
					}
					f.Close()
				}
			}
		}
		if iters%50 == 0 {
			fmt.Fprintf(os.Stderr, "[profit-watch] iters=%d clean_sims=%d best_edge=%.2fbp (need >0 to profit)\n", iters, clean, bestEdgeBps)
		}
		time.Sleep(pollDur)
	}
}
