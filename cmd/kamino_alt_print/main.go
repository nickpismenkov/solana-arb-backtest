// Emit the address set for the Kamino liquidation ALT: fixed accounts
// (programs, sysvars, main market + authority + scope, USDC repay-reserve set,
// JupLend flash-loan constants, wallet + USDC ATA) plus the TOP-K collateral
// reserves by deposit frequency, each with its 5 liquidate sub-accounts. This
// compresses the fire tx under 1232 bytes for the common collateral; rare
// collateral falls back to inline (executor logs + skips if it overflows).
//
// Setup (one-time):
//
//	solana address-lookup-table create --keypair ~/arb-keypair.json -u <rpc>
//	solana address-lookup-table extend <TABLE> --addresses "$(kamino_alt_print | paste -sd, -)" …
//
// Usage: HELIUS_RPC=<url> [AUTHORITY=<pk>] [TOP_K=20] go run ./cmd/kamino_alt_print
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
	"solana-arb-backtest-go/internal/flashloan"
	"solana-arb-backtest-go/internal/kamino"
)

const (
	klend            = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"
	mainMarket       = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"
	usdcMint         = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	obligationSize   = 3344
	reserveSize      = 8624
	defaultAuthority = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak"
	tokenProgramID   = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
)

// JupLend flash-loan constants (from flashloan.go).
const jupLendProgram = "jupgfSgfuAXv4B6R2Uxu85Z1qdzgju79s6MfZekN6XS"

var jupM = []string{
	"ALXWtv2P4GqH1B7Lq731joag52yRBRqmHV4naiXPTYWL",
	"94vK29npVbyRHXH63rRcTiSr26SFhrQTzbpNJuhQEDu",
	"J9dyC4pBTBPvzzPh7J9rhFhg8RvgerDNKkUH9kEwGMsj",
	"5pjzT5dFTsXcwixoab1QDLvZQvpYJxJeBphkyfHGn688",
	"BmkUoKMFYBxNSzWXyUjyMJjMAaVz4d8ZnxwwmhDCUXFB",
	"7s1da8DduuBFqGra5bJBjpnvL5E9mGzCuMk1Qkh4or2Z",
	"jupeiUmn818Jg1ekPURTpr4mFo29p46vygyykFJ3wZC",
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

func rpc(endpoint string, body map[string]any) (map[string]any, bool) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, false
	}
	for attempt := 0; attempt < 4; attempt++ {
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
func b64(data any) ([]byte, bool) {
	arr, ok := data.([]any)
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

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func main() {
	envfile.LoadDotEnv()

	endpoint := os.Getenv("HELIUS_RPC")
	if endpoint == "" {
		endpoint = os.Getenv("RPC_HTTP")
	}
	if endpoint == "" {
		fail("HELIUS_RPC")
	}
	authority := os.Getenv("AUTHORITY")
	if authority == "" {
		authority = defaultAuthority
	}
	topK := 20
	if v := os.Getenv("TOP_K"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			topK = n
		}
	}
	market := solana.MustPublicKeyFromBase58(mainMarket)
	authPk, err := solana.PublicKeyFromBase58(authority)
	if err != nil {
		fail("bad AUTHORITY: %v", err)
	}
	usdc := solana.MustPublicKeyFromBase58(usdcMint)
	tokenProgram := solana.MustPublicKeyFromBase58(tokenProgramID)

	// Rank collateral reserves by deposit frequency across main-market obligations.
	resp, _ := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{klend, map[string]any{"encoding": "base64", "dataSlice": map[string]any{"offset": 0, "length": 2288},
			"filters": []any{map[string]any{"dataSize": obligationSize}, map[string]any{"memcmp": map[string]any{"offset": 32, "bytes": mainMarket}}}}}})
	entries := asArray(resp["result"])
	freq := map[solana.PublicKey]uint32{}
	for _, ev := range entries {
		e := asMap(ev)
		raw, ok := b64(asMap(e["account"])["data"])
		if !ok {
			continue
		}
		ob, ok := kamino.DecodeObligation(raw)
		if !ok {
			continue
		}
		for _, d := range ob.Deposits {
			freq[d.Reserve]++
		}
	}
	type rankEntry struct {
		reserve solana.PublicKey
		n       uint32
	}
	var ranked []rankEntry
	for r, n := range freq {
		ranked = append(ranked, rankEntry{r, n})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].n > ranked[j].n })
	var top []solana.PublicKey
	for i, r := range ranked {
		if i >= topK {
			break
		}
		top = append(top, r.reserve)
	}
	fmt.Fprintf(os.Stderr, "[alt] %d obligations, top %d collateral reserves by deposit count\n", len(entries), len(top))
	for i, r := range ranked {
		if i >= topK {
			break
		}
		fmt.Fprintf(os.Stderr, "  %s : %d\n", r.reserve, r.n)
	}

	// Fixed accounts.
	addrs := []string{
		klend,
		"FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr",
		"JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4",
		jupLendProgram,
		"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA",
		"TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb",
		"ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL",
		"11111111111111111111111111111111",
		"ComputeBudget111111111111111111111111111111",
		"Sysvar1nstructions1111111111111111111111111",
		mainMarket,
		kamino.LendingMarketAuthority(market).String(),
		usdcMint,
		authority,
		// USDC ATA (repay source + swap out).
		flashloan.AtaFor(authPk, usdc, tokenProgram).String(),
	}
	addrs = append(addrs, jupM...)

	// Find the main-market USDC reserve (always the v1 repay side).
	usdcResp, _ := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{klend, map[string]any{"encoding": "base64",
			"filters": []any{map[string]any{"dataSize": reserveSize},
				map[string]any{"memcmp": map[string]any{"offset": 32, "bytes": mainMarket}},
				map[string]any{"memcmp": map[string]any{"offset": 128, "bytes": usdcMint}}}}}})
	usdcEntries := asArray(usdcResp["result"])
	if len(usdcEntries) == 0 {
		fail("USDC reserve")
	}
	usdcReservePkStr := asStr(asMap(usdcEntries[0])["pubkey"])
	usdcReserve, err := solana.PublicKeyFromBase58(usdcReservePkStr)
	if err != nil {
		fail("USDC reserve")
	}
	fmt.Fprintf(os.Stderr, "[alt] USDC repay reserve: %s\n", usdcReserve)

	// USDC repay-reserve + top collateral reserves, each with its 5 sub-accounts.
	reservePks := append([]solana.PublicKey{usdcReserve}, top...)
	strs := make([]string, len(reservePks))
	for i, k := range reservePks {
		strs[i] = k.String()
	}
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getMultipleAccounts",
		"params": []any{strs, map[string]any{"encoding": "base64"}}})
	if !ok {
		fail("reserves")
	}
	result := asMap(v["result"])
	values := asArray(result["value"])
	for i, accV := range values {
		acc := asMap(accV)
		if acc == nil {
			continue
		}
		data, ok := b64(acc["data"])
		if !ok {
			continue
		}
		r, ok := kamino.DecodeReserveAccounts(reservePks[i], data)
		if !ok {
			continue
		}
		for _, a := range []solana.PublicKey{r.Reserve, r.LiquidityMint, r.LiquiditySupply, r.FeeReceiver, r.CollateralMint, r.CollateralSupply, r.ScopePrices} {
			addrs = append(addrs, a.String())
		}
	}

	// Dedup, preserve order.
	seen := map[string]bool{}
	for _, a := range addrs {
		if seen[a] {
			continue
		}
		seen[a] = true
		fmt.Println(a)
	}
}
