// Package lazer implements the Pyth Lazer ("Pyth Pro") low-latency price
// feed: a reconnecting WebSocket client that streams live prices into a
// shared in-memory table, plus the liquidation-executor pre-positioning
// logic built on top of it.
//
// Liquidation eligibility is gated by the protocol's ON-CHAIN oracle, so
// Lazer can't make an account liquidatable sooner than the chain sees it.
// Its value is a LEADING signal: Lazer ticks at ms latency, minutes ahead of
// the reserve/bank oracle crank. Blending Lazer prices into the health
// recompute lets the executor ARM (pre-select + keep hot) exactly the
// accounts about to cross the threshold, so when the on-chain oracle catches
// up the fire tx is already built and only needs sign+submit. The FIRE
// decision itself stays gated by full on-chain simulation — Lazer never
// affects safety, only which accounts we spend sim budget on.
//
// Feed ids are Lazer's numeric ids (SOL=6, BTC=1, ETH=2, USDC=7). Only the
// volatile majors matter for arming — a borrower crosses the threshold when
// its volatile collateral drops or volatile debt rises; stables don't move.
//
// ── WebSocket client (ported from the Rust source's pyth.rs) ───────────────
// Auth: Bearer token via PYTH_LAZER_TOKEN (never hardcode; .env only).
// Endpoint + subscribe shape are VERIFIED live against the SOL/USDC feeds.
//
// Feeds are numeric IDs (SOL=6, USDC=7, BTC=1, ETH=2). The parsed message
// carries the raw integer price; the exponent is per-feed static (all the
// crypto majors are -8). We request "exponent" as a property and fall back
// to -8 if the server omits it.
package lazer

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/liquidation"
)

// ── Lazer numeric feed ids ───────────────────────────────────────────────
// VERIFIED live in pyth_probe: SOL=6, USDC=7, BTC=1, ETH=2; the rest
// verified against the Lazer symbol registry
// history.pyth-lazer.dourolabs.app/history/v1/symbols.
const (
	LazerSOL  uint32 = 6
	LazerBTC  uint32 = 1
	LazerETH  uint32 = 2
	LazerUSDC uint32 = 7
	LazerUSDT uint32 = 8
	LazerBONK uint32 = 9
	LazerWIF  uint32 = 10
	LazerPYTH uint32 = 3
	// LazerJUP / LazerW exist on Lazer but do NOT support the `real_time`
	// channel — including them errors the ENTIRE subscription ("Feeds do not
	// support channel real_time: 92, 102", verified live 2026-07-14) and the
	// stream goes dark. Their banks stay baseline-priced; do not add them to
	// ArmFeedIDs unless the channel support changes or a separate
	// fixed-rate subscription is wired.
	LazerJUP uint32 = 92
	LazerW   uint32 = 102
)

// ArmFeedIDs is every feed the executors subscribe to and arm on. The list
// is CENSUS-DRIVEN: a 7-day scan of landed marginfi liquidations
// (2026-07-14) showed BONK collateral in 91% of them, with PYTH/WIF next
// among Lazer-covered assets — while the old majors-only list
// (SOL/BTC/ETH/USDC) missed all of them between 300s rescans. Stables
// (USDC/USDT) included so stable-debt accounts are fully priced by Lazer.
// Known gaps (baseline-priced): HNT (no Lazer feed), JUP/W (no real_time
// channel — see above).
func ArmFeedIDs() []uint32 {
	return []uint32{LazerSOL, LazerBTC, LazerETH, LazerUSDC, LazerUSDT,
		LazerBONK, LazerWIF, LazerPYTH}
}

