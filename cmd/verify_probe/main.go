// Temporary local verification probe (not for prod):
//  1. send_bundle with a garbage tx → expect a 400 whose Jito response body is captured
//  2. getLatestBlockhash twice, 3s apart → expect different hashes
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"solana-arb-backtest-go/internal/jito"
)

func latestBlockhash(endpoint string) (string, bool) {
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getLatestBlockhash", "params": []any{map[string]any{"commitment": "confirmed"}}}
	b, _ := json.Marshal(body)
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(b))
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	var v map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", false
	}
	result, _ := v["result"].(map[string]any)
	value, _ := result["value"].(map[string]any)
	hash, ok := value["blockhash"].(string)
	return hash, ok
}

func main() {
	be := jito.DefaultBlockEngine()
	rpc := os.Getenv("RPC_ENDPOINT")
	if rpc == "" {
		rpc = "https://api.mainnet-beta.solana.com"
	}

	fmt.Println("── 1. Jito connectivity (getTipAccounts) ──")
	if t, err := jito.GetTipAccounts(be); err != nil {
		fmt.Printf("FAIL: %v\n", err)
	} else {
		fmt.Printf("OK: %d tip accounts\n", len(t))
	}

	fmt.Println("── 2. send_bundle with garbage tx → expect error WITH response body ──")
	if id, err := jito.SendBundle(be, []string{"aGVsbG8gd29ybGQ="}); err != nil {
		fmt.Printf("error captured: %v\n", err)
	} else {
		fmt.Printf("UNEXPECTED OK: %s\n", id)
	}

	fmt.Println("── 3. blockhash freshness (2 samples, 3s apart) ──")
	a, aok := latestBlockhash(rpc)
	time.Sleep(3 * time.Second)
	b, bok := latestBlockhash(rpc)
	switch {
	case aok && bok && a != b:
		fmt.Printf("OK: hashes differ (%s… vs %s…)\n", a[:8], b[:8])
	case aok && bok:
		fmt.Printf("FAIL: identical hash after 3s (%s == %s)\n", a, b)
	default:
		fmt.Println("FAIL: RPC call failed")
	}
}
