// Command liq_finder is a marginfi liquidatable-account finder (read-only,
// Stage 1 live test).
//
// It scans every MarginfiAccount in the main group, prices each position
// from the on-chain Pyth oracle the protocol itself reads (PriceUpdateV2),
// computes maintenance health, and lists who is liquidatable (+ the
// closest near-misses, so we can see how tight the market is). No money
// moves.
//
// Usage: HELIUS_RPC=<https json-rpc url> go run ./cmd/liq_finder
//
//	[NEAR=20]  how many near-liquidation accounts to show
package main

import (
	"fmt"
	"math"
	"os"
	"sort"
	"time"

	"arbengine/internal/config"
	"arbengine/internal/liquidation"
	"arbengine/internal/rpcclient"
	"arbengine/internal/solana"
)

const marginfiProgram = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA"
const marginfiGroup = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8"

// getMultiple batches getMultipleAccounts (100 keys/call) -> map pubkey ->
// raw bytes. The stdlib rpcclient.GetMultipleAccounts issues a single
// unbatched call and returns an error on failure; the Rust original instead
// retries each chunk (with backoff) and silently skips a chunk that never
// succeeds, so we replicate that batching/retry loop locally rather than
// editing the shared client.
func getMultiple(c *rpcclient.Client, keys []solana.Pubkey) map[solana.Pubkey][]byte {
	out := make(map[solana.Pubkey][]byte)
	for i := 0; i < len(keys); i += 100 {
		end := i + 100
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[i:end]
		infos, err := retryGetMultiple(c, chunk)
		if err != nil {
			continue
		}
		for j, info := range infos {
			if info != nil {
				out[chunk[j]] = info.Data
			}
		}
	}
	return out
}

func retryGetMultiple(c *rpcclient.Client, chunk []solana.Pubkey) ([]*rpcclient.AccountInfo, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		infos, err := c.GetMultipleAccounts(chunk)
		if err == nil {
			return infos, nil
		}
		lastErr = err
		time.Sleep(time.Duration(400<<uint(attempt)) * time.Millisecond)
	}
	return nil, lastErr
}

