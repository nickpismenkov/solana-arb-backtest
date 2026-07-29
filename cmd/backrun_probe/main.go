// Command backrun_probe is the go/no-go measurement. On each ShredStream
// trigger (a swap hitting one of our pools), simulate that victim tx
// against current state and read the POST-victim pool prices — the
// residual cross-venue gap a backrun placed right after it could capture.
// Real chain math (CPI and all), no tx construction, no money at risk.
//
// Reverts (the victim's own slippage guard) are skipped — those wouldn't
// have moved the pool anyway. Sampling: while a simulate is in flight,
// queued triggers are drained and counted as skipped (we measure at the
// RPC's pace).
//
// Usage (on the box):
//
//	RPC_ENDPOINT=<helius-url> SHREDSTREAM_PORT=20000 RUN_MS=600000 \
//	  go run ./cmd/backrun_probe
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
	"sort"
	"time"

	"arbengine/internal/config"
	"arbengine/internal/pools"
	"arbengine/internal/shredstream"
)

// tipCushionBps is rough gas+tip headroom.
const tipCushionBps = 2.0

var sharedClient = &http.Client{Timeout: 15 * time.Second}

func simulateVictim(rpc, txB64 string) (float64, float64, bool) {
	cfg := pools.Pair()
	body := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "simulateTransaction",
		"params": []any{txB64, map[string]any{
			"encoding": "base64", "sigVerify": false, "replaceRecentBlockhash": true,
			"accounts": map[string]any{"encoding": "base64", "addresses": []string{cfg.OrcaPool, cfg.RayPool}},
		}},
	}
	b, err := json.Marshal(body)
	if err != nil {
		return 0, 0, false
	}
	resp, err := sharedClient.Post(rpc, "application/json", bytes.NewReader(b))
	if err != nil {
		return 0, 0, false
	}
	defer resp.Body.Close()
	var v struct {
		Result struct {
			Value struct {
				Err      any `json:"err"`
				Accounts []struct {
					Data []string `json:"data"`
				} `json:"accounts"`
			} `json:"value"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return 0, 0, false
	}
	if v.Result.Value.Err != nil {
		return 0, 0, false // victim reverted (slippage) — wouldn't move the pool
	}
	accs := v.Result.Value.Accounts
	if len(accs) < 2 {
		return 0, 0, false
	}
	dec := func(i int) ([]byte, bool) {
		if i >= len(accs) || len(accs[i].Data) == 0 {
			return nil, false
		}
		b, err := base64.StdEncoding.DecodeString(accs[i].Data[0])
		if err != nil {
			return nil, false
		}
		return b, true
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
		return 0.0
	}
	sorted := append([]float64{}, v...)
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
	config.LoadDotenv()

	rpc, ok := config.EnvOptional("RPC_ENDPOINT")
	if !ok {
		fmt.Fprintln(os.Stderr, "set RPC_ENDPOINT (Helius) for simulate + ALT")
		os.Exit(1)
	}
	port := config.EnvInt("SHREDSTREAM_PORT", 20000)
	runMs := config.EnvInt("RUN_MS", 600_000)

	feeBps := pools.Pair().RoundTripFeeBps()
	fmt.Printf(
		"backrun-probe — simulate victims → residual gap. pair %s, threshold: fee %gbp (+%gbp cushion). Running %ds…\n\n",
		pools.Pair().Label, feeBps, tipCushionBps, runMs/1000,
	)

	triggers := make(chan shredstream.Trigger, 4096)
	shredstream.RunShredstreamFeed(uint16(port), rpc, triggers)

	var nTriggers, skipped, simmed, reverted, opps, oppsNet uint64
	var gaps, nets []float64

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(runMs)*time.Millisecond)
	defer cancel()

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case t, ok := <-triggers:
			if !ok {
				break loop
			}
			nTriggers++
			// Drain backlog accumulated while the last sim ran → sample at RPC pace.
		drain:
			for {
				select {
				case _, ok := <-triggers:
					if !ok {
						break drain
					}
					nTriggers++
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
			gap := math.Abs((ray - orca) / math.Min(orca, ray) * 10_000.0)
			gaps = append(gaps, gap)
			if gap > feeBps {
				opps++
				net := gap - feeBps
				nets = append(nets, net)
				if gap > feeBps+tipCushionBps {
					oppsNet++
				}
				fmt.Printf(
					"⚡ backrunnable via %s slot %d — gap %.1fbp, net %.1fbp (post-victim Orca $%.4f / Ray $%.4f)\n",
					t.Venue, t.Slot, gap, net, orca, ray,
				)
			}
		}
	}

	fmt.Printf("\n──────── backrun-probe report (%ds) ────────\n", runMs/1000)
	fmt.Printf("pool triggers seen:        %d\n", nTriggers)
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
