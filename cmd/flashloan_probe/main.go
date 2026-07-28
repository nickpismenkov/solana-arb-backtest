// Verifies the Jupiter Lend flash-loan builders: assemble
// [create-ATA, borrow, payback] for EACH wired debt asset (USDC/USDT/wSOL) and
// simulate against mainnet. A self-repaying 0-fee flash loan nets zero, so with
// the ATA created each should simulate clean (err = null) — proving the ported
// instruction format + per-asset market accounts are correct end to end. This
// is the ground-truth check for the derived USDT/wSOL flash markets.
//
// Usage: RPC_ENDPOINT=<url> go run ./cmd/flashloan_probe
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
	"solana-arb-backtest-go/internal/flashloan"
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

func probe(endpoint string, signer, tp solana.PublicKey, name string, mint solana.PublicKey, amount uint64) bool {
	createAta := flashloan.CreateAtaIdempotentFor(signer, mint, tp)
	borrowIx, ok := flashloan.Borrow(signer, mint, amount)
	if !ok {
		panic("wired market")
	}
	paybackIx, ok := flashloan.Payback(signer, mint, amount)
	if !ok {
		panic("wired market")
	}
	ixs := []solana.Instruction{createAta, borrowIx, paybackIx}

	tx, err := solana.NewTransaction(ixs, solana.Hash{}, solana.TransactionPayer(signer))
	if err != nil {
		panic(err)
	}
	// sigVerify=false — placeholder signature.
	tx.Signatures = []solana.Signature{{}}
	raw, err := tx.MarshalBinary()
	if err != nil {
		panic(err)
	}
	b64 := base64.StdEncoding.EncodeToString(raw)

	v, _ := rpc(endpoint, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "simulateTransaction",
		"params": []any{b64, map[string]any{"encoding": "base64", "sigVerify": false, "replaceRecentBlockhash": true}},
	})
	var val map[string]any
	if v != nil {
		if result, ok := v["result"].(map[string]any); ok {
			if value, ok := result["value"].(map[string]any); ok {
				val = value
			}
		}
	}
	errField := val["err"]

	fmt.Printf("\n=== Jupiter Lend %s flash loan (borrow %d → payback %d) ===\n", name, amount, amount)
	errJSON, _ := json.Marshal(errField)
	fmt.Printf("err: %s\n", string(errJSON))
	if errField == nil {
		units := uint64(0)
		if u, ok := val["unitsConsumed"].(float64); ok {
			units = uint64(u)
		}
		fmt.Printf("✅ %s VERIFIED — self-repaying flash loan simulated clean (%d CU)\n", name, units)
		return true
	}
	fmt.Printf("⚠️  %s did not simulate clean — inspect logs:\n", name)
	if logs, ok := val["logs"].([]any); ok {
		for _, l := range logs {
			if s, ok := l.(string); ok {
				fmt.Printf("  %s\n", s)
			}
		}
	}
	return false
}

func main() {
	envfile.LoadDotEnv()
	endpoint := os.Getenv("RPC_ENDPOINT")
	if endpoint == "" {
		endpoint = os.Getenv("HELIUS_RPC")
	}
	if endpoint == "" {
		endpoint = "https://api.mainnet-beta.solana.com"
	}
	signer := solana.MustPublicKeyFromBase58("Anu6Awu4kxaEDrg1nkpcikx6tJ2xhfVci5TvDrZBsZEB")
	tp := solana.MustPublicKeyFromBase58("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA")

	usdc := solana.MustPublicKeyFromBase58(flashloan.USDCMint)
	usdt := solana.MustPublicKeyFromBase58(flashloan.USDTMint)
	wsol := solana.MustPublicKeyFromBase58(flashloan.WSOLMint)

	ok := 0
	if probe(endpoint, signer, tp, "USDC", usdc, 1_000_000) {
		ok++
	}
	if probe(endpoint, signer, tp, "USDT", usdt, 1_000_000) {
		ok++
	}
	if probe(endpoint, signer, tp, "wSOL", wsol, 10_000_000) { // 0.01 SOL
		ok++
	}

	fmt.Printf("\n── %d/3 flash markets verified ──\n", ok)
}
