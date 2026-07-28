// Package pyth implements the Pyth Lazer ("Pyth Pro") price feed — a
// reconnecting WebSocket client that streams live prices into a shared
// in-memory table. This is the fast trigger + fair-value source for the
// liquidation engine: Lazer delivers ms-grade updates so we can PRE-BUILD a
// liquidation the instant a price approaches the threshold, then fire the
// moment it crosses.
//
// Auth: Bearer token via PYTH_LAZER_TOKEN (never hardcode; .env only).
// Endpoint + subscribe shape are VERIFIED live against the SOL/USDC feeds.
//
// Feeds are numeric IDs (SOL=6, USDC=7, BTC=1, ETH=2). The parsed message
// carries the raw integer price; the exponent is per-feed static (all the
// crypto majors are -8). We request "exponent" as a property and fall back to
// -8 if the server omits it.
package pyth

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

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

const defaultExponent = -8

// PricePoint is a single price observation, already scaled to a real number
// (price x 10^exp).
type PricePoint struct {
	Price float64
	// TsUs is the Lazer publish timestamp, microseconds since epoch (0 if absent).
	TsUs uint64
}

// PriceTable is a shared, lock-guarded map feed_id -> latest price. Cheap to
// share (pass the pointer around).
type PriceTable struct {
	mu     sync.RWMutex
	prices map[uint32]PricePoint
}

func NewTable() *PriceTable {
	return &PriceTable{prices: make(map[uint32]PricePoint)}
}

// Get reads the latest price for a feed, if we've received one.
func Get(table *PriceTable, feedID uint32) (PricePoint, bool) {
	table.mu.RLock()
	defer table.mu.RUnlock()
	p, ok := table.prices[feedID]
	return p, ok
}

// SpawnLazer spawns the Lazer feed as a background goroutine. Reconnects
// forever on drop/error until ctx is canceled. The returned table is updated
// in place as prices arrive.
func SpawnLazer(ctx context.Context, token string, feedIDs []uint32, table *PriceTable) {
	go func() {
		backoffMs := int64(500)
		for {
			if ctx.Err() != nil {
				return
			}
			if err := runLazer(ctx, token, feedIDs, table); err != nil {
				fmt.Fprintf(os.Stderr, "[pyth-lazer] %v; reconnecting in %dms\n", err, backoffMs)
			} else {
				// Clean close — reconnect promptly.
				backoffMs = 500
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(backoffMs) * time.Millisecond):
			}
			backoffMs *= 2
			if backoffMs > 10_000 {
				backoffMs = 10_000
			}
		}
	}()
}

// runLazer is one connection lifecycle: connect, subscribe, pump updates
// into the table. Returns nil on a clean server close, an error on any
// failure (-> reconnect).
func runLazer(ctx context.Context, token string, feedIDs []uint32, table *PriceTable) error {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)

	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	ws, _, err := dialer.DialContext(ctx, LazerURL(), header)
	if err != nil {
		return err
	}
	defer ws.Close()

	sub := map[string]any{
		"type":           "subscribe",
		"subscriptionId": 1,
		"priceFeedIds":   feedIDs,
		"properties":     []string{"price", "exponent"},
		"formats":        []string{},
		"channel":        LazerChannel(),
		"deliveryFormat": "json",
	}
	subBytes, err := json.Marshal(sub)
	if err != nil {
		return err
	}
	if err := ws.WriteMessage(websocket.TextMessage, subBytes); err != nil {
		return err
	}

	// Close the connection promptly when ctx is canceled.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			ws.Close()
		case <-done:
		}
	}()

	for {
		msgType, data, err := ws.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return nil
			}
			return err
		}
		switch msgType {
		case websocket.TextMessage:
			applyUpdate(string(data), table)
		case websocket.PingMessage:
			_ = ws.WriteMessage(websocket.PongMessage, data)
		case websocket.CloseMessage:
			return nil
		}
	}
}

// applyUpdate parses a Lazer text frame and updates the table. Ignores
// non-price frames (e.g. the initial {"type":"subscribed"} ack) — EXCEPT
// subscription errors, which must be loud: a rejected subscription (e.g. a
// feed that doesn't support the channel) otherwise looks exactly like a
// dead-calm market with 0/N feeds live.
func applyUpdate(text string, table *PriceTable) {
	var v map[string]any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		return
	}
	if t, ok := v["type"].(string); ok {
		lt := strings.ToLower(t)
		if lt == "subscriptionerror" || lt == "error" {
			fmt.Fprintf(os.Stderr, "[pyth-lazer] SUBSCRIPTION REJECTED: %s — no prices will flow; fix the feed list\n", text)
			return
		}
	}
	parsed, _ := v["parsed"].(map[string]any)
	if parsed == nil {
		return
	}
	feeds, ok := parsed["priceFeeds"].([]any)
	if !ok {
		return
	}

	// timestampUs may arrive as a string or a number depending on delivery.
	tsUs := uint64(0)
	switch tv := parsed["timestampUs"].(type) {
	case string:
		if n, err := strconv.ParseUint(tv, 10, 64); err == nil {
			tsUs = n
		}
	case float64:
		tsUs = uint64(tv)
	}

	table.mu.Lock()
	defer table.mu.Unlock()
	for _, fa := range feeds {
		f, ok := fa.(map[string]any)
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
		var haveRaw bool
		switch pv := f["price"].(type) {
		case string:
			if n, err := strconv.ParseFloat(pv, 64); err == nil {
				raw, haveRaw = n, true
			}
		case float64:
			raw, haveRaw = pv, true
		}
		if !haveRaw {
			continue
		}

		exp := defaultExponent
		if ev, ok := f["exponent"].(float64); ok {
			exp = int(ev)
		}
		price := raw * math.Pow(10, float64(exp))
		table.prices[id] = PricePoint{Price: price, TsUs: tsUs}
	}
}
