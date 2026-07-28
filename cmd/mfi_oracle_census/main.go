// Census of marginfi bank oracles — groups the group's banks by oracle_setup
// and inspects each oracle account (owner program, size, disc) so we know
// exactly which decoders to build for full pricing coverage. Read-only.
//
// Usage: HELIUS_RPC=<url> go run ./cmd/mfi_oracle_census
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/liquidation"
)

const (
	marginfiProgram = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA"
	marginfiGroup   = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8"
	bankSize        = 1864
)

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

// b64 decodes the standard ["<base64>", "base64"] account-data tuple.
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

	// Banks of the main group (Bank.group at offset 41).
	resp, _ := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{marginfiProgram, map[string]any{"encoding": "base64",
			"filters": []any{
				map[string]any{"dataSize": bankSize},
				map[string]any{"memcmp": map[string]any{"offset": 41, "bytes": marginfiGroup}},
			}}}})
	entries := asArray(asMap(resp)["result"])
	fmt.Printf("%d banks in group\n", len(entries))

	type bankEntry struct {
		pk   solana.PublicKey
		bank *liquidation.Bank
	}
	bySetup := map[uint8][]bankEntry{}
	for _, ev := range entries {
		e := asMap(ev)
		pkStr := asStr(e["pubkey"])
		pk, err := solana.PublicKeyFromBase58(pkStr)
		if err != nil {
			continue
		}
		raw, ok := b64(asMap(e["account"])["data"])
		if !ok {
			continue
		}
		bank, ok := liquidation.DecodeBank(raw)
		if !ok {
			continue
		}
		bySetup[bank.OracleSetup] = append(bySetup[bank.OracleSetup], bankEntry{pk, bank})
	}

	var setups []uint8
	for s := range bySetup {
		setups = append(setups, s)
	}
	sort.Slice(setups, func(i, j int) bool { return setups[i] < setups[j] })

	for _, setup := range setups {
		banks := bySetup[setup]
		fmt.Printf("\n──── oracle_setup=%d (%d banks)\n", setup, len(banks))
		n := len(banks)
		if n > 6 {
			n = 6
		}
		for _, be := range banks[:n] {
			info, _ := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
				"params": []any{be.bank.OracleKey.String(), map[string]any{"encoding": "base64"}}})
			owner := "?"
			length := 0
			disc := ""
			if info != nil {
				val := asMap(asMap(info["result"])["value"])
				if val != nil {
					if o := asStr(val["owner"]); o != "" {
						owner = o
					} else {
						owner = "MISSING"
					}
					if data, ok := b64(val["data"]); ok {
						length = len(data)
						nb := len(data)
						if nb > 8 {
							nb = 8
						}
						for _, b := range data[:nb] {
							disc += fmt.Sprintf("%02x", b)
						}
					}
				} else {
					owner = "MISSING"
				}
			}
			pkStr := be.pk.String()
			mintStr := be.bank.Mint.String()
			fmt.Printf("  bank %s…  mint %s…  oracle %s  owner %s  len %d disc %s\n",
				pkStr[:8], mintStr[:8], be.bank.OracleKey.String(), owner, length, disc)
		}
		if len(banks) > 6 {
			fmt.Printf("  … +%d more\n", len(banks)-6)
		}
	}
}
