// Recon for the Save (formerly Solend) liquidation integration: derive the
// liquidate instruction from captured mainnet truth (the marginfi/Kamino
// lesson). Save is the original SPL token-lending model — a NATIVE program,
// so each instruction is identified by its first data byte (a u8 tag), not
// an 8-byte Anchor discriminator.
//
// Two passes over recent program txs: (1) histogram the instruction tags to
// see what exists and how often, (2) dump the first example of each tag with
// full account list + data, so we can identify the liquidate ix (the classic
// LiquidateObligation is tag 12; Solend's atomic
// LiquidateObligationAndRedeemReserveCollateral is a later tag) and its exact
// account layout before building anything.
//
// Usage: HELIUS_RPC=<url> [PAGES=3] go run ./cmd/save_liq_decode
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gagliardetto/solana-go/base58"

	"solana-arb-backtest-go/internal/envfile"
)

const solendProgram = "So1endDq2YkqhipRh3WViPa8hdiSpxWy6z3Z6tMCpAo"

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
func asStr(v any) string         { s, _ := v.(string); return s }

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
	pages := 3
	if v := os.Getenv("PAGES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			pages = n
		}
	}

	// Page back through recent program signatures.
	var sigs []string
	var before string
	for i := 0; i < pages; i++ {
		params := map[string]any{"limit": 1000}
		if before != "" {
			params["before"] = before
		}
		v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getSignaturesForAddress",
			"params": []any{solendProgram, params}})
		if !ok {
			break
		}
		page := asArray(v["result"])
		if len(page) == 0 {
			break
		}
		before = asStr(asMap(page[len(page)-1])["signature"])
		for _, ev := range page {
			e := asMap(ev)
			if e["err"] == nil {
				if s := asStr(e["signature"]); s != "" {
					sigs = append(sigs, s)
				}
			}
		}
		fmt.Fprintf(os.Stderr, "[save] paged: %d sigs\n", len(sigs))
	}

	// Targeted: dump the FULL tx for the liquidate tags only —
	// 12 = LiquidateObligation, 17 = LiquidateObligationAndRedeemReserveCollateral.
	// Print every Solend ix in the tx (tag + accounts + data) so we get the
	// liquidate account layout AND the surrounding refresh_reserve/obligation ixs.
	want := map[byte]bool{12: true, 17: true}
	target := 3
	if v := os.Getenv("TARGET"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			target = n
		}
	}
	found := 0
	for _, sig := range sigs {
		if found >= target {
			break
		}
		tx, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getTransaction",
			"params": []any{sig, map[string]any{"encoding": "jsonParsed", "maxSupportedTransactionVersion": 0, "commitment": "confirmed"}}})
		if !ok {
			continue
		}
		result := asMap(tx["result"])
		if result == nil {
			continue
		}
		message := asMap(asMap(result["transaction"])["message"])
		var ixs []map[string]any
		for _, ixv := range asArray(message["instructions"]) {
			ixs = append(ixs, asMap(ixv))
		}
		meta := asMap(result["meta"])
		for _, innerV := range asArray(meta["innerInstructions"]) {
			inner := asMap(innerV)
			for _, ixv := range asArray(inner["instructions"]) {
				ixs = append(ixs, asMap(ixv))
			}
		}
		hasLiq := false
		for _, ix := range ixs {
			if asStr(ix["programId"]) != solendProgram {
				continue
			}
			data, err := base58.Decode(asStr(ix["data"]))
			if err == nil && len(data) > 0 && want[data[0]] {
				hasLiq = true
				break
			}
		}
		if !hasLiq {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		found++
		fmt.Printf("\n════════ LIQUIDATION tx #%d: %s\n", found, sig)
		accountKeys := asArray(message["accountKeys"])
		feePayer := ""
		if len(accountKeys) > 0 {
			feePayer = asStr(asMap(accountKeys[0])["pubkey"])
		}
		fmt.Printf("  fee payer: %s\n", feePayer)
		for _, ix := range ixs {
			if asStr(ix["programId"]) != solendProgram {
				continue
			}
			data, err := base58.Decode(asStr(ix["data"]))
			if err != nil || len(data) == 0 {
				continue
			}
			tag := data[0]
			name := "?"
			switch tag {
			case 3:
				name = "RefreshReserve"
			case 7:
				name = "RefreshObligation"
			case 12:
				name = "LiquidateObligation"
			case 17:
				name = "LiquidateObligationAndRedeemReserveCollateral"
			}
			hexData := ""
			for _, b := range data {
				hexData += fmt.Sprintf("%02x", b)
			}
			fmt.Printf("  ── tag %d %s  (%dB data)  data=%s\n", tag, name, len(data), hexData)
			for i, a := range asArray(ix["accounts"]) {
				fmt.Printf("      [%2d] %s\n", i, asStr(a))
			}
		}
	}
	if found == 0 {
		fmt.Printf("no liquidation (tag 12/17) in %d sigs — raise PAGES\n", len(sigs))
	}
}
