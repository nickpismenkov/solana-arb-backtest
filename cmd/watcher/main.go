// Command watcher is the ground-truth competitor watcher — SEPARATE process,
// fully off the executor's hot path. Rolling scan of both pools' recent
// transactions; a tx whose resolved account set touches BOTH pools is a
// cross-venue arb. If the signer isn't us, a competitor captured it. We
// estimate their profit (fee-payer USDC delta) and cross-reference our own
// decisions.jsonl to classify what happened on our side: never-triggered /
// skipped / fired-and-lost. Appends missed.jsonl.
//
// This is the only way to see the opportunities our own logs can't — the
// ones we didn't act on or lost. Runs on RPC, seconds-lagged; never touches
// the executor.
//
// Usage: RPC_ENDPOINT=<url> [RUN_DIR=runs] [POLL_SECS=10] [OUR_WALLET=<pk>] \
//
//	go run ./cmd/watcher
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"arbengine/internal/config"
	"arbengine/internal/pools"
	"arbengine/internal/rpcclient"
)

type sigEntry struct {
	Sig  string
	Slot uint64
}

func recentSigs(c *rpcclient.Client, pool string, limit int) []sigEntry {
	raw, err := c.Call("getSignaturesForAddress", []any{pool, map[string]any{"limit": limit}})
	if err != nil {
		return nil
	}
	var entries []struct {
		Signature string      `json:"signature"`
		Slot      uint64      `json:"slot"`
		Err       interface{} `json:"err"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	var out []sigEntry
	for _, e := range entries {
		if e.Err != nil {
			continue
		}
		out = append(out, sigEntry{Sig: e.Signature, Slot: e.Slot})
	}
	return out
}

const usdcMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

// txTouchAndProfit returns the full resolved account key set (static +
// ALT-loaded) + fee payer + USDC delta.
func txTouchAndProfit(c *rpcclient.Client, sig string) (map[string]bool, string, float64, bool) {
	raw, err := c.GetTransaction(sig)
	if err != nil || raw == nil {
		return nil, "", 0, false
	}
	// GetTransaction already unwraps to the `result` field (see rpcclient),
	// so unmarshal directly into the transaction shape.
	var r struct {
		Meta struct {
			Err               interface{} `json:"err"`
			PreTokenBalances  []tokBal    `json:"preTokenBalances"`
			PostTokenBalances []tokBal    `json:"postTokenBalances"`
			LoadedAddresses   struct {
				Writable []string `json:"writable"`
				Readonly []string `json:"readonly"`
			} `json:"loadedAddresses"`
		} `json:"meta"`
		Transaction struct {
			Message struct {
				AccountKeys []struct {
					Pubkey string `json:"pubkey"`
				} `json:"accountKeys"`
			} `json:"message"`
		} `json:"transaction"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, "", 0, false
	}
	if r.Meta.Err != nil {
		return nil, "", 0, false
	}
	keys := make(map[string]bool)
	for _, k := range r.Transaction.Message.AccountKeys {
		keys[k.Pubkey] = true
	}
	for _, k := range r.Meta.LoadedAddresses.Writable {
		keys[k] = true
	}
	for _, k := range r.Meta.LoadedAddresses.Readonly {
		keys[k] = true
	}
	if len(r.Transaction.Message.AccountKeys) == 0 {
		return nil, "", 0, false
	}
	payer := r.Transaction.Message.AccountKeys[0].Pubkey

	sum := func(balances []tokBal) float64 {
		var total float64
		for _, b := range balances {
			if b.Mint == usdcMint && b.Owner == payer {
				total += b.UITokenAmount.UIAmount
			}
		}
		return total
	}
	profit := sum(r.Meta.PostTokenBalances) - sum(r.Meta.PreTokenBalances)
	return keys, payer, profit, true
}

type tokBal struct {
	Mint          string `json:"mint"`
	Owner         string `json:"owner"`
	UITokenAmount struct {
		UIAmount float64 `json:"uiAmount"`
	} `json:"uiTokenAmount"`
}

