// The go/no-go measurement. On each ShredStream trigger (a swap hitting one of
// our pools), simulate that victim tx against current state and read the
// POST-victim pool prices — the residual cross-venue gap a backrun placed
// right after it could capture. Real chain math (CPI and all), no tx
// construction, no money at risk.
//
// Reverts (the victim's own slippage guard) are skipped — those wouldn't have
// moved the pool anyway. Sampling: while a simulate is in flight, queued
// triggers are drained and counted as skipped (we measure at the RPC's pace).
//
// Usage (on the box):
//
//	RPC_ENDPOINT=<helius-url> SHREDSTREAM_PORT=20000 RUN_MS=600000 \
//	  go run ./cmd/backrun_probe
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/pools"
	"solana-arb-backtest-go/internal/shredstream"
)

const tipCushionBps = 2.0 // rough gas+tip headroom

var httpClient = &http.Client{Timeout: 15 * time.Second}

// simulateVictim simulates the victim tx and returns (orcaPrice, rayPrice)
// from the post-simulation account state; ok=false if the victim reverted or
// the sim/decode failed.
func simulateVictim(rpc, txB64 string) (float64, float64, bool) {
	body := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "simulateTransaction",
		"params": []any{txB64, map[string]any{
			"encoding": "base64", "sigVerify": false, "replaceRecentBlockhash": true,
			"accounts": map[string]any{"encoding": "base64", "addresses": []string{pools.Pair().OrcaPool, pools.Pair().RayPool}},
		}},
	}
	b, _ := json.Marshal(body)
	resp, err := httpClient.Post(rpc, "application/json", bytes.NewReader(b))
	if err != nil {
		return 0, 0, false
	}
	defer resp.Body.Close()
	var v map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return 0, 0, false
	}
	result, _ := v["result"].(map[string]any)
	value, _ := result["value"].(map[string]any)
	if value == nil {
		return 0, 0, false
	}
	if value["err"] != nil {
		return 0, 0, false // victim reverted (slippage) — wouldn't move the pool
	}
	accs, _ := value["accounts"].([]any)
	if len(accs) < 2 {
		return 0, 0, false
	}
	dec := func(i int) ([]byte, bool) {
		am, _ := accs[i].(map[string]any)
		if am == nil {
			return nil, false
		}
		dataArr, _ := am["data"].([]any)
		if len(dataArr) == 0 {
			return nil, false
		}
		s, ok := dataArr[0].(string)
		if !ok {
			return nil, false
		}
		raw, err := base64.StdEncoding.DecodeString(s)
		return raw, err == nil
	}
	orcaData, ok := dec(0)
	if !ok {
		return 0, 0, false
	}
	rayData, ok := dec(1)
	if !ok {
		return 0, 0, false
	}
	orca, ok := pools.OrcaPrice(orcaData)
	if !ok {
		return 0, 0, false
	}
	ray, ok := pools.RayClmmPrice(rayData)
	if !ok {
		return 0, 0, false
	}
	return orca, ray, true
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

func main() {
	envfile.LoadDotEnv()
	rpc := os.Getenv("RPC_ENDPOINT")
	if rpc == "" {
		fmt.Fprintln(os.Stderr, "set RPC_ENDPOINT (Helius) for simulate + ALT")
		os.Exit(1)
	}
	port := uint64(20000)
	if v := os.Getenv("SHREDSTREAM_PORT"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 16); err == nil {
			port = n
		}
	}
	runMs := uint64(600_000)
	if v := os.Getenv("RUN_MS"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			runMs = n
		}
	}

	feeBps := pools.Pair().RoundTripFeeBps()
	fmt.Printf("backrun-probe — simulate victims → residual gap. pair %s, threshold: fee %gbp (+%gbp cushion). Running %ds…\n\n",
		pools.Pair().Label, feeBps, tipCushionBps, runMs/1000)

	triggers := make(chan shredstream.Trigger, 256)
	go func() {
		if err := shredstream.RunFeed(uint16(port), rpc, nil, triggers); err != nil {
			fmt.Fprintf(os.Stderr, "shredstream feed error: %v\n", err)
		}
	}()

	var triggerCount, skipped, simmed, reverted, opps, oppsNet uint64
	var gaps, nets []float64

	deadline := time.After(time.Duration(runMs) * time.Millisecond)

loop:
	for {
		select {
		case <-deadline:
			break loop
		case t, ok := <-triggers:
			if !ok {
				break loop
			}
			triggerCount++
			// Drain backlog accumulated while the last sim ran → sample at RPC pace.
		drain:
			for {
				select {
				case <-triggers:
					triggerCount++
					skipped++
				default:
					break drain
				}
			}
			if len(t.Raw) == 0 {
				continue
			}
			txB64 := base64.StdEncoding.EncodeToString(t.Raw)
			orca, ray, ok := simulateVictim(rpc, txB64)
			if !ok {
				reverted++
				continue
			}
			simmed++
			m := orca
			if ray < m {
				m = ray
			}
			gap := (ray - orca) / m * 10_000.0
			if gap < 0 {
				gap = -gap
			}
			gaps = append(gaps, gap)
			if gap > feeBps {
				opps++
				net := gap - feeBps
				nets = append(nets, net)
				if gap > feeBps+tipCushionBps {
					oppsNet++
				}
				fmt.Printf("⚡ backrunnable via %s slot %d — gap %.1fbp, net %.1fbp (post-victim Orca $%.4f / Ray $%.4f)\n",
					t.Venue, t.Slot, gap, net, orca, ray)
			}
		}
	}

	fmt.Printf("\n──────── backrun-probe report (%ds) ────────\n", runMs/1000)
	fmt.Printf("pool triggers seen:        %d\n", triggerCount)
	fmt.Printf("  simulated (sampled):     %d\n", simmed+reverted)
	fmt.Printf("  skipped (RPC-paced):     %d\n", skipped)
	fmt.Printf("victim sims applied ok:    %d\n", simmed)
	fmt.Printf("victim sims reverted:      %d  (own slippage — no pool move)\n", reverted)
	fmt.Println("── residual cross-venue gap after a real swap ──")
	if simmed > 0 {
		fmt.Printf("  median gap: %.1f bp   max gap: %.1f bp\n", median(gaps), maxOf(gaps))
		fmt.Printf("  fee-clearing (>%gbp):        %d/%d (%.0f%%)\n", feeBps, opps, simmed, float64(opps)/float64(simmed)*100.0)
		fmt.Printf("  after tip cushion (>%.0fbp): %d/%d (%.0f%%)\n", feeBps+tipCushionBps, oppsNet, simmed, float64(oppsNet)/float64(simmed)*100.0)
		if len(nets) > 0 {
			fmt.Printf("  net edge when present: median %.1f bp, max %.1f bp\n", median(nets), maxOf(nets))
		}
	} else {
		fmt.Println("  no successful victim sims — check RPC / freshness.")
	}
	fmt.Println()
}
