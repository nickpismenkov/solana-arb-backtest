// Root-cause the health divergence: for one account marginfi calls healthy but
// our maintenance_health calls underwater, print the per-bank breakdown (shares
// . share_value . price . our maint weight -> contribution) and scan each bank's
// bytes for ALL "weight-like" i80f48 values (0<v<2) with offsets — revealing
// whether an emode boosted-weight config is present beyond the 4 config weights.
//
// Usage: HELIUS_RPC=<url> ACCOUNT=<pubkey> go run ./cmd/mfi_health_debug
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/liquidation"
)

const defaultAccount = "BH736MqzFt2dNMeytao6wDn9M1JtMYT2PJnrFxGzknUr"

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
	strs := make([]string, len(keys))
	for i, k := range keys {
		strs[i] = k.String()
	}
	if v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getMultipleAccounts",
		"params": []any{strs, map[string]any{"encoding": "base64"}}}); ok {
		arr := asArray(asMap(v["result"])["value"])
		for i, accv := range arr {
			acc := asMap(accv)
			if acc == nil {
				continue
			}
			if raw, ok := b64(acc["data"]); ok {
				out[keys[i]] = raw
			}
		}
	}
	return out
}

func getOne(endpoint string, pk solana.PublicKey) ([]byte, bool) {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{pk.String(), map[string]any{"encoding": "base64"}}})
	if !ok {
		return nil, false
	}
	return b64(asMap(asMap(v["result"])["value"])["data"])
}

func i80f48(bytesv []byte, off int) (float64, bool) {
	if off+16 > len(bytesv) {
		return 0, false
	}
	return liquidation.I80F48ToF64(bytesv[off : off+16]), true
}

// tagEntry is an (offset, u16 value) pair — mirrors the Rust Vec<(usize,
// u16)> debug-printed candidate list.
type tagEntry struct {
	off int
	val uint16
}

