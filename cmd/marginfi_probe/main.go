// marginfi flash-loan GO/NO-GO. Does an empty marginfi flashloan (borrow 1
// USDC → deposit it straight back → end, net-zero balances) LAND in a Jito
// bundle — the thing Jupiter Lend can't do? One-time: creates a MarginfiAccount
// (plain keypair, persisted). Always simulates first; LIVE=1 submits.
// MODE=jito (default, the real test) or MODE=rpc (control — proves the tx is
// valid regardless of Jito).
//
// Usage: RPC_ENDPOINT=<url> KEYPAIR_PATH=<path> \
//
//	MARGINFI_ACCOUNT_KEYPAIR=<path, created if absent> \
//	[LIVE=1] [MODE=jito|rpc] [TIP_LAMPORTS=1000000] \
//	[MARGINFI_USDC_VAULT=<pk> MARGINFI_USDC_VAULT_AUTH=<pk>] [MARGINFI_DEPOSIT_OPT=1] \
//	go run ./cmd/marginfi_probe
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

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/arb"
	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/flashloan"
	"solana-arb-backtest-go/internal/jito"
	"solana-arb-backtest-go/internal/marginfi"
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

func finalizedBlockhash(endpoint string) solana.Hash {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getLatestBlockhash",
		"params": []any{map[string]any{"commitment": "finalized"}}})
	if !ok {
		fmt.Fprintln(os.Stderr, "blockhash: rpc failed")
		os.Exit(1)
	}
	value := asMap(asMap(v["result"])["value"])
	bh, err := solana.HashFromBase58(asStr(value["blockhash"]))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad blockhash: %v\n", err)
		os.Exit(1)
	}
	return bh
}

func accountExists(endpoint string, pk solana.PublicKey) bool {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{pk.String(), map[string]any{"encoding": "base64"}}})
	if !ok {
		return false
	}
	value := asMap(v["result"])["value"]
	return value != nil
}

func landed(endpoint, sig string) (map[string]any, bool) {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getTransaction",
		"params": []any{sig, map[string]any{"encoding": "json", "maxSupportedTransactionVersion": 0, "commitment": "confirmed"}}})
	if !ok {
		return nil, false
	}
	result := v["result"]
	if result == nil {
		return nil, false
	}
	return asMap(result), true
}

// loadOrMakeKeypair loads a JSON-array keypair file (solana-keygen format),
// creating a fresh one and persisting it if the path doesn't exist.
func loadOrMakeKeypair(path string) (solana.PrivateKey, bool) {
	if kp, err := solana.PrivateKeyFromSolanaKeygenFile(path); err == nil {
		return kp, false
	}
	kp, err := solana.NewRandomPrivateKey()
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate keypair: %v\n", err)
		os.Exit(1)
	}
	b, _ := json.Marshal([]byte(kp))
	if err := os.WriteFile(path, b, 0600); err != nil {
		fmt.Fprintf(os.Stderr, "write keypair: %v\n", err)
		os.Exit(1)
	}
	return kp, true
}

