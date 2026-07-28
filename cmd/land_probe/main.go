// End-to-end LANDING certification: fire a Jito bundle that does NOT depend
// on any market spread — [flash-borrow 1 USDC, payback 1 USDC, tip] — so it
// always succeeds and should land on-chain. Proves the whole live path:
// signing, blockhash, flash loan, bundle submission, tip payment, readback.
// Cost when it lands: tip + base fee + priority (~10k lamports, <$0.01).
//
// Default is simulate-only. LIVE=1 submits for real.
// MODE=jito (default) submits as a Jito bundle; MODE=rpc submits the SAME tx
// via plain sendTransaction — bisects "tx invalid" from "Jito bundle path
// broken": if rpc lands and jito doesn't, the tx is fine and Jito is the issue.
//
// Usage: RPC_ENDPOINT=<url> KEYPAIR_PATH=<path> [LIVE=1] [MODE=jito|rpc] \
//
//	[TIP_LAMPORTS=5000] go run ./cmd/land_probe
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/arb"
	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/flashloan"
	"solana-arb-backtest-go/internal/jito"
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

func rpc(endpoint string, body map[string]any) (map[string]any, bool) {
	for attempt := 0; attempt < 3; attempt++ {
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
		time.Sleep(time.Duration(300<<attempt) * time.Millisecond)
	}
	return nil, false
}

func mustEnv(key string) string {
	v, ok := os.LookupEnv(key)
	if !ok {
		fmt.Fprintf(os.Stderr, "%s must be set\n", key)
		os.Exit(1)
	}
	return v
}

func envBool(key string) bool {
	return os.Getenv(key) == "1"
}

