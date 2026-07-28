// Decode a real Pyth receiver `post_update` / `post_update_atomic` instruction
// from mainnet to pin the exact discriminator, account layout, and data shape
// before building our own crank ix. Receiver traffic is constant, so a small
// scan finds examples fast. Also reports which target price-feed account each
// writes (to confirm we can crank marginfi's sponsored feed, e.g. Dpw1EAVr…).
//
// Usage: HELIUS_RPC=<url> [SAMPLES=4] [LIMIT=400] go run ./cmd/pyth_recv_decode
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/gagliardetto/solana-go/base58"

	"solana-arb-backtest-go/internal/envfile"
)

const pythReceiver = "rec5EKMGg6MxZYaMdyBfgwp4d5rB9T1VQH5pJv5LtFJ"

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

func hexs(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, x := range b {
		out[i*2] = hexdigits[x>>4]
		out[i*2+1] = hexdigits[x&0xf]
	}
	return string(out)
}

func asArray(v any) []any {
	a, _ := v.([]any)
	return a
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asStr(v any) string {
	s, _ := v.(string)
	return s
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
	samples := 4
	if v := os.Getenv("SAMPLES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			samples = n
		}
	}
	limit := 400
	if v := os.Getenv("LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}

	sigsResp, _ := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getSignaturesForAddress",
		"params": []any{pythReceiver, map[string]any{"limit": limit}}})
	var sigs []string
	if sigsResp != nil {
		if arr, ok := sigsResp["result"].([]any); ok {
			for _, ev := range arr {
				em := asMap(ev)
				if em["err"] == nil {
					if s := asStr(em["signature"]); s != "" {
						sigs = append(sigs, s)
					}
				}
			}
		}
	}
	fmt.Fprintf(os.Stderr, "[recv] %d receiver signatures\n", len(sigs))

	seen := map[string]int{}
	dumped := 0
	for _, sig := range sigs {
		if dumped >= samples {
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
		var ixs []any
		message := asMap(asMap(result["transaction"])["message"])
		ixs = append(ixs, asArray(message["instructions"])...)
		meta := asMap(result["meta"])
		for _, inner := range asArray(meta["innerInstructions"]) {
			ixs = append(ixs, asArray(asMap(inner)["instructions"])...)
		}
		for _, ixv := range ixs {
			ix := asMap(ixv)
			if asStr(ix["programId"]) != pythReceiver {
				continue
			}
			data, _ := base58.Decode(asStr(ix["data"]))
			if len(data) < 8 {
				continue
			}
			disc := hexs(data[:8])
			seen[disc]++
			if seen[disc] == 1 && dumped < samples {
				dumped++
				fmt.Printf("\n════ receiver ix disc=%s  data_len=%d  sig=%s\n", disc, len(data), sig)
				for i, a := range asArray(ix["accounts"]) {
					fmt.Printf("    [%2d] %s\n", i, asStr(a))
				}
			}
		}
		time.Sleep(40 * time.Millisecond)
	}
	fmt.Println("\n──── disc histogram ────")
	type kv struct {
		d string
		n int
	}
	var kvs []kv
	for d, n := range seen {
		kvs = append(kvs, kv{d, n})
	}
	sort.Slice(kvs, func(i, j int) bool { return kvs[i].n > kvs[j].n })
	for _, e := range kvs {
		fmt.Printf("  %s  ×%d\n", e.d, e.n)
	}
}
