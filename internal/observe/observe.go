// Package observe implements observability for the executor — append-only
// JSONL ledgers + realized-P&L readback + alerts. STRICTLY off the hot path:
// all writes happen after a bundle is submitted; realized P&L is read on a
// later poll.
package observe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
)

func appendJSONL(dir, file string, row any) {
	_ = os.MkdirAll(dir, 0o755)
	f, err := os.OpenFile(filepath.Join(dir, file), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	line, err := json.Marshal(row)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
}

// LogDecision logs every evaluated trigger — the denominator (why we did/didn't fire).
func LogDecision(dir string, row any) {
	appendJSONL(dir, "decisions.jsonl", row)
}

// LogTrade logs every fired bundle — the source of truth (quoted, then resolved P&L).
func LogTrade(dir string, row any) {
	appendJSONL(dir, "trades.jsonl", row)
}

// RealizedUSDC returns the realized USDC delta of the fee payer across a
// landed tx (actual result, not the quote). ok=false if the tx isn't on
// chain yet.
func RealizedUSDC(rpc, signature, owner string) (float64, bool) {
	const usdc = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getTransaction",
		"params": []any{signature, map[string]any{
			"encoding": "jsonParsed", "maxSupportedTransactionVersion": 0, "commitment": "confirmed",
		}},
	})
	resp, err := http.Post(rpc, "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	var v struct {
		Result struct {
			Meta json.RawMessage `json:"meta"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil || v.Result.Meta == nil {
		return 0, false
	}
	var meta struct {
		PreTokenBalances  []tokenBalance `json:"preTokenBalances"`
		PostTokenBalances []tokenBalance `json:"postTokenBalances"`
	}
	if err := json.Unmarshal(v.Result.Meta, &meta); err != nil {
		return 0, false
	}
	sum := func(bals []tokenBalance) float64 {
		var s float64
		for _, b := range bals {
			if b.Mint == usdc && b.Owner == owner {
				s += b.UiTokenAmount.UiAmount
			}
		}
		return s
	}
	return sum(meta.PostTokenBalances) - sum(meta.PreTokenBalances), true
}

type tokenBalance struct {
	Mint          string `json:"mint"`
	Owner         string `json:"owner"`
	UiTokenAmount struct {
		UiAmount float64 `json:"uiAmount"`
	} `json:"uiTokenAmount"`
}

// Alert is a fire-and-forget alert to a webhook (Slack/Discord/generic) if set.
func Alert(webhook *string, key, message string) {
	fmt.Fprintf(os.Stderr, "[ALERT:%s] %s\n", key, message)
	if webhook != nil && *webhook != "" {
		body, _ := json.Marshal(map[string]string{"text": fmt.Sprintf("arb [%s] %s", key, message)})
		resp, err := http.Post(*webhook, "application/json", bytes.NewReader(body))
		if err == nil {
			resp.Body.Close()
		}
	}
}