func main() {
	envfile.LoadDotEnv()
	endpoint := mustEnv("RPC_ENDPOINT")
	keypairPath := mustEnv("KEYPAIR_PATH")
	live := envBool("LIVE")
	mode := os.Getenv("MODE")
	if mode == "" {
		mode = "jito"
	}
	tipLamports := uint64(5000)
	if v := os.Getenv("TIP_LAMPORTS"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			tipLamports = n
		}
	}
	blockEngine := jito.DefaultBlockEngine()

	kp, err := solana.PrivateKeyFromSolanaKeygenFile(keypairPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read keypair: %v\n", err)
		os.Exit(1)
	}
	signer := kp.PublicKey()
	usdc := solana.MustPublicKeyFromBase58(flashloan.USDCMint)

	tipAccounts, err := jito.GetTipAccounts(blockEngine)
	if err != nil || len(tipAccounts) == 0 {
		fmt.Fprintf(os.Stderr, "tip accounts: %v\n", err)
		os.Exit(1)
	}
	tipTo := tipAccounts[0]

	// FINALIZED blockhash: visible to every bank (confirmed-fresh hashes can be
	// rejected as BlockhashNotFound by validators/preflight still on finalized).
	// Still ~60s of validity left — plenty for a probe.
	bhResp, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getLatestBlockhash",
		"params": []any{map[string]any{"commitment": "finalized"}}})
	if !ok {
		fmt.Fprintln(os.Stderr, "blockhash: rpc failed")
		os.Exit(1)
	}
	bhResult := bhResp["result"].(map[string]any)
	bhValue := bhResult["value"].(map[string]any)
	bhContext := bhResult["context"].(map[string]any)
	bhStr := bhValue["blockhash"].(string)
	fmt.Printf("blockhash %s (slot %v, lastValidBlockHeight %v)\n", bhStr, bhContext["slot"], bhValue["lastValidBlockHeight"])
	bh, err := solana.HashFromBase58(bhStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad blockhash: %v\n", err)
		os.Exit(1)
	}

	// No-spread-required bundle: borrow 1 USDC, pay it straight back, tip.
	// BARE=1 drops the flash-loan legs (isolates "Jito filters the flash-loan
	// program" — a bare self-transfer + tip has nothing left to object to).
	bare := envBool("BARE")
	withAta := envBool("ATA")
	var ixs []solana.Instruction
	if bare {
		ixs = []solana.Instruction{arb.CuLimitIx(50_000), arb.CuPriceIx(10_000)}
		if withAta {
			ixs = append(ixs, flashloan.CreateAtaIdempotent(signer, usdc))
		}
		ixs = append(ixs, arb.TransferIx(signer, signer, 1_000))
		ixs = append(ixs, arb.TransferIx(signer, tipTo, tipLamports))
	} else {
		ixs = []solana.Instruction{
			arb.CuLimitIx(200_000),
			arb.CuPriceIx(10_000),
			flashloan.CreateAtaIdempotent(signer, usdc),
			flashloan.BorrowUSDC(signer, 1_000_000),
			flashloan.PaybackUSDC(signer, 1_000_000),
			arb.TransferIx(signer, tipTo, tipLamports),
		}
	}

	tx, err := arb.CompileV0(signer, ixs, nil, bh)
	if err != nil {
		fmt.Fprintf(os.Stderr, "compile: %v\n", err)
		os.Exit(1)
	}
	if _, err := tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(signer) {
			return &kp
		}
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "sign: %v\n", err)
		os.Exit(1)
	}
	sig := tx.Signatures[0].String()
	raw, err := tx.MarshalBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "serialize: %v\n", err)
		os.Exit(1)
	}
	b64 := base64.StdEncoding.EncodeToString(raw)

	fmt.Printf("land-probe: signer=%s tx=%dB tip=%d lamports sig=%s\n", signer, len(raw), tipLamports, sig)

	// Always simulate first — refuse to submit a tx that wouldn't succeed.
	sim, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "simulateTransaction",
		"params": []any{b64, map[string]any{"encoding": "base64", "sigVerify": false, "replaceRecentBlockhash": true}}})
	if !ok {
		fmt.Fprintln(os.Stderr, "simulate: rpc failed")
		os.Exit(1)
	}
	simResult := sim["result"].(map[string]any)
	simValue := simResult["value"].(map[string]any)
	if simErr := simValue["err"]; simErr != nil {
		fmt.Printf("⛔ simulation failed, NOT submitting: %v\n", simErr)
		if logs, ok := simValue["logs"].([]any); ok {
			for _, l := range logs {
				if s, ok := l.(string); ok {
					fmt.Printf("  %s\n", s)
				}
			}
		}
		os.Exit(1)
	}
	fmt.Printf("✅ simulates clean (%v CU)\n", simValue["unitsConsumed"])

	// MODE=simbundle: Jito simulateBundle — executes the bundle as the block
	// engine would (needs Helius/QuickNode Lil-JIT). Read-only, no cost.
	if mode == "simbundle" {
		v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "simulateBundle",
			"params": []any{map[string]any{"encodedTransactions": []string{b64}}}})
		switch {
		case ok && v["error"] != nil:
			fmt.Printf("simulateBundle error: %v\n", v["error"])
		case ok:
			result := asMap(v["result"])
			value := asMap(result["value"])
			fmt.Printf("simulateBundle summary: %v\n", value["summary"])
			for i, r := range asArray(value["transactionResults"]) {
				rm := asMap(r)
				fmt.Printf("  tx[%d] err=%v cu=%v\n", i, rm["err"], rm["unitsConsumed"])
			}
		default:
			fmt.Println("simulateBundle: no response (RPC must support it — use Helius)")
		}
		return
	}

	if !live {
		fmt.Printf("dry run (set LIVE=1 to submit for real — costs ~%d lamports)\n", tipLamports+10_000)
		return
	}

	var bundleID string
	switch mode {
	case "jitotx":
		// Jito transactions endpoint, bundleOnly=true → single-tx bundle
		// WITH revert protection; documented low-latency send path.
		url := fmt.Sprintf("%s/api/v1/transactions?bundleOnly=true", blockEngine)
		body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "sendTransaction",
			"params": []any{b64, map[string]any{"encoding": "base64"}}}
		b, _ := json.Marshal(body)
		var v map[string]any
		resp, err := httpClient.Post(url, "application/json", bytes.NewReader(b))
		if err != nil {
			v = map[string]any{"error": err.Error()}
		} else {
			defer resp.Body.Close()
			if resp.StatusCode >= 300 {
				raw, _ := io.ReadAll(resp.Body)
				v = map[string]any{"error": fmt.Sprintf("HTTP %d: %s", resp.StatusCode, raw)}
			} else if json.NewDecoder(resp.Body).Decode(&v) != nil {
				v = map[string]any{}
			}
		}
		if e := v["error"]; e != nil {
			fmt.Printf("⛔ jito sendTransaction rejected: %v\n", e)
			os.Exit(1)
		}
		fmt.Printf("⚡ sent via jito transactions endpoint (bundleOnly): %v\n", v["result"])
	case "rpc":
		// Plain sendTransaction — no Jito. If THIS lands, the tx is valid
		// and any Jito non-landing is a bundle-path problem, not ours.
		v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "sendTransaction",
			"params": []any{b64, map[string]any{"encoding": "base64", "skipPreflight": false, "preflightCommitment": "confirmed", "maxRetries": 5}}})
		if !ok {
			fmt.Fprintln(os.Stderr, "sendTransaction: rpc failed")
			os.Exit(1)
		}
		if e := v["error"]; e != nil {
			fmt.Printf("⛔ sendTransaction rejected: %v\n", e)
			os.Exit(1)
		}
		fmt.Printf("⚡ sent via plain RPC: %v\n", v["result"])
	default:
		// The unauth lane 429s often — retry with backoff for up to ~60s.
		attempt := 0
		for {
			attempt++
			id, err := jito.SendBundle(blockEngine, []string{b64})
			if err == nil {
				bundleID = id
				break
			}
			if strings.Contains(err.Error(), "429") && attempt < 12 {
				fmt.Printf("  [attempt %d] rate limited, retrying in 5s…\n", attempt)
				time.Sleep(5 * time.Second)
				continue
			}
			fmt.Fprintf(os.Stderr, "send bundle: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("⚡ submitted bundle %s (attempt %d)\n", bundleID, attempt)
	}

	// Poll until landed (or give up after ~90s).
	for i := 1; i <= 18; i++ {
		time.Sleep(5 * time.Second)
		status := "n/a"
		if mode == "jito" {
			if s, ok := jito.BundleStatus(blockEngine, bundleID); ok {
				status = s
			} else {
				status = "unknown"
			}
		}
		txMeta, txOk := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getTransaction",
			"params": []any{sig, map[string]any{"encoding": "json", "maxSupportedTransactionVersion": 0, "commitment": "confirmed"}}})
		landed := txOk && txMeta["result"] != nil
		fmt.Printf("[%ds] jito_status=%s on_chain=%v\n", i*5, status, landed)
		if landed {
			meta := asMap(txMeta["result"])
			metaMeta := asMap(meta["meta"])
			fmt.Printf("\n🎉 LANDED — slot %v fee %v lamports err %v\n", meta["slot"], metaMeta["fee"], metaMeta["err"])
			fmt.Printf("https://solscan.io/tx/%s\n", sig)
			return
		}
	}
	fmt.Println("\n⚠️ not seen on-chain after 90s — if MODE=rpc also fails, the tx itself is the problem; if only jito fails, raise TIP_LAMPORTS or the bundle path is at fault")
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asArray(v any) []any {
	a, _ := v.([]any)
	return a
}
