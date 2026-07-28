// Package jupswap is a Jupiter swap API client (lite-api.jup.ag, keyless) —
// quote + composable swap instructions for ARBITRARY mint pairs. Built for
// the liquidation fire path (seized collateral → debt token can be any mint,
// unlike the arb path's fixed pool basket).
package jupswap

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

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/arb"
)

// apiBase defaults to the keyless lite-api (rate-limited); override with
// JUP_API_BASE to a Pro endpoint (e.g. https://api.jup.ag) under heavy load.
func apiBase() string {
	if v, ok := os.LookupEnv("JUP_API_BASE"); ok {
		return v
	}
	return "https://lite-api.jup.ag"
}
func quoteURL() string  { return apiBase() + "/swap/v1/quote" }
func swapIxURL() string { return apiBase() + "/swap/v1/swap-instructions" }

// apiKey is the Jupiter API key (paid api.jup.ag tier). When set, sent as
// x-api-key on every request — lifts the keyless lite-api rate limit.
func apiKey() string { return os.Getenv("JUP_API_KEY") }

// maxAttempts is the number of HTTP attempts (JUP_MAX_RETRIES = retries, so
// attempts = retries+1). Default 5 attempts.
func maxAttempts() int {
	if v, ok := os.LookupEnv("JUP_MAX_RETRIES"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n + 1
		}
	}
	return 5
}

// httpTimeout is an optional hard per-request timeout (JUP_HTTP_TIMEOUT_MS).
func httpTimeout() (time.Duration, bool) {
	if v, ok := os.LookupEnv("JUP_HTTP_TIMEOUT_MS"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Millisecond, true
		}
	}
	return 0, false
}

func client() *http.Client {
	c := &http.Client{}
	if t, ok := httpTimeout(); ok {
		c.Timeout = t
	}
	return c
}

func doReq(req *http.Request) (map[string]any, error) {
	if k := apiKey(); k != "" {
		req.Header.Set("x-api-key", k)
	}
	resp, err := client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, retriableErr{code: resp.StatusCode, body: string(raw)}
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

type retriableErr struct {
	code int
	body string
}

func (e retriableErr) Error() string { return fmt.Sprintf("HTTP %d: %s", e.code, e.body) }

// getJSONRetry does a GET with exponential backoff on 429 / 5xx (the
// lite-api throttles under load).
func getJSONRetry(url string) (map[string]any, error) {
	attempts := maxAttempts()
	delay := 150 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		v, err := doReq(req)
		if err == nil {
			return v, nil
		}
		lastErr = err
		if re, ok := err.(retriableErr); ok && (re.code == 429 || re.code >= 500) && attempt+1 < attempts {
			time.Sleep(delay)
			delay *= 2
			continue
		}
		return nil, err
	}
	return nil, fmt.Errorf("jup GET: exhausted retries (429/5xx): %w", lastErr)
}

// postJSONRetry does a POST with the same backoff.
func postJSONRetry(url string, body any) (map[string]any, error) {
	attempts := maxAttempts()
	delay := 150 * time.Millisecond
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		v, err := doReq(req)
		if err == nil {
			return v, nil
		}
		lastErr = err
		if re, ok := err.(retriableErr); ok && (re.code == 429 || re.code >= 500) && attempt+1 < attempts {
			time.Sleep(delay)
			delay *= 2
			continue
		}
		return nil, err
	}
	return nil, fmt.Errorf("jup POST: exhausted retries (429/5xx): %w", lastErr)
}

// SwapPlan is a quoted swap ready to splice into our own v0 transaction.
type SwapPlan struct {
	// Setup + swap + cleanup, in order. Jupiter's compute-budget ixs are
	// dropped — the enclosing tx owns its budget.
	Instructions []solana.Instruction
	AltAddresses []solana.PublicKey
	QuotedOut    uint64
	// Jupiter's own slippage floor (min-out for ExactIn); the fire path's
	// real guard is repay_all, this just reverts earlier/cheaper.
	MinOut uint64
}

// Quote does an ExactIn quote. maxAccounts bounds route complexity so the
// swap fits in a tx that already carries the flashloan + liquidate accounts.
func Quote(inputMint, outputMint solana.PublicKey, amountIn uint64, slippageBps uint32, maxAccounts int) (map[string]any, error) {
	url := fmt.Sprintf("%s?inputMint=%s&outputMint=%s&amount=%d&slippageBps=%d&swapMode=ExactIn&maxAccounts=%d",
		quoteURL(), inputMint, outputMint, amountIn, slippageBps, maxAccounts)
	v, err := getJSONRetry(url)
	if err != nil {
		return nil, err
	}
	if e, ok := v["error"]; ok && e != nil {
		return nil, fmt.Errorf("jup quote: %v", e)
	}
	if _, ok := v["outAmount"].(string); !ok {
		return nil, fmt.Errorf("jup quote: no route (%v)", v)
	}
	return v, nil
}

