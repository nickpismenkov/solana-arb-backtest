// Package jito implements Jito bundle submission via the block engine:
// getTipAccounts + sendBundle, plus a SOL tip-transfer instruction helper.
// Region-matched to the box (Amsterdam) by default. Used only when going
// live — building/holding a bundle costs nothing; you pay only if it lands.
package jito

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
	"github.com/gagliardetto/solana-go/base58"
)

func DefaultBlockEngine() string {
	if v, ok := os.LookupEnv("JITO_BLOCK_ENGINE"); ok {
		return v
	}
	return "https://amsterdam.mainnet.block-engine.jito.wtf"
}

// agent is a shared HTTP client with connection pooling / keep-alive, so
// submits reuse a warm TLS connection instead of paying a fresh handshake
// (~several ms) every time — the single biggest submit-latency win for a
// co-located box.
var agent = &http.Client{
	Timeout: 5 * time.Second,
	Transport: &http.Transport{
		MaxIdleConnsPerHost: 4,
	},
}

func postJSON(client *http.Client, url string, body any) (map[string]any, int, string, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, 0, "", err
	}
	resp, err := client.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		return nil, 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, string(raw), nil
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, resp.StatusCode, string(raw), err
	}
	return v, resp.StatusCode, string(raw), nil
}

// GetTipAccounts fetches the current Jito tip accounts (pick one at random per bundle).
func GetTipAccounts(blockEngine string) ([]solana.PublicKey, error) {
	url := blockEngine + "/api/v1/bundles"
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getTipAccounts", "params": []any{}}
	v, code, raw, err := postJSON(agent, url, body)
	if err != nil {
		return nil, err
	}
	if code >= 300 {
		return nil, fmt.Errorf("getTipAccounts HTTP %d: %s", code, raw)
	}
	arr, ok := v["result"].([]any)
	if !ok {
		return nil, fmt.Errorf("getTipAccounts: no result (%s)", raw)
	}
	var out []solana.PublicKey
	for _, s := range arr {
		if str, ok := s.(string); ok {
			if pk, err := solana.PublicKeyFromBase58(str); err == nil {
				out = append(out, pk)
			}
		}
	}
	return out, nil
}

// SendSender submits a single signed tx via Helius Sender (dual-routes to
// validators + Jito for fast landing; no 1/sec Jito-unauth cap). Requires a
// tip ≥0.0002 SOL as a transfer to a Jito tip account inside the tx.
// skipPreflight=true — Sender blasts, doesn't simulate; our guard is the real
// check. Returns the signature.
func SendSender(senderURL, txB64 string) (string, error) {
	body := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "sendTransaction",
		"params": []any{txB64, map[string]any{"encoding": "base64", "skipPreflight": true, "maxRetries": 0}},
	}
	v, code, raw, err := postJSON(agent, senderURL, body)
	if err != nil {
		return "", err
	}
	if code >= 300 {
		return "", fmt.Errorf("sender HTTP %d: %s", code, raw)
	}
	if e, ok := v["error"]; ok && e != nil {
		return "", fmt.Errorf("sender error: %v", e)
	}
	sig, _ := v["result"].(string)
	if sig == "" {
		return "", fmt.Errorf("sender: no signature")
	}
	return sig, nil
}

// BundleStatus returns the post-hoc status of a submitted bundle: "Landed",
// "Failed" (dropped), "Pending", or "Invalid" (expired/never seen). Off the
// hot path — call seconds after firing.
func BundleStatus(blockEngine, bundleID string) (string, bool) {
	url := blockEngine + "/api/v1/getInflightBundleStatuses"
	body := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getInflightBundleStatuses",
		"params": []any{[]string{bundleID}},
	}
	v, code, _, err := postJSON(http.DefaultClient, url, body)
	if err != nil || code >= 300 {
		return "", false
	}
	result, ok := v["result"].(map[string]any)
	if !ok {
		return "", false
	}
	value, ok := result["value"].([]any)
	if !ok || len(value) == 0 {
		return "", false
	}
	entry, ok := value[0].(map[string]any)
	if !ok {
		return "", false
	}
	status, ok := entry["status"].(string)
	return status, ok
}

// SendBundle submits an atomic bundle (base64-encoded txs). Returns the
// bundle id. JITO_BUNDLE_ENCODING=base58 re-encodes and submits via Jito's
// default (base58) path instead — diagnostic for base64-path drops.
func SendBundle(blockEngine string, txsB64 []string) (string, error) {
	url := blockEngine + "/api/v1/bundles"
	useB58 := os.Getenv("JITO_BUNDLE_ENCODING") == "base58"
	var body map[string]any
	if useB58 {
		txsB58 := make([]string, 0, len(txsB64))
		for _, b64 := range txsB64 {
			raw, _ := base64.StdEncoding.DecodeString(b64)
			txsB58 = append(txsB58, base58.Encode(raw))
		}
		body = map[string]any{"jsonrpc": "2.0", "id": 1, "method": "sendBundle", "params": []any{txsB58}}
	} else {
		body = map[string]any{"jsonrpc": "2.0", "id": 1, "method": "sendBundle", "params": []any{txsB64, map[string]string{"encoding": "base64"}}}
	}
	v, code, raw, err := postJSON(agent, url, body)
	if err != nil {
		return "", err
	}
	if code >= 300 {
		// Jito puts the real rejection reason in the error response body —
		// surface it instead of just the status line.
		return "", fmt.Errorf("sendBundle HTTP %d: %s", code, raw)
	}
	if e, ok := v["error"]; ok && e != nil {
		return "", fmt.Errorf("sendBundle error: %v", e)
	}
	result, _ := v["result"].(string)
	return result, nil
}
