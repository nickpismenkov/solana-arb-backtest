// Step-5 gate: simulate the FULL crank+liquidate bundle against REAL
// marginfi accounts. Scans for the nearest-to-threshold borrowers whose
// asset bank has a crankable (shard-0 sponsored) oracle, fetches a fresh
// Hermes update for that feed, and simulateBundles:
//
//	[crank_setup, crank_fire, (start_fl · liquidate · end_fl)]
//
// Expected on a healthy market: crank txs SUCCEED (feed advances) and the
// liquidate hits marginfi's HealthyAccount guard (custom 6068) — proving the
// whole chain composes and the chain judged AT the cranked price. If an
// account is genuinely underwater at the true price, the gate passes
// outright (that's a live opportunity).
//
// Usage: HELIUS_RPC=<url> [TOP=3] [SEIZE_FRAC=0.1] go run ./cmd/liq_crank_probe
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

	"solana-arb-backtest-go/internal/arb"
	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/liquidation"
	"solana-arb-backtest-go/internal/marginfi"
	"solana-arb-backtest-go/internal/pyth"
)

const (
	marginfiProgram     = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA"
	marginfiGroup       = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8"
	defaultLiquidatorMA = "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD"
	defaultAuthority    = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak"
	healthyAccountErr   = 6068.0
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

func b64(data []any) ([]byte, bool) {
	if len(data) == 0 {
		return nil, false
	}
	s, ok := data[0].(string)
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
		result, _ := v["result"].(map[string]any)
		value, _ := result["value"].([]any)
		for j, accAny := range value {
			acc, ok := accAny.(map[string]any)
			if !ok || acc == nil {
				continue
			}
			dataArr, _ := acc["data"].([]any)
			if bytes, ok := b64(dataArr); ok {
				out[chunk[j]] = bytes
			}
		}
	}
	return out
}