func main() {
	config.LoadDotenv()
	endpoint, ok := config.EnvOptional("HELIUS_RPC")
	if !ok {
		endpoint, ok = config.EnvOptional("RPC_HTTP")
	}
	if !ok {
		fmt.Fprintln(os.Stderr, "config: set HELIUS_RPC (a getProgramAccounts-capable JSON-RPC url) in .env")
		os.Exit(1)
	}
	nearN := config.EnvInt("NEAR", 20)

	c := rpcclient.New(endpoint)

	// 1) All MarginfiAccounts in the main group. dataSlice trims to the balances
	//    region (1736 B) so the payload is ~half. Server-side dataSize filter
	//    still guarantees these are full 2312-byte accounts.
	fmt.Fprintf(os.Stderr, "[finder] getProgramAccounts (group %s) …\n", marginfiGroup[:8])
	program := solana.MustPubkeyFromBase58(marginfiProgram)
	accountsJSON, err := c.GetProgramAccounts(program, rpcclient.GetProgramAccountsOpts{
		Filters: []any{
			map[string]any{"dataSize": liquidation.MASize},
			map[string]any{"memcmp": map[string]any{"offset": 8, "bytes": marginfiGroup}},
		},
		DataSlice: &struct {
			Offset int `json:"offset"`
			Length int `json:"length"`
		}{Offset: 0, Length: 1736},
	})
	if err != nil {
		accountsJSON = nil
	}
	fmt.Fprintf(os.Stderr, "[finder] %d accounts in group\n", len(accountsJSON))
	if len(accountsJSON) == 0 {
		fmt.Fprintln(os.Stderr, "[finder] nothing returned — check RPC supports getProgramAccounts + the group filter")
		return
	}

	var accounts []*liquidation.MarginfiAccount
	for _, e := range accountsJSON {
		if len(e.Account.Data) == 0 {
			continue
		}
		a, ok := liquidation.DecodeMarginfiAccount(e.Account.Data)
		if !ok || len(a.Balances) == 0 {
			continue
		}
		accounts = append(accounts, a)
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
	bankSet := make(map[solana.Pubkey]struct{})
	for _, a := range borrowers {
		for _, b := range a.Balances {
			bankSet[b.BankPk] = struct{}{}
		}
	}
	bankPks := make([]solana.Pubkey, 0, len(bankSet))
	for pk := range bankSet {
		bankPks = append(bankPks, pk)
	}
	fmt.Fprintf(os.Stderr, "[finder] fetching %d banks …\n", len(bankPks))
	bankRaw := getMultiple(c, bankPks)
	banks := make(liquidation.BankMap)
	oracleOf := make(map[solana.Pubkey]solana.Pubkey)
	for pk, raw := range bankRaw {
		if bank, ok := liquidation.DecodeBank(raw); ok {
			oracleOf[pk] = bank.OracleKey
			banks[pk] = bank
		}
	}

	// 3) Price each bank from its on-chain Pyth oracle (PriceUpdateV2).
	oracleSet := make(map[solana.Pubkey]struct{})
	for _, oracle := range oracleOf {
		oracleSet[oracle] = struct{}{}
	}
	oraclePks := make([]solana.Pubkey, 0, len(oracleSet))
	for pk := range oracleSet {
		oraclePks = append(oraclePks, pk)
	}
	fmt.Fprintf(os.Stderr, "[finder] fetching %d oracle accounts …\n", len(oraclePks))
	oracleRaw := getMultiple(c, oraclePks)
	oraclePrice := make(map[solana.Pubkey]float64)
	for pk, raw := range oracleRaw {
		if usd, ok := liquidation.DecodeOraclePrice(raw); ok {
			oraclePrice[pk] = usd
		}
	}
	prices := make(liquidation.PriceMap)
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
			mint := bank.Mint.String()
			fmt.Fprintf(os.Stderr, "    %s… (dec %d) = $%.4f\n", mint[:8], bank.MintDecimals, p)
		}
	}

	// Dust threshold: below this seizable collateral (USD) a liquidation can't
	// cover gas+priority, so it isn't a real opportunity.
	minCollateral := config.EnvFloat("MIN_COLLATERAL_USD", 10.0)

	// 4) Health — only for borrowers whose EVERY bank we could price. An
	//    account with any unpriced bank is "incomplete", not a signal.
	type scored struct {
		assets  float64
		deficit float64
		ratio   float64
		a       *liquidation.MarginfiAccount
	}
	var scoredList []scored
	incomplete := 0
	for _, a := range borrowers {
		r := liquidation.MaintenanceHealth(a, banks, prices)
		if r.Missing > 0 {
			incomplete++
			continue
		}
		scoredList = append(scoredList, scored{
			assets:  r.Health.WeightedAssets,
			deficit: r.Health.Value(),
			ratio:   r.Health.Ratio(),
			a:       a,
		})
	}

	fmt.Println("\n════ marginfi liquidatable finder ════")
	fmt.Printf("borrowers scanned:        %d\n", len(borrowers))
	fmt.Printf("fully priced (judgable):  %d\n", len(scoredList))
	fmt.Printf("incomplete (unpriced bank, skipped): %d\n", incomplete)

	// Liquidatable with REAL collateral to seize, ranked by seizable value.
	var real []scored
	for _, sc := range scoredList {
		if sc.deficit < 0.0 && sc.assets >= minCollateral {
			real = append(real, sc)
		}
	}
	sort.SliceStable(real, func(i, j int) bool { return real[i].assets > real[j].assets })
	dust := 0
	for _, sc := range scoredList {
		if sc.deficit < 0.0 && sc.assets < minCollateral {
			dust++
		}
	}

	fmt.Printf("LIQUIDATABLE (collateral ≥ $%.0f): %d   [+%d dust ignored]\n", minCollateral, len(real), dust)
	realShown := real
	if len(realShown) > 50 {
		realShown = realShown[:50]
	}
	for _, sc := range realShown {
		auth := sc.a.Authority.String()
		fmt.Printf("  authority %s…  collateral=$%.2f  deficit=%+.2f USD  liab/asset=%.4f\n",
			auth[:8], sc.assets, sc.deficit, sc.ratio)
	}

	// Per-bank breakdown of the largest liquidatable account — tells us whether
	// the collateral is liquid (real opportunity) or a stuck/illiquid token.
	if len(real) > 0 {
		top := real[0]
		auth := top.a.Authority.String()
		fmt.Printf("\n── breakdown: %s… (largest liquidatable) ──\n", auth[:8])
		for _, b := range top.a.Balances {
			bank, ok := banks[b.BankPk]
			if !ok {
				bankStr := b.BankPk.String()
				fmt.Printf("  bank %s… UNPRICED\n", bankStr[:8])
				continue
			}
			price, ok := prices[b.BankPk]
			if !ok {
				price = math.NaN()
			}
			scale := math.Pow(10, float64(bank.MintDecimals))
			mint := bank.Mint.String()
			if b.AssetShares > 0.0 {
				ui := b.AssetShares * bank.AssetShareValue / scale
				fmt.Printf("  COLLATERAL mint %s… %.4f tok × $%.4f = $%.2f  (maint w %.2f, tier via weights)\n",
					mint[:8], ui, price, ui*price, bank.AssetWeightMaint)
			}
			if b.LiabilityShares > 0.0 {
				ui := b.LiabilityShares * bank.LiabilityShareValue / scale
				fmt.Printf("  BORROW     mint %s… %.4f tok × $%.4f = $%.2f  (maint w %.2f)\n",
					mint[:8], ui, price, ui*price, bank.LiabilityWeightMaint)
			}
		}
	}

	// Closest healthy accounts WITH real collateral — the ones worth monitoring.
	var near []scored
	for _, sc := range scoredList {
		if sc.deficit >= 0.0 && sc.assets >= minCollateral {
			near = append(near, sc)
		}
	}
	sort.SliceStable(near, func(i, j int) bool { return near[i].ratio > near[j].ratio })
	fmt.Printf("\nclosest to liquidation (collateral ≥ $%.0f, liab/asset→1.0):\n", minCollateral)
	nearShown := near
	if len(nearShown) > nearN {
		nearShown = nearShown[:nearN]
	}
	for _, sc := range nearShown {
		auth := sc.a.Authority.String()
		fmt.Printf("  %s…  liab/asset=%.4f  collateral=$%.2f  buffer=%+.2f USD\n",
			auth[:8], sc.ratio, sc.assets, sc.deficit)
	}
	fmt.Println()
}
