// marginfi liquidatable-account finder (read-only, Stage 1 live test).
//
// Scans every MarginfiAccount in the main group, prices each position from
// the on-chain Pyth oracle the protocol itself reads (PriceUpdateV2),
// computes maintenance health, and lists who is liquidatable (+ the closest
// near-misses, so we can see how tight the market is). No money moves.
//
// Usage: HELIUS_RPC=<https json-rpc url> go run ./cmd/liq_finder
//
//	[NEAR=20]  how many near-liquidation accounts to show
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

// getMultiple batches getMultipleAccounts (100 keys/call) → map pubkey → raw bytes.
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
		time.Sleep(50 * time.Millisecond)
	}
	return out
}

func short(pk solana.PublicKey) string {
	s := pk.String()
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
		fmt.Fprintln(os.Stderr, "HELIUS_RPC (a getProgramAccounts-capable JSON-RPC url) in .env")
		os.Exit(1)
	}
	nearN := 20
	if v := os.Getenv("NEAR"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			nearN = n
		}
	}

	// 1) All MarginfiAccounts in the main group. dataSlice trims to the
	//    balances region (1736 B) so the payload is ~half. Server-side
	//    dataSize filter still guarantees these are full 2312-byte accounts.
	fmt.Fprintf(os.Stderr, "[finder] getProgramAccounts (group %s) …\n", marginfiGroup[:8])
	resp, _ := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{marginfiProgram, map[string]any{
			"encoding":  "base64",
			"dataSlice": map[string]any{"offset": 0, "length": 1736},
			"filters": []any{
				map[string]any{"dataSize": liquidation.MASize},
				map[string]any{"memcmp": map[string]any{"offset": 8, "bytes": marginfiGroup}},
			},
		}},
	})
	var accountsJSON []any
	if resp != nil {
		accountsJSON, _ = resp["result"].([]any)
	}
	fmt.Fprintf(os.Stderr, "[finder] %d accounts in group\n", len(accountsJSON))
	if len(accountsJSON) == 0 {
		fmt.Fprintln(os.Stderr, "[finder] nothing returned — check RPC supports getProgramAccounts + the group filter")
		return
	}

	var accounts []*liquidation.MarginfiAccount
	for _, eAny := range accountsJSON {
		e, ok := eAny.(map[string]any)
		if !ok {
			continue
		}
		acc, _ := e["account"].(map[string]any)
		dataArr, _ := acc["data"].([]any)
		raw, ok := b64(dataArr)
		if !ok {
			continue
		}
		ma, ok := liquidation.DecodeMarginfiAccount(raw)
		if !ok || len(ma.Balances) == 0 {
			continue
		}
		accounts = append(accounts, ma)
	}
	var borrowers []*liquidation.MarginfiAccount
	for _, a := range accounts {
		for _, b := range a.Balances {
			if b.LiabilityShares > 0.0 {
				borrowers = append(borrowers, a)
				break
			}
		}
	}
	fmt.Fprintf(os.Stderr, "[finder] %d accounts with balances, %d with an open borrow\n", len(accounts), len(borrowers))

	// 2) Fetch every referenced Bank.
	bankSet := map[solana.PublicKey]struct{}{}
	for _, a := range borrowers {
		for _, b := range a.Balances {
			bankSet[b.BankPk] = struct{}{}
		}
	}
	bankPks := make([]solana.PublicKey, 0, len(bankSet))
	for pk := range bankSet {
		bankPks = append(bankPks, pk)
	}
	fmt.Fprintf(os.Stderr, "[finder] fetching %d banks …\n", len(bankPks))
	bankRaw := getMultiple(endpoint, bankPks)
	banks := liquidation.BankMap{}
	oracleOf := map[solana.PublicKey]solana.PublicKey{}
	for pk, raw := range bankRaw {
		if bank, ok := liquidation.DecodeBank(raw); ok {
			oracleOf[pk] = bank.OracleKey
			banks[pk] = bank
		}
	}

	// 3) Price each bank from its on-chain Pyth oracle (PriceUpdateV2).
	oracleSet := map[solana.PublicKey]struct{}{}
	for _, o := range oracleOf {
		oracleSet[o] = struct{}{}
	}
	oraclePks := make([]solana.PublicKey, 0, len(oracleSet))
	for pk := range oracleSet {
		oraclePks = append(oraclePks, pk)
	}
	fmt.Fprintf(os.Stderr, "[finder] fetching %d oracle accounts …\n", len(oraclePks))
	oracleRaw := getMultiple(endpoint, oraclePks)
	oraclePrice := map[solana.PublicKey]float64{}
	for pk, raw := range oracleRaw {
		if usd, ok := liquidation.DecodeOraclePrice(raw); ok {
			oraclePrice[pk] = usd
		}
	}
	prices := liquidation.PriceMap{}
	for bankPk, oraclePk := range oracleOf {
		if p, ok := oraclePrice[oraclePk]; ok {
			prices[bankPk] = p
		}
	}
	fmt.Fprintf(os.Stderr, "[finder] priced %d/%d banks\n", len(prices), len(banks))

	// Sanity: dump a few priced banks (eyeball USDC≈$1, SOL≈$82, …).
	fmt.Fprintln(os.Stderr, "[finder] price sanity (mint → USD):")
	shown := 0
	for pk, bank := range banks {
		if shown >= 200 {
			break
		}
		shown++
		if p, ok := prices[pk]; ok {
			fmt.Fprintf(os.Stderr, "    %s… (dec %d) = $%.4f\n", short(bank.Mint), bank.MintDecimals, p)
		}
	}

	// Dust threshold: below this seizable collateral (USD) a liquidation
	// can't cover gas+priority, so it isn't a real opportunity.
	minCollateral := 10.0
	if v := os.Getenv("MIN_COLLATERAL_USD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			minCollateral = f
		}
	}

	// 4) Health — only for borrowers whose EVERY bank we could price. An
	//    account with any unpriced bank is "incomplete", not a signal.
	type scored struct {
		assets  float64
		deficit float64
		ratio   float64
		a       *liquidation.MarginfiAccount
	}
	var allScored []scored
	incomplete := 0
	for _, a := range borrowers {
		r := liquidation.MaintenanceHealth(a, banks, prices)
		if r.Missing > 0 {
			incomplete++
			continue
		}
		allScored = append(allScored, scored{
			assets:  r.Health.WeightedAssets,
			deficit: r.Health.Value(),
			ratio:   r.Health.Ratio(),
			a:       a,
		})
	}

	fmt.Println("\n════ marginfi liquidatable finder ════")
	fmt.Printf("borrowers scanned:        %d\n", len(borrowers))
	fmt.Printf("fully priced (judgable):  %d\n", len(allScored))
	fmt.Printf("incomplete (unpriced bank, skipped): %d\n", incomplete)

	// Liquidatable with REAL collateral to seize, ranked by seizable value.
	var real []scored
	for _, s := range allScored {
		if s.deficit < 0.0 && s.assets >= minCollateral {
			real = append(real, s)
		}
	}
	sort.Slice(real, func(i, j int) bool { return real[i].assets > real[j].assets })
	dust := 0
	for _, s := range allScored {
		if s.deficit < 0.0 && s.assets < minCollateral {
			dust++
		}
	}

	fmt.Printf("LIQUIDATABLE (collateral ≥ $%.0f): %d   [+%d dust ignored]\n", minCollateral, len(real), dust)
	for i, s := range real {
		if i >= 50 {
			break
		}
		fmt.Printf("  authority %s…  collateral=$%.2f  deficit=%+.2f USD  liab/asset=%.4f\n",
			short(s.a.Authority), s.assets, s.deficit, s.ratio)
	}

	// Per-bank breakdown of the largest liquidatable account — tells us
	// whether the collateral is liquid (real opportunity) or a stuck/illiquid
	// token.
	if len(real) > 0 {
		top := real[0]
		fmt.Printf("\n── breakdown: %s… (largest liquidatable) ──\n", short(top.a.Authority))
		for _, b := range top.a.Balances {
			bank, ok := banks[b.BankPk]
			if !ok {
				fmt.Printf("  bank %s… UNPRICED\n", short(b.BankPk))
				continue
			}
			price, hasPrice := prices[b.BankPk]
			if !hasPrice {
				price = math.NaN()
			}
			scale := math.Pow10(int(bank.MintDecimals))
			if b.AssetShares > 0.0 {
				ui := b.AssetShares * bank.AssetShareValue / scale
				fmt.Printf("  COLLATERAL mint %s… %.4f tok × $%.4f = $%.2f  (maint w %.2f, tier via weights)\n",
					short(bank.Mint), ui, price, ui*price, bank.AssetWeightMaint)
			}
			if b.LiabilityShares > 0.0 {
				ui := b.LiabilityShares * bank.LiabilityShareValue / scale
				fmt.Printf("  BORROW     mint %s… %.4f tok × $%.4f = $%.2f  (maint w %.2f)\n",
					short(bank.Mint), ui, price, ui*price, bank.LiabilityWeightMaint)
			}
		}
	}

	// Closest healthy accounts WITH real collateral — the ones worth monitoring.
	var near []scored
	for _, s := range allScored {
		if s.deficit >= 0.0 && s.assets >= minCollateral {
			near = append(near, s)
		}
	}
	sort.Slice(near, func(i, j int) bool { return near[i].ratio > near[j].ratio })
	fmt.Printf("\nclosest to liquidation (collateral ≥ $%.0f, liab/asset→1.0):\n", minCollateral)
	for i, s := range near {
		if i >= nearN {
			break
		}
		fmt.Printf("  %s…  liab/asset=%.4f  collateral=$%.2f  buffer=%+.2f USD\n",
			short(s.a.Authority), s.ratio, s.assets, s.deficit)
	}
	fmt.Println()
}