// MintFeedMap maps mint -> Lazer feed id for assets whose price Lazer leads.
// SOL-correlated LSTs map to SOL (their on-chain valuation moves with SOL,
// scaled by the LST exchange rate — see OneToOneMints for why that scale
// matters).
func MintFeedMap() map[solana.PublicKey]uint32 {
	e := func(s string, id uint32) (solana.PublicKey, uint32) {
		return solana.MustPublicKeyFromBase58(s), id
	}
	m := map[solana.PublicKey]uint32{}
	add := func(s string, id uint32) {
		k, v := e(s, id)
		m[k] = v
	}
	add("So11111111111111111111111111111111111111112", LazerSOL)   // wSOL
	add("mSoLzYCxHdYgdzU16g5QSh3i5K3z3KZK7ytfqcJm7So", LazerSOL)   // mSOL
	add("J1toso1uCk3RLmjorhTtrVwY9HJ7X8V9yYac6Y7kGCPn", LazerSOL)  // jitoSOL
	add("bSo13r4TkiE4KumL71LsHTPpL2euBYLFx6h9HP3piy1", LazerSOL)   // bSOL
	add("7dHbWXmci3dT8UFYWYZweBLXgycu7Y3iL6trKn1Y7ARj", LazerSOL)  // stSOL
	add("LSTxxxnJzKDFSLr4dUkPcmCf5VyryEqzPLz5j4bpxFp", LazerSOL)   // LST (Marinade)
	add("5oVNBeEEQvYi1cX3ir8Dx5n1P7pdxydbGF2X4TxVusJm", LazerSOL)  // INF
	add("jupSoLaHXQiZZTSfEWMTRRgpnyFm8f6sZdosWBjx93v", LazerSOL)   // jupSOL
	add("he1iusmfkpAdwvxLNGV8Y1iSbj4rUy6yMhEA3fotn9A", LazerSOL)   // hSOL (Helius)
	add("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", LazerUSDC) // USDC
	add("Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", LazerUSDT) // USDT
	add("cbbtcf3aa214zXHbiAZQwf4122FBYbraNdFqgw4iMij", LazerBTC)   // cbBTC
	add("3NZ9JMVBmGAqocybic2c7LQCJScmgsAZ6vQqTDzcqmJh", LazerBTC)  // wBTC (Wormhole)
	add("7vfCXTUXx5WJV5JADk17DUJ4ksgau7utNKj4b963voxs", LazerETH)  // wETH (Wormhole)
	add("DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263", LazerBONK) // BONK
	add("EKpQGSJtjMFqKZ9KQanSqYXRcF8fBopzLHYxdM65zcjm", LazerWIF)  // WIF
	add("HZ1JovNiVvGrGNiiYvEozEVgZ58xaU3RKwX8eACQBCt3", LazerPYTH) // PYTH
	// JUP/W deliberately unmapped: their feeds aren't in ArmFeedIDs (no
	// real_time channel), and a mapped-but-unsubscribed feed would leave
	// accounts permanently !feeds_ready instead of baseline-priced.
	return m
}

// OneToOneMints is the set of mints whose on-chain price IS the feed price
// (1 token = 1 feed unit). LSTs are deliberately absent: an LST is worth
// (exchange rate)x SOL — pricing it at the RAW SOL feed undervalues the
// collateral by 15-35% and makes healthy LST-collateral accounts look deep
// underwater (the phantom-candidate bug found 2026-07-14). Consumers that
// substitute the feed price directly (Blend) must restrict themselves to
// this set; coefficient-based consumers (liq_engine) anchor-scale mapped
// banks to the on-chain baseline instead.
func OneToOneMints() map[solana.PublicKey]struct{} {
	mints := []string{
		"So11111111111111111111111111111111111111112",  // wSOL
		"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", // USDC
		"Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", // USDT
		"cbbtcf3aa214zXHbiAZQwf4122FBYbraNdFqgw4iMij",  // cbBTC
		"3NZ9JMVBmGAqocybic2c7LQCJScmgsAZ6vQqTDzcqmJh", // wBTC
		"7vfCXTUXx5WJV5JADk17DUJ4ksgau7utNKj4b963voxs", // wETH
		"DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263", // BONK
		"EKpQGSJtjMFqKZ9KQanSqYXRcF8fBopzLHYxdM65zcjm", // WIF
		"HZ1JovNiVvGrGNiiYvEozEVgZ58xaU3RKwX8eACQBCt3", // PYTH
	}
	out := make(map[solana.PublicKey]struct{}, len(mints))
	for _, s := range mints {
		out[solana.MustPublicKeyFromBase58(s)] = struct{}{}
	}
	return out
}

