// Ground-truth competitor watcher — SEPARATE process, fully off the executor's
// hot path. Rolling scan of both pools' recent transactions; a tx whose
// resolved account set touches BOTH pools is a cross-venue arb. If the signer
// isn't us, a competitor captured it. We estimate their profit (fee-payer USDC
// delta) and cross-reference our own decisions.jsonl to classify what happened
// on our side: never-triggered / skipped / fired-and-lost. Appends missed.jsonl.
//
// This is the only way to see the opportunities our own logs can't — the ones
// we didn't act on or lost. Runs on RPC, seconds-lagged; never touches the
// executor.
//
// Usage: RPC_ENDPOINT=<url> [RUN_DIR=runs] [POLL_SECS=10] [OUR_WALLET=<pk>] \
//
//	go run ./cmd/watcher
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/pools"
)

const usdcMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

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

type sigSlot struct {
	sig  string
	slot uint64
}

func recentSigs(endpoint, pool string, limit int) []sigSlot {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getSignaturesForAddress",
		"params": []any{pool, map[string]any{"limit": limit}}})
	if !ok {
		return nil
	}
	arr, _ := v["result"].([]any)
	var out []sigSlot
	for _, ev := range arr {
		em, _ := ev.(map[string]any)
		if em == nil || em["err"] != nil {
			continue
		}
		sig, ok := em["signature"].(string)
		if !ok {
			continue
		}
		var slot uint64
		if f, ok := em["slot"].(float64); ok {
			slot = uint64(f)
		}
		out = append(out, sigSlot{sig: sig, slot: slot})
	}
	return out
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asArray(v any) []any {
	a, _ := v.([]any)
	return a
}

// txTouchAndProfit returns the full resolved account key set (static +
// ALT-loaded) + fee payer + USDC delta for a transaction, or ok=false if it
// failed to fetch/decode or the tx reverted.
func txTouchAndProfit(endpoint, sig string) (keys map[string]bool, payer string, profit float64, ok bool) {
	v, fetched := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getTransaction",
		"params": []any{sig, map[string]any{"encoding": "jsonParsed", "maxSupportedTransactionVersion": 0, "commitment": "confirmed"}}})
	if !fetched {
		return nil, "", 0, false
	}
	r := asMap(v["result"])
	if r == nil {
		return nil, "", 0, false
	}
	meta := asMap(r["meta"])
	if meta == nil || meta["err"] != nil {
		return nil, "", 0, false
	}
	message := asMap(asMap(r["transaction"])["message"])
	accountKeys := asArray(message["accountKeys"])
	keys = map[string]bool{}
	for _, k := range accountKeys {
		if s, ok := asMap(k)["pubkey"].(string); ok {
			keys[s] = true
		}
	}
	loadedAddresses := asMap(meta["loadedAddresses"])
	for _, grp := range []string{"writable", "readonly"} {
		for _, k := range asArray(loadedAddresses[grp]) {
			if s, ok := k.(string); ok {
				keys[s] = true
			}
		}
	}
	if len(accountKeys) == 0 {
		return nil, "", 0, false
	}
	payer, ok = asMap(accountKeys[0])["pubkey"].(string)
	if !ok {
		return nil, "", 0, false
	}
	sum := func(key string) float64 {
		var total float64
		for _, b := range asArray(meta[key]) {
			bm := asMap(b)
			if bm["mint"] != usdcMint || bm["owner"] != payer {
				continue
			}
			if f, ok := asMap(bm["uiTokenAmount"])["uiAmount"].(float64); ok {
				total += f
			}
		}
		return total
	}
	profit = sum("postTokenBalances") - sum("preTokenBalances")
	return keys, payer, profit, true
}

