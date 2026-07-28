// marginfi liquidation SIMULATION probe — assembles the flashloan-wrapped
// liquidate against a REAL liquidatable account and simulates it on mainnet
// (sigVerify=false, replaceRecentBlockhash). Proves the instruction wiring
// executes: we want to see the LendingAccountLiquidate handler run (state
// change or a meaningful marginfi error), NOT a deserialize/account error.
//
// Picks the top liquidatable borrower with exactly one collateral + one debt
// bank (simplest case). Tx = [start_flashloan, liquidate(2% of collateral),
// end_flashloan]; end_flashloan re-checks health over both liquidator balances.
//
// Usage: HELIUS_RPC=<url> [LIQUIDATOR_MA=<acct>] [AUTHORITY=<pk>] go run ./cmd/liq_marginfi_sim
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
	"solana-arb-backtest-go/internal/liquidation"
	"solana-arb-backtest-go/internal/marginfi"
)

const (
	marginfiProgram     = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA"
	marginfiGroup       = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8"
	tokenProgram        = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	defaultLiquidatorMA = "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD"
	defaultAuthority    = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak"
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

func pow10(n float64) float64 {
	r := 1.0
	for i := 0.0; i < n; i++ {
		r *= 10
	}
	return r
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
	liquidatorMAStr := os.Getenv("LIQUIDATOR_MA")
	if liquidatorMAStr == "" {
		liquidatorMAStr = defaultLiquidatorMA
	}
	liquidatorMA := solana.MustPublicKeyFromBase58(liquidatorMAStr)
	authorityStr := os.Getenv("AUTHORITY")
	if authorityStr == "" {
		authorityStr = defaultAuthority
	}
	authority := solana.MustPublicKeyFromBase58(authorityStr)
	tp := solana.MustPublicKeyFromBase58(tokenProgram)

	// 1) Scan group → borrowers.
	fmt.Fprintln(os.Stderr, "[sim] scanning marginfi group …")
	resp, _ := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{marginfiProgram, map[string]any{"encoding": "base64", "dataSlice": map[string]any{"offset": 0, "length": 1736},
			"filters": []any{
				map[string]any{"dataSize": liquidation.MASize},
				map[string]any{"memcmp": map[string]any{"offset": 8, "bytes": marginfiGroup}},
			}}}})
	entries := asArray(asMap(resp)["result"])

	type acctEntry struct {
		pk solana.PublicKey
		a  *liquidation.MarginfiAccount
	}
	var accts []acctEntry
	for _, ev := range entries {
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
		if hasLiab {
			accts = append(accts, acctEntry{pk, a})
		}
	}
	fmt.Fprintf(os.Stderr, "[sim] %d borrowers\n", len(accts))

	// 2) Banks + oracle prices.
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
	for pk, raw := range bankRaw {
		if bk, ok := liquidation.DecodeBank(raw); ok {
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
	oprice := map[solana.PublicKey]float64{}
	for pk, raw := range oracleRaw {
		if usd, ok := liquidation.DecodeOraclePrice(raw); ok {
			oprice[pk] = usd
		}
	}
	prices := liquidation.PriceMap{}
	for bk, oc := range oracleOf {
		if p, ok := oprice[oc]; ok {
			prices[bk] = p
		}
	}

	// 3) Pick top liquidatable with exactly 1 collateral + 1 debt bank, both priced.
	type best struct {
		liquidatee solana.PublicKey
		acct       *liquidation.MarginfiAccount
		assetBank  solana.PublicKey
		liabBank   solana.PublicKey
		collat     float64
	}
	var bestPick *best
	for _, ae := range accts {
		pk, a := ae.pk, ae.a
		r := liquidation.MaintenanceHealth(a, banks, prices)
		if r.Missing > 0 || !r.Health.Liquidatable() {
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
		if len(assets) != 1 || len(liabs) != 1 {
			continue
		}
		if r.Health.WeightedAssets < 50.0 {
			continue
		}
		if bestPick == nil || r.Health.WeightedAssets > bestPick.collat {
			bestPick = &best{pk, a, assets[0].BankPk, liabs[0].BankPk, r.Health.WeightedAssets}
		}
	}
	if bestPick == nil {
		fmt.Fprintln(os.Stderr, "[sim] no single-collateral/single-debt liquidatable account found")
		return
	}
	liquidatee, acct, assetBank, liabBank, collat := bestPick.liquidatee, bestPick.acct, bestPick.assetBank, bestPick.liabBank, bestPick.collat
	assetBk := banks[assetBank]
	assetOracle := oracleOf[assetBank]
	liabOracle := oracleOf[liabBank]
	// asset_amount = 2% of the liquidatee's collateral native units.
	var assetBal liquidation.Balance
	for _, b := range acct.Balances {
		if b.BankPk.Equals(assetBank) {
			assetBal = b
			break
		}
	}
	native := assetBal.AssetShares * assetBk.AssetShareValue
	assetAmount := uint64(native * 0.02)
	// Diagnostic: reconcile my weights vs marginfi's on-chain calc.
	px := prices[assetBank]
	dec := pow10(float64(assetBk.MintDecimals))
	rawVal := native / dec * px
	fmt.Fprintf(os.Stderr, "[sim] asset_bank %s decimals=%d price=$%.4f\n", assetBank, assetBk.MintDecimals, px)
	fmt.Fprintf(os.Stderr, "[sim] asset_weight_init=%.4f asset_weight_maint=%.4f\n", assetBk.AssetWeightInit, assetBk.AssetWeightMaint)
	fmt.Fprintf(os.Stderr, "[sim] raw collateral value=$%.0f  × init=%.0f  × maint=%.0f  (marginfi said assets=$39558)\n",
		rawVal, rawVal*assetBk.AssetWeightInit, rawVal*assetBk.AssetWeightMaint)
	fmt.Fprintf(os.Stderr, "[sim] liquidatee %s collateral=$%.0f\n", liquidatee.String()[:8], collat)
	fmt.Fprintf(os.Stderr, "[sim] asset_bank %s… liab_bank %s… asset_amount=%d (2%% of %.0f native)\n",
		assetBank.String()[:8], liabBank.String()[:8], assetAmount, native)

	// 4) Build flashloan-wrapped [start_fl, liquidate, end_fl].
	// liquidatee obs: for each active balance [bank, oracle] in slot order.
	var liquidateeObs solana.AccountMetaSlice
	for _, b := range acct.Balances {
		liquidateeObs = append(liquidateeObs, solana.NewAccountMeta(b.BankPk, false, false))
		liquidateeObs = append(liquidateeObs, solana.NewAccountMeta(oracleOf[b.BankPk], false, false))
	}
	endIndex := uint64(2) // ixs: 0 start_fl, 1 liquidate, 2 end_fl
	start := marginfi.StartFlashloan(liquidatorMA, authority, endIndex)
	liqIx := marginfi.LendingAccountLiquidate(assetBank, liabBank, liquidatorMA, authority, liquidatee, tp,
		assetAmount, assetOracle, liabOracle, liquidateeObs)
	// end_flashloan obs = liquidator's post-liquidation balances: seized asset + new liab.
	endObs := solana.AccountMetaSlice{
		solana.NewAccountMeta(assetBank, false, false), solana.NewAccountMeta(assetOracle, false, false),
		solana.NewAccountMeta(liabBank, false, false), solana.NewAccountMeta(liabOracle, false, false),
	}
	end := marginfi.EndFlashloan(liquidatorMA, authority, endObs)

	// 5) Assemble a v0 tx + simulate (sigVerify=false, replaceRecentBlockhash).
	tx, err := solana.NewTransaction([]solana.Instruction{start, liqIx, end}, solana.Hash{}, solana.TransactionPayer(authority))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[sim] compile: %v\n", err)
		return
	}
	tx.Signatures = []solana.Signature{{}}
	raw, err := tx.MarshalBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[sim] serialize: %v\n", err)
		return
	}
	b64tx := base64.StdEncoding.EncodeToString(raw)

	fmt.Fprintln(os.Stderr, "[sim] simulating …")
	sim, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "simulateTransaction",
		"params": []any{b64tx, map[string]any{"sigVerify": false, "replaceRecentBlockhash": true, "commitment": "processed", "encoding": "base64"}}})
	if !ok {
		fmt.Fprintln(os.Stderr, "[sim] no response")
		return
	}
	res := asMap(asMap(sim["result"])["value"])
	fmt.Println("\n──── simulation result ────")
	var errJSON []byte
	if res != nil {
		errJSON, _ = json.Marshal(res["err"])
	}
	fmt.Printf("err: %s\n", string(errJSON))
	if res != nil {
		if logs, ok := res["logMessages"].([]any); ok {
			for _, l := range logs {
				fmt.Printf("  %s\n", asStr(l))
			}
			return
		}
	}
	b, _ := json.Marshal(sim)
	fmt.Printf("  (no logs — %s)\n", string(b))
}
