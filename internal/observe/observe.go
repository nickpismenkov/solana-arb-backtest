// Package observe provides observability for the executor — append-only
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
	"time"

	"arbengine/internal/rpcclient"
)

func appendJSONL(dir, file string, row any) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, file), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	line, err := json.Marshal(row)
	if err != nil {
		return
	}
	f.Write(line)
	f.Write([]byte("\n"))
}

// LogDecision appends to decisions.jsonl — every evaluated trigger, the
// denominator (why we did/didn't fire).
func LogDecision(dir string, row any) {
	appendJSONL(dir, "decisions.jsonl", row)
}

// LogTrade appends to trades.jsonl — every fired bundle, the source of
// truth (quoted, then resolved P&L).
func LogTrade(dir string, row any) {
	appendJSONL(dir, "trades.jsonl", row)
}

const usdcMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

// RealizedUSDC returns the realized USDC delta of `owner` across a landed
// tx (actual result, not the quote). ok=false if the tx isn't on chain yet.
func RealizedUSDC(rpc, signature, owner string) (float64, bool) {
	c := rpcclient.New(rpc)
	raw, err := c.GetTransaction(signature)
	if err != nil || raw == nil {
		return 0, false
	}
	var parsed struct {
		Meta struct {
			PreTokenBalances  []tokenBalance `json:"preTokenBalances"`
			PostTokenBalances []tokenBalance `json:"postTokenBalances"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return 0, false
	}
	sum := func(balances []tokenBalance) float64 {
		var total float64
		for _, b := range balances {
			if b.Mint == usdcMint && b.Owner == owner {
				total += b.UITokenAmount.UIAmount
			}
		}
		return total
	}
	return sum(parsed.Meta.PostTokenBalances) - sum(parsed.Meta.PreTokenBalances), true
}

type tokenBalance struct {
	Mint          string `json:"mint"`
	Owner         string `json:"owner"`
	UITokenAmount struct {
		UIAmount float64 `json:"uiAmount"`
	} `json:"uiTokenAmount"`
}

// Alert is a fire-and-forget alert to a webhook (Slack/Discord/generic) if set.
func Alert(webhook, key, message string) {
	fmt.Fprintf(os.Stderr, "[ALERT:%s] %s\n", key, message)
	if webhook == "" {
		return
	}
	body, err := json.Marshal(map[string]string{"text": fmt.Sprintf("arb [%s] %s", key, message)})
	if err != nil {
		return
	}
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(webhook, "application/json", bytes.NewReader(body))
	if err != nil {
		return
	}
	resp.Body.Close()
}
