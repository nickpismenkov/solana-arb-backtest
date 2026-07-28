// Print the FIXED accounts a Jupiter Lend (Fluid) liquidation fire tx needs in
// its dedicated address-lookup-table (JUP_ALT), so the marginfi-flash-loan-
// wrapped `liquidate`+swap+repay tx fits under the 1232-byte single-packet limit.
// Without an ALT the wrapped tx is ~1723B (see jupiter_fire_probe STAGE 5);
// moving these ~24 fixed accounts off the static keys (~31B saved each) brings it
// under 1232, exactly as SAVE_ALT / the Kamino ALT do for their paths.
//
// What's FIXED (goes in the ALT) vs per-fire (stays inline / from Jupiter's ALTs):
//
//	FIXED  — programs + sysvars, the marginfi USDC flash-loan set, the Fluid
//	         liquidity global PDA, the USDC *borrow-side* per-mint liquidity
//	         accounts (reserve / rate_model / liquidity token vault — identical
//	         for every USDC-debt vault), the Fluid oracle program, and the
//	         wallet + its USDC ATA.
//	PER-FIRE — vault_config/state, oracle (+ its price sources), the collateral
//	         (supply) mint and its reserve/position/rate_model/token vault, the
//	         vault's borrow position, new_branch, and the tick/branch/tick_has_debt
//	         remaining accounts. These vary per vault; the collateral swap route
//	         rides Jupiter's own ALTs. (A future per-collateral ALT could fold the
//	         common collateral side in too, like the Kamino top-K approach.)
//
// Setup (one-time; ALT creation needs wallet signing — do this on the box):
//
//	solana address-lookup-table create --keypair ~/arb-keypair.json -u <rpc>
//	solana address-lookup-table extend <TABLE> --addresses "$(jup_alt_print | paste -sd, -)" …
//
// Then export JUP_ALT=<TABLE> for liq_jupiter_executor / jupiter_fire_probe.
//
// Usage: [HELIUS_RPC=<url>] [AUTHORITY=<pk>] [LIQUIDATOR_MA=<pk>] go run ./cmd/jup_alt_print
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/base58"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/flashloan"
	"solana-arb-backtest-go/internal/jupiterlend"
	"solana-arb-backtest-go/internal/marginfi"
)

const (
	defaultAuthority    = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak"
	defaultLiquidatorMA = "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD"
	tokenProgramID      = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	token22ProgramID    = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"
	ataProgramID        = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"
	systemProgramID     = "11111111111111111111111111111111"
	computeBudgetID     = "ComputeBudget111111111111111111111111111111"
	jupiterProgramID    = "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4"
	// Fluid oracle program (owner of every vault `oracle` account; constant on-chain,
	// verified: it is both the `oracle_program` config field and the oracle owner).
	oracleProgramID = "jupnw4B6Eqs7ft6rxpzYLJZYSnrpRgPcr589n5Kv4oc"
)

