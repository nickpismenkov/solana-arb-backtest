// Command marginfi_probe is a marginfi flash-loan GO/NO-GO test. Does an
// empty marginfi flashloan (borrow 1 USDC -> deposit it straight back ->
// end, net-zero balances) LAND in a Jito bundle — the thing Jupiter Lend
// can't do? One-time: creates a MarginfiAccount (plain keypair, persisted).
// Always simulates first; LIVE=1 submits. MODE=jito (default, the real
// test) or MODE=rpc (control — proves the tx is valid regardless of Jito).
//
// Usage: RPC_ENDPOINT=<url> KEYPAIR_PATH=<path> \
//
//	MARGINFI_ACCOUNT_KEYPAIR=<path, created if absent> \
//	[LIVE=1] [MODE=jito|rpc] [TIP_LAMPORTS=1000000] \
//	[MARGINFI_USDC_VAULT=<pk> MARGINFI_USDC_VAULT_AUTH=<pk>] [MARGINFI_DEPOSIT_OPT=1] \
//	go run ./cmd/marginfi_probe
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"arbengine/internal/arb"
	"arbengine/internal/config"
	"arbengine/internal/flashloan"
	"arbengine/internal/jito"
	"arbengine/internal/marginfi"
	"arbengine/internal/rpcclient"
	"arbengine/internal/solana"
)

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// rpcRaw issues a raw JSON-RPC call with up to 3 retries and exponential
// backoff, mirroring the Rust probe's ad-hoc `rpc()` helper (used for calls
// rpcclient doesn't wrap 1:1, e.g. simulateBundle / getAccountInfo-exists /
// getTransaction-json).
func rpcRaw(endpoint, method string, params any) (map[string]any, bool) {
	c := rpcclient.New(endpoint)
	for attempt := 0; attempt < 3; attempt++ {
		raw, err := c.Call(method, params)
		if err == nil {
			var v any
			if json.Unmarshal(raw, &v) == nil {
				return map[string]any{"result": v}, true
			}
		}
		time.Sleep(time.Duration(300<<attempt) * time.Millisecond)
	}
	return nil, false
}

func finalizedBlockhash(endpoint string) solana.Hash {
	v, ok := rpcRaw(endpoint, "getLatestBlockhash", []any{map[string]string{"commitment": "finalized"}})
	if !ok {
		fatal("blockhash")
	}
	value, _ := v["result"].(map[string]any)["value"].(map[string]any)
	bhStr, _ := value["blockhash"].(string)
	bh, err := solana.HashFromBase58(bhStr)
	if err != nil {
		fatal("bh str")
	}
	return bh
}

func accountExists(endpoint string, pk solana.Pubkey) bool {
	v, ok := rpcRaw(endpoint, "getAccountInfo", []any{pk.String(), map[string]string{"encoding": "base64"}})
	if !ok {
		return false
	}
	result, _ := v["result"].(map[string]any)
	value, hasValue := result["value"]
	return hasValue && value != nil
}

func landed(endpoint, sig string) (map[string]any, bool) {
	v, ok := rpcRaw(endpoint, "getTransaction", []any{sig, map[string]any{
		"encoding": "json", "maxSupportedTransactionVersion": 0, "commitment": "confirmed",
	}})
	if !ok {
		return nil, false
	}
	result := v["result"]
	if result == nil {
		return nil, false
	}
	m, ok := result.(map[string]any)
	return m, ok
}

func loadOrMakeKeypair(path string) (solana.Keypair, bool) {
	if data, err := os.ReadFile(path); err == nil {
		var bytes []byte
		if err := json.Unmarshal(data, &bytes); err != nil {
			fatal("parse keypair")
		}
		kp, err := solana.KeypairFromBytes(bytes)
		if err != nil {
			fatal("keypair")
		}
		return kp, false
	}
	kp, err := solana.NewKeypair()
	if err != nil {
		fatal("generate keypair")
	}
	wire := append(append([]byte{}, kp.Private[:32]...), kp.Public.Bytes()...)
	b, _ := json.Marshal(wire)
	if err := os.WriteFile(path, b, 0600); err != nil {
		fatal("write keypair")
	}
	return kp, true
}

