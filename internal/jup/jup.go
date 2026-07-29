// Package jup is the Jupiter swap API client (lite-api.jup.ag, keyless) —
// quote + composable swap instructions for ARBITRARY mint pairs. Built for
// the liquidation fire path (seized collateral -> debt token can be any
// mint, unlike the arb path's fixed pool basket). The arb hot path keeps
// its direct Orca/Ray builders (no HTTP hop); liquidations are
// block-granularity, so a quote round-trip at build time is affordable.
package jup

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"arbengine/internal/arb"
	"arbengine/internal/solana"
)

// apiBase is the API base. Defaults to the keyless lite-api
// (rate-limited); override with JUP_API_BASE to a Pro endpoint (e.g.
// https://api.jup.ag) under heavy load — running several executors hammers
// lite-api and gets 429'd.
func apiBase() string {
	if v := os.Getenv("JUP_API_BASE"); v != "" {
		return v
	}
	return "https://lite-api.jup.ag"
}

func quoteURL() string  { return apiBase() + "/swap/v1/quote" }
func swapIxURL() string { return apiBase() + "/swap/v1/swap-instructions" }

// apiKey returns the Jupiter API key (paid api.jup.ag tier), if set. When
// set, sent as `x-api-key` on every request — lifts the keyless lite-api
// rate limit that otherwise 429s live fires.
func apiKey() (string, bool) {
	k := os.Getenv("JUP_API_KEY")
	return k, k != ""
}

// maxAttempts is the number of HTTP attempts (JUP_MAX_RETRIES = retries, so
// attempts = retries+1). Default 5 attempts (block-granular backtest /
// batch use). The FIRE PATH sets JUP_MAX_RETRIES=0 so a 429 fails INSTANTLY
// instead of sleeping 150+300+600+1200+2400~=4.65s — a >300ms fire has
// already lost the race, and a multi-second hang also holds a MAX_INFLIGHT
// slot, starving the fast direct-DEX fires.
func maxAttempts() int {
	if v := os.Getenv("JUP_MAX_RETRIES"); v != "" {
		if r, err := strconv.ParseUint(v, 10, 32); err == nil {
			return int(r) + 1
		}
	}
	return 5
}

// httpTimeout is the optional hard per-request timeout
// (JUP_HTTP_TIMEOUT_MS). The fire path sets a tight bound so a
// slow/stalled Jupiter never parks a firing thread for seconds. Returns
// (timeout, true) if set.
func httpTimeout() (time.Duration, bool) {
	v := os.Getenv("JUP_HTTP_TIMEOUT_MS")
	if v == "" {
		return 0, false
	}
	ms, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return time.Duration(ms) * time.Millisecond, true
}

func httpClient() *http.Client {
	c := &http.Client{}
	if t, ok := httpTimeout(); ok {
		c.Timeout = t
	}
	return c
}

// getJSONRetry does a GET with exponential backoff on 429 / 5xx (the
// lite-api throttles under load).
func getJSONRetry(url string) (map[string]any, error) {
	attempts := maxAttempts()
	delay := 150 * time.Millisecond
	client := httpClient()
	for attempt := 0; attempt < attempts; attempt++ {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		if k, ok := apiKey(); ok {
			req.Header.Set("x-api-key", k)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			var v map[string]any
			if err := json.Unmarshal(body, &v); err != nil {
				return nil, err
			}
			return v, nil
		}
		if (resp.StatusCode == 429 || resp.StatusCode >= 500) && attempt+1 < attempts {
			time.Sleep(delay)
			delay *= 2
			continue
		}
		return nil, fmt.Errorf("jup GET %d: %s", resp.StatusCode, string(body))
	}
	return nil, fmt.Errorf("jup GET: exhausted retries (429/5xx)")
}

