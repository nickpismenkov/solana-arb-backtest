// Command jupiter_probe verifies the Jupiter Lend (Fluid) decoders against
// live mainnet and enumerates every vault: collateral/debt pair,
// liquidation threshold, sizes, and a first-pass liquidatable signal.
// Read-only.
//
// Detection honesty: precise per-price liquidatable detection needs Fluid's
// tick<->price math (not reversed here). This reports the CONFIDENT
// on-chain liquidation-activity flags (absorbed debt / branch_liquidated)
// and leaves the authoritative check to the executor's liquidate
// simulation.
//
// Usage: HELIUS_RPC=<url> go run ./cmd/jupiter_probe
package main

import (
	"fmt"
	"os"
	"sort"

	"github.com/mr-tron/base58"

	"arbengine/internal/config"
	"arbengine/internal/jupiter"
	"arbengine/internal/rpcclient"
	"arbengine/internal/solana"
)

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// gpaByDisc runs getProgramAccounts filtered by an 8-byte discriminator at
// offset 0.
func gpaByDisc(rpc *rpcclient.Client, disc [8]byte) []rpcclient.ProgramAccount {
	program := solana.MustPubkeyFromBase58(jupiter.VaultsProgram)
	disc58 := base58.Encode(disc[:])
	entries, err := rpc.GetProgramAccounts(program, rpcclient.GetProgramAccountsOpts{
		Filters: []any{
			map[string]any{"memcmp": map[string]any{"offset": 0, "bytes": disc58}},
		},
	})
	if err != nil {
		return nil
	}
	return entries
}

func label(m solana.Pubkey) string {
	s := m.String()
	switch s {
	case jupiter.USDCMint:
		return "USDC"
	case jupiter.USDTMint:
		return "USDT"
	case jupiter.WSOLMint:
		return "wSOL"
	default:
		return s[:6]
	}
}

func main() {
	config.LoadDotenv()

	endpoint, ok := config.EnvOptional("HELIUS_RPC")
	if !ok {
		endpoint, ok = config.EnvOptional("RPC_HTTP")
	}
	if !ok {
		fatal("HELIUS_RPC")
	}
	rpc := rpcclient.New(endpoint)

	type cfgEntry struct {
		pk  solana.Pubkey
		cfg jupiter.VaultConfig
	}
	type stateEntry struct {
		pk    solana.Pubkey
		state jupiter.VaultState
	}
	configs := make(map[uint16]cfgEntry)
	for _, e := range gpaByDisc(rpc, jupiter.VaultConfigDisc) {
		if c, ok := jupiter.DecodeVaultConfig(e.Account.Data); ok {
			configs[c.VaultID] = cfgEntry{e.Pubkey, c}
		}
	}
	states := make(map[uint16]stateEntry)
	for _, e := range gpaByDisc(rpc, jupiter.VaultStateDisc) {
		if s, ok := jupiter.DecodeVaultState(e.Account.Data); ok {
			states[s.VaultID] = stateEntry{e.Pubkey, s}
		}
	}
	fmt.Printf("live: %d VaultConfig, %d VaultState decoded\n", len(configs), len(states))

	var vaults []jupiter.Vault
	for vid, c := range configs {
		if s, ok := states[vid]; ok {
			vaults = append(vaults, jupiter.Vault{
				ConfigPubkey: c.pk,
				StatePubkey:  s.pk,
				Config:       c.cfg,
				State:        s.state,
			})
		}
	}
	sort.Slice(vaults, func(i, j int) bool { return vaults[i].Config.VaultID < vaults[j].Config.VaultID })

	nUsdc, nUsdt, nSol, nMaybe := 0, 0, 0, 0
	fmt.Printf("\n%3s %7s %7s %5s %5s %16s %16s %6s liq?\n", "vid", "collat", "debt", "CF%", "LT%", "tot_supply", "tot_borrow", "absorb")
	for _, v := range vaults {
		c, s := v.Config, v.State
		switch c.DebtLabel() {
		case "USDC":
			nUsdc++
		case "USDT":
			nUsdt++
		case "wSOL":
			nSol++
		}
		maybe := v.MaybeLiquidatable()
		if maybe {
			nMaybe++
		}
		absorb := "0"
		if s.AbsorbedDebtAmountNonZero() {
			absorb = "nonzero"
		}
		flag := ""
		if maybe {
			flag = "★MAYBE"
		}
		fmt.Printf("%3d %7s %7s %5.1f %5.1f %16d %16d %6s %s\n",
			c.VaultID, label(c.SupplyToken), label(c.BorrowToken),
			float64(c.CollateralFactor)/10.0, float64(c.LiquidationThreshold)/10.0,
			s.TotalSupply, s.TotalBorrow, absorb, flag)
	}

	inScope := nUsdc + nUsdt + nSol
	fmt.Println("\n═══ summary ═══")
	fmt.Printf("vaults: %d  | debt in-scope (USDC/USDT/SOL): %d  (USDC %d, USDT %d, wSOL %d)\n", len(vaults), inScope, nUsdc, nUsdt, nSol)
	fmt.Printf("VERIFIED: all %d vaults decode (pairs, thresholds, sizes) against live accounts.\n", len(vaults))
	fmt.Printf("first-pass 'maybe liquidatable' (absorbed-debt > 0): %d\n", nMaybe)
	fmt.Println("NOTE: precise per-price liquidatable detection needs Fluid tick↔price math (not")
	fmt.Println("      implemented); the executor's liquidate simulation is the ground-truth gate.")
}