func main() {
	envfile.LoadDotEnv()

	authorityStr := os.Getenv("AUTHORITY")
	if authorityStr == "" {
		authorityStr = defaultAuthority
	}
	authority, err := solana.PublicKeyFromBase58(authorityStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad AUTHORITY: %v\n", err)
		os.Exit(1)
	}
	liquidatorMAStr := os.Getenv("LIQUIDATOR_MA")
	if liquidatorMAStr == "" {
		liquidatorMAStr = defaultLiquidatorMA
	}
	liquidatorMA, err := solana.PublicKeyFromBase58(liquidatorMAStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad LIQUIDATOR_MA: %v\n", err)
		os.Exit(1)
	}
	usdc := solana.MustPublicKeyFromBase58(marginfi.USDCMint)
	usdcBank := solana.MustPublicKeyFromBase58(marginfi.USDCBank)
	token := solana.MustPublicKeyFromBase58(tokenProgramID)

	// Fluid liquidity USDC borrow-side (identical for every USDC-debt vault).
	liquidity := jupiterlend.LiquidityPDA()
	usdcReserve := jupiterlend.ReservePDA(usdc)
	usdcRateModel := jupiterlend.RateModelPDA(usdc)
	usdcVaultTokAcct := flashloan.AtaFor(liquidity, usdc, token) // vault_borrow_token_account

	// Fluid oracle program: resolve live from any vault's oracle owner if we
	// have RPC (authoritative); else fall back to the known constant.
	oracleProgram := liveOracleProgram()
	if oracleProgram == "" {
		oracleProgram = oracleProgramID
	}

	addrs := []string{
		// ── programs + sysvars ──
		jupiterlend.VaultsProgram,
		jupiterlend.LiquidityProgram,
		oracleProgram,
		jupiterProgramID,
		marginfi.MarginfiProgram,
		tokenProgramID,
		token22ProgramID,
		ataProgramID,
		systemProgramID,
		computeBudgetID,
		// ── marginfi USDC flash-loan fixed set ──
		marginfi.MarginfiGroup,
		marginfi.USDCBank,
		marginfi.BankLiquidityVault(usdcBank).String(),
		marginfi.BankLiquidityVaultAuth(usdcBank).String(),
		marginfi.BankInsuranceVault(usdcBank).String(),
		// ── Fluid liquidity USDC borrow-side (per-mint; same for all USDC vaults) ──
		liquidity.String(),
		usdcReserve.String(),
		usdcRateModel.String(),
		usdcVaultTokAcct.String(),
		// ── wallet + USDC ──
		marginfi.USDCMint,
		authority.String(),
		liquidatorMA.String(),
		flashloan.AtaFor(authority, usdc, token).String(),
	}

	// Dedup, preserve order.
	seen := map[string]bool{}
	n := 0
	for _, a := range addrs {
		if seen[a] {
			continue
		}
		seen[a] = true
		fmt.Println(a)
		n++
	}
	fmt.Fprintf(os.Stderr, "[jup-alt] %d fixed accounts. Extend the JUP_ALT with these, then export JUP_ALT=<table>.\n", n)
	fmt.Fprintf(os.Stderr, "[jup-alt] liquidity global PDA = %s\n", liquidity)
	fmt.Fprintf(os.Stderr, "[jup-alt] USDC reserve = %s  rate_model = %s  vault_tok_acct = %s\n",
		usdcReserve, usdcRateModel, usdcVaultTokAcct)
}

// liveOracleProgram reads the oracle program id as the OWNER of any vault's
// oracle account (ground truth), if RPC is available; returns "" otherwise.
func liveOracleProgram() string {
	endpoint := os.Getenv("HELIUS_RPC")
	if endpoint == "" {
		endpoint = os.Getenv("RPC_HTTP")
	}
	if endpoint == "" {
		return ""
	}
	disc58 := base58.Encode(jupiterlend.VaultConfigDisc[:])
	v, ok := postJSON(endpoint, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{jupiterlend.VaultsProgram, map[string]any{
			"encoding":  "base64",
			"dataSlice": map[string]any{"offset": 0, "length": 0},
			"filters":   []any{map[string]any{"memcmp": map[string]any{"offset": 0, "bytes": disc58}}},
		}},
	})
	if !ok {
		return ""
	}
	result, _ := v["result"].([]any)
	if len(result) == 0 {
		return ""
	}
	first, _ := result[0].(map[string]any)
	cfgPk, _ := first["pubkey"].(string)
	if cfgPk == "" {
		return ""
	}

	cv, ok := postJSON(endpoint, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{cfgPk, map[string]any{"encoding": "base64"}},
	})
	if !ok {
		return ""
	}
	raw, ok := decodeAccountData(cv)
	if !ok {
		return ""
	}
	cfg, ok := jupiterlend.DecodeVaultConfig(raw)
	if !ok {
		return ""
	}

	ov, ok := postJSON(endpoint, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{cfg.Oracle.String(), map[string]any{"encoding": "base64"}},
	})
	if !ok {
		return ""
	}
	result2, _ := ov["result"].(map[string]any)
	value, _ := result2["value"].(map[string]any)
	owner, _ := value["owner"].(string)
	return owner
}

func decodeAccountData(v map[string]any) ([]byte, bool) {
	result, _ := v["result"].(map[string]any)
	value, _ := result["value"].(map[string]any)
	data, _ := value["data"].([]any)
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

func postJSON(endpoint string, body map[string]any) (map[string]any, bool) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, false
	}
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(b))
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false
	}
	var v map[string]any
	if json.Unmarshal(raw, &v) != nil {
		return nil, false
	}
	return v, true
}
