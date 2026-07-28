// ADDRESSABLE-MARKET census: how much money is actually within reach on marginfi?
//
// The liquidations that LAND on a calm day are dust (2026-07-14: 119 liquidations
// across all 4 protocols moved $171 total). That says nothing about the size of
// the opportunity when volatility hits — for that you have to look at the standing
// borrower population, not the fills. This bins every marginfi borrower by distance
// to liquidation and sums the collateral in each bin, so we can answer: "if the
// market drops X%, how much collateral comes into liquidation range, and is any of
// it big enough to be worth firing at?"
//
// Also reports how much of that collateral our fire path could actually TAKE (v1
// shape: 1 collateral / 1 USDC|USDT|wSOL debt) vs. what it would skip.
//
// Uses the same decoders as the live executor (liquidation.MaintenanceHealth,
// on-chain oracle prices) so the numbers match what the bot sees.
//
// Usage: HELIUS_RPC=<url> [DROP_PCT=10] go run ./cmd/mfi_watchset_value
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/liquidation"
)

const (
	marginfiProgram = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA"
	marginfiGroup   = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8"
	usdcMint        = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	usdtMint        = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"
	solMint         = "So11111111111111111111111111111111111111112"
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
	return math.Pow(10, n)
}

// isFireableShape: 1 collateral / 1 stable-or-SOL debt — the shape the fire path can act on.
func isFireableShape(a *liquidation.MarginfiAccount, banks liquidation.BankMap) bool {
	na, nl := 0, 0
	var liab liquidation.Balance
	for _, b := range a.Balances {
		if b.AssetShares > 0.0 {
			na++
		}
		if b.LiabilityShares > 0.0 {
			nl++
			liab = b
		}
	}
	if na != 1 || nl != 1 {
		return false
	}
	lb, ok := banks[liab.BankPk]
	if !ok {
		return false
	}
	m := lb.Mint.String()
	return m == usdcMint || m == usdtMint || m == solMint
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
	dropPct := 10.0
	if v := os.Getenv("DROP_PCT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			dropPct = f
		}
	}

	fmt.Fprintln(os.Stderr, "scanning marginfi borrowers …")
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
	for pk, r := range bankRaw {
		if bk, ok := liquidation.DecodeBank(r); ok {
			oracleOf[pk] = bk.OracleKey
			banks[pk] = bk
		}
	}
	slotResp, _ := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getSlot", "params": []any{map[string]any{"commitment": "confirmed"}}})
	slot := uint64(0)
	if s, ok := slotResp["result"].(float64); ok {
		slot = uint64(s)
	}
	maxStale := liquidation.DefaultMaxSBStaleSlots
	if v := os.Getenv("MAX_SB_STALE_SLOTS"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			maxStale = n
		}
	}
	gate := os.Getenv("STALE_GATE") != "0" // STALE_GATE=0 → old behavior
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
		var p float64
		var ok bool
		if gate {
			p, ok = liquidation.DecodeOraclePriceFresh(r, slot, maxStale)
		} else {
			p, ok = liquidation.DecodeOraclePrice(r)
		}
		if ok {
			priceByOracle[pk] = p
		}
	}
	gateLabel := "ON"
	if !gate {
		gateLabel = "OFF"
	}
	fmt.Fprintf(os.Stderr, "slot %d, max_stale %d slots, gate %s\n", slot, maxStale, gateLabel)
	prices := liquidation.PriceMap{}
	for bk, oc := range oracleOf {
		if p, ok := priceByOracle[oc]; ok {
			prices[bk] = p
		}
	}
	fmt.Fprintf(os.Stderr, "%d borrowers, %d banks priced\n\n", len(accts), len(prices))

	// Health today, and health after an adverse move of DROP_PCT on every
	// non-stable collateral (the "what does a real selloff put in range" question).
	stable := func(m string) bool { return m == usdcMint || m == usdtMint }
	shocked := liquidation.PriceMap{}
	for k, v := range prices {
		shocked[k] = v
	}
	for bankPk, bank := range banks {
		if stable(bank.Mint.String()) {
			continue
		}
		if p, ok := shocked[bankPk]; ok {
			shocked[bankPk] = p * (1.0 - dropPct/100.0)
		}
	}

	type row struct {
		pk           solana.PublicKey
		coll         float64
		ratio        float64
		ratioShocked float64
		fireable     bool
	}
	var rows []row
	for _, ae := range accts {
		pk, a := ae.pk, ae.a
		now := liquidation.MaintenanceHealth(a, banks, prices)
		if now.Missing > 0 || now.Health.WeightedAssets <= 0.0 || now.Health.WeightedLiabilities <= 0.0 {
			continue
		}
		then := liquidation.MaintenanceHealth(a, banks, shocked)
		// Unweighted collateral USD = what a liquidator can actually seize against.
		var coll float64
		for _, b := range a.Balances {
			if b.AssetShares <= 0.0 {
				continue
			}
			bank, ok := banks[b.BankPk]
			if !ok {
				continue
			}
			px, ok := prices[b.BankPk]
			if !ok {
				continue
			}
			scale := pow10(float64(bank.MintDecimals))
			coll += b.AssetShares * bank.AssetShareValue / scale * px
		}
		rows = append(rows, row{pk, coll, now.Health.Ratio(), then.Health.Ratio(), isFireableShape(a, banks)})
	}

	bins := []struct {
		lo, hi float64
		label  string
	}{
		{0.0, 0.85, "< 0.85  (safe)"},
		{0.85, 0.95, "0.85 – 0.95"},
		{0.95, 0.97, "0.95 – 0.97"},
		{0.97, 1.00, "0.97 – 1.00  (ARM)"},
		{1.00, math.Inf(1), "≥ 1.00  (LIQUIDATABLE)"},
	}
	fmt.Printf("MARGINFI BORROWER POPULATION — %d priced accounts with debt\n\n", len(rows))
	fmt.Printf("%-24s %7s %16s %10s %16s\n", "health ratio", "accts", "collateral $", "≥ $1k", "fireable coll $")
	fmt.Println("------------------------------------------------------------------------------")
	for _, bin := range bins {
		var sel []row
		for _, r := range rows {
			if r.ratio >= bin.lo && r.ratio < bin.hi {
				sel = append(sel, r)
			}
		}
		var tot float64
		big := 0
		var fire float64
		for _, r := range sel {
			tot += r.coll
			if r.coll >= 1000.0 {
				big++
			}
			if r.fireable {
				fire += r.coll
			}
		}
		fmt.Printf("%-24s %7d %16s %10d %16s\n", bin.label, len(sel), fmt.Sprintf("%.0f", tot), big, fmt.Sprintf("%.0f", fire))
	}

	// The money question: a DROP_PCT selloff — what comes into range?
	var newly []row
	for _, r := range rows {
		if r.ratio < 1.0 && r.ratioShocked >= 1.0 {
			newly = append(newly, r)
		}
	}
	var newlyColl, newlyFire float64
	newlyBig := 0
	for _, r := range newly {
		newlyColl += r.coll
		if r.fireable {
			newlyFire += r.coll
		}
		if r.coll >= 1000.0 {
			newlyBig++
		}
	}
	fmt.Printf("\n▶ IF EVERY VOLATILE COLLATERAL DROPS %g%%:\n", dropPct)
	fmt.Printf("   %d accounts newly cross into liquidation range\n", len(newly))
	fmt.Printf("   $%.0f collateral comes into range  ($%.0f of it in our fireable shape)\n", newlyColl, newlyFire)
	fmt.Printf("   of those, %d are ≥ $1k positions (worth firing at)\n", newlyBig)

	var top []row
	for _, r := range rows {
		if r.ratio >= 0.90 {
			top = append(top, r)
		}
	}
	sort.Slice(top, func(i, j int) bool { return top[i].coll > top[j].coll })
	fmt.Println("\n▶ LARGEST POSITIONS ALREADY WITHIN 10% OF THE THRESHOLD (ratio ≥ 0.90):")
	n := 12
	if n > len(top) {
		n = len(top)
	}
	for _, r := range top[:n] {
		shapeLabel := "fireable"
		if !r.fireable {
			shapeLabel = "SKIP (shape)"
		}
		fmt.Printf("   $%12s  ratio %.3f  %-12s  %s\n", fmt.Sprintf("%.0f", r.coll), r.ratio, shapeLabel, r.pk)
	}
	// The phantom question: big accounts our math says are ALREADY liquidatable
	// and that our fire path could shape-wise take. If these were real, the
	// competitor bots would have eaten them in seconds — they persist for days,
	// so either our health math over-flags or the chain refuses for a reason we
	// do not model. These are the accounts to simulate against.
	var phantoms []row
	for _, r := range rows {
		if r.ratio >= 1.0 && r.fireable && r.coll >= 1000.0 {
			phantoms = append(phantoms, r)
		}
	}
	sort.Slice(phantoms, func(i, j int) bool { return phantoms[i].coll > phantoms[j].coll })
	fmt.Println("\n▶ BIG 'LIQUIDATABLE' + FIREABLE-SHAPE ACCOUNTS (the phantom suspects):")
	// Report each survivor's collateral-oracle staleness so we can see whether a
	// tighter (still-safe) MAX_SB_STALE_SLOTS would catch it. Healthy feeds run
	// ~350 slots behind head; a survivor far above that is a stale-oracle phantom.
	acctByPk := map[solana.PublicKey]*liquidation.MarginfiAccount{}
	for _, ae := range accts {
		acctByPk[ae.pk] = ae.a
	}
	np := 10
	if np > len(phantoms) {
		np = len(phantoms)
	}
	for _, r := range phantoms[:np] {
		stale := "(pyth or fresh)"
		if a, ok := acctByPk[r.pk]; ok {
			for _, b := range a.Balances {
				if b.AssetShares <= 0.0 {
					continue
				}
				oc, ok := oracleOf[b.BankPk]
				if !ok {
					continue
				}
				raw, ok := oracleRaw[oc]
				if !ok {
					continue
				}
				if s, ok := liquidation.DecodeSwitchboardPullSlot(raw); ok {
					behind := uint64(0)
					if slot > s {
						behind = slot - s
					}
					stale = fmt.Sprintf("SB oracle %d slots behind head", behind)
				}
				break
			}
		}
		fmt.Printf("   $%12s  ratio %.3f  %s  [%s]\n", fmt.Sprintf("%.0f", r.coll), r.ratio, r.pk, stale)
	}
	var nearTotal float64
	for _, r := range top {
		nearTotal += r.coll
	}
	fmt.Printf("   → %d accounts, $%.0f total collateral\n", len(top), nearTotal)
}
