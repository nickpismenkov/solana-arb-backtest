// Deploy an address-lookup-table (ALT) on-chain from a list of accounts on
// stdin — so the box doesn't need the `solana` CLI to create SAVE_ALT / JUP_ALT.
//
// Reads pubkeys (one per line) from stdin, creates a fresh lookup table owned by
// the KEYPAIR_PATH wallet, extends it with those addresses (batched to fit the
// 1232B tx limit), and prints the resulting table address.
//
// Pipe it from the *_alt_print bins:
//
//	LIVE=1 go run ./cmd/save_alt_print | go run ./cmd/alt_deploy
//	LIVE=1 go run ./cmd/jup_alt_print  | go run ./cmd/alt_deploy
//
// then `export SAVE_ALT=<printed table>` (or JUP_ALT=…).
//
// SAFETY: DRY-RUN by default — it only prints the plan. Set LIVE=1 to submit
// real txs (creates on-chain state + spends a little SOL for rent + fees, signed
// by KEYPAIR_PATH). Uses the official solana-go address-lookup-table program
// instruction builders, not a hand-rolled format.
//
// Env: HELIUS_RPC|RPC_HTTP|RPC_ENDPOINT, KEYPAIR_PATH, [LIVE=1], [CU_PRICE=<micro-lamports>]
package main

import (
	"bufio"
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
	addresslookuptable "github.com/gagliardetto/solana-go/programs/address-lookup-table"

	"solana-arb-backtest-go/internal/arb"
	"solana-arb-backtest-go/internal/envfile"
)

// extendBatch is the number of addresses per extend tx. Each pubkey is 32B of
// ix data; 20 keeps the tx well under the 1232B single-packet limit with room
// for the header + signature.
const extendBatch = 20

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

func latestBlockhash(endpoint string) solana.Hash {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getLatestBlockhash",
		"params": []any{map[string]string{"commitment": "finalized"}}})
	if !ok {
		panic("getLatestBlockhash")
	}
	result := v["result"].(map[string]any)
	value := result["value"].(map[string]any)
	bh, ok := value["blockhash"].(string)
	if !ok {
		panic("blockhash")
	}
	hash, err := solana.HashFromBase58(bh)
	if err != nil {
		panic(err)
	}
	return hash
}

// sendAndConfirm builds a tx from ixs, signs with kp, submits, and polls until
// confirmed. Returns the signature. Exits the process on rejection or confirm
// timeout.
func sendAndConfirm(endpoint string, kp solana.PrivateKey, ixs []solana.Instruction, label string) string {
	bh := latestBlockhash(endpoint)
	tx, err := solana.NewTransaction(ixs, bh, solana.TransactionPayer(kp.PublicKey()))
	if err != nil {
		panic(fmt.Sprintf("compile: %v", err))
	}
	if _, err := tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(kp.PublicKey()) {
			return &kp
		}
		return nil
	}); err != nil {
		panic(fmt.Sprintf("sign: %v", err))
	}
	sig := tx.Signatures[0].String()
	raw, err := tx.MarshalBinary()
	if err != nil {
		panic(err)
	}
	b64 := base64.StdEncoding.EncodeToString(raw)

	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "sendTransaction",
		"params": []any{b64, map[string]any{"encoding": "base64", "skipPreflight": false, "preflightCommitment": "confirmed", "maxRetries": 5}}})
	if !ok {
		fmt.Fprintf(os.Stderr, "⛔ %s: sendTransaction failed\n", label)
		os.Exit(1)
	}
	if e, has := v["error"]; has && e != nil {
		fmt.Fprintf(os.Stderr, "⛔ %s: sendTransaction rejected: %v\n", label, e)
		os.Exit(1)
	}
	fmt.Printf("  %s: submitted %s — confirming…\n", label, sig)

	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(1500 * time.Millisecond)
		s, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getSignatureStatuses",
			"params": []any{[]string{sig}, map[string]any{"searchTransactionHistory": false}}})
		if !ok {
			continue
		}
		result, ok := s["result"].(map[string]any)
		if !ok {
			continue
		}
		valueArr, ok := result["value"].([]any)
		if !ok || len(valueArr) == 0 || valueArr[0] == nil {
			continue
		}
		st, ok := valueArr[0].(map[string]any)
		if !ok {
			continue
		}
		if errVal, has := st["err"]; has && errVal != nil {
			fmt.Fprintf(os.Stderr, "⛔ %s: tx failed on-chain: %v\n", label, errVal)
			os.Exit(1)
		}
		cs, _ := st["confirmationStatus"].(string)
		if cs == "confirmed" || cs == "finalized" {
			fmt.Printf("  %s: ✅ %s\n", label, cs)
			return sig
		}
	}
	fmt.Fprintf(os.Stderr, "⛔ %s: not confirmed within 45s (sig %s); check explorer before retrying to avoid a duplicate table\n", label, sig)
	os.Exit(1)
	return ""
}