func main() {
	envfile.LoadDotEnv()

	endpoint := os.Getenv("RPC_ENDPOINT")
	if endpoint == "" {
		fmt.Fprintln(os.Stderr, "RPC_ENDPOINT")
		os.Exit(1)
	}
	keypairPath := os.Getenv("KEYPAIR_PATH")
	if keypairPath == "" {
		fmt.Fprintln(os.Stderr, "KEYPAIR_PATH")
		os.Exit(1)
	}
	// Either a keypair path (to create/own the account) OR just the pubkey
	// (MARGINFI_ACCOUNT) for read-only flows like MODE=simbundle — the flashloan
	// tx doesn't need the marginfi account to sign, only the authority.
	var mfiAccOverride *solana.PublicKey
	if v := os.Getenv("MARGINFI_ACCOUNT"); v != "" {
		if pk, err := solana.PublicKeyFromBase58(v); err == nil {
			mfiAccOverride = &pk
		}
	}
	mfiAccPath := os.Getenv("MARGINFI_ACCOUNT_KEYPAIR")
	live := os.Getenv("LIVE") == "1"
	mode := os.Getenv("MODE")
	if mode == "" {
		mode = "jito"
	}
	tipLamports := uint64(1_000_000)
	if v := os.Getenv("TIP_LAMPORTS"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			tipLamports = n
		}
	}
	blockEngine := jito.DefaultBlockEngine()

	authority, err := solana.PrivateKeyFromSolanaKeygenFile(keypairPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read keypair: %v\n", err)
		os.Exit(1)
	}
	signer := authority.PublicKey()
	usdc := solana.MustPublicKeyFromBase58(flashloan.USDCMint)
	usdcAta := flashloan.Ata(signer, usdc)

	fmt.Printf("authority=%s\n", signer)
	fmt.Printf("usdc vault=%s auth=%s\n", marginfi.USDCVault(), marginfi.USDCVaultAuthority())

	// Resolve marginfi account: pubkey override (read-only) or keypair (owner).
	var mfiAcc solana.PublicKey
	var mfiAccKp *solana.PrivateKey
	if mfiAccOverride != nil {
		mfiAcc = *mfiAccOverride
		fmt.Printf("marginfi account=%s (pubkey override — read-only)\n", mfiAcc)
	} else {
		if mfiAccPath == "" {
			fmt.Fprintln(os.Stderr, "set MARGINFI_ACCOUNT=<pubkey> or MARGINFI_ACCOUNT_KEYPAIR=<path>")
			os.Exit(1)
		}
		kp, freshlyMade := loadOrMakeKeypair(mfiAccPath)
		mfiAcc = kp.PublicKey()
		suffix := ""
		if freshlyMade {
			suffix = " (NEW keypair generated)"
		}
		fmt.Printf("marginfi account=%s%s\n", mfiAcc, suffix)
		mfiAccKp = &kp
	}

	// ── one-time: create the MarginfiAccount on-chain ──
	if !accountExists(endpoint, mfiAcc) {
		if mfiAccKp == nil {
			fmt.Printf("account %s doesn't exist and no keypair to create it (pubkey-override mode)\n", mfiAcc)
			return
		}
		if !live {
			fmt.Println("marginfi account does not exist yet — rerun with LIVE=1 to create it (one-time, ~0.016 SOL rent)")
			return
		}
		fmt.Println("creating MarginfiAccount…")
		bh := finalizedBlockhash(endpoint)
		ixs := []solana.Instruction{
			arb.CuLimitIx(60_000),
			arb.CuPriceIx(10_000),
			marginfi.AccountInitialize(mfiAcc, signer, signer),
		}
		tx, err := solana.NewTransaction(ixs, bh, solana.TransactionPayer(signer))
		if err != nil {
			fmt.Fprintf(os.Stderr, "compile init: %v\n", err)
			os.Exit(1)
		}
		if _, err := tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
			if key.Equals(signer) {
				return &authority
			}
			if key.Equals(mfiAcc) {
				return mfiAccKp
			}
			return nil
		}); err != nil {
			fmt.Fprintf(os.Stderr, "sign init: %v\n", err)
			os.Exit(1)
		}
		raw, err := tx.MarshalBinary()
		if err != nil {
			fmt.Fprintf(os.Stderr, "serialize init: %v\n", err)
			os.Exit(1)
		}
		b64 := base64.StdEncoding.EncodeToString(raw)
		v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "sendTransaction",
			"params": []any{b64, map[string]any{"encoding": "base64", "skipPreflight": false, "preflightCommitment": "confirmed", "maxRetries": 5}}})
		if !ok {
			fmt.Fprintln(os.Stderr, "send init: rpc failed")
			os.Exit(1)
		}
		if e, has := v["error"]; has && e != nil {
			fmt.Printf("⛔ MarginfiAccount init rejected: %v\n", e)
			os.Exit(1)
		}
		sig := asStr(v["result"])
		fmt.Printf("  init sig %s — waiting for confirmation…\n", sig)
		ok = false
		for i := 0; i < 20; i++ {
			time.Sleep(3 * time.Second)
			if meta, found := landed(endpoint, sig); found {
				metaMeta := asMap(meta["meta"])
				fmt.Printf("  ✅ MarginfiAccount created (slot %v, err %v)\n", meta["slot"], metaMeta["err"])
				ok = true
				break
			}
		}
		if !ok {
			fmt.Printf("⚠️ init not confirmed after 60s — check %s and rerun (keypair saved at %s)\n", sig, mfiAccPath)
			return
		}
	} else {
		fmt.Println("MarginfiAccount already exists — reusing")
	}

	// ── the flashloan test tx ──
	// ix layout (end_index = 6): 0 cu_limit, 1 cu_price, 2 create-ATA,
	// 3 start_flashloan(6), 4 borrow 1 USDC, 5 deposit 1 USDC, 6 end_flashloan, 7 tip.
	var tipTo solana.PublicKey
	{
		found := false
		for i := 0; i < 12; i++ {
			if v, err := jito.GetTipAccounts(blockEngine); err == nil && len(v) > 0 {
				tipTo = v[0]
				found = true
				break
			}
			time.Sleep(3 * time.Second)
		}
		if !found {
			fmt.Fprintln(os.Stderr, "tip accounts (rate limited)")
			os.Exit(1)
		}
	}
	bh := finalizedBlockhash(endpoint)
	fmt.Printf("blockhash %s\n", bh)
	ixs := []solana.Instruction{
		arb.CuLimitIx(400_000),
		arb.CuPriceIx(10_000),
		flashloan.CreateAtaIdempotent(signer, usdc),
		marginfi.StartFlashloan(mfiAcc, signer, 6),
		marginfi.BorrowUSDC(mfiAcc, signer, usdcAta, 1_000_000),
		marginfi.PaybackUSDC(mfiAcc, signer, usdcAta, 1_000_000, true),
		marginfi.EndFlashloan(mfiAcc, signer, nil), // net-zero → empty remaining
		arb.TransferIx(signer, tipTo, tipLamports),
	}
	tx, err := solana.NewTransaction(ixs, bh, solana.TransactionPayer(signer))
	if err != nil {
		fmt.Fprintf(os.Stderr, "compile flashloan: %v\n", err)
		os.Exit(1)
	}
	if _, err := tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(signer) {
			return &authority
		}
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "sign flashloan: %v\n", err)
		os.Exit(1)
	}
	sig := tx.Signatures[0].String()
	raw, err := tx.MarshalBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "serialize flashloan: %v\n", err)
		os.Exit(1)
	}
	b64 := base64.StdEncoding.EncodeToString(raw)
	fmt.Printf("marginfi flashloan tx %dB sig=%s tip=%d\n", len(raw), sig, tipLamports)

	// simulate first
	sim, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "simulateTransaction",
		"params": []any{b64, map[string]any{"encoding": "base64", "sigVerify": false, "replaceRecentBlockhash": true}}})
	if !ok {
		fmt.Fprintln(os.Stderr, "simulate: rpc failed")
		os.Exit(1)
	}
	simValue := asMap(asMap(sim["result"])["value"])
	if errV, has := simValue["err"]; has && errV != nil {
		fmt.Printf("⛔ simulation FAILED: %v\n", errV)
		for _, l := range asArray(simValue["logs"]) {
			fmt.Printf("  %s\n", asStr(l))
		}
		fmt.Println("\n(fix accounts/args from the logs above — likely vault PDA or deposit arg; see env overrides)")
		os.Exit(1)
	}
	fmt.Printf("✅ simulates clean (%v CU)\n", simValue["unitsConsumed"])

	// MODE=simbundle: run Jito's simulateBundle — executes the bundle exactly as
	// the block engine would (needs a simulateBundle-capable RPC, e.g. Helius).
	// Read-only, no cost. Rules out a filter/execution problem: if this succeeds
	// but the live bundle never lands, the barrier is the AUCTION (tip/profit),
	// not Jito rejecting the bundle.
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
				logs := asArray(rm["logs"])
				start := len(logs) - 3
				if start < 0 {
					start = 0
				}
				for _, l := range logs[start:] {
					fmt.Printf("    %s\n", asStr(l))
				}
			}
		default:
			fmt.Println("simulateBundle: no response (does this RPC support it? use Helius)")
		}
		return
	}

	if !live {
		fmt.Printf("dry run — rerun with LIVE=1 to submit via %s (~%d lamports if it lands)\n", mode, tipLamports+10_000)
		return
	}

	// submit
	switch mode {
	case "rpc":
		v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "sendTransaction",
			"params": []any{b64, map[string]any{"encoding": "base64", "skipPreflight": false, "preflightCommitment": "confirmed", "maxRetries": 5}}})
		if !ok {
			fmt.Fprintln(os.Stderr, "send: rpc failed")
			os.Exit(1)
		}
		if e, has := v["error"]; has && e != nil {
			fmt.Printf("⛔ rpc rejected: %v\n", e)
			os.Exit(1)
		}
		fmt.Printf("⚡ sent via plain RPC: %v\n", v["result"])
	default:
		attempt := 0
		var id string
		for {
			attempt++
			var err error
			id, err = jito.SendBundle(blockEngine, []string{b64})
			if err == nil {
				break
			}
			if strings.Contains(err.Error(), "429") && attempt < 12 {
				fmt.Printf("  [attempt %d] rate limited, retry in 5s…\n", attempt)
				time.Sleep(5 * time.Second)
				continue
			}
			fmt.Fprintf(os.Stderr, "send bundle: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("⚡ submitted Jito bundle %s (attempt %d)\n", id, attempt)
	}

	for i := 1; i <= 18; i++ {
		time.Sleep(5 * time.Second)
		if meta, found := landed(endpoint, sig); found {
			metaMeta := asMap(meta["meta"])
			fmt.Printf("\n🎉 LANDED via %s — slot %v fee %v err %v\n", mode, meta["slot"], metaMeta["fee"], metaMeta["err"])
			fmt.Printf("https://solscan.io/tx/%s\n", sig)
			return
		}
		fmt.Printf("[%ds] on_chain=false\n", i*5)
	}
	fmt.Printf("\n⚠️ not landed after 90s via %s. If MODE=rpc landed but jito didn't → flash loans are filtered on Jito generally → pivot to inventory mode.\n", mode)
}
