// Capture a REAL marginfi liquidation that embeds a Pyth price update, and
// dump the Pyth-receiver instruction(s) — program, discriminator, accounts,
// data length — so the embedded-update builder is derived from observed
// mainnet truth (the marginfi/Kamino lesson). Top liquidators post the fresh
// Pyth price IN THEIR OWN TX (post_update / post_update_atomic) right before
// the liquidate, so they don't wait for anyone's crank. This finds an example
// to copy.
//
// Usage: HELIUS_RPC=<url> [SAMPLES=3] [LIMIT=8000] go run ./cmd/mfi_pyth_decode
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

const (
	marginfi     = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA"
	pythReceiver = "rec5EKMGg6MxZYaMdyBfgwp4d5rB9T1VQH5pJv5LtFJ"
)

// LendingAccountLiquidate disc.
var discLiq = [8]byte{214, 169, 151, 213, 251, 167, 86, 219}

var httpClient = &http.Client{Timeout: 15 * time.Second}

func rpc(endpoint string, body map[string]any) (map[string]any, bool) {
	for attempt := 0; attempt < 4; attempt++ {
		b, _ := json.Marshal(body)
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
	fmt.Printf("  %s: prog=%s disc=%02x data_len=%d\n", label, asStr(ix["programId"]), data[:n], len(data))
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
	limit := 8000
	if v := os.Getenv("LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}

	// Page marginfi signatures back until we have `limit`.
	var sigs []string
	var before string
	for len(sigs) < limit {
		params := map[string]any{"limit": 1000}
		if before != "" {
			params["before"] = before
		}
		v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getSignaturesForAddress",
			"params": []any{marginfi, params}})
		if !ok {
			break
		}
		page := asArray(v["result"])
		if len(page) == 0 {
			break
		}
		before = asStr(asMap(page[len(page)-1])["signature"])
		for _, ev := range page {
			e := asMap(ev)
			if e["err"] == nil {
				if s := asStr(e["signature"]); s != "" {
					sigs = append(sigs, s)
				}
			}
		}
		fmt.Fprintf(os.Stderr, "[pyth] paged: %d sigs\n", len(sigs))
	}

	found := 0
	for _, sig := range sigs {
		if found >= samples {
			break
		}
		tx, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getTransaction",
			"params": []any{sig, map[string]any{"encoding": "jsonParsed", "maxSupportedTransactionVersion": 0, "commitment": "confirmed"}}})
		if !ok {
			continue
		}
		result := asMap(tx["result"])
		if result == nil {
			continue
		}

		var all []labeledIx
		message := asMap(asMap(result["transaction"])["message"])
		for ti, ixv := range asArray(message["instructions"]) {
			all = append(all, labeledIx{fmt.Sprintf("top[%d]", ti), asMap(ixv)})
		}
		meta := asMap(result["meta"])
		for _, innerV := range asArray(meta["innerInstructions"]) {
			inner := asMap(innerV)
			p := int64(0)
			if f, ok := inner["index"].(float64); ok {
				p = int64(f)
			}
			for ii, ixv := range asArray(inner["instructions"]) {
				all = append(all, labeledIx{fmt.Sprintf("inner[%d.%d]", p, ii), asMap(ixv)})
			}
		}

		hasLiq := false
		for _, li := range all {
			if asStr(li.ix["programId"]) != marginfi {
				continue
			}
			data, err := base58.Decode(asStr(li.ix["data"]))
			if err == nil && len(data) >= 8 && [8]byte(data[:8]) == discLiq {
				hasLiq = true
				break
			}
		}
		hasPyth := false
		for _, li := range all {
			if asStr(li.ix["programId"]) == pythReceiver {
				hasPyth = true
				break
			}
		}
		if !(hasLiq && hasPyth) {
			time.Sleep(50 * time.Millisecond)
			continue
		}

		found++
		fmt.Printf("\n════ marginfi liq + Pyth update #%d: %s\n", found, sig)
		fmt.Printf("  fee payer: %s\n", asStr(asMap(asArray(message["accountKeys"])[0])["pubkey"]))
		for _, li := range all {
			prog := asStr(li.ix["programId"])
			if prog == pythReceiver {
				dumpIx(li.label+" PYTH", li.ix)
			} else if prog == marginfi {
				data, err := base58.Decode(asStr(li.ix["data"]))
				if err == nil && len(data) >= 8 && [8]byte(data[:8]) == discLiq {
					fmt.Printf("  %s MARGINFI_LIQUIDATE\n", li.label)
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if found == 0 {
		fmt.Printf("no marginfi-liq-with-embedded-Pyth-update found in %d sigs — raise LIMIT\n", len(sigs))
	}
}
