// Wiring proof for MULTI-POSITION liquidation.
//
// The live fire path skips any account with >1 collateral or >1 debt
// (liq_executor is_v1_fireable / try_arm's `assets.len()!=1 || liabs.len()!=1`).
// The census showed that's where ~99% of at-risk collateral sits ($2.6M / $941k /
// $791k positions). marginfi's `lending_account_liquidate` is single-leg (one
// asset_bank, one liab_bank) but carries the FULL balance list in the
// observation accounts — so liquidating ONE leg of a multi-position account is
// supported by the program. This probe proves the single-leg tx COMPOSES against
// a real multi-position account: build [start_fl, liquidate(one leg), end_fl] and
// simulate. Outcome classification:
//
//	err=null            → the leg is fireable right now (real opportunity)
//	HealthyAccount 6068 → wiring OK; account healthy at this leg/size (expected calm)
//	other Custom code   → an account-specific gate (stale oracle, etc.), still wiring-OK
//	error at a DIFFERENT ix index → a WIRING BUG (what this probe exists to catch)
//
// Usage: HELIUS_RPC=<url> [LIQUIDATOR_MA=…] [AUTHORITY=…] [TOPN=5] go run ./cmd/mfi_multipos_probe
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
)

const (
	marginfiProgram      = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA"
	marginfiGroup        = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8"
	defaultLiquidatorMA  = "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD"
	defaultAuthority     = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak"
	usdcMint             = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	usdtMint             = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"
	solMint              = "So11111111111111111111111111111111111111112"
	tokenProgramFallback = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
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

func mintOwner(endpoint string, mint solana.PublicKey) (solana.PublicKey, bool) {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{mint.String(), map[string]any{"encoding": "jsonParsed"}}})
	if !ok {
		return solana.PublicKey{}, false
	}
	owner := asStr(asMap(asMap(v["result"])["value"])["owner"])
	if owner == "" {
		return solana.PublicKey{}, false
	}
	pk, err := solana.PublicKeyFromBase58(owner)
	if err != nil {
		return solana.PublicKey{}, false
	}
	return pk, true
}

func isDebtMint(m solana.PublicKey) bool {
	s := m.String()
	return s == usdcMint || s == usdtMint || s == solMint
}