// ── Types shared with the liquidation engine ────────────────────────────
// The Rust source pulls Bank/BankMap/PriceMap from crate::liquidation and
// PriceTable from crate::pyth. The liquidation package (this port's true
// home for Bank/BankMap/PriceMap — see internal/liquidation/liquidation.go)
// now exists, so this package reuses its real types instead of the
// placeholder shapes an earlier porting pass declared locally here.
// liquidation does not import lazer, so this is a one-way dependency with
// no cycle.

// Bank is an alias for liquidation.Bank (the full marginfi Bank risk
// parameters — Blend only reads .Mint, matching the original placeholder's
// scope).
type Bank = liquidation.Bank

// BankMap is bank pubkey -> Bank, matching the Rust source's
// liquidation::BankMap.
type BankMap = liquidation.BankMap

// PriceMap is bank pubkey -> USD (or SOL, per convention) price, matching
// the Rust source's liquidation::PriceMap.
type PriceMap = liquidation.PriceMap

// ── Pyth Lazer price table (ported from the Rust source's pyth.rs) ──────

// DefaultExponent is used when the server omits the exponent property (all
// the crypto majors are -8).
const DefaultExponent int32 = -8

// PricePoint is a single price observation, already scaled to a real number
// (price * 10^exp).
type PricePoint struct {
	Price float64
	// TsUs is the Lazer publish timestamp, microseconds since epoch (0 if
	// absent).
	TsUs uint64
}

// PriceTable is a shared, lock-guarded map feed_id -> latest price. Cheap to
// share (pointer + embedded mutex); pass by pointer.
type PriceTable struct {
	mu     sync.RWMutex
	prices map[uint32]PricePoint
}

// NewPriceTable creates an empty PriceTable.
func NewPriceTable() *PriceTable {
	return &PriceTable{prices: make(map[uint32]PricePoint)}
}

// Get reads the latest price for a feed, if one has been received.
func (t *PriceTable) Get(feedID uint32) (PricePoint, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	p, ok := t.prices[feedID]
	return p, ok
}

func (t *PriceTable) set(feedID uint32, p PricePoint) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prices[feedID] = p
}

// LazerURL is the Pyth Lazer WebSocket endpoint. Default is the
// Cloudflare-anycast host (pyth-lazer.dourolabs.app) which routes to the
// nearest edge — measured ~2.5x faster to connect than the pinned
// pyth-lazer-0 origin. Override with LAZER_URL; measure `curl -w
// %{time_connect}` to each candidate from the box and pick the lowest.
// (Also available: pyth-lazer-0/-1 direct origins.)
func LazerURL() string {
	if v, ok := os.LookupEnv("LAZER_URL"); ok {
		return v
	}
	return "wss://pyth-lazer.dourolabs.app/v1/stream"
}

// LazerChannel is the Lazer delivery channel. Default `real_time` = lowest
// latency (each update pushed the instant it's computed, 1-50ms), vs
// `fixed_rate@50ms` which only snapshots every 50ms and adds up to that much
// batching latency to detect_lag. Override with LAZER_CHANNEL (e.g.
// `fixed_rate@1ms`, `fixed_rate@50ms`).
func LazerChannel() string {
	if v, ok := os.LookupEnv("LAZER_CHANNEL"); ok {
		return v
	}
	return "real_time"
}

type subscribeMsg struct {
	Type           string   `json:"type"`
	SubscriptionID int      `json:"subscriptionId"`
	PriceFeedIDs   []uint32 `json:"priceFeedIds"`
	Properties     []string `json:"properties"`
	Formats        []string `json:"formats"`
	Channel        string   `json:"channel"`
	DeliveryFormat string   `json:"deliveryFormat"`
}