func main() {
	envfile.LoadDotEnv()
	endpoint := os.Getenv("HELIUS_RPC")
	if endpoint == "" {
		endpoint = os.Getenv("RPC_HTTP")
	}
	if endpoint == "" {
		endpoint = os.Getenv("RPC_ENDPOINT")
	}
	if endpoint == "" {
		fmt.Fprintln(os.Stderr, "set HELIUS_RPC / RPC_HTTP / RPC_ENDPOINT")
		os.Exit(1)
	}
	keypairPath := os.Getenv("KEYPAIR_PATH")
	if keypairPath == "" {
		fmt.Fprintln(os.Stderr, "set KEYPAIR_PATH")
		os.Exit(1)
	}
	live := os.Getenv("LIVE") == "1"
	cuPrice := uint64(100_000)
	if s := os.Getenv("CU_PRICE"); s != "" {
		if n, err := strconv.ParseUint(s, 10, 64); err == nil {
			cuPrice = n
		}
	}

	kp, err := solana.PrivateKeyFromSolanaKeygenFile(keypairPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read keypair: %v\n", err)
		os.Exit(1)
	}
	authority := kp.PublicKey()

	// Read addresses from stdin (one per line). Non-parseable lines are skipped
	// so piping a print bin's stdout (addresses only; notes go to stderr) is clean.
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read stdin: %v\n", err)
		os.Exit(1)
	}
	seen := make(map[solana.PublicKey]bool)
	var addrs []solana.PublicKey
	sc := bufio.NewScanner(strings.NewReader(string(input)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		pk, err := solana.PublicKeyFromBase58(line)
		if err != nil {
			continue
		}
		if seen[pk] {
			continue
		}
		seen[pk] = true
		addrs = append(addrs, pk)
	}
	if len(addrs) == 0 {
		fmt.Fprintln(os.Stderr, "⛔ no valid pubkeys on stdin — pipe from save_alt_print / jup_alt_print")
		os.Exit(1)
	}

	// Recent (finalized) slot for the CreateLookupTable derivation.
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getSlot",
		"params": []any{map[string]any{"commitment": "finalized"}}})
	if !ok {
		fmt.Fprintln(os.Stderr, "getSlot failed")
		os.Exit(1)
	}
	slotF, ok := v["result"].(float64)
	if !ok {
		fmt.Fprintln(os.Stderr, "getSlot: bad result")
		os.Exit(1)
	}
	slot := uint64(slotF)

	createBuilder, table, err := addresslookuptable.NewCreateLookupTableInstruction(authority, authority, slot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create ix: %v\n", err)
		os.Exit(1)
	}
	createIx, err := createBuilder.ValidateAndBuild()
	if err != nil {
		fmt.Fprintf(os.Stderr, "create ix build: %v\n", err)
		os.Exit(1)
	}
	batches := (len(addrs) + extendBatch - 1) / extendBatch

	fmt.Println("ALT deploy plan:")
	fmt.Printf("  authority/payer : %s\n", authority.String())
	fmt.Printf("  addresses       : %d\n", len(addrs))
	fmt.Printf("  recent slot     : %d\n", slot)
	fmt.Printf("  table address   : %s\n", table.String())
	fmt.Printf("  txs             : 1 create + %d extend (batches of %d)\n", batches, extendBatch)

	if !live {
		fmt.Println("\nDRY RUN — nothing submitted. Set LIVE=1 to deploy for real.")
		fmt.Println("NOTE: the table address above is derived from the current slot and will DIFFER on the live run;")
		fmt.Println("      the real address is printed at the end of the LIVE run.")
		return
	}

	fmt.Println("\nLIVE — submitting…")
	sendAndConfirm(endpoint, kp, []solana.Instruction{arb.CuLimitIx(60_000), arb.CuPriceIx(cuPrice), createIx}, "create")
	for i := 0; i < len(addrs); i += extendBatch {
		end := i + extendBatch
		if end > len(addrs) {
			end = len(addrs)
		}
		chunk := addrs[i:end]
		extendBuilder := addresslookuptable.NewExtendLookupTableInstruction(table, authority, authority, chunk)
		extendIx, err := extendBuilder.ValidateAndBuild()
		if err != nil {
			fmt.Fprintf(os.Stderr, "extend ix build: %v\n", err)
			os.Exit(1)
		}
		label := fmt.Sprintf("extend %d/%d", i/extendBatch+1, batches)
		sendAndConfirm(endpoint, kp, []solana.Instruction{arb.CuLimitIx(60_000), arb.CuPriceIx(cuPrice), extendIx}, label)
	}

	fmt.Printf("\n✅ ALT deployed with %d addresses.\n", len(addrs))
	fmt.Printf("   table = %s\n", table.String())
	fmt.Println("   → export the matching var, e.g.  export SAVE_ALT=" + table.String() + "   (or JUP_ALT=" + table.String() + ")")
	fmt.Println("   (an ALT needs ~1 slot to warm up before it can be used in a tx.)")
}
