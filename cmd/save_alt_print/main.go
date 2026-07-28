// Print the FIXED accounts a Save (Solend) liquidation fire tx needs in its
// dedicated address-lookup-table (SAVE_ALT), so the JupLend-flash-loan-wrapped
// `liquidate_and_redeem` + swap + payback tx fits under the 1232-byte
// single-packet limit. Without an ALT the wrapped cross-mint tx is ~1716–1936B
// (see the Save widen PR / save_fire_probe); moving these fixed accounts off
// the static keys (~31B saved each) brings it under 1232 — exactly as
// jup_alt_print / the Kamino ALT do for their paths.
//
// What's FIXED (goes in the ALT) vs per-fire (stays inline / rides Jupiter's ALTs):
//
//	FIXED  — programs + sysvars; the Solend main pool + its lending-market
//	         authority; and, for EACH supported debt asset (USDC/USDT/wSOL): the
//	         Solend debt (repay) reserve + its sub-accounts (liquidity supply,
//	         pyth/switchboard oracles, collateral mint/supply, fee receiver), the
//	         JupLend flash-market account set (reserve/token/rate_model/vault +
//	         globals), and the wallet's debt ATA. A given fire uses only ONE debt
//	         asset, but the ALT holds all three so any is covered.
//	PER-FIRE — the obligation, the COLLATERAL (withdraw) reserve + its
//	         sub-accounts, and the collateral→debt swap route (rides Jupiter's
//	         own ALTs). These vary per liquidation.
//
// The account lists are pulled from the REAL ix builders (flashloan.Borrow +
// the decoded Reserve fields), so they are guaranteed to match what
// BuildSaveFireTx actually references — no hand-maintained duplicate list.
//
// Setup (one-time; ALT creation needs wallet signing — do this on the box):
//
//	solana address-lookup-table create --keypair ~/arb-keypair.json -u <rpc>
//	solana address-lookup-table extend <TABLE> --addresses "$(save_alt_print | paste -sd, -)" …
//
// Then export SAVE_ALT=<TABLE> for liq_save_executor / save_fire_probe.
//
// Usage: HELIUS_RPC=<url> [AUTHORITY=<pk>] go run ./cmd/save_alt_print
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/flashloan"
	"solana-arb-backtest-go/internal/save"
)

const (
	defaultAuthority = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak"
	tokenProgramID   = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	token22ProgramID = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"
	ataProgramID     = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"
	systemProgramID  = "11111111111111111111111111111111"
	computeBudgetID  = "ComputeBudget111111111111111111111111111111"
	jupiterProgramID = "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4"
)

var httpClient = &http.Client{Timeout: 20 * time.Second}

func rpc(endpoint string, body map[string]any) (map[string]any, bool) {
	for attempt := 0; attempt < 4; attempt++ {
		b, _ := json.Marshal(body)
		resp, err := httpClient.Post(endpoint, "application/json", bytes.NewReader(b))
		if err == nil {
			raw, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr == nil {
				var v map[string]any
				if json.Unmarshal(raw, &v) == nil {
					return v, true
				}
			}
		}
		time.Sleep(time.Duration(400<<attempt) * time.Millisecond)
	}
	return nil, false
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func asArray(v any) []any        { a, _ := v.([]any); return a }

func getReserve(endpoint string, pk solana.PublicKey) (*save.Reserve, bool) {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{pk.String(), map[string]any{"encoding": "base64"}}})
	if !ok {
		return nil, false
	}
	data := asArray(asMap(asMap(v["result"])["value"])["data"])
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
	return save.DecodeReserve(pk, raw)
}

type ordered struct {
	seen map[string]bool
	out  []string
}

func (o *ordered) push(pk string) {
	if o.seen == nil {
		o.seen = map[string]bool{}
	}
	if !o.seen[pk] {
		o.seen[pk] = true
		o.out = append(o.out, pk)
	}
}

func main() {
	envfile.LoadDotEnv()

	endpoint := os.Getenv("HELIUS_RPC")
	if endpoint == "" {
		endpoint = os.Getenv("RPC_HTTP")
	}
	if endpoint == "" {
		fmt.Fprintln(os.Stderr, "set HELIUS_RPC (needed to read the debt reserves' oracle/supply sub-accounts)")
		os.Exit(1)
	}
	authorityStr := os.Getenv("AUTHORITY")
	if authorityStr == "" {
		authorityStr = defaultAuthority
	}
	authority, err := solana.PublicKeyFromBase58(authorityStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad AUTHORITY: %v\n", err)
		os.Exit(1)
	}

	mainPool := solana.MustPublicKeyFromBase58(save.MainPool)

	// Ordered, deduped accumulator (preserve first-seen order for readable output).
	acc := &ordered{}

	// ── programs + sysvars + Solend globals ──
	for _, s := range []string{
		save.SolendProgram,
		flashloan.JupLendProgram,
		jupiterProgramID,
		tokenProgramID,
		token22ProgramID,
		ataProgramID,
		systemProgramID,
		computeBudgetID,
		save.MainPool,
	} {
		acc.push(s)
	}
	acc.push(save.LendingMarketAuthority(mainPool).String())

	// ── per debt asset (USDC/USDT/wSOL): Solend reserve sub-accounts + JupLend flash set + ATA ──
	type debtAsset struct {
		label      string
		reserveStr string
		mintStr    string
	}
	debts := []debtAsset{
		{"USDC", save.USDCReserve, save.USDCMint},
		{"USDT", save.USDTReserve, save.USDTMint},
		{"wSOL", save.WSOLReserve, save.WSOLMint},
	}
	token := solana.MustPublicKeyFromBase58(tokenProgramID)
	for _, d := range debts {
		reservePk := solana.MustPublicKeyFromBase58(d.reserveStr)
		mint := solana.MustPublicKeyFromBase58(d.mintStr)

		// Solend debt-reserve fixed sub-accounts, straight from the decoded
		// reserve (these are exactly what refresh_reserve + liquidate_and_redeem
		// reference for the repay side).
		if r, ok := getReserve(endpoint, reservePk); ok {
			for _, pk := range []solana.PublicKey{
				r.Reserve,
				r.LiquidityMint,
				r.LiquiditySupply,
				r.PythOracle,
				r.SwitchboardOracle,
				r.CollateralMint,
				r.CollateralSupply,
				r.FeeReceiver,
			} {
				acc.push(pk.String())
			}
		} else {
			fmt.Fprintf(os.Stderr, "[save-alt] WARN could not fetch %s reserve %s — its sub-accounts are missing from this list; re-run with a working RPC\n", d.label, reservePk)
		}

		// JupLend flash-market fixed set — pulled from the REAL borrow ix so it
		// matches BuildSaveFireTx exactly (signer/ATA/mint/reserve/token/
		// rate_model/vault + JupLend globals).
		if ix, ok := flashloan.Borrow(authority, mint, 0); ok {
			for _, m := range ix.Accounts() {
				acc.push(m.PublicKey.String())
			}
		}

		// Wallet's debt ATA (classic SPL — USDC/USDT/wSOL are all classic).
		acc.push(flashloan.AtaFor(authority, mint, token).String())
	}

	// ── wallet ──
	acc.push(authority.String())

	for _, a := range acc.out {
		fmt.Println(a)
	}
	fmt.Fprintf(os.Stderr, "[save-alt] %d fixed accounts. Create the ALT + extend with these, then export SAVE_ALT=<table>.\n", len(acc.out))
	fmt.Fprintf(os.Stderr, "[save-alt] lending_market_authority = %s\n", save.LendingMarketAuthority(mainPool))
}