// ourSlots reads slots we triggered / fired on, from our decisions ledger.
func ourSlots(dir string) (map[uint64]bool, map[uint64]bool) {
	triggered := make(map[uint64]bool)
	fired := make(map[uint64]bool)
	f, err := os.Open(fmt.Sprintf("%s/decisions.jsonl", dir))
	if err != nil {
		return triggered, fired
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	for {
		var v map[string]any
		if err := dec.Decode(&v); err != nil {
			break
		}
		if slotF, ok := v["slot"].(float64); ok {
			slot := uint64(slotF)
			triggered[slot] = true
			if b, ok := v["fired"].(bool); ok && b {
				fired[slot] = true
			}
		}
	}
	return triggered, fired
}

// inWindow mirrors Rust's `(0..=5).any(|d| set.contains(&slot.saturating_sub(d)))`.
func inWindow(set map[uint64]bool, slot uint64) bool {
	for d := uint64(0); d <= 5; d++ {
		s := uint64(0)
		if d <= slot {
			s = slot - d
		}
		if set[s] {
			return true
		}
	}
	return false
}

func main() {
	config.LoadDotenv()
	endpoint, ok := config.EnvOptional("RPC_ENDPOINT")
	if !ok {
		fmt.Fprintln(os.Stderr, "RPC_ENDPOINT")
		os.Exit(1)
	}
	dir := config.EnvOr("RUN_DIR", "runs")
	poll := config.EnvUint64("POLL_SECS", 10)
	ourWallet := config.EnvOr("OUR_WALLET", "")
	cfg := pools.Pair()
	_ = os.MkdirAll(dir, 0o755)
	c := rpcclient.New(endpoint)

	fmt.Fprintf(os.Stderr, "watcher %s — scanning for cross-venue arbs every %ds → %s/missed.jsonl\n", cfg.Label, poll, dir)
	seen := make(map[string]bool)
	var competitorWins, ourWins uint64

	for {
		var sigs []sigEntry
		for _, pool := range []string{cfg.OrcaPool, cfg.RayPool} {
			sigs = append(sigs, recentSigs(c, pool, 40)...)
		}
		triggered, fired := ourSlots(dir)

		for _, se := range sigs {
			if seen[se.Sig] {
				continue
			}
			seen[se.Sig] = true
			keys, payer, profit, ok := txTouchAndProfit(c, se.Sig)
			if !ok {
				continue
			}
			// Cross-venue arb = touches BOTH pools in one tx.
			if !keys[cfg.OrcaPool] || !keys[cfg.RayPool] {
				continue
			}
			ours := ourWallet != "" && payer == ourWallet
			// The arb lands a few slots after the victim we'd have triggered on;
			// match our trigger/fire within a small window ending at the arb slot.
			var status string
			if ours {
				ourWins++
				status = "we_won"
			} else if inWindow(fired, se.Slot) {
				status = "fired_lost"
			} else if inWindow(triggered, se.Slot) {
				status = "triggered_skipped"
			} else {
				status = "not_triggered"
			}
			if !ours {
				competitorWins++
			}
			row := map[string]any{
				"sig": se.Sig, "payer": payer, "competitor": !ours,
				"est_profit_usd": profit, "our_status": status,
			}
			appendJSONL(dir, "missed.jsonl", row)

			sigPrefix := se.Sig
			if len(sigPrefix) > 12 {
				sigPrefix = sigPrefix[:12]
			}
			payerPrefix := payer
			if len(payerPrefix) > 8 {
				payerPrefix = payerPrefix[:8]
			}
			fmt.Fprintf(os.Stderr, "arb %s… by %s… profit $%.4f [%s]\n", sigPrefix, payerPrefix, profit, status)
		}
		// Cap the seen-set so it doesn't grow unbounded.
		if len(seen) > 20_000 {
			seen = make(map[string]bool)
		}
		fmt.Fprintf(os.Stderr, "[watcher] competitor_wins=%d our_wins=%d\n", competitorWins, ourWins)
		time.Sleep(time.Duration(poll) * time.Second)
	}
}

func appendJSONL(dir, file string, row any) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(dir+"/"+file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
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
