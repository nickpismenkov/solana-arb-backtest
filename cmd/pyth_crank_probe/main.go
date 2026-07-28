// Simulate the FULL self-crank against the sponsored feed marginfi reads
// (step-4 gate): fetch a fresh Hermes update, build the two-tx crank
// (pyth crank), simulateBundle [setup, fire] with skipSigVerify, and confirm
// the sponsored feed's publish_time ADVANCES past the live on-chain value.
// Read-only — nothing is submitted; the payer is a funded mainnet cranker
// (sim only needs its lamports), the buffer an ephemeral keypair.
//
// Usage: HELIUS_RPC=<url> [FEED=<hex feed id>] [PAYER=<pubkey>]
//
//	go run ./cmd/pyth_crank_probe
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/liquidation"
	"solana-arb-backtest-go/internal/pyth"
)

// Canonical Pyth feed ids (hex).
const (
	sol  = "ef0d8b6fda2ceba41da15d4095d1da392a0d2f8ed0c6c7bc0f4cfac8c280b56d"
	usdc = "eaa020c61cc479712813461ce153894a96a6c00b21ed0cfc2798d1f9a9e9c94a"
	// A real, funded sponsored-feed cranker (payer for simulation only).
	defaultPayer = "4p16wya1Vw2u9w22oah4yXQgySb6eWKRRLMsEXCreish"
)

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

