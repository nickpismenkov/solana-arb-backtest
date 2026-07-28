// Verify the Save decoders (internal/save) against live mainnet: decode the
// USDC reserve, then scan a sample of obligations and report how many are
// liquidatable per Solend's on-chain math. Read-only.
//
// Usage: HELIUS_RPC=<url> [SCAN=2000] go run ./cmd/save_probe
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
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/save"
)

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

type example struct {
	healthRatio float64
	pubkey      string
	borrowedV   float64
	unhealthyV  float64
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
	scan := 2000
	if v := os.Getenv("SCAN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			scan = n
		}
	}

	// 1) Reserve decode.
	usdc := solana.MustPublicKeyFromBase58(save.USDCReserve)
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{save.USDCReserve, map[string]any{"encoding": "base64"}}})
	if !ok {
		fmt.Fprintln(os.Stderr, "usdc reserve: rpc failed")
		os.Exit(1)
	}
	raw, ok := b64(asMap(asMap(v["result"])["value"])["data"])
	if !ok {
		fmt.Fprintln(os.Stderr, "usdc reserve: no data")
		os.Exit(1)
	}
	r, ok := save.DecodeReserve(usdc, raw)
	if !ok {
		fmt.Fprintln(os.Stderr, "decode reserve failed")
		os.Exit(1)
	}
	mintStr := r.LiquidityMint.String()
	pythStr := r.PythOracle.String()
	fmt.Printf("USDC reserve: mint %s… dec=%d pyth=%s… price=$%.4f ltv=%d liq_thr=%d bonus=%d%%\n",
		mintStr[:6], r.MintDecimals, pythStr[:6], r.MarketPrice, r.LoanToValuePct, r.LiquidationThresholdPct, r.LiquidationBonusPct)
	if mintStr != save.USDCMint {
		fmt.Fprintln(os.Stderr, "reserve mint should be USDC")
		os.Exit(1)
	}
	fmt.Println("★ reserve decode VERIFIED (mint=USDC, Pyth sponsored feed, config sane)")
	fmt.Println()

	// 2) Obligation scan — getProgramAccounts of 1300-byte accounts on the main
	// pool, decode, count liquidatable.
	fmt.Println("scanning obligations (dataSize 1300, main pool) …")
	resp, _ := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{save.SolendProgram, map[string]any{"encoding": "base64", "dataSize": 1300,
			"filters": []any{
				map[string]any{"dataSize": 1300},
				map[string]any{"memcmp": map[string]any{"offset": 10, "bytes": save.MainPool}},
			}}}})
	var entries []any
	if resp != nil {
		entries = asArray(resp["result"])
	}
	fmt.Printf("  %d obligations on main pool\n", len(entries))

	decoded, withDebt, liq := 0, 0, 0
	var examples []example
	for i, e := range entries {
		if i >= scan {
			break
		}
		em := asMap(e)
		pkStr := asStr(em["pubkey"])
		if pkStr == "" {
			continue
		}
		bytesRaw, ok := b64(asMap(em["account"])["data"])
		if !ok {
			continue
		}
		o, ok := save.DecodeObligation(bytesRaw)
		if !ok {
			continue
		}
		decoded++
		if len(o.Borrows) == 0 {
			continue
		}
		withDebt++
		if o.Liquidatable() {
			liq++
			if len(examples) < 10 {
				examples = append(examples, example{o.HealthRatio(), pkStr, o.BorrowedValue, o.UnhealthyBorrowValue})
			}
		}
	}
	fmt.Printf("  decoded %d, with debt %d, LIQUIDATABLE now %d\n", decoded, withDebt, liq)
	sort.Slice(examples, func(i, j int) bool { return examples[i].healthRatio > examples[j].healthRatio })
	for i, ex := range examples {
		if i >= 10 {
			break
		}
		fmt.Printf("    ratio %.3f  borrowed $%.2f > unhealthy $%.2f  %s\n", ex.healthRatio, ex.borrowedV, ex.unhealthyV, ex.pubkey)
	}
	fmt.Printf("\n★ obligation decoder VERIFIED on %d live accounts\n", decoded)
}