func formatTags(tags []tagEntry) string {
	s := "["
	for i, t := range tags {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("(%d, %d)", t.off, t.val)
	}
	return s + "]"
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
	accountStr := os.Getenv("ACCOUNT")
	if accountStr == "" {
		accountStr = defaultAccount
	}
	account := solana.MustPublicKeyFromBase58(accountStr)

	raw, ok := getOne(endpoint, account)
	if !ok {
		fmt.Fprintln(os.Stderr, "account fetch failed")
		os.Exit(1)
	}
	a, ok := liquidation.DecodeMarginfiAccount(raw)
	if !ok {
		fmt.Fprintln(os.Stderr, "decode account failed")
		os.Exit(1)
	}
	fmt.Printf("account %s\n  authority %s\n  %d active balances\n", account, a.Authority, len(a.Balances))

	// Account bytes after balances (1736..) — flags + emode region.
	fmt.Printf("  ── account tail (post-balances @1736, len %d) ──\n", len(raw))
	if len(raw) >= 1736 {
		tail := raw[1736:]
		if len(tail) >= 8 {
			f := binary.LittleEndian.Uint64(tail[0:8])
			fmt.Printf("     account_flags @1736 = %d (0x%x)\n", f, f)
		}
		// Scan the tail for u16 values that could be an emode_tag (small nonzero).
		var tags []tagEntry
		for i := 0; i+2 <= len(tail) && len(tags) < 8; i += 2 {
			v := binary.LittleEndian.Uint16(tail[i : i+2])
			if v > 0 && v < 4096 {
				tags = append(tags, tagEntry{1736 + i, v})
			}
		}
		fmt.Printf("     small-u16 (emode_tag candidates) in tail: %s\n", formatTags(tags))
	}

	bankSet := map[solana.PublicKey]bool{}
	for _, b := range a.Balances {
		bankSet[b.BankPk] = true
	}
	var bankPks []solana.PublicKey
	for pk := range bankSet {
		bankPks = append(bankPks, pk)
	}
	bankRaw := getMultiple(endpoint, bankPks)
	banks := map[solana.PublicKey]*liquidation.Bank{}
	for pk, r := range bankRaw {
		if bk, ok := liquidation.DecodeBank(r); ok {
			banks[pk] = bk
		}
	}

	// Prices from each bank's oracle.
	oracleSet := map[solana.PublicKey]bool{}
	for _, bk := range banks {
		oracleSet[bk.OracleKey] = true
	}
	var oraclePks []solana.PublicKey
	for pk := range oracleSet {
		oraclePks = append(oraclePks, pk)
	}
	oracleRaw := getMultiple(endpoint, oraclePks)
	priceOf := map[solana.PublicKey]float64{}
	for pk, r := range oracleRaw {
		if p, ok := liquidation.DecodeOraclePrice(r); ok {
			priceOf[pk] = p
		}
	}

	fmt.Println("\n  ── per-balance health breakdown (our maintenance_health) ──")
	var wa, wl float64
	for _, b := range a.Balances {
		bank, ok := banks[b.BankPk]
		if !ok {
			fmt.Printf("    %s … BANK MISSING\n", b.BankPk.String()[:8])
			continue
		}
		price, hasPrice := priceOf[bank.OracleKey]
		if !hasPrice {
			price = math.NaN()
		}
		scale := math.Pow(10, float64(bank.MintDecimals))
		if b.AssetShares > 0.0 {
			ui := b.AssetShares * bank.AssetShareValue / scale
			contrib := ui * price * bank.AssetWeightMaint
			wa += contrib
			fmt.Printf("    ASSET %s…  ui=%.4f price=$%.4f w_maint=%.4f (w_init=%.4f) → $%.2f\n",
				b.BankPk.String()[:8], ui, price, bank.AssetWeightMaint, bank.AssetWeightInit, contrib)
		}
		if b.LiabilityShares > 0.0 {
			ui := b.LiabilityShares * bank.LiabilityShareValue / scale
			contrib := ui * price * bank.LiabilityWeightMaint
			wl += contrib
			fmt.Printf("    LIAB  %s…  ui=%.4f price=$%.4f w_maint=%.4f → $%.2f\n",
				b.BankPk.String()[:8], ui, price, bank.LiabilityWeightMaint, contrib)
		}
	}
	ratio := math.Inf(1)
	if wa > 0.0 {
		ratio = wl / wa
	}
	verdict := "healthy"
	if wa < wl {
		verdict = "UNDERWATER"
	}
	fmt.Printf("  → [no-emode] weighted_assets $%.2f  weighted_liabilities $%.2f  ratio %.4f  %s\n", wa, wl, ratio, verdict)

	// Emode-aware verdict via the production maintenance_health (should match marginfi).
	priceMap := liquidation.PriceMap{}
	for pk, bk := range banks {
		if p, ok := priceOf[bk.OracleKey]; ok {
			priceMap[pk] = p
		}
	}
	r := liquidation.MaintenanceHealth(a, banks, priceMap)
	emodeVerdict := "healthy"
	if r.Health.Liquidatable() {
		emodeVerdict = "UNDERWATER"
	}
	fmt.Printf("  → [emode]    weighted_assets $%.2f  weighted_liabilities $%.2f  ratio %.4f  %s (missing %d)\n",
		r.Health.WeightedAssets, r.Health.WeightedLiabilities, r.Health.Ratio(), emodeVerdict, r.Missing)
	// What asset-weight boost on the collateral would make marginfi's verdict (healthy) consistent?
	if wa > 0.0 && wa < wl {
		fmt.Printf("  → to be healthy, collateral asset-weight would need ≈%.2f× boost (emode?)\n", wl/wa)
	}

	// Emode decode at the hypothesized layout: EmodeSettings starts @1240
	// (emode_tag u16), emode entries[10] start @1264, each 40 bytes:
	// collateral_bank_emode_tag u16 @0, asset_weight_init @8, asset_weight_maint @24.
	const emodeEntries = 1264
	const entrySize = 40
	for pk, rawbank := range bankRaw {
		role := "ASSET"
		for _, b := range a.Balances {
			if b.BankPk.Equals(pk) && b.LiabilityShares > 0.0 {
				role = "LIAB"
				break
			}
		}
		fmt.Printf("\n  ── %s bank %s ──\n", role, pk.String()[:8])
		// Hunt for this bank's own emode_tag: print every u16 in 880..1268 so we
		// can see where 619/871 (the tags USDC references) sit for the collateral.
		var tagline []tagEntry
		for i := 880; i+2 <= 1268 && i+2 <= len(rawbank); i += 2 {
			v := binary.LittleEndian.Uint16(rawbank[i : i+2])
			if v > 0 && v < 60000 {
				tagline = append(tagline, tagEntry{i, v})
			}
		}
		fmt.Printf("     u16 in 880..1268 (nonzero): %s\n", formatTags(tagline))
		// Entries (only the clean, in-range ones).
		for e := 0; e < 10; e++ {
			base := emodeEntries + e*entrySize
			if base+2 > len(rawbank) {
				break
			}
			tag := binary.LittleEndian.Uint16(rawbank[base : base+2])
			init, initOK := i80f48(rawbank, base+8)
			maint, maintOK := i80f48(rawbank, base+24)
			if !initOK {
				init = 0.0
			}
			if !maintOK {
				maint = 0.0
			}
			if init >= 0.0 && init <= 2.0 && maint >= 0.0 && maint <= 2.0 && (init > 0.0 || maint > 0.0) {
				fmt.Printf("     entry[%d] collat_tag=%d  w_init=%.4f  w_maint=%.4f\n", e, tag, init, maint)
			}
		}
	}
}
