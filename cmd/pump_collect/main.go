// pump_collect — real-time, read-only recorder of every pump.fun bonding-curve
// event (token create / buy / sell / migrate) to `runs/pump/events.jsonl`.
//
// ── Transport ────────────────────────────────────────────────────────────────
// **Helius WebSocket `logsSubscribe`** with a `mentions:[pump_program]` filter,
// at `processed` commitment. Chosen because:
//   - It is real-time (sub-second) and works with the standard Helius API key in
//     `.env` (`HELIUS_RPC` → derive the `wss://` host). No gRPC/Laserstream add-on
//     or separate credential is required. (The repo's `GRPC_ENDPOINT` is a Tatum
//     gateway, not a Helius Yellowstone stream, so we do not depend on it here.)
//   - pump emits every event as an anchor self-CPI **`Program data:` log blob**,
//     so the log stream alone carries the full structured payload (mint, amounts,
//     reserves, dev, …) — no second `getTransaction` round-trip, hence lowest
//     latency. See `internal/pump` for the verified layouts.
//
// Trade-off / caveat: `logsSubscribe` can, under load, drop or lag; it is the
// standard-tier tool though, and for a measurement collector completeness is
// "best effort, high coverage" rather than "every single tx". If a run needs
// guaranteed completeness, back-fill later with getSignaturesForAddress. This
// binary favours latency + simplicity, which is what Phase-1 recon needs.
//
// Robustness: reconnects with capped backoff on any drop, flushes each event to
// disk immediately, prints a heartbeat every 10s (events/sec, launches seen,
// migrations seen). It NEVER signs or submits a transaction.
//
// Usage: `HELIUS_RPC=<url> go run ./cmd/pump_collect`
//
//	env: PUMP_WS (override ws url), PUMP_OUT (override output path).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/pump"
)

func nowMs() int64 {
	return time.Now().UnixMilli()
}

// wsURL derives the Helius `wss://` endpoint from the `https://` RPC url
// (keeps the `?api-key=…`). Override entirely with PUMP_WS.
func wsURL() string {
	if u := os.Getenv("PUMP_WS"); u != "" {
		return u
	}
	http := os.Getenv("HELIUS_RPC")
	if http == "" {
		http = os.Getenv("RPC_HTTP")
	}
	if http == "" {
		fmt.Fprintln(os.Stderr, "set HELIUS_RPC (or RPC_HTTP)")
		os.Exit(1)
	}
	u := strings.Replace(http, "https://", "wss://", 1)
	u = strings.Replace(u, "http://", "ws://", 1)
	return u
}

type counts struct {
	total    uint64
	creates  uint64
	buys     uint64
	sells    uint64
	migrates uint64
}

func main() {
	envfile.LoadDotEnv()

	outPath := os.Getenv("PUMP_OUT")
	if outPath == "" {
		outPath = "runs/pump/events.jsonl"
	}
	if dir := filepath.Dir(outPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "create %s: %v\n", dir, err)
			os.Exit(1)
		}
	}
	file, err := os.OpenFile(outPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open %s: %v\n", outPath, err)
		os.Exit(1)
	}
	defer file.Close()
	// Exclusive advisory lock: a second collector appending to the same file
	// interleaves write fragments and tears lines (observed 9.7% corrupt lines
	// when two instances overlapped). Fail fast instead.
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Fprintf(os.Stderr, "[pump_collect] %s is locked by another pump_collect (%v); refusing to double-write. exiting.\n", outPath, err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "[pump_collect] program %s\n", pump.PumpProgram)
	fmt.Fprintf(os.Stderr, "[pump_collect] appending events to %s\n", outPath)
	fmt.Fprintln(os.Stderr, "[pump_collect] transport: Helius WebSocket logsSubscribe (read-only)")

	c := &counts{}
	backoff := 500 * time.Millisecond

	for {
		err := runOnce(file, c)
		if err == nil {
			fmt.Fprintln(os.Stderr, "[pump_collect] stream closed cleanly; reconnecting")
			backoff = 500 * time.Millisecond
		} else {
			fmt.Fprintf(os.Stderr, "[pump_collect] error: %v; reconnecting in %s\n", err, backoff)
			time.Sleep(backoff)
			backoff *= 2
			if backoff > 15*time.Second {
				backoff = 15 * time.Second
			}
		}
	}
}

