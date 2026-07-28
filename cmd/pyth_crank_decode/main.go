// Dump a REAL sponsored-feed crank tx from mainnet so the crank builder is
// derived from observed truth (the marginfi/Kamino lesson): scan recent Pyth
// receiver txs for one that goes through the PUSH WRAPPER (program id starts
// "pythWSns" — the only writer of the shared sponsored feeds marginfi reads),
// then print the FULL instruction sequence — Wormhole encoded-VAA
// init/write/verify, the wrapper update — with full program ids, account
// lists (signer/writable flags), discriminators, and data hex. Also decodes
// the target PriceUpdateV2 feed (feed id, write_authority, publish_time).
//
// Usage: HELIUS_RPC=<url> [SAMPLES=2] [LIMIT=300] go run ./cmd/pyth_crank_decode
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go/base58"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/liquidation"
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

func b64acc(v map[string]any) ([]byte, bool) {
	result := asMap(v["result"])
	value := asMap(result["value"])
	dataArr := asArray(value["data"])
	if len(dataArr) == 0 {
		return nil, false
	}
	s, ok := dataArr[0].(string)
	if !ok {
		return nil, false
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	return b, true
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

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
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
	samples := 2
	if v := os.Getenv("SAMPLES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			samples = n
		}
	}
	limit := 300
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
	fmt.Fprintf(os.Stderr, "[crank] %d receiver signatures\n", len(sigs))

	found := 0
	feedsSeen := map[string]int{}
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

		// Sponsored-feed cranks go through the push wrapper.
		wrapper := ""
		for _, li := range all {
			prog := asStr(li.ix["programId"])
			if strings.HasPrefix(prog, "pythWSns") {
				wrapper = prog
				break
			}
		}
		if wrapper == "" {
			time.Sleep(30 * time.Millisecond)
			continue
		}

		found++
		fmt.Printf("\n════ sponsored crank #%d: %s\n", found, sig)
		fmt.Printf("  push wrapper program: %s\n", wrapper)
		fmt.Println("  ── accountKeys (s=signer w=writable) ──")
		for i, kv := range asArray(message["accountKeys"]) {
			k := asMap(kv)
			s := "-"
			if asBool(k["signer"]) {
				s = "s"
			}
			w := "-"
			if asBool(k["writable"]) {
				w = "w"
			}
			fmt.Printf("    [%2d] %s %s%s\n", i, asStr(k["pubkey"]), s, w)
		}
		fmt.Println("  ── instruction sequence ──")
		for _, li := range all {
			prog := asStr(li.ix["programId"])
			data, _ := base58.Decode(asStr(li.ix["data"]))
			n := len(data)
			if n > 8 {
				n = 8
			}
			disc := hexs(data[:n])
			fmt.Printf("  %s: prog=%s\n", li.label, prog)
			fmt.Printf("      disc=%s data_len=%d\n", disc, len(data))
			// Full data hex for everything except huge VAA-write chunks (cap 96B shown).
			if len(data) <= 96 {
				fmt.Printf("      data=%s\n", hexs(data))
			} else {
				fmt.Printf("      data[..96]=%s…\n", hexs(data[:96]))
			}
			for i, a := range asArray(li.ix["accounts"]) {
				fmt.Printf("      [%2d] %s\n", i, asStr(a))
			}
		}
		// Target feed = writable non-signer account of the wrapper's ix that
		// decodes as PriceUpdateV2. Just decode every wrapper-ix account.
		for _, li := range all {
			if asStr(li.ix["programId"]) != wrapper {
				continue
			}
			for _, av := range asArray(li.ix["accounts"]) {
				pk, ok := av.(string)
				if !ok {
					continue
				}
				info, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
					"params": []any{pk, map[string]any{"encoding": "base64"}}})
				if !ok {
					continue
				}
				b, ok := b64acc(info)
				if !ok {
					continue
				}
				if fid, usd, ts, ok := liquidation.DecodePriceUpdateV2(b); ok {
					wa := ""
					if len(b) >= 40 {
						wa = hexs(b[8:40])
					}
					selfBytes, _ := base58.Decode(pk)
					selfHex := hexs(selfBytes)
					fmt.Printf("  ── target feed %s\n", pk)
					fmt.Printf("      feed_id=%s price=$%.4f publish_time=%d\n", hexs(fid[:]), usd, ts)
					fmt.Printf("      write_authority==self: %v\n", wa == selfHex)
					feedsSeen[pk]++
				}
			}
		}
		time.Sleep(40 * time.Millisecond)
	}
	fmt.Println("\n──── sponsored feeds seen ────")
	for f, n := range feedsSeen {
		fmt.Printf("  %s  ×%d\n", f, n)
	}
	if found == 0 {
		fmt.Printf("no push-wrapper crank in %d sigs — raise LIMIT\n", len(sigs))
	}
}
