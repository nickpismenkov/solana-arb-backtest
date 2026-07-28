// Verify the shred swap decoder against real on-chain swaps. Pulls recent
// signatures for our pools, fetches each tx, resolves ALTs, and decodes the
// swaps — so we confirm direction/amount extraction (incl. ALT-referenced
// swaps) before wiring the decoder into the shred-time pricer.
//
// Usage: RPC_ENDPOINT=https://api.mainnet-beta.solana.com go run ./cmd/decode_probe
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

func recentSigs(endpoint, pool string, limit int) []string {
	v, ok := rpcCall(endpoint, "getSignaturesForAddress", []any{pool, map[string]any{"limit": limit}})
	if !ok {
		return nil
	}
	arr, ok := v["result"].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, s := range arr {
		m, ok := s.(map[string]any)
		if !ok {
			continue
		}
		sig, ok := m["signature"].(string)
		if !ok {
			continue
		}
		out = append(out, sig)
	}
	return out
}

func fetchTx(endpoint, sig string) (*solana.Transaction, bool) {
	v, ok := rpcCall(endpoint, "getTransaction", []any{sig, map[string]any{"encoding": "base64", "maxSupportedTransactionVersion": 0}})
	if !ok {
		return nil, false
	}
	result, ok := v["result"].(map[string]any)
	if !ok || result == nil {
		return nil, false
	}
	txField, ok := result["transaction"].([]any)
	if !ok || len(txField) == 0 {
		return nil, false
	}
	b64, ok := txField[0].(string)
	if !ok {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, false
	}
	tx, err := solana.TransactionFromBytes(raw)
	if err != nil {
		return nil, false
	}
	return tx, true
}

func main() {
	endpoint := os.Getenv("RPC_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://api.mainnet-beta.solana.com"
	}
	altCache := decode.NewAltCache(endpoint)

	var nTx, nSwaps, nAlt int
	cfg := pools.Pair()
	pairs := []struct{ label, pool string }{
		{"Orca", cfg.OrcaPool},
		{"Raydium", cfg.RayPool},
	}
	for _, p := range pairs {
		fmt.Printf("\n=== %s pool %s — recent swaps ===\n", p.label, p.pool)
		poolPk := solana.MustPublicKeyFromBase58(p.pool)
		for _, sig := range recentSigs(endpoint, p.pool, 8) {
			tx, ok := fetchTx(endpoint, sig)
			if !ok {
				continue
			}
			nTx++
			usesAlt := tx.Message.IsVersioned() && len(tx.Message.AddressTableLookups) > 0
			if usesAlt {
				nAlt++
			}
			keys, ok := altCache.ResolveKeys(&tx.Message)
			if !ok {
				fmt.Printf("  %s… ALT resolve failed\n", sig[:8])
				continue
			}
			swaps := decode.DecodeSwaps(&tx.Message, keys)
			for _, s := range swaps {
				nSwaps++
				dir := "SellBase"
				if s.Dir == decode.BuyBase {
					dir = "BuyBase"
				}
				fmt.Printf("  %s… %7s %s amount=%d input=%v alt=%v\n",
					sig[:8], s.Kind, dir, s.Amount, s.AmountIsInput, usesAlt)
			}
			if len(swaps) == 0 {
				poolInKeys := false
				for _, k := range keys {
					if k.Equals(poolPk) {
						poolInKeys = true
						break
					}
				}
				var progs []string
				if tx.Message.IsVersioned() || true {
					for _, ix := range tx.Message.Instructions {
						if int(ix.ProgramIDIndex) < len(keys) {
							s := keys[ix.ProgramIDIndex].String()
							if len(s) > 8 {
								s = s[:8]
							}
							progs = append(progs, s)
						}
					}
				}
				fmt.Printf("  %s… no swap; pool_in_resolved_keys=%v top_level_programs=%v\n", sig[:8], poolInKeys, progs)
			}
		}
	}
	fmt.Printf("\ntxs fetched=%d  swaps decoded=%d  txs using ALTs=%d\n", nTx, nSwaps, nAlt)
}
