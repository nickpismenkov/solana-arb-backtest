// Command decode_probe verifies the shred swap decoder against real
// on-chain swaps. Pulls recent signatures for our pools, fetches each tx,
// resolves ALTs, and decodes the swaps — so we confirm direction/amount
// extraction (incl. ALT-referenced swaps) before wiring the decoder into
// the shred-time pricer.
//
// Usage: RPC_ENDPOINT=https://api.mainnet-beta.solana.com go run ./cmd/decode_probe
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"arbengine/internal/config"
	"arbengine/internal/decode"
	"arbengine/internal/pools"
	"arbengine/internal/rpcclient"
	"arbengine/internal/solana"
)

func recentSigs(rpc *rpcclient.Client, pool string, limit int) []string {
	raw, err := rpc.Call("getSignaturesForAddress", []any{pool, map[string]any{"limit": limit}})
	if err != nil {
		return nil
	}
	var entries []struct {
		Signature string `json:"signature"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	sigs := make([]string, 0, len(entries))
	for _, e := range entries {
		sigs = append(sigs, e.Signature)
	}
	return sigs
}

func fetchTx(rpc *rpcclient.Client, sig string) (solana.VersionedTransaction, bool) {
	raw, err := rpc.Call("getTransaction", []any{sig, map[string]any{
		"encoding":                       "base64",
		"maxSupportedTransactionVersion": 0,
	}})
	if err != nil || raw == nil {
		return solana.VersionedTransaction{}, false
	}
	var withResult struct {
		Transaction []string `json:"transaction"`
	}
	if err := json.Unmarshal(raw, &withResult); err != nil || len(withResult.Transaction) == 0 {
		return solana.VersionedTransaction{}, false
	}
	bytes, err := base64.StdEncoding.DecodeString(withResult.Transaction[0])
	if err != nil {
		return solana.VersionedTransaction{}, false
	}
	txn, err := solana.UnmarshalVersionedTransaction(bytes)
	if err != nil {
		return solana.VersionedTransaction{}, false
	}
	return txn, true
}

func main() {
	config.LoadDotenv()

	endpoint := config.EnvOr("RPC_ENDPOINT", "https://api.mainnet-beta.solana.com")
	rpc := rpcclient.New(endpoint)
	alt := decode.NewAltCache(endpoint)

	var nTx, nSwaps, nAlt uint32
	cfg := pools.Pair()
	type labeled struct{ label, pool string }
	for _, lp := range []labeled{{"Orca", cfg.OrcaPool}, {"Raydium", cfg.RayPool}} {
		fmt.Printf("\n=== %s pool %s — recent swaps ===\n", lp.label, lp.pool)
		for _, sig := range recentSigs(rpc, lp.pool, 8) {
			txn, ok := fetchTx(rpc, sig)
			if !ok {
				continue
			}
			nTx++
			usesAlt := txn.Message.IsV0 && len(txn.Message.V0.AddressTableLookups) > 0
			if usesAlt {
				nAlt++
			}
			keys, ok := alt.ResolveKeys(txn.Message)
			if !ok {
				fmt.Printf("  %s… ALT resolve failed\n", sig[:8])
				continue
			}
			swaps := decode.DecodeSwaps(txn, keys)
			for _, s := range swaps {
				nSwaps++
				fmt.Printf("  %s… %7s %v amount=%d input=%v alt=%v\n",
					sig[:8], s.Kind, s.Dir, s.Amount, s.AmountIsInput, usesAlt)
			}
			if len(swaps) == 0 {
				// Diagnose: is the pool present in resolved keys (ALT ok) and
				// what top-level programs are calling it (CPI/router)?
				poolPk := solana.MustPubkeyFromBase58(lp.pool)
				poolInKeys := containsPubkey(keys, poolPk)
				var progs []string
				if txn.Message.IsV0 {
					for _, ix := range txn.Message.V0.Instructions {
						if int(ix.ProgramIDIndex) < len(keys) {
							s := keys[ix.ProgramIDIndex].String()
							if len(s) > 8 {
								s = s[:8]
							}
							progs = append(progs, s)
						}
					}
				}
				fmt.Printf("  %s… no swap; pool_in_resolved_keys=%v top_level_programs=%v\n",
					sig[:8], poolInKeys, progs)
			}
		}
	}
	fmt.Printf("\ntxs fetched=%d  swaps decoded=%d  txs using ALTs=%d\n", nTx, nSwaps, nAlt)
}

func containsPubkey(keys []solana.Pubkey, want solana.Pubkey) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}
