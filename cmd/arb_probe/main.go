// Command arb_probe verifies the shared arb builder (arb.BuildArbTx — the
// exact code the executor runs) by simulating BOTH directions against
// mainnet via the ALT. With no spread each direction reverts at leg2
// (insufficient funds) — the profit-or-revert guard. A structural error
// would look different (bad meta, sqrt-limit, layout).
//
// Usage: RPC_ENDPOINT=<url> ALT_ADDRESS=<alt> [BORROW_USDC=500] [SIGNER=<pubkey>] \
//
//	[SHOW_LOGS=1] go run ./cmd/arb_probe
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"arbengine/internal/arb"
	"arbengine/internal/config"
	"arbengine/internal/pools"
	"arbengine/internal/solana"
)

// sharedClient mirrors ureq's default behavior closely enough for this
// probe's retry loop (rpc's own 4-attempt/backoff logic lives here instead
// of rpcclient.Client, which doesn't retry).
var sharedClient = &http.Client{Timeout: 15 * time.Second}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// leg2Ix is the instruction order in BuildArbTx: [0 cu_limit, 1 cu_price,
// 2 ata, 3 ata, 4 borrow, 5 leg1, 6 leg2(guard), 7 payback, 8 tip]. Only a
// leg2 revert is the guard doing its job; a revert anywhere else is a
// structural bug.
const leg2Ix = 6

func rpcCall(endpoint string, body map[string]any) (map[string]any, bool) {
	for attempt := 0; attempt < 4; attempt++ {
		b, _ := json.Marshal(body)
		resp, err := sharedClient.Post(endpoint, "application/json", bytesReader(b))
		if err == nil {
			defer resp.Body.Close()
			var v map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&v); err == nil {
				return v, true
			}
		}
		time.Sleep(time.Duration(400<<attempt) * time.Millisecond)
	}
	return nil, false
}

func accountData(endpoint, addr string) []byte {
	v, ok := rpcCall(endpoint, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{addr, map[string]string{"encoding": "base64"}},
	})
	if !ok {
		fmt.Fprintln(os.Stderr, "rpc")
		os.Exit(1)
	}
	result, _ := v["result"].(map[string]any)
	value, _ := result["value"].(map[string]any)
	dataArr, _ := value["data"].([]any)
	if len(dataArr) == 0 {
		fmt.Fprintln(os.Stderr, "data")
		os.Exit(1)
	}
	s, _ := dataArr[0].(string)
	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return data
}

func classify(errVal any) string {
	if errVal == nil {
		return "✅ SIMULATED CLEAN — a profitable arb exists right now; tx would land"
	}
	errMap, ok := errVal.(map[string]any)
	if !ok {
		return fmt.Sprintf("⚠️  inconclusive — %v", errVal)
	}
	instrErr, ok := errMap["InstructionError"].([]any)
	if !ok || len(instrErr) == 0 {
		return fmt.Sprintf("⚠️  inconclusive — %v", errVal)
	}
	ixNum, ok := instrErr[0].(float64)
	if !ok {
		return fmt.Sprintf("⚠️  inconclusive — %v", errVal)
	}
	ix := uint64(ixNum)
	if ix == leg2Ix {
		return "✅ GUARD WORKING — borrow+leg1 executed, leg2 reverted (no spread → profit-or-revert)"
	}
	return fmt.Sprintf("❌ STRUCTURAL ERROR — reverted at instruction %d (before the guard): %v", ix, errVal)
}

func main() {
	config.LoadDotenv()

	endpoint := config.EnvOr("RPC_ENDPOINT", "https://api.mainnet-beta.solana.com")
	altAddr, ok := config.EnvOptional("ALT_ADDRESS")
	if !ok {
		fmt.Fprintln(os.Stderr, "set ALT_ADDRESS")
		os.Exit(1)
	}
	borrowUI := config.EnvFloat("BORROW_USDC", 500.0)
	borrowAmount := uint64(borrowUI * 1e6)
	cfg := pools.Pair()
	signerStr := config.EnvOr("SIGNER", "Anu6Awu4kxaEDrg1nkpcikx6tJ2xhfVci5TvDrZBsZEB")
	signer, err := solana.PubkeyFromBase58(signerStr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad SIGNER pubkey")
		os.Exit(1)
	}
	showLogs := config.EnvOr("SHOW_LOGS", "") == "1"

	alt := arb.LoadALT(altAddr, accountData(endpoint, altAddr))
	poolData := arb.PoolData{
		Orca: accountData(endpoint, cfg.OrcaPool),
		Ray:  accountData(endpoint, cfg.RayPool),
	}

	fmt.Printf("arb-probe %s borrow %g USDC — verifying both directions via arb.BuildArbTx\n\n", cfg.Label, borrowUI)

	for _, orcaFirst := range []bool{true, false} {
		dir := "ray→orca (buy Ray, sell Orca)"
		if orcaFirst {
			dir = "orca→ray (buy Orca, sell Ray)"
		}
		tx, err := arb.BuildArbTx(poolData, signer, alt, borrowAmount, orcaFirst, nil, 0, 10_000, solana.Hash{}, 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		raw, err := tx.MarshalBinary()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		b64 := base64.StdEncoding.EncodeToString(raw)
		v, ok := rpcCall(endpoint, map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "simulateTransaction",
			"params": []any{b64, map[string]any{"encoding": "base64", "sigVerify": false, "replaceRecentBlockhash": true}},
		})
		if ok {
			if e, has := v["error"]; has && e != nil {
				emap, _ := e.(map[string]any)
				msg, _ := emap["message"].(string)
				fmt.Printf("=== %s ===\n  ⛔ not simulated: %s\n\n", dir, msg)
				continue
			}
		}
		var val map[string]any
		if ok {
			result, _ := v["result"].(map[string]any)
			val, _ = result["value"].(map[string]any)
		}
		var logs []string
		if logsArr, has := val["logs"].([]any); has {
			for _, l := range logsArr {
				if s, ok := l.(string); ok {
					logs = append(logs, s)
				}
			}
		}
		fmt.Printf("=== %s ===\n", dir)
		fmt.Printf("  signer %s | tx %d bytes | err %v\n", signer, len(raw), val["err"])
		fmt.Printf("  %s\n", classify(val["err"]))
		if showLogs {
			for _, l := range logs {
				fmt.Printf("    %s\n", l)
			}
		}
		fmt.Println()
	}
}