func hex32(s string) ([32]byte, error) {
	var out [32]byte
	if len(s) != 64 {
		return out, fmt.Errorf("expected 64 hex chars, got %d", len(s))
	}
	for i := 0; i < 32; i++ {
		var b byte
		if _, err := fmt.Sscanf(s[2*i:2*i+2], "%02x", &b); err != nil {
			return out, err
		}
		out[i] = b
	}
	return out, nil
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

func feedState(endpoint string, feed solana.PublicKey) (float64, int64, bool) {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{feed.String(), map[string]any{"encoding": "base64"}}})
	if !ok {
		return 0, 0, false
	}
	result := asMap(v["result"])
	value := asMap(result["value"])
	dataArr := asArray(value["data"])
	if len(dataArr) == 0 {
		return 0, 0, false
	}
	s, ok := dataArr[0].(string)
	if !ok {
		return 0, 0, false
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return 0, 0, false
	}
	_, usd, ts, ok := liquidation.DecodePriceUpdateV2(raw)
	return usd, ts, ok
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
	feedHex := os.Getenv("FEED")
	if feedHex == "" {
		feedHex = sol
	}
	payerStr := os.Getenv("PAYER")
	if payerStr == "" {
		payerStr = defaultPayer
	}
	payer, err := solana.PublicKeyFromBase58(payerStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "PAYER: %v\n", err)
		os.Exit(1)
	}
	feedID, err := hex32(feedHex)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FEED: %v\n", err)
		os.Exit(1)
	}

	// Where marginfi looks: shard-0 sponsored feed PDAs.
	feedAcct := pyth.SponsoredFeed(0, feedID)
	fmt.Printf("sponsored feed (shard 0): %s\n", feedAcct)
	usdcID, _ := hex32(usdc)
	fmt.Printf("  (USDC shard-0 ref: %s)\n", pyth.SponsoredFeed(0, usdcID))

	preUSD, preTS, preOK := feedState(endpoint, feedAcct)
	if preOK {
		fmt.Printf("live feed:  price=$%.4f publish_time=%d\n", preUSD, preTS)
	} else {
		fmt.Println("live feed: <not found / undecodable>")
	}

	// Fresh Hermes update for this feed.
	hermes := os.Getenv("HERMES")
	if hermes == "" {
		hermes = "https://hermes.pyth.network"
	}
	update, err := pyth.FetchHermes(hermes, []string{feedHex})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes fetch+parse: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("hermes: VAA %dB, %d update(s)\n", len(update.VAA), len(update.Updates))
	var mu *pyth.MerkleUpdate
	for i := range update.Updates {
		if id, ok := update.Updates[i].FeedID(); ok && id == feedID {
			mu = &update.Updates[i]
			break
		}
	}
	if mu == nil {
		fmt.Fprintln(os.Stderr, "feed not in blob")
		os.Exit(1)
	}

	// Two-tx crank with an ephemeral buffer (BuildCrankTxs generates it and
	// leaves placeholder zero signatures — MarshalBinary/ToBase64 pad them,
	// which is exactly what simulateBundle's skipSigVerify wants).
	txs, err := pyth.BuildCrankTxs(payer, update.VAA, []pyth.MerkleUpdate{*mu}, 0, 0, solana.Hash{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "build crank ixs: %v\n", err)
		os.Exit(1)
	}
	setupB64, fireB64, err := txs.ToB64()
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode crank txs: %v\n", err)
		os.Exit(1)
	}
	if setupRaw, err := base64.StdEncoding.DecodeString(setupB64); err == nil {
		fmt.Fprintf(os.Stderr, "[crank] setup tx: %dB (%d ixs)\n", len(setupRaw), len(pythMustIxs(txs.Setup)))
	}
	if fireRaw, err := base64.StdEncoding.DecodeString(fireB64); err == nil {
		fmt.Fprintf(os.Stderr, "[crank] fire tx: %dB (%d ixs)\n", len(fireRaw), len(pythMustIxs(txs.Fire)))
	}

	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "simulateBundle",
		"params": []any{
			map[string]any{"encodedTransactions": []string{setupB64, fireB64}},
			map[string]any{
				"skipSigVerify":                true,
				"replaceRecentBlockhash":       true,
				"preExecutionAccountsConfigs":  []any{nil, nil},
				"postExecutionAccountsConfigs": []any{nil, map[string]any{"encoding": "base64", "addresses": []string{feedAcct.String()}}},
			},
		}})
	if !ok {
		fmt.Fprintln(os.Stderr, "simulateBundle: request failed")
		os.Exit(1)
	}
	if e, ok := v["error"]; ok && e != nil {
		eb, _ := json.Marshal(e)
		fmt.Printf("⛔ simulateBundle error: %s\n", string(eb))
		os.Exit(1)
	}
	result := asMap(v["result"])
	val := asMap(result["value"])
	summaryB, _ := json.Marshal(val["summary"])
	fmt.Printf("simulateBundle summary: %s\n", string(summaryB))

	var postUSD float64
	var postTS int64
	var postOK bool
	for i, rv := range asArray(val["transactionResults"]) {
		r := asMap(rv)
		errB, _ := json.Marshal(r["err"])
		fmt.Printf("  tx[%d] err=%s cu=%v\n", i, string(errB), r["unitsConsumed"])
		if r["err"] != nil {
			for _, lv := range asArray(r["logs"]) {
				fmt.Printf("    %s\n", asStr(lv))
			}
		}
		for _, av := range asArray(r["postExecutionAccounts"]) {
			a := asMap(av)
			dataArr := asArray(a["data"])
			if len(dataArr) == 0 {
				continue
			}
			s, ok := dataArr[0].(string)
			if !ok {
				continue
			}
			b, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				continue
			}
			if _, usd, ts, ok := liquidation.DecodePriceUpdateV2(b); ok {
				postUSD, postTS, postOK = usd, ts, true
			}
		}
	}

	switch {
	case preOK && postOK:
		fmt.Printf("post-crank: price=$%.4f publish_time=%d\n", postUSD, postTS)
		adv := postTS - preTS
		if adv > 0 {
			fmt.Printf("★ CRANK VERIFIED — publish_time advanced %ds past the live feed ($%.4f@%d → $%.4f@%d)\n",
				adv, preUSD, preTS, postUSD, postTS)
		} else {
			fmt.Printf("✗ publish_time did NOT advance (%d → %d) — feed already fresher than Hermes blob?\n", preTS, postTS)
		}
	case !preOK && postOK:
		fmt.Printf("post-crank: price=$%.4f publish_time=%d (no live baseline)\n", postUSD, postTS)
	default:
		fmt.Println("✗ no post-execution feed state returned — check simulateBundle output above")
	}
}

// pythMustIxs reports the instruction count of a compiled transaction's
// message (for the "N ixs" log line); errors are treated as 0 since this is
// diagnostic-only.
func pythMustIxs(tx *solana.Transaction) []solana.CompiledInstruction {
	if tx == nil {
		return nil
	}
	return tx.Message.Instructions
}
