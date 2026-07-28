// Capture REAL Kamino (KLend) liquidation transactions and dump the exact
// instruction sequence — account lists (resolved through ALTs via jsonParsed)
// and data bytes — for refresh_reserve / refresh_obligation /
// liquidate_obligation_and_redeem_reserve_collateral. The builders in
// kamino.go are derived from THESE captured bytes, not from docs (the
// marginfi lesson: build from observed mainnet truth, verify by simulation).
//
// Usage: HELIUS_RPC=<url> [SAMPLES=3] [LIMIT=1000] go run ./cmd/kamino_liq_decode
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gagliardetto/solana-go/base58"

	"solana-arb-backtest-go/internal/envfile"
)

const klend = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"

var (
	discLiqV1 = [8]byte{177, 71, 154, 188, 226, 133, 74, 55}
	discLiqV2 = [8]byte{162, 161, 35, 143, 30, 187, 185, 103}
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

func rpc(endpoint string, body map[string]any) (map[string]any, bool) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, false
	}
	for attempt := 0; attempt < 4; attempt++ {
		resp, err := httpClient.Post(endpoint, "application/json", bytes.NewReader(b))
		if err == nil {
			var v map[string]any
			decErr := json.NewDecoder(resp.Body).Decode(&v)
			resp.Body.Close()
			if decErr == nil {
				return v, true
			}
		}
		time.Sleep(time.Duration(400<<attempt) * time.Millisecond)
	}
	return nil, false
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}
func asArray(v any) []any {
	a, _ := v.([]any)
	return a
}
func asStr(v any) string {
	s, _ := v.(string)
	return s
}

func dumpIx(label string, ix map[string]any) {
	data, _ := base58.Decode(asStr(ix["data"]))
	n := len(data)
	if n > 8 {
		n = 8
	}
	fmt.Printf("  %s: disc=%02x data_len=%d rest=%02x\n", label, data[:n], len(data), data[n:])
	for i, a := range asArray(ix["accounts"]) {
		fmt.Printf("    [%2d] %s\n", i, asStr(a))
	}
}

type labeledIx struct {
	label string
	ix    map[string]any
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
	samples := 3
	if v := os.Getenv("SAMPLES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			samples = n
		}
	}
	limit := 1000
	if v := os.Getenv("LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}

	// Page back until we have enough signatures (one page ≈ a minute of KLend
	// activity; liquidations are ~1 per 5 min).
	var sigs []string
	var before string
	for len(sigs) < limit {
		params := map[string]any{"limit": 1000}
		if before != "" {
			params["before"] = before
		}
		v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getSignaturesForAddress",
			"params": []any{klend, params}})
		if !ok {
			break
		}
		page := asArray(v["result"])
		if len(page) == 0 {
			break
		}
		last := asMap(page[len(page)-1])
		before = asStr(last["signature"])
		for _, ev := range page {
			e := asMap(ev)
			if e["err"] == nil {
				if s := asStr(e["signature"]); s != "" {
					sigs = append(sigs, s)
				}
			}
		}
		fmt.Fprintf(os.Stderr, "[decode] paged: %d signatures\n", len(sigs))
	}

	found := 0
	for _, sig := range sigs {
		if found >= samples {
			break
		}
		v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getTransaction",
			"params": []any{sig, map[string]any{"encoding": "jsonParsed", "maxSupportedTransactionVersion": 0, "commitment": "confirmed"}}})
		if !ok {
			continue
		}
		result := asMap(v["result"])
		if result == nil {
			continue
		}

		// Gather ALL KLend instructions in execution order: top-level + inner.
		var klendIxs []labeledIx
		txn := asMap(result["transaction"])
		message := asMap(txn["message"])
		for ti, ixv := range asArray(message["instructions"]) {
			ix := asMap(ixv)
			if asStr(ix["programId"]) == klend {
				klendIxs = append(klendIxs, labeledIx{fmt.Sprintf("top[%d]", ti), ix})
			}
		}
		meta := asMap(result["meta"])
		for _, innerV := range asArray(meta["innerInstructions"]) {
			inner := asMap(innerV)
			parent := int(inner["index"].(float64))
			for ii, ixv := range asArray(inner["instructions"]) {
				ix := asMap(ixv)
				if asStr(ix["programId"]) == klend {
					klendIxs = append(klendIxs, labeledIx{fmt.Sprintf("inner[%d.%d]", parent, ii), ix})
				}
			}
		}
		hasLiq := false
		for _, li := range klendIxs {
			data, _ := base58.Decode(asStr(li.ix["data"]))
			if len(data) >= 8 {
				var got [8]byte
				copy(got[:], data[:8])
				if got == discLiqV1 || got == discLiqV2 {
					hasLiq = true
					break
				}
			}
		}
		if !hasLiq {
			time.Sleep(60 * time.Millisecond)
			continue
		}

		found++
		fmt.Printf("\n════ liquidation tx #%d: %s\n", found, sig)
		accountKeys := asArray(message["accountKeys"])
		feePayer := ""
		if len(accountKeys) > 0 {
			feePayer = asStr(asMap(accountKeys[0])["pubkey"])
		}
		fmt.Printf("  fee payer: %s\n", feePayer)
		for _, li := range klendIxs {
			data, _ := base58.Decode(asStr(li.ix["data"]))
			name := "?"
			if len(data) >= 8 {
				var got [8]byte
				copy(got[:], data[:8])
				switch got {
				case discLiqV1:
					name = "LIQUIDATE_v1"
				case discLiqV2:
					name = "LIQUIDATE_v2"
				default:
					name = "other"
				}
			}
			dumpIx(fmt.Sprintf("%s %s", li.label, name), li.ix)
		}
		time.Sleep(60 * time.Millisecond)
	}
	if found == 0 {
		fmt.Printf("no liquidation found in the last %d txs — raise LIMIT\n", len(sigs))
	}
}