func decodeIx(v map[string]any) (solana.Instruction, error) {
	progStr, _ := v["programId"].(string)
	if progStr == "" {
		return nil, fmt.Errorf("ix missing programId")
	}
	programID, err := solana.PublicKeyFromBase58(progStr)
	if err != nil {
		return nil, err
	}
	var accounts solana.AccountMetaSlice
	if arr, ok := v["accounts"].([]any); ok {
		for _, av := range arr {
			am, _ := av.(map[string]any)
			pkStr, _ := am["pubkey"].(string)
			if pkStr == "" {
				return nil, fmt.Errorf("acct missing pubkey")
			}
			pk, err := solana.PublicKeyFromBase58(pkStr)
			if err != nil {
				return nil, err
			}
			isSigner, _ := am["isSigner"].(bool)
			isWritable, _ := am["isWritable"].(bool)
			accounts = append(accounts, &solana.AccountMeta{PublicKey: pk, IsSigner: isSigner, IsWritable: isWritable})
		}
	}
	dataStr, _ := v["data"].(string)
	data, err := base64.StdEncoding.DecodeString(dataStr)
	if err != nil {
		return nil, err
	}
	return solana.NewInstruction(programID, accounts, data), nil
}

// SwapInstructions turns a quote into instructions signable by user. wrapSol
// only matters when a leg is native SOL: the fire path swaps token ATAs
// directly (false); a wallet-balance swap needs the wrap (true).
func SwapInstructions(quote map[string]any, user solana.PublicKey, wrapSol bool) (*SwapPlan, error) {
	body := map[string]any{
		"quoteResponse":           quote,
		"userPublicKey":           user.String(),
		"wrapAndUnwrapSol":        wrapSol,
		"dynamicComputeUnitLimit": false,
	}
	v, err := postJSONRetry(swapIxURL(), body)
	if err != nil {
		return nil, err
	}
	if _, ok := v["swapInstruction"].(map[string]any); !ok {
		return nil, fmt.Errorf("jup swap-instructions: %v", v)
	}
	var instructions []solana.Instruction
	if arr, ok := v["setupInstructions"].([]any); ok {
		for _, ixv := range arr {
			ix, err := decodeIx(ixv.(map[string]any))
			if err != nil {
				return nil, err
			}
			instructions = append(instructions, ix)
		}
	}
	swapIx, err := decodeIx(v["swapInstruction"].(map[string]any))
	if err != nil {
		return nil, err
	}
	instructions = append(instructions, swapIx)
	if cleanup, ok := v["cleanupInstruction"].(map[string]any); ok {
		ix, err := decodeIx(cleanup)
		if err != nil {
			return nil, err
		}
		instructions = append(instructions, ix)
	}
	var altAddrs []solana.PublicKey
	if arr, ok := v["addressLookupTableAddresses"].([]any); ok {
		for _, a := range arr {
			if s, ok := a.(string); ok {
				if pk, err := solana.PublicKeyFromBase58(s); err == nil {
					altAddrs = append(altAddrs, pk)
				}
			}
		}
	}
	quotedOut := parseU64(quote["outAmount"])
	minOut := parseU64(quote["otherAmountThreshold"])
	return &SwapPlan{Instructions: instructions, AltAddresses: altAddrs, QuotedOut: quotedOut, MinOut: minOut}, nil
}

func parseU64(v any) uint64 {
	s, ok := v.(string)
	if !ok {
		return 0
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// altCache caches address-lookup-tables: STATIC, so RPC-fetch each once and
// reuse from RAM. Removes a ~45ms getMultipleAccounts from every fire build.
var (
	altCacheMu sync.RWMutex
	altCache   = map[solana.PublicKey]*arb.ALT{}
)

func fetchOneAlt(endpoint string, addr solana.PublicKey) (*arb.ALT, error) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{addr.String(), map[string]string{"encoding": "base64"}},
	})
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var v struct {
		Result struct {
			Value struct {
				Data []string `json:"data"`
			} `json:"value"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, err
	}
	if len(v.Result.Value.Data) == 0 {
		return nil, fmt.Errorf("ALT %s not found", addr)
	}
	data, err := base64.StdEncoding.DecodeString(v.Result.Value.Data[0])
	if err != nil {
		return nil, err
	}
	return arb.LoadAlt(addr.String(), data), nil
}

func FetchAlts(endpoint string, addrs []solana.PublicKey) ([]*arb.ALT, error) {
	out := make([]*arb.ALT, 0, len(addrs))
	for _, a := range addrs {
		altCacheMu.RLock()
		cached, ok := altCache[a]
		altCacheMu.RUnlock()
		if ok {
			out = append(out, cached)
			continue
		}
		alt, err := fetchOneAlt(endpoint, a) // one-time RPC, then cached forever
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
