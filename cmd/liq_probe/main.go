// Liquidation opportunity probe (read-only). For each lending/perps
// protocol, scan recent program transactions, identify LIQUIDATIONS (by
// instruction discriminator at top-level or via CPI, or by a log marker),
// and measure: frequency (→ liquidations/day), liquidator concentration
// (are a few bots winning everything?), and a rough profit proxy (fee-payer
// USDC delta). Tells us which protocols are worth building an adapter for,
// before we build one.
//
// Discriminators/program IDs are VERIFIED per protocol before trust
// (marginfi computed; others pending research). Read-only, no money.
//
// Usage: RPC_ENDPOINT=<helius> [LIMIT=1000] [SLEEP_MS=25] go run ./cmd/liq_probe
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go/base58"

	"solana-arb-backtest-go/internal/envfile"
)

const usdcMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

type protocol struct {
	name       string
	program    string
	discs      [][8]byte // Anchor liquidation ix discriminators
	tags       []byte    // non-Anchor: match instruction data[0]
	logMarkers []string  // fallback: log substrings
}

// Verified program IDs + discriminators (research + local sha256). Profit
// note: Kamino/Solend expose liquidator gain in token balances; marginfi/Drift
// keep it as internal share/margin deltas → our USDC-delta proxy under-reads
// those (the reliable signals there are frequency + liquidator concentration).
func protocols() []protocol {
	return []protocol{
		{
			name:       "kamino-klend",
			program:    "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD",
			discs:      [][8]byte{{177, 71, 154, 188, 226, 133, 74, 55}, {162, 161, 35, 143, 30, 187, 185, 103}},
			logMarkers: []string{"LiquidateObligationAndRedeemReserveCollateral"},
		},
		{
			name:       "marginfi-v2",
			program:    "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA",
			discs:      [][8]byte{{214, 169, 151, 213, 251, 167, 86, 219}},
			logMarkers: []string{"LendingAccountLiquidate"},
		},
		{
			name:    "drift-v2",
			program: "dRiftyHA39MWEi3m9aunc5MzRF1JYuBsbn6VPcn33UH",
			discs: [][8]byte{
				{75, 35, 119, 247, 191, 18, 139, 2},   // liquidate_perp
				{107, 0, 128, 41, 35, 229, 251, 18},   // liquidate_spot
				{95, 111, 124, 105, 86, 169, 187, 34}, // liquidate_perp_with_fill
			},
		},
		{
			name:       "save-solend",
			program:    "So1endDq2YkqhipRh3WViPa8hdiSpxWy6z3Z6tMCpAo",
			tags:       []byte{12, 17}, // LiquidateObligation / …AndRedeemReserveCollateral
			logMarkers: []string{"LiquidateObligation"},
		},
	}
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
		time.Sleep(time.Duration(300<<attempt) * time.Millisecond)
	}
	return nil, false
}

func recentSigs(endpoint, program string, limit uint32) []string {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getSignaturesForAddress",
		"params": []any{program, map[string]any{"limit": limit}}})
	if !ok {
		return nil
	}
	result, _ := v["result"].([]any)
	var out []string
	for _, e := range result {
		m, ok := e.(map[string]any)
		if !ok || m["err"] != nil {
			continue
		}
		if sig, ok := m["signature"].(string); ok {
			out = append(out, sig)
		}
	}
	return out
}

func disc8(b58 string) ([8]byte, bool) {
	var d [8]byte
	bs, err := base58.Decode(b58)
	if err != nil || len(bs) < 8 {
		return d, false
	}
	copy(d[:], bs[:8])
	return d, true
}

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0.0
	}
	idx := int(float64(len(sorted)-1) * p)
	// Round to nearest, matching Rust's `.round()`.
	f := float64(len(sorted)-1) * p
	if f-float64(idx) >= 0.5 {
		idx++
	}
	return sorted[idx]
}