// postJSONRetry does a POST with the same backoff.
func postJSONRetry(url string, body map[string]any) (map[string]any, error) {
	attempts := maxAttempts()
	delay := 150 * time.Millisecond
	client := httpClient()
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < attempts; attempt++ {
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if k, ok := apiKey(); ok {
			req.Header.Set("x-api-key", k)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			var v map[string]any
			if err := json.Unmarshal(respBody, &v); err != nil {
				return nil, err
			}
			return v, nil
		}
		if (resp.StatusCode == 429 || resp.StatusCode >= 500) && attempt+1 < attempts {
			time.Sleep(delay)
			delay *= 2
			continue
		}
		return nil, fmt.Errorf("jup POST %d: %s", resp.StatusCode, string(respBody))
	}
	return nil, fmt.Errorf("jup POST: exhausted retries (429/5xx)")
}

// SwapPlan is a quoted swap ready to splice into our own v0 transaction.
type SwapPlan struct {
	// Instructions is setup + swap + cleanup, in order. Jupiter's
	// compute-budget ixs are dropped — the enclosing tx owns its budget.
	Instructions []solana.Instruction
	ALTAddresses []solana.Pubkey
	QuotedOut    uint64
	// MinOut is Jupiter's own slippage floor (min-out for ExactIn); the
	// fire path's real guard is repay_all, this just reverts
	// earlier/cheaper.
	MinOut uint64
}

// Quote issues an ExactIn quote. maxAccounts bounds route complexity so the
// swap fits in a tx that already carries the flashloan + liquidate
// accounts.
func Quote(inputMint, outputMint solana.Pubkey, amountIn uint64, slippageBps uint32, maxAccounts int) (map[string]any, error) {
	url := fmt.Sprintf(
		"%s?inputMint=%s&outputMint=%s&amount=%d&slippageBps=%d&swapMode=ExactIn&maxAccounts=%d",
		quoteURL(), inputMint.String(), outputMint.String(), amountIn, slippageBps, maxAccounts,
	)
	v, err := getJSONRetry(url)
	if err != nil {
		return nil, err
	}
	if e, ok := v["error"]; ok {
		return nil, fmt.Errorf("jup quote: %v", e)
	}
	if out, ok := v["outAmount"].(string); !ok || out == "" {
		return nil, fmt.Errorf("jup quote: no route (%v)", v)
	}
	return v, nil
}

func decodeIx(v map[string]any) (solana.Instruction, error) {
	programIDStr, _ := v["programId"].(string)
	if programIDStr == "" {
		return solana.Instruction{}, fmt.Errorf("ix missing programId")
	}
	programID, err := solana.PubkeyFromBase58(programIDStr)
	if err != nil {
		return solana.Instruction{}, err
	}
	var accounts []solana.AccountMeta
	if accs, ok := v["accounts"].([]any); ok {
		for _, a := range accs {
			am, ok := a.(map[string]any)
			if !ok {
				continue
			}
			pkStr, _ := am["pubkey"].(string)
			if pkStr == "" {
				return solana.Instruction{}, fmt.Errorf("acct missing pubkey")
			}
			pk, err := solana.PubkeyFromBase58(pkStr)
			if err != nil {
				return solana.Instruction{}, err
			}
			isSigner, _ := am["isSigner"].(bool)
			isWritable, _ := am["isWritable"].(bool)
			accounts = append(accounts, solana.AccountMeta{
				Pubkey:     pk,
				IsSigner:   isSigner,
				IsWritable: isWritable,
			})
		}
	}
	dataStr, _ := v["data"].(string)
	data, err := base64.StdEncoding.DecodeString(dataStr)
	if err != nil {
		return solana.Instruction{}, err
	}
	return solana.Instruction{ProgramID: programID, Accounts: accounts, Data: data}, nil
}