func main() {
	config.LoadDotenv()

	endpoint, ok := config.EnvOptional("RPC_ENDPOINT")
	if !ok {
		fatal("RPC_ENDPOINT")
	}
	keypairPath, ok := config.EnvOptional("KEYPAIR_PATH")
	if !ok {
		fatal("KEYPAIR_PATH")
	}
	// Either a keypair path (to create/own the account) OR just the pubkey
	// (MARGINFI_ACCOUNT) for read-only flows like MODE=simbundle — the
	// flashloan tx doesn't need the marginfi account to sign, only the
	// authority.
	var mfiAccOverride solana.Pubkey
	var haveOverride bool
	if s, ok := config.EnvOptional("MARGINFI_ACCOUNT"); ok {
		if pk, err := solana.PubkeyFromBase58(s); err == nil {
			mfiAccOverride, haveOverride = pk, true
		}
	}
	mfiAccPath := config.EnvOr("MARGINFI_ACCOUNT_KEYPAIR", "")
	live := config.EnvOr("LIVE", "") == "1"
	mode := config.EnvOr("MODE", "jito")
	tipLamports := config.EnvUint64("TIP_LAMPORTS", 1_000_000)
	blockEngine := jito.DefaultBlockEngine()

	data, err := os.ReadFile(keypairPath)
	if err != nil {
		fatal("read keypair")
	}
	var kpBytes []byte
	if err := json.Unmarshal(data, &kpBytes); err != nil {
		fatal("parse")
	}
	authority, err := solana.KeypairFromBytes(kpBytes)
	if err != nil {
		fatal("keypair")
	}
	signer := authority.Public
	usdc := solana.MustPubkeyFromBase58(marginfi.USDCMint)
	usdcAta := flashloan.Ata(signer, usdc)

	fmt.Printf("authority=%s\n", signer)
	fmt.Printf("usdc vault=%s auth=%s\n", marginfi.USDCVault(), marginfi.USDCVaultAuthority())

	// Resolve marginfi account: pubkey override (read-only) or keypair (owner).
	var mfiAcc solana.Pubkey
	var mfiAccKp *solana.Keypair
	if haveOverride {
		fmt.Printf("marginfi account=%s (pubkey override — read-only)\n", mfiAccOverride)
		mfiAcc = mfiAccOverride
	} else {
		if mfiAccPath == "" {
			fatal("set MARGINFI_ACCOUNT=<pubkey> or MARGINFI_ACCOUNT_KEYPAIR=<path>")
		}
		kp, freshlyMade := loadOrMakeKeypair(mfiAccPath)
		suffix := ""
		if freshlyMade {
			suffix = " (NEW keypair generated)"
		}
		fmt.Printf("marginfi account=%s%s\n", kp.Public, suffix)
		mfiAcc = kp.Public
		mfiAccKp = &kp
	}

	// one-time: create the MarginfiAccount on-chain.
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
		msg, err := solana.CompileV0(signer, ixs, nil, bh)
		if err != nil {
			fatal("compile init: %v", err)
		}
		tx := solana.NewUnsignedVersionedTransaction(msg)
		if err := tx.Sign([]solana.Keypair{authority, *mfiAccKp}); err != nil {
			fatal("sign init: %v", err)
		}
		b64, err := tx.Base64()
		if err != nil {
			fatal("encode init: %v", err)
		}
		v, ok := rpcRaw(endpoint, "sendTransaction", []any{b64, map[string]any{
			"encoding": "base64", "skipPreflight": false, "preflightCommitment": "confirmed", "maxRetries": 5,
		}})
		if !ok {
			fatal("send init")
		}
		if e, hasErr := v["result"].(map[string]any)["error"]; hasErr && e != nil {
			fmt.Printf("⛔ MarginfiAccount init rejected: %v\n", e)
			os.Exit(1)
		}
		sig, _ := v["result"].(string)
		fmt.Printf("  init sig %s — waiting for confirmation…\n", sig)
		okConfirmed := false
		for i := 0; i < 20; i++ {
			time.Sleep(3 * time.Second)
			if meta, found := landed(endpoint, sig); found {
				fmt.Printf("  ✅ MarginfiAccount created (slot %v, err %v)\n", meta["slot"], metaErr(meta))
				okConfirmed = true
				break
			}
		}
		if !okConfirmed {
			fmt.Printf("⚠️ init not confirmed after 60s — check %s and rerun (keypair saved at %s)\n", sig, mfiAccPath)
			return
		}
	} else {
		fmt.Println("MarginfiAccount already exists — reusing")
	}

	// the flashloan test tx.
	// ix layout (end_index = 6): 0 cu_limit, 1 cu_price, 2 create-ATA,
	// 3 start_flashloan(6), 4 borrow 1 USDC, 5 deposit 1 USDC, 6 end_flashloan, 7 tip.
	var tipTo solana.Pubkey
	haveTip := false
	for i := 0; i < 12; i++ {
		if accts, err := jito.GetTipAccounts(blockEngine); err == nil && len(accts) > 0 {
			tipTo, haveTip = accts[0], true
			break
		}
		time.Sleep(3 * time.Second)
	}
	if !haveTip {
		fatal("tip accounts (rate limited)")
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
		marginfi.EndFlashloan(mfiAcc, signer, nil), // net-zero -> empty remaining
		arb.TransferIx(signer, tipTo, tipLamports),
	}
	msg, err := solana.CompileV0(signer, ixs, nil, bh)
	if err != nil {
		fatal("compile flashloan: %v", err)
	}
	tx := solana.NewUnsignedVersionedTransaction(msg)
	if err := tx.Sign([]solana.Keypair{authority}); err != nil {
		fatal("sign: %v", err)
	}
	sig := tx.Signatures[0].String()
	raw, err := tx.MarshalBinary()
	if err != nil {
		fatal("marshal: %v", err)
	}
	b64, err := tx.Base64()
	if err != nil {
		fatal("encode: %v", err)
	}
	fmt.Printf("marginfi flashloan tx %dB sig=%s tip=%d\n", len(raw), sig, tipLamports)

	// simulate first
	c := rpcclient.New(endpoint)
	simVal, err := c.SimulateTransaction(b64)
	if err != nil || simVal == nil {
		fatal("simulate")
	}
	var sim struct {
		Err           json.RawMessage `json:"err"`
		Logs          []string        `json:"logs"`
		UnitsConsumed json.RawMessage `json:"unitsConsumed"`
	}
	if err := json.Unmarshal(simVal, &sim); err != nil {
		fatal("simulate")
	}
	if !isNullJSON(sim.Err) {
		fmt.Printf("⛔ simulation FAILED: %s\n", string(sim.Err))
		for _, l := range sim.Logs {
			fmt.Printf("  %s\n", l)
		}
		fmt.Println("\n(fix accounts/args from the logs above — likely vault PDA or deposit arg; see env overrides)")
		os.Exit(1)
	}
	fmt.Printf("✅ simulates clean (%s CU)\n", string(sim.UnitsConsumed))

	// MODE=simbundle: run Jito's simulateBundle — executes the bundle
	// exactly as the block engine would (needs a simulateBundle-capable
	// RPC, e.g. Helius). Read-only, no cost. Rules out a filter/execution
	// problem: if this succeeds but the live bundle never lands, the
	// barrier is the AUCTION (tip/profit), not Jito rejecting the bundle.
	if mode == "simbundle" {
		v, ok := rpcRaw(endpoint, "simulateBundle", []any{map[string]any{"encodedTransactions": []string{b64}}})
		if !ok {
			fmt.Println("simulateBundle: no response (does this RPC support it? use Helius)")
			return
		}
		result, _ := v["result"].(map[string]any)
		if errVal, hasErr := result["error"]; hasErr && errVal != nil {
			fmt.Printf("simulateBundle error: %v\n", errVal)
			return
		}
		if e2, hasErr := v["error"]; hasErr && e2 != nil {
			fmt.Printf("simulateBundle error: %v\n", e2)
			return
		}
		val, _ := result["result"].(map[string]any)
		if val == nil {
			val, _ = v["result"].(map[string]any)["value"].(map[string]any)
		}
		fmt.Printf("simulateBundle summary: %v\n", val["summary"])
		txResults, _ := val["transactionResults"].([]any)
		for i, r := range txResults {
			rm, _ := r.(map[string]any)
			fmt.Printf("  tx[%d] err=%v cu=%v\n", i, rm["err"], rm["unitsConsumed"])
			logs, _ := rm["logs"].([]any)
			start := len(logs) - 3
			if start < 0 {
				start = 0
			}
			for _, l := range logs[start:] {
				fmt.Printf("    %v\n", l)
			}
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
		v, ok := rpcRaw(endpoint, "sendTransaction", []any{b64, map[string]any{
			"encoding": "base64", "skipPreflight": false, "preflightCommitment": "confirmed", "maxRetries": 5,
		}})
		if !ok {
			fatal("send")
		}
		if e, hasErr := v["result"].(map[string]any)["error"]; hasErr && e != nil {
			fmt.Printf("⛔ rpc rejected: %v\n", e)
			os.Exit(1)
		}
		fmt.Printf("⚡ sent via plain RPC: %v\n", v["result"])
	default:
		attempt := 0
		var id string
		for {
			attempt++
			gotID, err := jito.SendBundle(blockEngine, []string{b64})
			if err == nil {
				id = gotID
				break
			}
			if strings.Contains(err.Error(), "429") && attempt < 12 {
				fmt.Printf("  [attempt %d] rate limited, retry in 5s…\n", attempt)
				time.Sleep(5 * time.Second)
				continue
			}
			fatal("send bundle: %v", err)
		}
		fmt.Printf("⚡ submitted Jito bundle %s (attempt %d)\n", id, attempt)
	}

	for i := 1; i <= 18; i++ {
		time.Sleep(5 * time.Second)
		if meta, found := landed(endpoint, sig); found {
			fmt.Printf("\n🎉 LANDED via %s — slot %v fee %v err %v\n", mode, meta["slot"], metaFee(meta), metaErr(meta))
			fmt.Printf("https://solscan.io/tx/%s\n", sig)
			return
		}
		fmt.Printf("[%ds] on_chain=false\n", i*5)
	}
	fmt.Printf("\n⚠️ not landed after 90s via %s. If MODE=rpc landed but jito didn't → flash loans are filtered on Jito generally → pivot to inventory mode.\n", mode)
}

func metaErr(top map[string]any) any {
	meta, _ := top["meta"].(map[string]any)
	if meta == nil {
		return nil
	}
	return meta["err"]
}

func metaFee(top map[string]any) any {
	meta, _ := top["meta"].(map[string]any)
	if meta == nil {
		return nil
	}
	return meta["fee"]
}

func isNullJSON(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}
