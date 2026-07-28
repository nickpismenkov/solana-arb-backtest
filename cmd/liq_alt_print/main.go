// Print the constant accounts of every marginfi-USDC liquidation fire tx —
// the address set for the dedicated liquidation ALT (LIQ_ALT). Candidate-
// specific accounts (liquidatee, asset bank/mint/oracle/ATA, Jupiter route)
// stay static or come via Jupiter's own ALTs.
//
// Setup (one-time, ~0.002 SOL reclaimable rent):
//
//	solana address-lookup-table create --keypair ~/arb-keypair.json -u <rpc>
//	solana address-lookup-table extend <TABLE> --addresses "$(liq_alt_print | paste -sd, -)" …
//
// Usage: [HELIUS_RPC=<url>] go run ./cmd/liq_alt_print
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/flashloan"
	"solana-arb-backtest-go/internal/liquidation"
	"solana-arb-backtest-go/internal/marginfi"
)

const (
	defaultAuthority  = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak"
	defaultLiquidator = "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD"
	// USDC bank oracle (Pyth PriceUpdateV2) — cross-checked against the live
	// bank when HELIUS_RPC is set.
	usdcOracle      = "Dpw1EAVrSB1ibxiDQyTAW6Zip3J4Btk2x4SgApQCeFbX"
	jupiterProgram  = "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4"
	tokenProgram    = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	token2022       = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"
	ataProgram      = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"
	systemProgram   = "11111111111111111111111111111111"
	computeBudget   = "ComputeBudget111111111111111111111111111111"
	instructionsSys = "Sysvar1nstructions1111111111111111111111111"
)

func liveUSDCOracle(endpoint string) (solana.PublicKey, bool) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{marginfi.USDCBank, map[string]string{"encoding": "base64"}},
	})
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return solana.PublicKey{}, false
	}
	defer resp.Body.Close()
	var v map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return solana.PublicKey{}, false
	}
	result, _ := v["result"].(map[string]any)
	value, _ := result["value"].(map[string]any)
	dataArr, _ := value["data"].([]any)
	if len(dataArr) == 0 {
		return solana.PublicKey{}, false
	}
	s, ok := dataArr[0].(string)
	if !ok {
		return solana.PublicKey{}, false
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return solana.PublicKey{}, false
	}
	bank, ok := liquidation.DecodeBank(raw)
	if !ok {
		return solana.PublicKey{}, false
	}
	return bank.OracleKey, true
}

func main() {
	envfile.LoadDotEnv()

	authorityStr := os.Getenv("AUTHORITY")
	if authorityStr == "" {
		authorityStr = defaultAuthority
	}
	authority := solana.MustPublicKeyFromBase58(authorityStr)

	liquidatorStr := os.Getenv("LIQUIDATOR_MA")
	if liquidatorStr == "" {
		liquidatorStr = defaultLiquidator
	}
	liquidatorMA := solana.MustPublicKeyFromBase58(liquidatorStr)

	usdc := solana.MustPublicKeyFromBase58(marginfi.USDCMint)
	usdcBank := solana.MustPublicKeyFromBase58(marginfi.USDCBank)
	tp := solana.MustPublicKeyFromBase58(tokenProgram)

	oracle := usdcOracle
	if endpoint := os.Getenv("HELIUS_RPC"); endpoint != "" {
		if live, ok := liveUSDCOracle(endpoint); ok {
			if live.String() != usdcOracle {
				fmt.Fprintf(os.Stderr, "⚠ live USDC oracle %s differs from constant %s — using live\n", live, usdcOracle)
			}
			oracle = live.String()
		}
	}

	addrs := []string{
		marginfi.MarginfiProgram,
		marginfi.MarginfiGroup,
		marginfi.USDCBank,
		marginfi.USDCMint,
		marginfi.BankLiquidityVault(usdcBank).String(),
		marginfi.BankLiquidityVaultAuth(usdcBank).String(),
		marginfi.BankInsuranceVault(usdcBank).String(),
		oracle,
		authority.String(),
		liquidatorMA.String(),
		flashloan.AtaFor(authority, usdc, tp).String(),
		tp.String(),
		token2022, // Token-2022
		ataProgram,
		systemProgram,
		computeBudget,
		instructionsSys,
		jupiterProgram,
	}
	for _, a := range addrs {
		fmt.Println(a)
	}
}
