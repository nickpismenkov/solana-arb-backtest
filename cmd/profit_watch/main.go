// Command profit_watch is the profitability watcher — proves the WINNING
// path with zero cost. Loops over live mainnet: refresh both pools, build
// the guarded arb (both directions) from fresh state, simulateTransaction.
// Most iterations show the guard reverting at leg 2 (no edge). The instant a
// real edge appears — standing or transient — the sim comes back clean
// (err=null), meaning a profitable arb exists right now and our tx would
// land. Logs every clean hit + the spot edge to profit_watch.jsonl. No
// money, no submission — pure measurement.
//
// Usage: RPC_ENDPOINT=<url> ALT_ADDRESS=<alt> [BORROW_USDC=500] [POLL_MS=800] \
//
//	[RUN_DIR=runs] go run ./cmd/profit_watch
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"time"

	"arbengine/internal/arb"
	"arbengine/internal/config"
	"arbengine/internal/pools"
	"arbengine/internal/rpcclient"
	"arbengine/internal/solana"
)

func now() uint64 {
	return uint64(time.Now().Unix())
}

func main() {
	config.LoadDotenv()
	endpoint, ok := config.EnvOptional("RPC_ENDPOINT")
	if !ok {
		fmt.Fprintln(os.Stderr, "RPC_ENDPOINT")
		os.Exit(1)
	}
	altAddr, ok := config.EnvOptional("ALT_ADDRESS")
	if !ok {
		fmt.Fprintln(os.Stderr, "ALT_ADDRESS")
		os.Exit(1)
	}
	borrowUI := config.EnvFloat("BORROW_USDC", 500.0)
	pollMs := config.EnvUint64("POLL_MS", 800)
	runDir := config.EnvOr("RUN_DIR", "runs")
	borrowAmount := uint64(borrowUI * 1e6)
	cfg := pools.Pair()
	// Placeholder signer — simulate with sigVerify=false, so no keypair needed.
	signer := solana.MustPubkeyFromBase58("Anu6Awu4kxaEDrg1nkpcikx6tJ2xhfVci5TvDrZBsZEB")

	c := rpcclient.New(endpoint)
	altPk := solana.MustPubkeyFromBase58(altAddr)
	altData, err := c.GetAccountData(altPk)
	if err != nil || altData == nil {
		fmt.Fprintln(os.Stderr, "ALT")
		os.Exit(1)
	}
	alt := arb.LoadALT(altAddr, altData)
	_ = os.MkdirAll(runDir, 0o755)
	out := fmt.Sprintf("%s/profit_watch.jsonl", runDir)

	fmt.Fprintf(os.Stderr, "profit-watch %s borrow %g USDC poll %dms — simulating both dirs; logs clean hits → %s\n",
		cfg.Label, borrowUI, pollMs, out)
	var iters, clean uint64
	bestEdgeBps := math.Inf(-1)

	for {
		iters++
		od, errO := c.GetAccountData(solana.MustPubkeyFromBase58(cfg.OrcaPool))
		rd, errR := c.GetAccountData(solana.MustPubkeyFromBase58(cfg.RayPool))
		if errO != nil || errR != nil || od == nil || rd == nil {
			time.Sleep(time.Duration(pollMs) * time.Millisecond)
			continue
		}
		// Spot edge for context (stale-free here: just-fetched pools).
		edgeBps := math.NaN()
		po, okO := pools.OrcaPrice(od)
		pr, okR := pools.RayClmmPrice(rd)
		if okO && okR && po > 0 && pr > 0 {
			edgeBps = (math.Abs(pr-po)/math.Min(po, pr))*1e4 - cfg.RoundTripFeeBps()
		}
		if !math.IsNaN(edgeBps) && !math.IsInf(edgeBps, 0) && edgeBps > bestEdgeBps {
			bestEdgeBps = edgeBps
		}
		poolData := arb.PoolData{Orca: od, Ray: rd}
		bh := solana.Hash{}

		for _, orcaFirst := range []bool{false, true} {
			tx, err := arb.BuildArbTx(poolData, signer, alt, borrowAmount, orcaFirst, nil, 0, 10_000, bh, 0)
			if err != nil {
				continue
			}
			b64, err := tx.Base64()
			if err != nil {
				continue
			}
			raw, _ := c.SimulateTransaction(b64)
			if raw == nil {
				continue
			}
			var val struct {
				Err           json.RawMessage `json:"err"`
				UnitsConsumed json.RawMessage `json:"unitsConsumed"`
			}
			if err := json.Unmarshal(raw, &val); err != nil {
				continue
			}
			isNull := len(val.Err) == 0 || string(val.Err) == "null"
			if isNull {
				clean++
				dir := "ray→orca"
				if orcaFirst {
					dir = "orca→ray"
				}
				cu := "null"
				if len(val.UnitsConsumed) > 0 {
					cu = string(val.UnitsConsumed)
				}
				fmt.Fprintf(os.Stderr, "🎉 CLEAN SIM [%s] — profitable arb exists NOW, tx would land (edge≈%.2fbp, cu=%s)\n", dir, edgeBps, cu)
				row := map[string]any{"t": now(), "dir": dir, "edge_bps": edgeBps, "cu": json.RawMessage(cu), "borrow_usdc": borrowUI}
				appendJSONL(out, row)
			}
		}
		if iters%50 == 0 {
			fmt.Fprintf(os.Stderr, "[profit-watch] iters=%d clean_sims=%d best_edge=%.2fbp (need >0 to profit)\n", iters, clean, bestEdgeBps)
		}
		time.Sleep(time.Duration(pollMs) * time.Millisecond)
	}
}

func appendJSONL(path string, row any) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	line, err := json.Marshal(row)
	if err != nil {
		return
	}
	f.Write(line)
	f.Write([]byte("\n"))
}