func hexs(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hexdigits[c>>4]
		out[i*2+1] = hexdigits[c&0xf]
	}
	return string(out)
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
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
	top := 3
	if v := os.Getenv("TOP"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			top = n
		}
	}
	seizeFrac := 0.1
	if v := os.Getenv("SEIZE_FRAC"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			seizeFrac = f
		}
	}
	hermes := os.Getenv("HERMES")
	if hermes == "" {
		hermes = "https://hermes.pyth.network"
	}
	authorityStr := os.Getenv("AUTHORITY")
	if authorityStr == "" {
		authorityStr = defaultAuthority
	}
	authority := solana.MustPublicKeyFromBase58(authorityStr)
	liquidatorStr := os.Getenv("LIQUIDATOR_MA")
	if liquidatorStr == "" {
		liquidatorStr = defaultLiquidatorMA
	}
	liquidatorMA := solana.MustPublicKeyFromBase58(liquidatorStr)
	usdcBank := solana.MustPublicKeyFromBase58(marginfi.USDCBank)
	tp := solana.MustPublicKeyFromBase58("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA")

	// Scan borrowers + banks (same shape as the executor's full_scan).
	fmt.Fprintln(os.Stderr, "[probe] scanning marginfi accounts…")
	resp, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{marginfiProgram, map[string]any{
			"encoding":  "base64",
			"dataSlice": map[string]any{"offset": 0, "length": 1736},
			"filters": []any{
				map[string]any{"dataSize": liquidation.MASize},
				map[string]any{"memcmp": map[string]any{"offset": 8, "bytes": marginfiGroup}},
			},
		}},
	})
	if !ok {
		fmt.Fprintln(os.Stderr, "scan: request failed")
		os.Exit(1)
	}
	result, _ := resp["result"].([]any)

	type acctEntry struct {
		pk   solana.PublicKey
		acct *liquidation.MarginfiAccount
	}
	var accts []acctEntry
	for _, eAny := range result {
		e, ok := eAny.(map[string]any)
		if !ok {
			continue
		}
		pkStr, _ := e["pubkey"].(string)
		pk, err := solana.PublicKeyFromBase58(pkStr)
		if err != nil {
			continue
		}
		acc, _ := e["account"].(map[string]any)
		dataArr, _ := acc["data"].([]any)
		raw, ok := b64(dataArr)
		if !ok {
			continue
		}
		ma, ok := liquidation.DecodeMarginfiAccount(raw)
		if !ok {
			continue
		}
		hasLiab := false
		for _, b := range ma.Balances {
			if b.LiabilityShares > 0.0 {
				hasLiab = true
				break
			}
		}
		if hasLiab {
			accts = append(accts, acctEntry{pk, ma})
		}
	}

	bankSet := map[solana.PublicKey]struct{}{}
	for _, e := range accts {
		for _, b := range e.acct.Balances {
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
	for _, o := range oracleOf {
		oracleSet[o] = struct{}{}
	}
	oraclePks := make([]solana.PublicKey, 0, len(oracleSet))
	for pk := range oracleSet {
		oraclePks = append(oraclePks, pk)
	}
	oracleRaw := getMultiple(endpoint, oraclePks)
	feedOf := map[solana.PublicKey][32]byte{}
	crankable := map[solana.PublicKey]struct{}{}
	prices := liquidation.PriceMap{}
	for bank, oracle := range oracleOf {
		raw, ok := oracleRaw[oracle]
		if !ok {
			continue
		}
		if usd, ok := liquidation.DecodeOraclePrice(raw); ok {
			prices[bank] = usd
		}
		if fid, _, _, ok := liquidation.DecodePriceUpdateV2(raw); ok {
			feedOf[bank] = fid
			if pyth.SponsoredFeed(0, fid).Equals(oracle) {
				crankable[bank] = struct{}{}
			}
		}
	}
	fmt.Fprintf(os.Stderr, "[probe] %d borrowers, %d banks, %d crankable\n", len(accts), len(banks), len(crankable))

	// Candidates: 1-asset/1-liab-USDC, crankable asset bank, ranked by ratio.
	type candidate struct {
		ratio     float64
		pk        solana.PublicKey
		acct      *liquidation.MarginfiAccount
		assetBank solana.PublicKey
	}
	var cands []candidate
	for _, e := range accts {
		var assets, liabs []liquidation.Balance
		for _, b := range e.acct.Balances {
			if b.AssetShares > 0.0 {
				assets = append(assets, b)
			}
			if b.LiabilityShares > 0.0 {
				liabs = append(liabs, b)
			}
		}
		if len(assets) != 1 || len(liabs) != 1 || !liabs[0].BankPk.Equals(usdcBank) {
			continue
		}
		if _, ok := crankable[assets[0].BankPk]; !ok {
			continue
		}
		r := liquidation.MaintenanceHealth(e.acct, banks, prices)
		if r.Missing > 0 || r.Health.WeightedAssets < 50.0 {
			continue
		}
		cands = append(cands, candidate{r.Health.Ratio(), e.pk, e.acct, assets[0].BankPk})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].ratio > cands[j].ratio })
	if len(cands) > top {
		cands = cands[:top]
	}

	chainVerified := 0
	for _, c := range cands {
		feedID := feedOf[c.assetBank]
		bank := banks[c.assetBank]
		fmt.Printf("\n════ candidate %s  ratio %.4f  asset bank %s…  feed %s…\n",
			c.pk, c.ratio, short(c.assetBank.String()), short(hexs(feedID[:])))

		// Fresh Hermes update → crank txs.
		fidHex := hexs(feedID[:])
		update, err := pyth.FetchHermes(hermes, []string{fidHex})
		if err != nil {
			fmt.Printf("  ✗ hermes: %v\n", err)
			continue
		}
		var mu *pyth.MerkleUpdate
		for i := range update.Updates {
			if id, ok := update.Updates[i].FeedID(); ok && id == feedID {
				mu = &update.Updates[i]
				break
			}
		}
		if mu == nil {
			fmt.Println("  ✗ feed missing from blob")
			continue
		}
		txs, err := pyth.BuildCrankTxs(authority, update.VAA, []pyth.MerkleUpdate{*mu}, 0, 0, solana.Hash{})
		if err != nil {
			fmt.Printf("  ✗ crank build: %v\n", err)
			continue
		}
		setupB64, crankB64, err := txs.ToB64()
		if err != nil {
			fmt.Printf("  ✗ crank encode: %v\n", err)
			continue
		}

		// Gate tx at SEIZE_FRAC of the collateral.
		var nativeTotal float64
		for _, b := range c.acct.Balances {
			if b.AssetShares > 0.0 {
				nativeTotal = b.AssetShares * bank.AssetShareValue
				break
			}
		}
		amount := uint64(nativeTotal * seizeFrac)
		var obs solana.AccountMetaSlice
		for _, b := range c.acct.Balances {
			obs = append(obs, solana.NewAccountMeta(b.BankPk, false, false))
			obs = append(obs, solana.NewAccountMeta(oracleOf[b.BankPk], false, false))
		}
		start := marginfi.StartFlashloan(liquidatorMA, authority, 2)
		liqIx := marginfi.LendingAccountLiquidate(
			c.assetBank, usdcBank, liquidatorMA, authority, c.pk, tp, amount,
			oracleOf[c.assetBank], oracleOf[usdcBank], obs)
		endObs := solana.AccountMetaSlice{
			solana.NewAccountMeta(c.assetBank, false, false),
			solana.NewAccountMeta(oracleOf[c.assetBank], false, false),
			solana.NewAccountMeta(usdcBank, false, false),
			solana.NewAccountMeta(oracleOf[usdcBank], false, false),
		}
		end := marginfi.EndFlashloan(liquidatorMA, authority, endObs)
		gate, err := arb.CompileV0(authority, []solana.Instruction{start, liqIx, end}, nil, solana.Hash{})
		if err != nil {
			fmt.Printf("  ✗ compile gate tx: %v\n", err)
			continue
		}
		gate.Signatures = []solana.Signature{{}}
		gateRaw, err := gate.MarshalBinary()
		if err != nil {
			fmt.Printf("  ✗ marshal gate tx: %v\n", err)
			continue
		}
		gateB64 := base64.StdEncoding.EncodeToString(gateRaw)

		v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "simulateBundle",
			"params": []any{
				map[string]any{"encodedTransactions": []string{setupB64, crankB64, gateB64}},
				map[string]any{
					"skipSigVerify":                true,
					"replaceRecentBlockhash":       true,
					"preExecutionAccountsConfigs":  []any{nil, nil, nil},
					"postExecutionAccountsConfigs": []any{nil, nil, nil},
				},
			}})
		if !ok {
			fmt.Println("  ✗ simulateBundle: request failed")
			continue
		}
		if e, ok := v["error"]; ok && e != nil {
			eb, _ := json.Marshal(e)
			fmt.Printf("  ✗ simulateBundle error: %s\n", string(eb))
			continue
		}
		resultMap, _ := v["result"].(map[string]any)
		val, _ := resultMap["value"].(map[string]any)
		results, _ := val["transactionResults"].([]any)
		okCount := 0
		for _, rAny := range results {
			r, ok := rAny.(map[string]any)
			if ok && r["err"] == nil {
				okCount++
			}
		}
		summaryStr := "succeeded"
		if s, ok := val["summary"].(string); !ok || s != "succeeded" {
			sb, _ := json.Marshal(val["summary"])
			summaryStr = string(sb)
		}
		fmt.Printf("  bundle: %d of 3 txs succeeded  summary=%s\n", okCount, summaryStr)
		for i, rAny := range results {
			r, _ := rAny.(map[string]any)
			errB, _ := json.Marshal(r["err"])
			fmt.Printf("    tx[%d] err=%s cu=%v\n", i, string(errB), r["unitsConsumed"])
		}
		// Crank landed iff the first two txs succeeded; the gate's verdict is
		// then the chain's judgment AT the cranked price.
		crankOK := len(results) >= 2
		if crankOK {
			for _, rAny := range results[:2] {
				r, _ := rAny.(map[string]any)
				if r["err"] != nil {
					crankOK = false
					break
				}
			}
		}
		var gateCode float64
		gateCodeOK := false
		if len(results) > 2 {
			r, _ := results[2].(map[string]any)
			if errV, ok := r["err"].(map[string]any); ok {
				if ie, ok := errV["InstructionError"].([]any); ok && len(ie) > 1 {
					if custom, ok := ie[1].(map[string]any); ok {
						if code, ok := custom["Custom"].(float64); ok {
							gateCode = code
							gateCodeOK = true
						}
					}
				}
			}
		}
		if crankOK {
			if okCount == 3 {
				fmt.Printf("  ★★ LIVE OPPORTUNITY — liquidate ACCEPTED at the cranked price (would seize %d)\n", amount)
				chainVerified++
			} else if gateCodeOK && gateCode == healthyAccountErr {
				fmt.Println("  ★ CHAIN-VERIFIED — crank landed, marginfi judged at the fresh price: HealthyAccount (6068), account not (yet) underwater")
				chainVerified++
			} else {
				var codeDisplay any
				if gateCodeOK {
					codeDisplay = gateCode
				}
				fmt.Printf("  ⚠ crank landed but liquidate failed with custom %v (emode/size guard?)\n", codeDisplay)
			}
		} else {
			fmt.Println("  ✗ crank txs failed in bundle — inspect logs above")
		}
	}
	fmt.Printf("\n%d of %d candidates chain-verified through the crank bundle\n", chainVerified, len(cands))
}
