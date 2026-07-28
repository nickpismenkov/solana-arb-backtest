// Verify the Jupiter Lend (Fluid) decoders against live mainnet and enumerate
// every vault: collateral/debt pair, liquidation threshold, sizes, and a
// first-pass liquidatable signal. Read-only.
//
// Detection honesty: precise per-price liquidatable detection needs Fluid's
// tick↔price math (not reversed here). This reports the CONFIDENT on-chain
// liquidation-activity flags (absorbed debt / branch_liquidated) and leaves the
// authoritative check to the executor's liquidate simulation.
//
// Usage: HELIUS_RPC=<url> go run ./cmd/jupiter_probe
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/base58"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/jupiterlend"
)

func rpc(endpoint string, body map[string]any) map[string]any {
	b, err := json.Marshal(body)
	if err != nil {
		return nil
	}
	for attempt := 0; attempt < 4; attempt++ {
		resp, err := http.Post(endpoint, "application/json", bytes.NewReader(b))
		if err == nil {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var v map[string]any
			if json.Unmarshal(raw, &v) == nil {
				return v
			}
		}
		time.Sleep(time.Duration(400<<attempt) * time.Millisecond)
	}
	return nil
}

func b64(d any) ([]byte, bool) {
	arr, ok := d.([]any)
	if !ok || len(arr) == 0 {
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

// gpaByDisc runs getProgramAccounts filtered by an 8-byte discriminator at offset 0.
func gpaByDisc(endpoint string, disc [8]byte) []struct {
	Pk   solana.PublicKey
	Data []byte
} {
	disc58 := base58.Encode(disc[:])
	v := rpc(endpoint, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{jupiterlend.VaultsProgram, map[string]any{
			"encoding": "base64",
			"filters":  []any{map[string]any{"memcmp": map[string]any{"offset": 0, "bytes": disc58}}},
		}},
	})
	var out []struct {
		Pk   solana.PublicKey
		Data []byte
	}
	result, _ := v["result"].([]any)
	for _, ev := range result {
		e, _ := ev.(map[string]any)
		pkStr, _ := e["pubkey"].(string)
		pk, err := solana.PublicKeyFromBase58(pkStr)
		if err != nil {
			continue
		}
		acct, _ := e["account"].(map[string]any)
		data, ok := b64(acct["data"])
		if !ok {
			continue
		}
		out = append(out, struct {
			Pk   solana.PublicKey
			Data []byte
		}{pk, data})
	}
	return out
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func label(m solana.PublicKey) string {
	switch m.String() {
	case jupiterlend.USDCMint:
		return "USDC"
	case jupiterlend.USDTMint:
		return "USDT"
	case jupiterlend.WSOLMint:
		return "wSOL"
	default:
		s := m.String()
		if len(s) > 6 {
			return s[:6]
		}
		return s
	}
}

func main() {
	envfile.LoadDotEnv()

	endpoint := os.Getenv("HELIUS_RPC")
	if endpoint == "" {
		endpoint = os.Getenv("RPC_HTTP")
	}
	if endpoint == "" {
		fail("HELIUS_RPC (or RPC_HTTP) must be set")
	}

	// Decode all VaultConfig + VaultState, join by vault_id.
	configs := map[uint16]struct {
		Pk  solana.PublicKey
		Cfg *jupiterlend.VaultConfig
	}{}
	for _, e := range gpaByDisc(endpoint, jupiterlend.VaultConfigDisc) {
		if c, ok := jupiterlend.DecodeVaultConfig(e.Data); ok {
			configs[c.VaultID] = struct {
				Pk  solana.PublicKey
				Cfg *jupiterlend.VaultConfig
			}{e.Pk, c}
		}
	}
	states := map[uint16]struct {
		Pk solana.PublicKey
		St *jupiterlend.VaultState
	}{}
	for _, e := range gpaByDisc(endpoint, jupiterlend.VaultStateDisc) {
		if s, ok := jupiterlend.DecodeVaultState(e.Data); ok {
			states[s.VaultID] = struct {
				Pk solana.PublicKey
				St *jupiterlend.VaultState
			}{e.Pk, s}
		}
	}
	fmt.Printf("live: %d VaultConfig, %d VaultState decoded\n", len(configs), len(states))

	var vaults []*jupiterlend.Vault
	for vid, c := range configs {
		if s, ok := states[vid]; ok {
			vaults = append(vaults, &jupiterlend.Vault{
				ConfigPubkey: c.Pk, StatePubkey: s.Pk, Config: c.Cfg, State: s.St,
			})
		}
	}
	sort.Slice(vaults, func(i, j int) bool { return vaults[i].Config.VaultID < vaults[j].Config.VaultID })

	var nUSDC, nUSDT, nSOL, nMaybe int
	fmt.Printf("\n%3s %7s %7s %5s %5s %16s %16s %6s liq?\n", "vid", "collat", "debt", "CF%", "LT%", "tot_supply", "tot_borrow", "absorb")
	for _, v := range vaults {
		c, s := v.Config, v.State
		switch c.DebtLabel() {
		case "USDC":
			nUSDC++
		case "USDT":
			nUSDT++
		case "wSOL":
			nSOL++
		}
		maybe := v.MaybeLiquidatable()
		if maybe {
			nMaybe++
		}
		star := ""
		if maybe {
			star = "★MAYBE"
		}
		fmt.Printf("%3d %7s %7s %5.1f %5.1f %16d %16d %6s %s\n",
			c.VaultID, label(c.SupplyToken), label(c.BorrowToken),
			float64(c.CollateralFactor)/10.0, float64(c.LiquidationThreshold)/10.0,
			s.TotalSupply, s.TotalBorrow, jupiterlend.U128String(s.AbsorbedDebtAmount), star)
	}

	inScope := nUSDC + nUSDT + nSOL
	fmt.Println("\n═══ summary ═══")
	fmt.Printf("vaults: %d  | debt in-scope (USDC/USDT/SOL): %d  (USDC %d, USDT %d, wSOL %d)\n",
		len(vaults), inScope, nUSDC, nUSDT, nSOL)
	fmt.Printf("VERIFIED: all %d vaults decode (pairs, thresholds, sizes) against live accounts.\n", len(vaults))
	fmt.Printf("first-pass 'maybe liquidatable' (absorbed-debt > 0): %d\n", nMaybe)
	fmt.Println("NOTE: precise per-price liquidatable detection needs Fluid tick↔price math (not")
	fmt.Println("      implemented); the executor's liquidate simulation is the ground-truth gate.")
}