// runOnce is one connection lifecycle: connect, subscribe, drain notifications
// until the socket drops. Returns nil on clean close, non-nil on any failure
// (→ reconnect).
func runOnce(file *os.File, c *counts) error {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.Dial(wsURL(), nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	sub := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "logsSubscribe",
		"params": []any{
			map[string]any{"mentions": []string{pump.PumpProgram}},
			map[string]any{"commitment": "processed"},
		},
	}
	if err := conn.WriteJSON(sub); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	fmt.Fprintln(os.Stderr, "[pump_collect] subscribed; waiting for events…")

	hbInterval := 10 * time.Second
	lastTotal := c.total
	lastAt := time.Now()

	// Heartbeat ticker running independently of the blocking ReadMessage loop.
	hbDone := make(chan struct{})
	defer close(hbDone)
	go func() {
		t := time.NewTicker(hbInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				dt := time.Since(lastAt).Seconds()
				if dt < 1e-9 {
					dt = 1e-9
				}
				rate := float64(c.total-lastTotal) / dt
				fmt.Fprintf(os.Stderr, "[pump_collect] hb: %.0f ev/s | total %d (create %d, buy %d, sell %d, migrate %d)\n",
					rate, c.total, c.creates, c.buys, c.sells, c.migrates)
				lastTotal = c.total
				lastAt = time.Now()
			case <-hbDone:
				return
			}
		}
	}()

	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		switch msgType {
		case websocket.TextMessage:
			handleNotification(data, file, c)
		case websocket.CloseMessage:
			return nil
		default:
			// Ping/Pong handled transparently by gorilla/websocket.
		}
	}
}

// handleNotification parses one logsNotification frame, decodes any pump
// event blobs in it, and appends a JSONL record per decoded event.
func handleNotification(data []byte, file *os.File, c *counts) {
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		return
	}
	params, _ := v["params"].(map[string]any)
	if params == nil {
		return // subscription ack or unrelated frame
	}
	result, _ := params["result"].(map[string]any)
	if result == nil {
		return
	}
	context, _ := result["context"].(map[string]any)
	var slot uint64
	if context != nil {
		if s, ok := context["slot"].(float64); ok {
			slot = uint64(s)
		}
	}
	value, _ := result["value"].(map[string]any)
	if value == nil {
		return
	}
	// Skip failed txs — a reverted tx's event logs would be misleading.
	if value["err"] != nil {
		return
	}
	signature, _ := value["signature"].(string)
	logsAny, _ := value["logs"].([]any)
	if logsAny == nil {
		return
	}

	ts := nowMs()
	for _, lineAny := range logsAny {
		s, ok := lineAny.(string)
		if !ok {
			continue
		}
		const prefix = "Program data: "
		if !strings.HasPrefix(s, prefix) {
			continue
		}
		b64 := s[len(prefix):]
		ev, ok := pump.ParseProgramDataB64(b64)
		if !ok {
			continue
		}
		rec := toRecord(ev, ts, slot, signature)
		c.total++
		switch ev.KindTag() {
		case "create":
			c.creates++
		case "buy":
			c.buys++
		case "sell":
			c.sells++
		case "migrate":
			c.migrates++
		}
		// One Write per record: writing fragment-by-fragment on an unbuffered
		// File issues a syscall per fragment, which interleaves if writers
		// ever overlap.
		line, err := json.Marshal(rec)
		if err != nil {
			continue
		}
		line = append(line, '\n')
		_, _ = file.Write(line)
	}
}

// toRecord builds the JSONL record for one event. Fields common to all:
// unix_ms, slot, signature, event_type, mint, bonding_curve, actor,
// sol_amount, token_amount.
func toRecord(ev *pump.PumpEvent, ts int64, slot uint64, sig string) map[string]any {
	rec := map[string]any{
		"unix_ms":    ts,
		"slot":       slot,
		"signature":  sig,
		"event_type": ev.KindTag(),
	}
	switch ev.Kind {
	case pump.EventCreate:
		c := ev.Create
		rec["mint"] = c.Mint.String()
		rec["bonding_curve"] = c.BondingCurve.String()
		rec["actor"] = c.User.String()
		rec["dev"] = c.User.String()
		rec["creator"] = c.Creator.String()
		rec["block_time"] = c.Timestamp
		rec["name"] = c.Name
		rec["symbol"] = c.Symbol
		rec["uri"] = c.URI
		rec["sol_amount"] = 0
		rec["token_amount"] = 0
		rec["init_virtual_sol_reserves"] = c.VirtualSolReserves
		rec["init_virtual_token_reserves"] = c.VirtualTokenReserves
		rec["init_real_token_reserves"] = c.RealTokenReserves
		rec["token_total_supply"] = c.TokenTotalSupply
	case pump.EventTrade:
		t := ev.Trade
		rec["mint"] = t.Mint.String()
		rec["bonding_curve"] = pump.BondingCurvePDA(t.Mint).String()
		rec["actor"] = t.User.String()
		if t.HasCreator {
			rec["creator"] = t.Creator.String()
		}
		rec["block_time"] = t.Timestamp
		rec["sol_amount"] = t.SolAmount
		rec["token_amount"] = t.TokenAmount
		rec["virtual_sol_reserves"] = t.VirtualSolReserves
		rec["virtual_token_reserves"] = t.VirtualTokenReserves
		rec["real_sol_reserves"] = t.RealSolReserves
		rec["real_token_reserves"] = t.RealTokenReserves
		rec["price_in_sol"] = t.PriceInSOL()
	case pump.EventMigrate:
		m := ev.Migrate
		rec["mint"] = m.Mint.String()
		rec["bonding_curve"] = pump.BondingCurvePDA(m.Mint).String()
		rec["actor"] = pump.MigrationAuthority
		rec["sol_amount"] = 0
		rec["token_amount"] = 0
	}
	return rec
}
