// Why do accounts our engine flags get rejected? Audit the REAL revert codes.
//
// liq_executor logs "chain says healthy at the actionable price" whenever the
// size-ladder sim returns Some(false) — but simulate_gate maps EVERY custom
// error to Some(false), not just HealthyAccount(6068). So that one log line
// actually hides: 6068 (genuinely healthy — if OUR maintenance_health says
// liquidatable at the SAME on-chain price, that's a health-MATH bug), 6049
// (Switchboard stale), 6210 (Kamino reserve), size guards, etc.
//
// This probe finds every account our maintenance_health flags liquidatable at
// FRESH on-chain prices (staleness-gated), sims the single-leg liquidate, and
// tallies the true codes so we can see the real cause distribution.
//
// Usage: HELIUS_RPC=<url> [LIQUIDATOR_MA=…] [AUTHORITY=…] go run ./cmd/mfi_reject_audit
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

func mintOwner(endpoint string, mint solana.PublicKey) solana.PublicKey {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{mint.String(), map[string]any{"encoding": "jsonParsed"}}})
	if ok {
		owner := asStr(asMap(asMap(asMap(v["result"])["value"]))["owner"])
		if owner != "" {
			if pk, err := solana.PublicKeyFromBase58(owner); err == nil {
				return pk
			}
		}
	}
	return solana.MustPublicKeyFromBase58(tokenProgramFallback)
}

func isDebtMint(m solana.PublicKey) bool {
	s := m.String()
	return s == usdcMint || s == usdtMint || s == solMint
}

// gateTxB64 builds [start_flashloan, liquidate, end_flashloan] and returns it base64-encoded.
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
	cap := 60
	if v := os.Getenv("CAP"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cap = n
		}
	}

	fmt.Fprintln(os.Stderr, "[audit] scanning marginfi group …")
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
	// PER-BANK staleness gate (the fix): each bank's on-chain oracle_max_age.
	prices := liquidation.PriceMap{}
	for bankPk, oraclePk := range oracleOf {
		raw, ok := oracleRaw[oraclePk]
		if !ok {
			continue
		}
		maxAge := uint16(0)
		if b, ok := banks[bankPk]; ok {
			maxAge = b.OracleMaxAge
		}
		maxStale := liquidation.MaxStaleSlotsFor(maxAge, liquidation.DefaultMaxSBStaleSlots)
		if usd, ok := liquidation.DecodeOraclePriceFresh(raw, slot, maxStale); ok {
			prices[bankPk] = usd
		}
	}

	// Accounts OUR maintenance_health flags liquidatable at FRESH on-chain price,
	// with a wired-debt leg (the ones try_arm would evaluate + reject).
	type flaggedEntry struct {
		pk        solana.PublicKey
		a         *liquidation.MarginfiAccount
		assetBank solana.PublicKey
		liabBank  solana.PublicKey
	}
	var flagged []flaggedEntry
	for _, ae := range accts {
		pk, a := ae.pk, ae.a
		h := liquidation.MaintenanceHealth(a, banks, prices)
		if h.Missing > 0 || !h.Health.Liquidatable() {
			continue
		}
		var bestAsset *liquidation.Balance
		bestAssetVal := -1.0
		for i := range a.Balances {
			b := &a.Balances[i]
			if b.AssetShares <= 0.0 {
				continue
			}
			bk, ok := banks[b.BankPk]
			if !ok {
				continue
			}
			p, ok := prices[b.BankPk]
			if !ok {
				continue
			}
			val := b.AssetShares * bk.AssetShareValue / pow10(float64(bk.MintDecimals)) * p
			if val > bestAssetVal {
				bestAssetVal = val
				bestAsset = b
			}
		}
		var debt *liquidation.Balance
		for i := range a.Balances {
			b := &a.Balances[i]
			if b.LiabilityShares <= 0.0 {
				continue
			}
			bk, ok := banks[b.BankPk]
			if !ok || !isDebtMint(bk.Mint) {
				continue
			}
			debt = b
			break
		}
		if bestAsset != nil && debt != nil {
			flagged = append(flagged, flaggedEntry{pk, a, bestAsset.BankPk, debt.BankPk})
		}
	}
	fmt.Fprintf(os.Stderr, "[audit] %d accounts our maintenance_health flags LIQUIDATABLE at fresh on-chain price (with a wired-debt leg)\n\n", len(flagged))

	tally := map[string]uint32{}
	examples := map[string]string{}
	n := cap
	if n > len(flagged) {
		n = len(flagged)
	}
	for _, f := range flagged[:n] {
		abk := banks[f.assetBank]
		var bal *liquidation.Balance
		for i := range f.a.Balances {
			if f.a.Balances[i].BankPk.Equals(f.assetBank) {
				bal = &f.a.Balances[i]
				break
			}
		}
		seize := uint64(bal.AssetShares * abk.AssetShareValue * 0.02)
		tp := mintOwner(endpoint, abk.Mint)
		gate, ok := gateTxB64(authority, liquidatorMA, tp, f.pk, f.a, f.assetBank, f.liabBank, seize, oracleOf)
		if !ok {
			continue
		}
		sim, _ := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "simulateTransaction",
			"params": []any{gate, map[string]any{"sigVerify": false, "replaceRecentBlockhash": true, "commitment": "processed", "encoding": "base64"}}})
		res := asMap(asMap(sim["result"])["value"])
		var key string
		if res == nil {
			key = "rpc-error/no-result"
		} else if errV, has := res["err"]; !has || errV == nil {
			key = "null → FIREABLE (real!)"
		} else {
			errMap := asMap(res["err"])
			ie := asArray(errMap["InstructionError"])
			var idx *int
			var code *int
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
			switch {
			case idx != nil && *idx == 1 && code != nil && *code == 6068:
				key = "6068 HealthyAccount  (our math DISAGREES w/ chain at same price → BUG)"
			case idx != nil && *idx == 1 && code != nil && *code == 6049:
				key = "6049 SwitchboardStalePrice (oracle stale — detection issue)"
			case idx != nil && *idx == 1 && code != nil && *code == 6210:
				key = "6210 KaminoReserveValidation"
			case idx != nil && *idx == 1 && code != nil:
				key = fmt.Sprintf("in-liquidate Custom(%d)", *code)
			case idx != nil:
				key = fmt.Sprintf("ix %d Custom(%v) — WIRING?", *idx, codeOrNil(code))
			default:
				b, _ := json.Marshal(res["err"])
				key = fmt.Sprintf("other: %s", string(b))
			}
		}
		tally[key]++
		if _, ok := examples[key]; !ok {
			examples[key] = f.pk.String()
		}
	}

	fmt.Println("\n═══ REJECT-CODE DISTRIBUTION (why flagged accounts don't fire) ═══")
	type row struct {
		k string
		n uint32
	}
	var rows []row
	for k, v := range tally {
		rows = append(rows, row{k, v})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].n > rows[j].n })
	for _, r := range rows {
		fmt.Printf("  %3d  %s\n", r.n, r.k)
		fmt.Printf("        e.g. %s\n", examples[r.k])
	}
	fmt.Println("\nKEY: 6068 = our health math over-flags vs the chain (a real logic bug to fix).")
	fmt.Println("     6049 = stale oracle (detection; the generous 5000-slot gate lets some through).")
	fmt.Println("     null = a genuinely fireable account we should have taken.")
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
