// Census the accounts our emode-aware maintenance_health flags as liquidatable
// (ratio >= 1.0, on-chain prices), categorized by shape — to explain why the
// live engine still reports a large "liquidatable now" after the emode fix:
// are they FIREABLE v1 accounts (1 collateral / 1 USDC debt / crankable), or
// non-v1 accounts the fire path skips anyway (multi-position, non-USDC debt)?
//
// Usage: HELIUS_RPC=<url> [SAMPLE=5] go run ./cmd/mfi_liq_census
package main

import (
	"bytes"
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
	"solana-arb-backtest-go/internal/liquidation"
	"solana-arb-backtest-go/internal/marginfi"
	"solana-arb-backtest-go/internal/pyth"
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
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	return raw, true
}

func getMultiple(endpoint string, keys []solana.PublicKey) map[solana.PublicKey][]byte {
	out := map[solana.PublicKey][]byte{}
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
		arr := asArray(asMap(v["result"])["value"])
		for j, accv := range arr {
			acc := asMap(accv)
			if acc == nil {
				continue
			}
			if raw, ok := b64(acc["data"]); ok {
				out[chunk[j]] = raw
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
	sample := 5
	if v := os.Getenv("SAMPLE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			sample = n
		}
	}
	usdcBank := solana.MustPublicKeyFromBase58(marginfi.USDCBank)

	resp, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{marginfiProgram, map[string]any{"encoding": "base64", "dataSlice": map[string]any{"offset": 0, "length": 1736},
			"filters": []any{
				map[string]any{"dataSize": liquidation.MASize},
				map[string]any{"memcmp": map[string]any{"offset": 8, "bytes": marginfiGroup}},
			}}}})
	if !ok {
		fmt.Fprintln(os.Stderr, "scan failed")
		os.Exit(1)
	}

	type acctEntry struct {
		pk solana.PublicKey
		a  *liquidation.MarginfiAccount
	}
	var accts []acctEntry
	for _, ev := range asArray(asMap(resp["result"])) {
		e := asMap(ev)
		pk, err := solana.PublicKeyFromBase58(asStr(e["pubkey"]))
		if err != nil {
			continue
		}
		raw, ok := b64(asMap(e["account"])["data"])
		if !ok {
			continue
		}
		a, ok := liquidation.DecodeMarginfiAccount(raw)
		if !ok {
			continue
		}
		hasLiab := false
		for _, b := range a.Balances {
			if b.LiabilityShares > 0.0 {
				hasLiab = true
				break
			}
		}
		if !hasLiab {
			continue
		}
		accts = append(accts, acctEntry{pk, a})
	}

	bankSet := map[solana.PublicKey]bool{}
	for _, ae := range accts {
		for _, b := range ae.a.Balances {
			bankSet[b.BankPk] = true
		}
	}
	var bankPks []solana.PublicKey
	for pk := range bankSet {
		bankPks = append(bankPks, pk)
	}
	bankRaw := getMultiple(endpoint, bankPks)
	banks := liquidation.BankMap{}
	oracleOf := map[solana.PublicKey]solana.PublicKey{}
	for pk, r := range bankRaw {
		if bk, ok := liquidation.DecodeBank(r); ok {
			oracleOf[pk] = bk.OracleKey
			banks[pk] = bk
		}
	}
	oracleSet := map[solana.PublicKey]bool{}
	for _, oc := range oracleOf {
		oracleSet[oc] = true
	}
	var oraclePks []solana.PublicKey
	for pk := range oracleSet {
		oraclePks = append(oraclePks, pk)
	}
	oracleRaw := getMultiple(endpoint, oraclePks)
	priceByOracle := map[solana.PublicKey]float64{}
	for pk, r := range oracleRaw {
		if p, ok := liquidation.DecodeOraclePrice(r); ok {
			priceByOracle[pk] = p
		}
	}
	crankable := map[solana.PublicKey]bool{}
	for bank, oracle := range oracleOf {
		if r, ok := oracleRaw[oracle]; ok {
			if fid, _, _, ok := liquidation.DecodePriceUpdateV2(r); ok {
				if pyth.SponsoredFeed(0, fid).Equals(oracle) {
					crankable[bank] = true
				}
			}
		}
	}
	prices := liquidation.PriceMap{}
	for bk, oc := range oracleOf {
		if p, ok := priceByOracle[oc]; ok {
			prices[bk] = p
		}
	}
	fmt.Printf("%d borrowers, %d banks priced, %d crankable\n", len(accts), len(prices), len(crankable))

	// MIN_COLLATERAL mirrors the executor's filter (default 100) — excludes the
	// tiny/mis-priced (weighted-assets~=0, absurd-ratio) accounts.
	minCollateral := 100.0
	if v := os.Getenv("MIN_COLLATERAL_USD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			minCollateral = f
		}
	}

	// Categorize the liquidatable (emode-aware, on-chain price) set by shape,
	// AFTER the min-collateral filter — i.e. what the engine would actually see.
	var v1USDC, v1Crank, v1NonUSDC, multi, missing, tiny uint32
	type example struct {
		ratio float64
		cat   string
		pk    string
	}
	var examples []example
	for _, ae := range accts {
		pk, a := ae.pk, ae.a
		r := liquidation.MaintenanceHealth(a, banks, prices)
		if r.Missing > 0 {
			if r.Health.Liquidatable() {
				missing++
			}
			continue
		}
		if !r.Health.Liquidatable() {
			continue
		}
		if r.Health.WeightedAssets < minCollateral {
			tiny++
			continue
		}
		var assets, liabs []liquidation.Balance
		for _, b := range a.Balances {
			if b.AssetShares > 0.0 {
				assets = append(assets, b)
			}
			if b.LiabilityShares > 0.0 {
				liabs = append(liabs, b)
			}
		}
		var cat string
		if len(assets) == 1 && len(liabs) == 1 {
			if liabs[0].BankPk.Equals(usdcBank) {
				v1USDC++
				if crankable[assets[0].BankPk] {
					v1Crank++
				}
				cat = "v1_USDC_debt(FIREABLE)"
			} else {
				v1NonUSDC++
				cat = "v1_nonUSDC_debt(skip)"
			}
		} else {
			multi++
			cat = "multi(skip)"
		}
		if len(examples) < sample*4 {
			examples = append(examples, example{r.Health.Ratio(), cat, pk.String()})
		}
	}
	fmt.Printf("\nLIQUIDATABLE (emode-aware, on-chain price, collateral >= $%g):\n", minCollateral)
	fmt.Printf("  v1 USDC-debt  (FIREABLE): %d   (of which crankable: %d)\n", v1USDC, v1Crank)
	fmt.Printf("  v1 non-USDC-debt (skip):  %d\n", v1NonUSDC)
	fmt.Printf("  multi-position   (skip):  %d\n", multi)
	fmt.Printf("  below min-collateral (excluded): %d\n", tiny)
	fmt.Printf("  incomplete/missing price: %d\n", missing)
	fmt.Println("\nexamples (ratio, category, account):")
	sort.Slice(examples, func(i, j int) bool { return examples[i].ratio > examples[j].ratio })
	n := sample * 2
	if n > len(examples) {
		n = len(examples)
	}
	for _, e := range examples[:n] {
		fmt.Printf("  %.3f  %-14s  %s\n", e.ratio, e.cat, e.pk)
	}
}