// RunLazerFeed runs one WebSocket connection lifecycle: connect, subscribe,
// pump updates into table, and also push every applied PricePoint (keyed by
// feed id) onto tx so a caller can drive a channel-based detector loop
// (mirrors the RunXFeed(ctx, ..., tx chan<- T) pattern used by the gRPC
// feed). Returns nil on a clean server close, non-nil on any failure — the
// caller is expected to reconnect with backoff (see RunLazerFeedForever).
func RunLazerFeed(ctx context.Context, token string, feedIDs []uint32, table *PriceTable, tx chan<- FeedUpdate) error {
	header := map[string][]string{"Authorization": {"Bearer " + token}}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, LazerURL(), header)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	sub := subscribeMsg{
		Type:           "subscribe",
		SubscriptionID: 1,
		PriceFeedIDs:   feedIDs,
		Properties:     []string{"price", "exponent"},
		Formats:        []string{},
		Channel:        LazerChannel(),
		DeliveryFormat: "json",
	}
	if err := conn.WriteJSON(sub); err != nil {
		return fmt.Errorf("send subscribe request: %w", err)
	}

	// Close the connection promptly if the context is canceled while we're
	// blocked in ReadMessage.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		switch msgType {
		case websocket.TextMessage:
			applyUpdate(data, table, tx, ctx)
		case websocket.CloseMessage:
			return nil
		default:
			// Ping/Pong are handled transparently by gorilla/websocket's
			// default handlers.
		}
	}
}

// FeedUpdate is one applied price update, pushed onto the tx channel as it
// arrives so a synchronous caller can react without polling PriceTable.
type FeedUpdate struct {
	FeedID uint32
	Point  PricePoint
}

// applyUpdate parses a Lazer text frame and updates the table. Ignores
// non-price frames (e.g. the initial {"type":"subscribed"} ack) — EXCEPT
// subscription errors, which must be loud: a rejected subscription (e.g. a
// feed that doesn't support the channel) otherwise looks exactly like a
// dead-calm market with 0/N feeds live.
func applyUpdate(text []byte, table *PriceTable, tx chan<- FeedUpdate, ctx context.Context) {
	var v map[string]any
	if err := json.Unmarshal(text, &v); err != nil {
		return
	}
	if t, ok := v["type"].(string); ok {
		lt := lower(t)
		if lt == "subscriptionerror" || lt == "error" {
			fmt.Fprintf(os.Stderr, "[pyth-lazer] SUBSCRIPTION REJECTED: %s — no prices will flow; fix the feed list\n", string(text))
			return
		}
	}
	parsed, _ := v["parsed"].(map[string]any)
	if parsed == nil {
		return
	}
	feedsRaw, ok := parsed["priceFeeds"].([]any)
	if !ok {
		return
	}

	// timestampUs may arrive as a string or a number depending on delivery.
	var tsUs uint64
	switch ts := parsed["timestampUs"].(type) {
	case string:
		if n, err := strconv.ParseUint(ts, 10, 64); err == nil {
			tsUs = n
		}
	case float64:
		tsUs = uint64(ts)
	}

	for _, fRaw := range feedsRaw {
		f, ok := fRaw.(map[string]any)
		if !ok {
			continue
		}
		idF, ok := f["priceFeedId"].(float64)
		if !ok {
			continue
		}
		id := uint32(idF)

		// price arrives as a string integer (may also be numeric).
		var raw float64
		haveRaw := false
		switch p := f["price"].(type) {
		case string:
			if n, err := strconv.ParseFloat(p, 64); err == nil {
				raw = n
				haveRaw = true
			}
		case float64:
			raw = p
			haveRaw = true
		}
		if !haveRaw {
			continue
		}
		exp := DefaultExponent
		if e, ok := f["exponent"].(float64); ok {
			exp = int32(e)
		}
		price := raw * math.Pow(10, float64(exp))
		point := PricePoint{Price: price, TsUs: tsUs}
		table.set(id, point)
		if tx != nil {
			select {
			case tx <- FeedUpdate{FeedID: id, Point: point}:
			case <-ctx.Done():
				return
			default:
				// Non-blocking: PriceTable is already updated above, which
				// is the authoritative state; a full tx channel just means
				// this particular tick notification is dropped.
			}
		}
	}
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// RunLazerFeedForever runs RunLazerFeed in a reconnect loop with exponential
// backoff (500ms doubling to a 10s cap), matching the Rust source's
// spawn_lazer. Blocks until ctx is canceled.
func RunLazerFeedForever(ctx context.Context, token string, feedIDs []uint32, table *PriceTable, tx chan<- FeedUpdate) {
	backoff := 500 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}
		err := RunLazerFeed(ctx, token, feedIDs, table, tx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			// Clean close — reconnect promptly.
			backoff = 500 * time.Millisecond
		} else {
			fmt.Fprintf(os.Stderr, "[pyth-lazer] %v; reconnecting in %dms\n", err, backoff.Milliseconds())
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff *= 2
		if backoff > 10*time.Second {
			backoff = 10 * time.Second
		}
	}
}