// ourSlots reads our decisions ledger and returns the sets of slots we
// triggered on and slots we actually fired on.
func ourSlots(dir string) (triggered map[uint64]bool, fired map[uint64]bool) {
	triggered = map[uint64]bool{}
	fired = map[uint64]bool{}
	f, err := os.Open(dir + "/decisions.jsonl")
	if err != nil {
		return triggered, fired
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var v map[string]any
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			continue
		}
		slotF, ok := v["slot"].(float64)
		if !ok {
			continue
		}
		slot := uint64(slotF)
		triggered[slot] = true
		if fireVal, ok := v["fired"].(bool); ok && fireVal {
			fired[slot] = true
		}
	}
	return triggered, fired
}

// inWindow reports whether set contains any slot in [slot-5, slot] (saturating
// at 0), mirroring the Rust set.contains(&slot.saturating_sub(d)) check.
func inWindow(set map[uint64]bool, slot uint64) bool {
	for d := uint64(0); d <= 5; d++ {
		s := uint64(0)
		if slot > d {
			s = slot - d
		}
		if set[s] {
			return true
		}
	}
	return false
}

func main() {
	envfile.LoadDotEnv()
	endpoint := os.Getenv("RPC_ENDPOINT")
	if endpoint == "" {
		fmt.Fprintln(os.Stderr, "RPC_ENDPOINT required")
		os.Exit(1)
	}
	dir := os.Getenv("RUN_DIR")
	if dir == "" {
		dir = "runs"
	}
	poll := uint64(10)
	if v := os.Getenv("POLL_SECS"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			poll = n
		}
	}
	ourWallet := os.Getenv("OUR_WALLET")
	cfg := pools.Pair()
	_ = os.MkdirAll(dir, 0o755)

	fmt.Fprintf(os.Stderr, "watcher %s — scanning for cross-venue arbs every %ds → %s/missed.jsonl\n", cfg.Label, poll, dir)
	seen := map[string]bool{}
	var competitorWins, ourWins uint64

	for {
		var sigs []sigSlot
		for _, pool := range []string{cfg.OrcaPool, cfg.RayPool} {
			sigs = append(sigs, recentSigs(endpoint, pool, 40)...)
		}
		triggered, fired := ourSlots(dir)

		for _, ss := range sigs {
			sig, slot := ss.sig, ss.slot
			if seen[sig] {
				continue
			}
			seen[sig] = true
			keys, payer, profit, ok := txTouchAndProfit(endpoint, sig)
			if !ok {
				continue
			}
			// Cross-venue arb = touches BOTH pools in one tx.
			if !keys[cfg.OrcaPool] || !keys[cfg.RayPool] {
				continue
			}
			ours := ourWallet != "" && payer == ourWallet
			// The arb lands a few slots after the victim we'd have triggered
			// on; match our trigger/fire within a small window ending at the
			// arb slot.
			var status string
			if ours {
				ourWins++
				status = "we_won"
			} else if inWindow(fired, slot) {
				status = "fired_lost"
			} else if inWindow(triggered, slot) {
				status = "triggered_skipped"
			} else {
				status = "not_triggered"
			}
			if !ours {
				competitorWins++
			}
			row := map[string]any{
				"sig": sig, "payer": payer, "competitor": !ours,
				"est_profit_usd": profit, "our_status": status,
			}
			if f, err := os.OpenFile(dir+"/missed.jsonl", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
				b, _ := json.Marshal(row)
				f.Write(append(b, '\n'))
				f.Close()
			}
			sigShort := sig
			if len(sigShort) > 12 {
				sigShort = sigShort[:12]
			}
			payerShort := payer
			if len(payerShort) > 8 {
				payerShort = payerShort[:8]
			}
			fmt.Fprintf(os.Stderr, "arb %s… by %s… profit $%.4f [%s]\n", sigShort, payerShort, profit, status)
		}
		// Cap the seen-set so it doesn't grow unbounded.
		if len(seen) > 20_000 {
			seen = map[string]bool{}
		}
		fmt.Fprintf(os.Stderr, "[watcher] competitor_wins=%d our_wins=%d\n", competitorWins, ourWins)
		time.Sleep(time.Duration(poll) * time.Second)
	}
}