func main() {
	envfile.LoadDotEnv()

	endpoint := os.Getenv("RPC_ENDPOINT")
	if endpoint == "" {
		fmt.Fprintln(os.Stderr, "RPC_ENDPOINT (use Helius)")
		os.Exit(1)
	}
	limit := uint32(1000)
	if v := os.Getenv("LIMIT"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil {
			limit = uint32(n)
		}
	}
	sleepMs := int64(25)
	if v := os.Getenv("SLEEP_MS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			sleepMs = n
		}
	}

	for _, p := range protocols() {
		short := p.program
		if len(short) > 8 {
			short = short[:8]
		}
		fmt.Fprintf(os.Stderr, "\n═══ %s (%s) — scanning %d recent program txns ═══\n", p.name, short, limit)
		sigs := recentSigs(endpoint, p.program, limit)
		if len(sigs) == 0 {
			fmt.Println("  no signatures (program not found / RPC issue)")
			continue
		}

		var liqs, minT, maxT uint64
		minT = ^uint64(0)
		byLiquidator := map[string]uint64{}
		var profits []float64
		var scanned uint64

		checkIx := func(ix map[string]any) bool {
			pid, _ := ix["programId"].(string)
			if pid != p.program {
				return false
			}
			data, _ := ix["data"].(string)
			if data == "" {
				return false
			}
			if d, ok := disc8(data); ok {
				for _, want := range p.discs {
					if d == want {
						return true
					}
				}
			}
			if bs, err := base58.Decode(data); err == nil && len(bs) > 0 {
				b0 := bs[0]
				for _, t := range p.tags {
					if t == b0 {
						return true
					}
				}
			}
			return false
		}

		for _, sig := range sigs {
			time.Sleep(time.Duration(sleepMs) * time.Millisecond)
			v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getTransaction",
				"params": []any{sig, map[string]any{"encoding": "jsonParsed", "maxSupportedTransactionVersion": 0, "commitment": "confirmed"}}})
			if !ok {
				continue
			}
			r, _ := v["result"].(map[string]any)
			if r == nil {
				continue
			}
			meta, _ := r["meta"].(map[string]any)
			if meta == nil || meta["err"] != nil {
				continue
			}
			scanned++

			// Is this a liquidation? Check top-level + inner instructions for our
			// program + a liquidation discriminator, or a log marker.
			isLiq := false
			var topIxs []any
			if tx, ok := r["transaction"].(map[string]any); ok {
				if msg, ok := tx["message"].(map[string]any); ok {
					topIxs, _ = msg["instructions"].([]any)
				}
			}
			for _, ixAny := range topIxs {
				if ix, ok := ixAny.(map[string]any); ok && checkIx(ix) {
					isLiq = true
					break
				}
			}
			if !isLiq {
				if inner, ok := meta["innerInstructions"].([]any); ok {
				outer:
					for _, grpAny := range inner {
						grp, ok := grpAny.(map[string]any)
						if !ok {
							continue
						}
						instrs, _ := grp["instructions"].([]any)
						for _, ixAny := range instrs {
							if ix, ok := ixAny.(map[string]any); ok && checkIx(ix) {
								isLiq = true
								break outer
							}
						}
					}
				}
			}
			if !isLiq && len(p.logMarkers) > 0 {
				logsArr, _ := meta["logMessages"].([]any)
				var logs string
				for i, l := range logsArr {
					if s, ok := l.(string); ok {
						if i > 0 {
							logs += "\n"
						}
						logs += s
					}
				}
				for _, m := range p.logMarkers {
					if strings.Contains(logs, m) {
						isLiq = true
						break
					}
				}
			}
			if !isLiq {
				continue
			}

			liqs++
			if btf, ok := r["blockTime"].(float64); ok {
				bt := uint64(btf)
				if bt < minT {
					minT = bt
				}
				if bt > maxT {
					maxT = bt
				}
			}
			payer := ""
			if tx, ok := r["transaction"].(map[string]any); ok {
				if msg, ok := tx["message"].(map[string]any); ok {
					if keys, ok := msg["accountKeys"].([]any); ok && len(keys) > 0 {
						if k0, ok := keys[0].(map[string]any); ok {
							payer, _ = k0["pubkey"].(string)
						}
					}
				}
			}
			byLiquidator[payer]++

			// Rough profit proxy: fee-payer USDC delta.
			sumBalances := func(key string) float64 {
				arr, _ := meta[key].([]any)
				var total float64
				for _, bAny := range arr {
					b, ok := bAny.(map[string]any)
					if !ok {
						continue
					}
					mint, _ := b["mint"].(string)
					owner, _ := b["owner"].(string)
					if mint != usdcMint || owner != payer {
						continue
					}
					if ui, ok := b["uiTokenAmount"].(map[string]any); ok {
						if amt, ok := ui["uiAmount"].(float64); ok {
							total += amt
						}
					}
				}
				return total
			}
			profits = append(profits, sumBalances("postTokenBalances")-sumBalances("preTokenBalances"))
		}

		spanH := 0.0
		if maxT > minT {
			spanH = float64(maxT-minT) / 3600.0
		}
		perDay := 0.0
		if spanH > 0.0 {
			perDay = float64(liqs) / spanH * 24.0
		}
		fmt.Printf("  scanned %d txns → %d liquidations over %.1fh  (~%.0f/day)\n", scanned, liqs, spanH, perDay)
		if liqs == 0 {
			fmt.Println("  → none found in window (disc/marker may need fixing, or genuinely rare here)")
			continue
		}
		// Liquidator concentration.
		type who struct {
			addr string
			n    uint64
		}
		top := make([]who, 0, len(byLiquidator))
		var total uint64
		for addr, n := range byLiquidator {
			top = append(top, who{addr, n})
			total += n
		}
		sort.Slice(top, func(i, j int) bool { return top[i].n > top[j].n })
		fmt.Printf("  distinct liquidators: %d  | top 3 share:\n", len(byLiquidator))
		for i, w := range top {
			if i >= 3 {
				break
			}
			short := w.addr
			if len(short) > 8 {
				short = short[:8]
			}
			fmt.Printf("    %s… %d (%.0f%%)\n", short, w.n, 100.0*float64(w.n)/float64(total))
		}
		var positive []float64
		for _, p := range profits {
			if p > 0.0 {
				positive = append(positive, p)
			}
		}
		sort.Float64s(positive)
		if len(positive) > 0 {
			fmt.Printf("  liquidator USDC gain (rough, n=%d): med $%.2f  p90 $%.2f  max $%.2f\n",
				len(positive), pct(positive, 0.5), pct(positive, 0.9), pct(positive, 1.0))
		}
	}
	fmt.Println()
}