// SwapInstructions turns a quote into instructions signable by `user`.
// wrapSol only matters when a leg is native SOL: the fire path swaps token
// ATAs directly (false, marginfi withdraw lands wSOL in the wSOL ATA); a
// wallet-balance swap needs the wrap (true).
func SwapInstructions(quote map[string]any, user solana.Pubkey, wrapSol bool) (SwapPlan, error) {
	body := map[string]any{
		"quoteResponse":           quote,
		"userPublicKey":           user.String(),
		"wrapAndUnwrapSol":        wrapSol,
		"dynamicComputeUnitLimit": false,
	}
	v, err := postJSONRetry(swapIxURL(), body)
	if err != nil {
		return SwapPlan{}, err
	}
	swapIx, ok := v["swapInstruction"].(map[string]any)
	if !ok {
		return SwapPlan{}, fmt.Errorf("jup swap-instructions: %v", v)
	}
	var instructions []solana.Instruction
	if setup, ok := v["setupInstructions"].([]any); ok {
		for _, raw := range setup {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			ix, err := decodeIx(m)
			if err != nil {
				return SwapPlan{}, err
			}
			instructions = append(instructions, ix)
		}
	}
	ix, err := decodeIx(swapIx)
	if err != nil {
		return SwapPlan{}, err
	}
	instructions = append(instructions, ix)
	if cleanup, ok := v["cleanupInstruction"].(map[string]any); ok {
		ix, err := decodeIx(cleanup)
		if err != nil {
			return SwapPlan{}, err
		}
		instructions = append(instructions, ix)
	}
	var altAddresses []solana.Pubkey
	if alts, ok := v["addressLookupTableAddresses"].([]any); ok {
		for _, a := range alts {
			s, ok := a.(string)
			if !ok {
				continue
			}
			pk, err := solana.PubkeyFromBase58(s)
			if err != nil {
				continue
			}
			altAddresses = append(altAddresses, pk)
		}
	}
	quotedOut := parseU64Field(quote, "outAmount")
	minOut := parseU64Field(quote, "otherAmountThreshold")
	return SwapPlan{
		Instructions: instructions,
		ALTAddresses: altAddresses,
		QuotedOut:    quotedOut,
		MinOut:       minOut,
	}, nil
}

func parseU64Field(v map[string]any, key string) uint64 {
	s, ok := v[key].(string)
	if !ok {
		return 0
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// ALT cache: address-lookup-tables are STATIC (LIQ_ALT never changes), so
// RPC-fetch each once and reuse from RAM. Removes a ~45ms
// getMultipleAccounts from every fire build — the hot-path killer for
// on-the-fly (uncached) fires.
var (
	altCacheMu sync.RWMutex
	altCache   = map[solana.Pubkey]solana.AddressLookupTableAccount{}
)

type rpcAccountInfoResponse struct {
	Result struct {
		Value *struct {
			Data []any `json:"data"`
		} `json:"value"`
	} `json:"result"`
}

func fetchOneALT(endpoint string, addr solana.Pubkey) (solana.AddressLookupTableAccount, error) {
	reqBody, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "getAccountInfo",
		"params":  []any{addr.String(), map[string]string{"encoding": "base64"}},
	})
	if err != nil {
		return solana.AddressLookupTableAccount{}, err
	}
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return solana.AddressLookupTableAccount{}, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return solana.AddressLookupTableAccount{}, err
	}
	var parsed rpcAccountInfoResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return solana.AddressLookupTableAccount{}, err
	}
	if parsed.Result.Value == nil || len(parsed.Result.Value.Data) == 0 {
		return solana.AddressLookupTableAccount{}, fmt.Errorf("ALT %s not found", addr.String())
	}
	dataStr, ok := parsed.Result.Value.Data[0].(string)
	if !ok {
		return solana.AddressLookupTableAccount{}, fmt.Errorf("ALT %s not found", addr.String())
	}
	data, err := base64.StdEncoding.DecodeString(dataStr)
	if err != nil {
		return solana.AddressLookupTableAccount{}, fmt.Errorf("ALT %s not found", addr.String())
	}
	return arb.LoadALT(addr.String(), data), nil
}

// FetchALTs fetches + decodes the plan's lookup tables so the caller can
// compile a v0 message (addresses start at byte 56 of an ALT account).
func FetchALTs(endpoint string, addrs []solana.Pubkey) ([]solana.AddressLookupTableAccount, error) {
	out := make([]solana.AddressLookupTableAccount, 0, len(addrs))
	for _, a := range addrs {
		altCacheMu.RLock()
		alt, ok := altCache[a]
		altCacheMu.RUnlock()
		if ok {
			out = append(out, alt)
			continue
		}
		alt, err := fetchOneALT(endpoint, a) // one-time RPC, then cached forever
		if err != nil {
			return nil, err
		}
		altCacheMu.Lock()
		altCache[a] = alt
		altCacheMu.Unlock()
		out = append(out, alt)
	}
	return out, nil
}