// SpawnLazerThread runs the Lazer WS feed on its own goroutine, writing into
// table. Mirrors the Rust source's spawn_lazer_thread, which ran the
// Lazer feed on a dedicated tokio runtime in a background OS thread so a
// synchronous executor loop could read fresh Lazer prices without adopting
// an async runtime; in Go a goroutine is the direct equivalent. Returns
// immediately. Pass a nil tx if the caller only wants to read via
// table.Get.
func SpawnLazerThread(ctx context.Context, token string, feedIDs []uint32, table *PriceTable, tx chan<- FeedUpdate) {
	go RunLazerFeedForever(ctx, token, feedIDs, table, tx)
}

// ── Blend + status ────────────────────────────────────────────────────────

// Blend blends Lazer prices over an on-chain baseline: start from the
// on-chain PriceMap (authoritative for anything Lazer doesn't cover) and
// override a bank's price with the fresher Lazer tick when we have both a
// mint mapping and a recent tick. Only 1:1 mints are overridden — an LST
// priced at the raw SOL feed would be undervalued by its exchange rate, so
// LST banks keep the on-chain baseline here (the tick-driven engine
// anchor-scales them instead). Returns the blended map + how many banks
// Lazer led.
func Blend(banks BankMap, onChain PriceMap, table *PriceTable, feedMap map[solana.PublicKey]uint32) (PriceMap, int) {
	direct := OneToOneMints()
	out := make(PriceMap, len(onChain))
	for k, v := range onChain {
		out[k] = v
	}
	led := 0
	for bankPk, bank := range banks {
		if _, ok := direct[bank.Mint]; !ok {
			continue
		}
		feed, ok := feedMap[bank.Mint]
		if !ok {
			continue
		}
		p, ok := table.Get(feed)
		if !ok {
			continue
		}
		out[bankPk] = p.Price
		led++
	}
	return out, led
}

// Status is a compact log line describing which Lazer majors are live (for
// boot output).
func Status(table *PriceTable) string {
	f := func(id uint32, name string) (string, bool) {
		p, ok := table.Get(id)
		if !ok {
			return "", false
		}
		return fmt.Sprintf("%s=$%.2f", name, p.Price), true
	}
	g := func(id uint32, name string) (string, bool) {
		p, ok := table.Get(id)
		if !ok {
			return "", false
		}
		return fmt.Sprintf("%s=$%.6f", name, p.Price), true
	}
	var parts []string
	for _, pair := range []struct {
		fn   func(uint32, string) (string, bool)
		id   uint32
		name string
	}{
		{f, LazerSOL, "SOL"},
		{f, LazerBTC, "BTC"},
		{f, LazerETH, "ETH"},
		{f, LazerUSDC, "USDC"},
		{g, LazerBONK, "BONK"},
		{f, LazerJUP, "JUP"},
	} {
		if s, ok := pair.fn(pair.id, pair.name); ok {
			parts = append(parts, s)
		}
	}
	out := ""
	for i, s := range parts {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}
