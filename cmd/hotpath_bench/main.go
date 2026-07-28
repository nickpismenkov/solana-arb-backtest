// Measure the real hot-path compute reaction (decode → predict → optimal_arb
// → build_arb_tx → sign → serialize+base64) with live pool data + the real
// keypair, and separately time a COLD vs WARM Jito connection to show the
// keep-alive Agent effect. Numbers, not estimates.
//
// Usage: RPC_ENDPOINT=<url> ALT_ADDRESS=<alt> KEYPAIR_PATH=<path> go run ./cmd/hotpath_bench
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

	"solana-arb-backtest-go/internal/arb"
	"solana-arb-backtest-go/internal/clmm"
	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/jito"
	"solana-arb-backtest-go/internal/pools"
)

func rpc(endpoint string, body map[string]any) map[string]any {
	b, _ := json.Marshal(body)
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(b))
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	var v map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		panic(err)
	}
	return v
}

func accountData(endpoint, addr string) []byte {
	v := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{addr, map[string]string{"encoding": "base64"}}})
	result := v["result"].(map[string]any)
	value := result["value"].(map[string]any)
	dataArr := value["data"].([]any)
	data, err := base64.StdEncoding.DecodeString(dataArr[0].(string))
	if err != nil {
		panic(err)
	}
	return data
}

func mustEnv(key string) string {
	v, ok := os.LookupEnv(key)
	if !ok {
		fmt.Fprintf(os.Stderr, "%s must be set\n", key)
		os.Exit(1)
	}
	return v
}

func main() {
	envfile.LoadDotEnv()
	endpoint := mustEnv("RPC_ENDPOINT")
	altAddr := mustEnv("ALT_ADDRESS")
	kpPath := mustEnv("KEYPAIR_PATH")
	cfg := pools.Pair()
	base := clmm.WSOL()

	kp, err := solana.PrivateKeyFromSolanaKeygenFile(kpPath)
	if err != nil {
		panic(err)
	}
	signer := kp.PublicKey()
	alt := arb.LoadAlt(altAddr, accountData(endpoint, altAddr))
	pd := &arb.PoolData{Orca: accountData(endpoint, cfg.OrcaPool), Ray: accountData(endpoint, cfg.RayPool)}
	bhResp := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getLatestBlockhash",
		"params": []any{map[string]any{"commitment": "confirmed"}}})
	bhStr := bhResp["result"].(map[string]any)["value"].(map[string]any)["blockhash"].(string)
	bh, err := solana.HashFromBase58(bhStr)
	if err != nil {
		panic(err)
	}

	orcaMintA := arb.PkAt(pd.Orca, 101)
	oda, odb := cfg.QuoteDec, cfg.BaseDec
	if orcaMintA.Equals(base) {
		oda, odb = cfg.BaseDec, cfg.QuoteDec
	}

	// Warm up.
	_, _ = arb.BuildArbTx(pd, signer, alt, 500_000_000, true, nil, 0, 10_000, bh, 0)

	const n = 2000
	t0 := time.Now()
	var sink uint64
	for i := 0; i < n; i++ {
		// decode + predict
		orca0, ok := clmm.FromOrca(pd.Orca, oda, odb, cfg.OrcaFeeBps)
		if !ok {
			panic("orca decode")
		}
		ray0, ok := clmm.FromRay(pd.Ray, cfg.RayFeeBps)
		if !ok {
			panic("ray decode")
		}
		orcaP := orca0.AfterBaseSwap(base, i%2 == 0, 3_000_000_000.0)
		sizeRaw, _, buyOrca := clmm.OptimalArb(orcaP, ray0, base, 500.0*1e6)
		// build + sign + serialize
		borrow := uint64(sizeRaw)
		if borrow < 1_000_000 {
			borrow = 1_000_000
		}
		tx, err := arb.BuildArbTx(pd, signer, alt, borrow, buyOrca, nil, 0, 10_000, bh, 0)
		if err != nil {
			panic(err)
		}
		if _, err := tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
			if key.Equals(signer) {
				return &kp
			}
			return nil
		}); err != nil {
			panic(err)
		}
		raw, err := tx.MarshalBinary()
		if err != nil {
			panic(err)
		}
		b64 := base64.StdEncoding.EncodeToString(raw)
		sink += uint64(len(b64))
	}
	per := time.Since(t0).Seconds() * 1e6 / n
	fmt.Printf("compute reaction (decode+predict+optimal_arb+build+sign+serialize): %.1f µs/iter  (n=%d, sink=%d)\n", per, n, sink)

	// Cold vs warm Jito connection.
	be := jito.DefaultBlockEngine()
	c0 := time.Now()
	_, _ = jito.GetTipAccounts(be)
	cold := time.Since(c0).Seconds() * 1e3
	w0 := time.Now()
	_, _ = jito.GetTipAccounts(be)
	warm := time.Since(w0).Seconds() * 1e3
	w2 := time.Now()
	_, _ = jito.GetTipAccounts(be)
	warm2 := time.Since(w2).Seconds() * 1e3
	fmt.Printf("Jito round trip: cold(handshake)=%.1f ms  warm=%.1f ms  warm2=%.1f ms\n", cold, warm, warm2)
	fmt.Println("(note: from laptop, not the co-located box — box RTT to Amsterdam is ~0.8ms)")
}
