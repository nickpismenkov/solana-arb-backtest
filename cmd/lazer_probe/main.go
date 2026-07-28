// Live verification of Pyth Lazer pre-positioning (run on the box, where
// PYTH_LAZER_TOKEN lives). Streams the volatile majors, scans the marginfi
// watch-set once, then each interval recomputes health with Lazer prices
// blended over the on-chain baseline and prints the nearest-to-liquidation
// accounts + the Lazer-vs-on-chain price delta per major. Confirms (a) the
// feed is live, (b) the mint→feed mapping resolves banks, and (c) Lazer leads
// the on-chain oracle (nonzero delta = the pre-positioning edge).
//
// Usage: HELIUS_RPC=<url> PYTH_LAZER_TOKEN=<token> [INTERVAL_MS=2000]
//
//	go run ./cmd/lazer_probe
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/lazer"
	"solana-arb-backtest-go/internal/liquidation"
)

const (
	marginfiProgram = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA"
	marginfiGroup   = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8"
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

func b64(d any) ([]byte, bool) {
	arr := asArray(d)
	if len(arr) == 0 {
		return nil, false
	}
	s, ok := arr[0].(string)
	if !ok {
		return nil, false
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	return b, true
}

func getMultiple(endpoint string, keys []solana.PublicKey) map[solana.PublicKey][]byte {
	out := make(map[solana.PublicKey][]byte)
	for i := 0; i < len(keys); i += 100 {
		end := i + 100
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[i:end]
		strs := make([]string, len(chunk))
		for j, k := range chunk {
			strs[j] = k.String()
		}
		v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getMultipleAccounts",
			"params": []any{strs, map[string]any{"encoding": "base64"}}})
		if !ok {
			continue
		}
		result := asMap(v["result"])
		for j, accV := range asArray(result["value"]) {
			acc := asMap(accV)
			if acc == nil {
				continue
			}
			if b, ok := b64(acc["data"]); ok {
				out[chunk[j]] = b
			}
		}
	}
	return out
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
	token := os.Getenv("PYTH_LAZER_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "PYTH_LAZER_TOKEN (lives on the box)")
		os.Exit(1)
	}
	intervalMs := uint64(2000)
	if v := os.Getenv("INTERVAL_MS"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			intervalMs = n
		}
	}
	interval := time.Duration(intervalMs) * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	table := lazer.NewPriceTable()
	lazer.SpawnLazerThread(ctx, token, lazer.ArmFeedIDs(), table, nil)
	fmt.Fprintln(os.Stderr, "[lazer] subscribed to majors; scanning marginfi group …")

	// Scan borrowers + banks + on-chain oracle prices once (baseline).
	resp, _ := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{marginfiProgram, map[string]any{
			"encoding":  "base64",
			"dataSlice": map[string]any{"offset": 0, "length": 1736},
			"filters": []any{
				map[string]any{"dataSize": liquidation.MASize},
				map[string]any{"memcmp": map[string]any{"offset": 8, "bytes": marginfiGroup}},
			},
		}}})
	var entries []any
	if resp != nil {
		entries = asArray(resp["result"])
	}

	type acctEntry struct {
		pk  solana.PublicKey
		acc *liquidation.MarginfiAccount
	}
	var accts []acctEntry
	for _, ev := range entries {
		e := asMap(ev)
		pkStr := asStr(e["pubkey"])
		pk, err := solana.PublicKeyFromBase58(pkStr)
		if err != nil {
			continue
		}
		account := asMap(e["account"])
		data, ok := b64(account["data"])
		if !ok {
			continue
		}
		acc, ok := liquidation.DecodeMarginfiAccount(data)
		if !ok {
			continue
		}
		hasLiab := false
		for _, b := range acc.Balances {
			if b.LiabilityShares > 0.0 {
				hasLiab = true
				break
			}
		}
		if hasLiab {
			accts = append(accts, acctEntry{pk, acc})
		}
	}

	bankSet := map[solana.PublicKey]struct{}{}
	for _, e := range accts {
		for _, b := range e.acc.Balances {
			bankSet[b.BankPk] = struct{}{}
		}
	}
	bankPks := make([]solana.PublicKey, 0, len(bankSet))
	for pk := range bankSet {
		bankPks = append(bankPks, pk)
	}

	banks := liquidation.BankMap{}
	oracleOf := map[solana.PublicKey]solana.PublicKey{}
	for pk, raw := range getMultiple(endpoint, bankPks) {
		if bk, ok := liquidation.DecodeBank(raw); ok {
			oracleOf[pk] = bk.OracleKey
			banks[pk] = bk
		}
	}
	oracleSet := map[solana.PublicKey]struct{}{}
	for _, oc := range oracleOf {
		oracleSet[oc] = struct{}{}
	}
	oraclePks := make([]solana.PublicKey, 0, len(oracleSet))
	for pk := range oracleSet {
		oraclePks = append(oraclePks, pk)
	}

	onChain := liquidation.PriceMap{}
	for pk, raw := range getMultiple(endpoint, oraclePks) {
		if usd, ok := liquidation.DecodeOraclePrice(raw); ok {
			for bk, oc := range oracleOf {
				if oc == pk {
					onChain[bk] = usd
				}
			}
		}
	}
	mintMap := lazer.MintFeedMap()
	fmt.Fprintf(os.Stderr, "[lazer] %d borrowers, %d banks, %d on-chain-priced\n", len(accts), len(banks), len(onChain))

	for {
		time.Sleep(interval)
		if _, ok := table.Get(lazer.LazerSOL); !ok {
			fmt.Fprintln(os.Stderr, "[lazer] waiting for first tick …")
			continue
		}
		blended, led := lazer.Blend(banks, onChain, table, mintMap)

		// Lazer-vs-on-chain delta on SOL (the leading-edge proof).
		solDelta := "no SOL bank"
		for pk, b := range banks {
			if mintMap[b.Mint] == lazer.LazerSOL {
				oc, ocOK := onChain[pk]
				lz, lzOK := blended[pk]
				if ocOK && lzOK {
					solDelta = fmt.Sprintf("SOL on-chain $%.2f → Lazer $%.2f (Δ%+.2f)", oc, lz, lz-oc)
				}
				break
			}
		}

		// Nearest-to-liquidation by Lazer-blended health.
		type ranked struct {
			pk     solana.PublicKey
			ratio  float64
			assets float64
		}
		var rankedList []ranked
		for _, e := range accts {
			r := liquidation.MaintenanceHealth(e.acc, banks, blended)
			if r.Missing == 0 && r.Health.WeightedAssets >= 100.0 {
				rankedList = append(rankedList, ranked{e.pk, r.Health.Ratio(), r.Health.WeightedAssets})
			}
		}
		sort.Slice(rankedList, func(i, j int) bool { return rankedList[i].ratio > rankedList[j].ratio })

		fmt.Printf("\n[%s] %s  (%d banks Lazer-led)\n", lazer.Status(table), solDelta, led)
		for i, e := range rankedList {
			if i >= 5 {
				break
			}
			marker := ""
			if e.ratio >= 1.0 {
				marker = "  ← LIQUIDATABLE (Lazer)"
			}
			pkStr := e.pk.String()
			n := 8
			if len(pkStr) < n {
				n = len(pkStr)
			}
			fmt.Printf("  %s  ratio %.4f  collateral $%.0f%s\n", pkStr[:n], e.ratio, e.assets, marker)
		}
	}
}
