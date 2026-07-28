// Package jito handles bundle submission via the Jito block engine:
// getTipAccounts + sendBundle, plus a SOL tip-transfer instruction helper.
// Used only when going live — building/holding a bundle costs nothing; you
// pay only if it lands (and a guarded arb that isn't profitable reverts ->
// the bundle never lands).
package jito

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/mr-tron/base58"

	"arbengine/internal/solana"
)

// DefaultBlockEngine returns JITO_BLOCK_ENGINE or the Amsterdam default.
func DefaultBlockEngine() string {
	if v := os.Getenv("JITO_BLOCK_ENGINE"); v != "" {
		return v
	}
	return "https://amsterdam.mainnet.block-engine.jito.wtf"
}

// sharedClient reuses a connection-pooled HTTP client so submits reuse a
// warm TLS connection instead of paying a fresh handshake every time — the
// single biggest submit-latency win for a co-located box.
var sharedClient = &http.Client{
	Timeout: 5 * time.Second,
	Transport: &http.Transport{
		MaxIdleConnsPerHost: 4,
	},
}

func postJSON(url string, body any) (map[string]any, int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	resp, err := sharedClient.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, resp.StatusCode, err
	}
	return out, resp.StatusCode, nil
}

// GetTipAccounts fetches the current Jito tip accounts (pick one at random per bundle).
func GetTipAccounts(blockEngine string) ([]solana.Pubkey, error) {
	url := blockEngine + "/api/v1/bundles"
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getTipAccounts", "params": []any{}}
	resp, _, err := postJSON(url, body)
	if err != nil {
		return nil, err
	}
	arr, ok := resp["result"].([]any)
	if !ok {
		return nil, fmt.Errorf("jito: getTipAccounts: no result (%v)", resp)
	}
	out := make([]solana.Pubkey, 0, len(arr))
	for _, v := range arr {
		s, ok := v.(string)
		if !ok {
			continue
		}
		pk, err := solana.PubkeyFromBase58(s)
		if err != nil {
			continue
		}
		out = append(out, pk)
	}
	return out, nil
}

// SendSender submits a single signed tx via Helius Sender (dual-routes to
// validators + Jito for fast landing; no 1/sec Jito-unauth cap). Requires a
// tip >=0.0002 SOL as a transfer to a Jito tip account inside the tx (the
// caller already includes one). skipPreflight=true — Sender blasts, doesn't
// simulate; the on-chain guard is the real check. Returns the signature.
func SendSender(senderURL, txB64 string) (string, error) {
	body := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "sendTransaction",
		"params": []any{txB64, map[string]any{"encoding": "base64", "skipPreflight": true, "maxRetries": 0}},
	}
	resp, status, err := postJSON(senderURL, body)
	if err != nil {
		return "", err
	}
	if status >= 300 {
		return "", fmt.Errorf("jito: sender HTTP %d: %v", status, resp)
	}
	if e, ok := resp["error"]; ok && e != nil {
		return "", fmt.Errorf("jito: sender error: %v", e)
	}
	sig, ok := resp["result"].(string)
	if !ok {
		return "", fmt.Errorf("jito: sender: no signature")
	}
	return sig, nil
}

// BundleStatus is the post-hoc status of a submitted bundle: "Landed",
// "Failed" (dropped — e.g. our guard would revert, or we lost the race),
// "Pending", or "Invalid" (expired/never seen). Off the hot path — call
// seconds after firing. Returns "" if unavailable.
func BundleStatus(blockEngine, bundleID string) string {
	url := blockEngine + "/api/v1/getInflightBundleStatuses"
	body := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getInflightBundleStatuses",
		"params": []any{[]string{bundleID}},
	}
	resp, _, err := postJSON(url, body)
	if err != nil {
		return ""
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		return ""
	}
	value, ok := result["value"].([]any)
	if !ok || len(value) == 0 {
		return ""
	}
	entry, ok := value[0].(map[string]any)
	if !ok {
		return ""
	}
	status, _ := entry["status"].(string)
	return status
}

// SendBundle submits an atomic bundle (base64-encoded txs). Returns the
// bundle id. JITO_BUNDLE_ENCODING=base58 re-encodes and submits via Jito's
// default (base58) path instead — diagnostic for base64-path drops.
func SendBundle(blockEngine string, txsB64 []string) (string, error) {
	url := blockEngine + "/api/v1/bundles"
	useB58 := os.Getenv("JITO_BUNDLE_ENCODING") == "base58"

	var body map[string]any
	if useB58 {
		txsB58 := make([]string, len(txsB64))
		for i, b64 := range txsB64 {
			raw, _ := base64.StdEncoding.DecodeString(b64)
			txsB58[i] = base58.Encode(raw)
		}
		body = map[string]any{"jsonrpc": "2.0", "id": 1, "method": "sendBundle", "params": []any{txsB58}}
	} else {
		body = map[string]any{"jsonrpc": "2.0", "id": 1, "method": "sendBundle", "params": []any{txsB64, map[string]string{"encoding": "base64"}}}
	}

	resp, status, err := postJSON(url, body)
	if err != nil {
		return "", err
	}
	if status >= 300 {
		// Jito puts the real rejection reason in the error response body —
		// surface it instead of just the status line.
		return "", fmt.Errorf("jito: sendBundle HTTP %d: %v", status, resp)
	}
	if e, ok := resp["error"]; ok && e != nil {
		return "", fmt.Errorf("jito: sendBundle error: %v", e)
	}
	result, _ := resp["result"].(string)
	return result, nil
}