// gateTxB64 builds [start_fl, liquidate(asset_bank, liab_bank, amount), end_fl] as base64.
func gateTxB64(authority, liquidatorMA, tp, liquidatee solana.PublicKey, acct *liquidation.MarginfiAccount,
	assetBank, liabBank solana.PublicKey, assetAmount uint64, oracleOf map[solana.PublicKey]solana.PublicKey) (string, bool) {
	var obs solana.AccountMetaSlice
	for _, b := range acct.Balances {
		oc, ok := oracleOf[b.BankPk]
		if !ok {
			return "", false
		}
		obs = append(obs, solana.NewAccountMeta(b.BankPk, false, false))
		obs = append(obs, solana.NewAccountMeta(oc, false, false))
	}
	start := marginfi.StartFlashloan(liquidatorMA, authority, 2)
	assetOracle, ok := oracleOf[assetBank]
	if !ok {
		return "", false
	}
	liabOracle, ok := oracleOf[liabBank]
	if !ok {
		return "", false
	}
	liqIx := marginfi.LendingAccountLiquidate(assetBank, liabBank, liquidatorMA, authority, liquidatee, tp,
		assetAmount, assetOracle, liabOracle, obs)
	endObs := solana.AccountMetaSlice{
		solana.NewAccountMeta(assetBank, false, false), solana.NewAccountMeta(assetOracle, false, false),
		solana.NewAccountMeta(liabBank, false, false), solana.NewAccountMeta(liabOracle, false, false),
	}
	end := marginfi.EndFlashloan(liquidatorMA, authority, endObs)

	tx, err := solana.NewTransaction([]solana.Instruction{start, liqIx, end}, solana.Hash{}, solana.TransactionPayer(authority))
	if err != nil {
		return "", false
	}
	tx.Signatures = []solana.Signature{{}}
	raw, err := tx.MarshalBinary()
	if err != nil {
		return "", false
	}
	return base64.StdEncoding.EncodeToString(raw), true
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
	topn := 5
	if v := os.Getenv("TOPN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			topn = n
		}
	}

	fmt.Fprintln(os.Stderr, "[mp] scanning marginfi group …")
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
	for _, ev := range asArray(resp["result"]) {
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
		na, nl := 0, 0
		for _, b := range a.Balances {
			if b.AssetShares > 0.0 {
				na++
			}
			if b.LiabilityShares > 0.0 {
				nl++
			}
		}
		if na+nl > 2 { // multi-position: more than one collateral OR more than one debt
			accts = append(accts, acctEntry{pk, a})
		}
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
	slotResp, _ := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getSlot", "params": []any{map[string]any{"commitment": "confirmed"}}})
	slot := uint64(0)
	if s, ok := slotResp["result"].(float64); ok {
		slot = uint64(s)
	}
	oracleRaw := getMultiple(endpoint, oraclePks)
	prices := liquidation.PriceMap{}
	for pk, raw := range oracleRaw {
		if usd, ok := liquidation.DecodeOraclePriceFresh(raw, slot, liquidation.DefaultMaxSBStaleSlots); ok {
			for bk, oc := range oracleOf {
				if oc.Equals(pk) {
					prices[bk] = usd
				}
			}
		}
	}

	// Rank multi-position accounts by collateral USD (fresh-priced, complete health).
	type ranked struct {
		pk    solana.PublicKey
		a     *liquidation.MarginfiAccount
		coll  float64
		ratio float64
	}
	var rankedList []ranked
	for _, ae := range accts {
		pk, a := ae.pk, ae.a
		h := liquidation.MaintenanceHealth(a, banks, prices)
		if h.Missing > 0 || h.Health.WeightedAssets <= 0.0 {
			continue
		}
		var coll float64
		for _, b := range a.Balances {
			if b.AssetShares <= 0.0 {
				continue
			}
			bk, ok := banks[b.BankPk]
			if !ok {
				continue
			}
			px, ok := prices[b.BankPk]
			if !ok {
				continue
			}
			coll += b.AssetShares * bk.AssetShareValue / pow10(float64(bk.MintDecimals)) * px
		}
		rankedList = append(rankedList, ranked{pk, a, coll, h.Health.Ratio()})
	}
	sort.Slice(rankedList, func(i, j int) bool { return rankedList[i].coll > rankedList[j].coll })
	fmt.Fprintf(os.Stderr, "[mp] %d multi-position accounts (complete health); probing top %d\n\n", len(rankedList), topn)

	logsMode := os.Getenv("LOGS") == "1"

	n := topn
	if n > len(rankedList) {
		n = len(rankedList)
	}
	for _, r := range rankedList[:n] {
		pk, a, coll, ratio := r.pk, r.a, r.coll, r.ratio
		// Leg picker: choose the (collateral, debt) pair maximizing seized-collateral
		// USD, restricted to a wired debt mint (USDC/USDT/wSOL).
		var assets, liabs []liquidation.Balance
		for _, b := range a.Balances {
			if b.AssetShares > 0.0 {
				assets = append(assets, b)
			}
			if b.LiabilityShares > 0.0 {
				liabs = append(liabs, b)
			}
		}
		var debtLeg *liquidation.Balance
		debtVal := -1.0
		for i := range liabs {
			b := &liabs[i]
			bk, ok := banks[b.BankPk]
			if !ok || !isDebtMint(bk.Mint) {
				continue
			}
			px, ok := prices[b.BankPk]
			if !ok {
				continue
			}
			v := b.LiabilityShares * bk.LiabilityShareValue / pow10(float64(bk.MintDecimals)) * px
			if v > debtVal {
				debtVal = v
				debtLeg = b
			}
		}
		var collLeg *liquidation.Balance
		collVal := -1.0
		for i := range assets {
			b := &assets[i]
			bk, ok := banks[b.BankPk]
			if !ok {
				continue
			}
			px, ok := prices[b.BankPk]
			if !ok {
				continue
			}
			v := b.AssetShares * bk.AssetShareValue / pow10(float64(bk.MintDecimals)) * px
			if v > collVal {
				collVal = v
				collLeg = b
			}
		}
		if debtLeg == nil || collLeg == nil {
			fmt.Printf("  %s coll≈$%.0f ratio %.3f  [SKIP: no wired-debt leg]\n", pk.String()[:8], coll, ratio)
			continue
		}
		assetBank, liabBank := collLeg.BankPk, debtLeg.BankPk
		abk := banks[assetBank]
		native := collLeg.AssetShares * abk.AssetShareValue
		seize := uint64(native * 0.02) // 2% rung — just proving composition
		tp, ok := mintOwner(endpoint, abk.Mint)
		if !ok {
			tp = solana.MustPublicKeyFromBase58(tokenProgramFallback)
		}
		na, nl := len(assets), len(liabs)
		gate, ok := gateTxB64(authority, liquidatorMA, tp, pk, a, assetBank, liabBank, seize, oracleOf)
		if !ok {
			fmt.Printf("  %s [%dc/%dd] coll≈$%.0f  [tx build failed]\n", pk.String()[:8], na, nl, coll)
			continue
		}
		sim, _ := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "simulateTransaction",
			"params": []any{gate, map[string]any{"sigVerify": false, "replaceRecentBlockhash": true, "commitment": "processed", "encoding": "base64"}}})
		res := asMap(asMap(sim["result"])["value"])
		if logsMode {
			fmt.Fprintf(os.Stderr, "  --- %s RAW sim response ---\n", pk.String()[:8])
			b, _ := json.MarshalIndent(sim, "", "  ")
			fmt.Fprintln(os.Stderr, string(b))
		}
		// An RPC-level error means the sim never ran (bad params/tx) — must never
		// be read as "no instruction error → fireable".
		if errV, has := sim["error"]; has && errV != nil {
			errMap := asMap(errV)
			fmt.Printf("  %s [%dc/%dd] coll≈$%.0f  →  ⚠ RPC error, sim did not run: %s\n",
				pk.String()[:8], len(assets), len(liabs), coll, asStr(errMap["message"]))
			continue
		}
		var idx, code *int
		var isNull bool
		if res == nil {
			isNull = false
		} else if errV, has := res["err"]; !has || errV == nil {
			isNull = true
		} else {
			errMap := asMap(errV)
			ie := asArray(errMap["InstructionError"])
			if len(ie) >= 2 {
				if f, ok := ie[0].(float64); ok {
					iv := int(f)
					idx = &iv
				}
				custom := asMap(ie[1])["Custom"]
				if f, ok := custom.(float64); ok {
					cv := int(f)
					code = &cv
				}
			}
		}
		var verdict string
		switch {
		case isNull:
			verdict = "✅ err=null — FIREABLE NOW (real multi-position opportunity)"
		case idx != nil && *idx == 1 && code != nil && *code == 6068:
			verdict = "✅ WIRING OK — liquidate ix ran, HealthyAccount(6068) at this leg/size"
		case idx != nil && *idx == 1 && code != nil:
			verdict = fmt.Sprintf("✅ WIRING OK — liquidate ix ran, reverted in-ix Custom(%d)", *code)
		case idx != nil:
			verdict = fmt.Sprintf("⚠ error at ix %d (not the liquidate ix) code=%v — INVESTIGATE", *idx, codeOrNil(code))
		default:
			var errDisp any
			if res != nil {
				errDisp = res["err"]
			}
			verdict = fmt.Sprintf("? unclassified: %v", errDisp)
		}
		fmt.Printf("  %s [%dc/%dd] coll≈$%.0f ratio %.3f  seize2%%=%d  →  %s\n",
			pk.String()[:8], na, nl, coll, ratio, seize, verdict)
	}
	fmt.Fprintln(os.Stderr, "\n[mp] If every top account shows 'WIRING OK' (ix 1), single-leg liquidation composes on\n     multi-position accounts and the fix is purely a leg-PICKER in try_arm, not an N-leg tx rewrite.")
}

func pow10(n float64) float64 {
	r := 1.0
	for i := 0.0; i < n; i++ {
		r *= 10
	}
	return r
}

func codeOrNil(c *int) any {
	if c == nil {
		return nil
	}
	return *c
}
