// Verify tick-array derivation against the chain (no wallet, no money). Reads
// each pool's live state, derives the current tick-array PDA, and checks that
// account actually exists and is owned by the DEX program. If both resolve to
// real program-owned accounts, the start-index math + PDA seeds are correct —
// the foundation the swap instructions stand on.
//
// Usage: RPC_ENDPOINT=<url> go run ./cmd/tickarray_probe
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/decode"
	"solana-arb-backtest-go/internal/pools"
	"solana-arb-backtest-go/internal/ticks"
)

func rpcCall(endpoint, method string, params any) (map[string]any, bool) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	var v map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

func account(endpoint, key string) ([]byte, string, bool) {
	v, ok := rpcCall(endpoint, "getAccountInfo", []any{key, map[string]string{"encoding": "base64"}})
	if !ok {
		return nil, "", false
	}
	result, ok := v["result"].(map[string]any)
	if !ok {
		return nil, "", false
	}
	value, ok := result["value"].(map[string]any)
	if !ok || value == nil {
		return nil, "", false
	}
	dataArr, ok := value["data"].([]any)
	if !ok || len(dataArr) == 0 {
		return nil, "", false
	}
	dataStr, ok := dataArr[0].(string)
	if !ok {
		return nil, "", false
	}
	data, err := base64.StdEncoding.DecodeString(dataStr)
	if err != nil {
		return nil, "", false
	}
	owner, _ := value["owner"].(string)
	return data, owner, true
}

func check(endpoint, label, program string, state *ticks.PoolState, tickArray solana.PublicKey, start int32) {
	fmt.Printf("\n%s: tick=%d spacing=%d liquidity=%s\n", label, state.Tick, state.TickSpacing, state.Liquidity.String())
	fmt.Printf("  current tick-array start index: %d\n", start)
	fmt.Printf("  derived tick-array PDA: %s\n", tickArray.String())
	data, owner, found := account(endpoint, tickArray.String())
	if !found {
		fmt.Println("  on-chain: account not found ✗ (derivation wrong or empty array)")
		return
	}
	okOwner := owner == program
	status := "✗ wrong owner"
	if okOwner {
		status = "✓ program-owned (derivation VALID)"
	}
	fmt.Printf("  on-chain: owner=%s len=%d → %s\n", owner, len(data), status)
}

func main() {
	endpoint := os.Getenv("RPC_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://api.mainnet-beta.solana.com"
	}

	cfg := pools.Pair()
	orcaPool := solana.MustPublicKeyFromBase58(cfg.OrcaPool)
	rayPool := solana.MustPublicKeyFromBase58(cfg.RayPool)

	if data, _, ok := account(endpoint, cfg.OrcaPool); ok {
		if st, ok := ticks.DecodeOrcaState(data); ok {
			start := ticks.OrcaStartIndex(st.Tick, st.TickSpacing)
			check(endpoint, "Orca", decode.OrcaProgram, st, ticks.OrcaTickArray(orcaPool, start), start)
		}
	}
	if data, _, ok := account(endpoint, cfg.RayPool); ok {
		if st, ok := ticks.DecodeRayState(data); ok {
			start := ticks.RayStartIndex(st.Tick, st.TickSpacing)
			check(endpoint, "Raydium CLMM", decode.RayClmmProgram, st, ticks.RayTickArray(rayPool, start), start)
		}
	}
}
