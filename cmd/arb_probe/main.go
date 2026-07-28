// Verifies the shared arb builder (arb.BuildArbTx — the exact code the
// executor runs) by simulating BOTH directions against mainnet via the ALT.
// With no spread each direction reverts at leg2 (insufficient funds) — the
// profit-or-revert guard. A structural error would look different (bad meta,
// sqrt-limit, layout).
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
	"strconv"
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/arb"
	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/pools"
)

func rpc(endpoint string, body map[string]any) (map[string]any, bool) {
	for attempt := 0; attempt < 4; attempt++ {
		b, _ := json.Marshal(body)
		resp, err := http.Post(endpoint, "application/json", bytes.NewReader(b))
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

func accountData(endpoint, addr string) []byte {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{addr, map[string]string{"encoding": "base64"}}})
	if !ok {
		panic("rpc")
	}
	result := v["result"].(map[string]any)
	value := result["value"].(map[string]any)
	dataArr := value["data"].([]any)
	data, err := base64.StdEncoding.DecodeString(dataArr[0].(string))
	if err != nil {
		panic("data")
	}
	return data
}

// Instruction order in build_arb_tx: [0 cu_limit, 1 cu_price, 2 ata, 3 ata,
// 4 borrow, 5 leg1, 6 leg2(guard), 7 payback, 8 tip]. Only a leg2 revert is
// the guard doing its job; a revert anywhere else is a structural bug.
const leg2Ix = float64(6)

func classify(errField any) string {
	if errField == nil {
		return "✅ SIMULATED CLEAN — a profitable arb exists right now; tx would land"
	}
	errMap, ok := errField.(map[string]any)
	if !ok {
		errJSON, _ := json.Marshal(errField)
		return fmt.Sprintf("⚠️  inconclusive — %s", string(errJSON))
	}
	instrErr, ok := errMap["InstructionError"].([]any)
	if !ok || len(instrErr) == 0 {
		errJSON, _ := json.Marshal(errField)
		return fmt.Sprintf("⚠️  inconclusive — %s", string(errJSON))
	}
	ix, ok := instrErr[0].(float64)
	if !ok {
		errJSON, _ := json.Marshal(errField)
		return fmt.Sprintf("⚠️  inconclusive — %s", string(errJSON))
	}
	if ix == leg2Ix {
		return "✅ GUARD WORKING — borrow+leg1 executed, leg2 reverted (no spread → profit-or-revert)"
	}
	errJSON, _ := json.Marshal(errField)
	return fmt.Sprintf("❌ STRUCTURAL ERROR — reverted at instruction %d (before the guard): %s", int(ix), string(errJSON))
}

func main() {
	envfile.LoadDotEnv()
	endpoint := os.Getenv("RPC_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://api.mainnet-beta.solana.com"
	}
	altAddr := os.Getenv("ALT_ADDRESS")
	if altAddr == "" {
		fmt.Fprintln(os.Stderr, "set ALT_ADDRESS")
		os.Exit(1)
	}
	borrowUI := 500.0
	if s := os.Getenv("BORROW_USDC"); s != "" {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			borrowUI = f
		}
	}
	borrowAmount := uint64(borrowUI * 1e6)
	cfg := pools.Pair()
	signerStr := os.Getenv("SIGNER")
	if signerStr == "" {
		signerStr = "Anu6Awu4kxaEDrg1nkpcikx6tJ2xhfVci5TvDrZBsZEB"
	}
	signer := solana.MustPublicKeyFromBase58(signerStr)
	showLogs := os.Getenv("SHOW_LOGS") == "1"

	alt := arb.LoadAlt(altAddr, accountData(endpoint, altAddr))
	poolData := &arb.PoolData{
		Orca: accountData(endpoint, cfg.OrcaPool),
		Ray:  accountData(endpoint, cfg.RayPool),
	}

	fmt.Printf("arb-probe %s borrow %v USDC — verifying both directions via arb.BuildArbTx\n\n", cfg.Label, borrowUI)

	for _, orcaFirst := range []bool{true, false} {
		dir := "ray→orca (buy Ray, sell Orca)"
		if orcaFirst {
			dir = "orca→ray (buy Orca, sell Ray)"
		}
		tx, err := arb.BuildArbTx(poolData, signer, alt, borrowAmount, orcaFirst, nil, 0, 10_000, solana.Hash{}, 0)
		if err != nil {
			panic(fmt.Sprintf("build: %v", err))
		}
		tx.Signatures = []solana.Signature{{}}
		raw, err := tx.MarshalBinary()
		if err != nil {
			panic(err)
		}
		b64 := base64.StdEncoding.EncodeToString(raw)
		v, _ := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "simulateTransaction",
			"params": []any{b64, map[string]any{"encoding": "base64", "sigVerify": false, "replaceRecentBlockhash": true}}})

		if v != nil {
			if e, ok := v["error"]; ok && e != nil {
				errMap, _ := e.(map[string]any)
				msg := ""
				if errMap != nil {
					msg, _ = errMap["message"].(string)
				}
				fmt.Printf("=== %s ===\n  ⛔ not simulated: %s\n\n", dir, msg)
				continue
			}
		}
		var val map[string]any
		if v != nil {
			if result, ok := v["result"].(map[string]any); ok {
				if value, ok := result["value"].(map[string]any); ok {
					val = value
				}
			}
		}
		var logs []string
		if logsArr, ok := val["logs"].([]any); ok {
			for _, l := range logsArr {
				if s, ok := l.(string); ok {
					logs = append(logs, s)
				}
			}
		}
		errJSON, _ := json.Marshal(val["err"])
		fmt.Printf("=== %s ===\n", dir)
		fmt.Printf("  signer %s | tx %d bytes | err %s\n", signer.String(), len(raw), string(errJSON))
		fmt.Printf("  %s\n", classify(val["err"]))
		if showLogs {
			for _, l := range logs {
				fmt.Printf("    %s\n", l)
			}
		}
		fmt.Println()
	}
}
