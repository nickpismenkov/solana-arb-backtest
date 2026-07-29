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
// crypto majors are -8). We request "exponent" as a property and fall back
// to -8 if the server omits it.
package pyth

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// LazerURL returns the Pyth Lazer WebSocket endpoint. Default is the
// Cloudflare-anycast host (pyth-lazer.dourolabs.app) which routes to the
// nearest edge — measured ~2.5x faster to connect than the pinned
// pyth-lazer-0 origin. Override with LAZER_URL; measure
// `curl -w %{time_connect}` to each candidate from the box and pick the
// lowest. (Also available: pyth-lazer-0/-1 direct origins.)
func LazerURL() string {
	if v := os.Getenv("LAZER_URL"); v != "" {
		return v
	}
	return "wss://pyth-lazer.dourolabs.app/v1/stream"
}

// LazerChannel returns the Lazer delivery channel. Default `real_time` =
// lowest latency (each update pushed the instant it's computed, 1-50ms),
// vs `fixed_rate@50ms` which only snapshots every 50ms and adds up to that
// much batching latency to detect_lag. Override with LAZER_CHANNEL (e.g.
// `fixed_rate@1ms`, `fixed_rate@50ms`).
func LazerChannel() string {
	if v := os.Getenv("LAZER_CHANNEL"); v != "" {
		return v
	}
	return "real_time"
}

const defaultExponent = -8

// PricePoint is a single price observation, already scaled to a real
// number (price x 10^exp).
type PricePoint struct {
	Price float64
	// TsUs is the Lazer publish timestamp, microseconds since epoch (0 if
	// absent).
	TsUs uint64
}

// PriceTable is a shared, lock-guarded map feed_id -> latest price. Cheap
// to copy (holds a pointer to the shared state).
type PriceTable struct {
	mu     sync.RWMutex
	prices map[uint32]PricePoint
}

// NewTable creates an empty, ready-to-use PriceTable.
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

func (t *PriceTable) set(feedID uint32, p PricePoint) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prices[feedID] = p
}

// SpawnLazer spawns the Lazer feed as a background goroutine. Reconnects
// forever on error. The returned table is updated in place as prices
// arrive.
func SpawnLazer(token string, feedIDs []uint32, table *PriceTable) {
	go func() {
		backoffMs := int64(500)
		for {
			err := run(context.Background(), token, feedIDs, table)
			if err == nil {
				// Clean close — reconnect promptly.
				backoffMs = 500
			} else {
				fmt.Fprintf(os.Stderr, "[pyth-lazer] %v; reconnecting in %dms\n", err, backoffMs)
			}
			time.Sleep(time.Duration(backoffMs) * time.Millisecond)
			backoffMs *= 2
			if backoffMs > 10_000 {
				backoffMs = 10_000
			}
		}
	}()
}

// subscribeMsg mirrors the Lazer "subscribe" request shape.
type subscribeMsg struct {
	Type           string   `json:"type"`
	SubscriptionID int      `json:"subscriptionId"`
	PriceFeedIDs   []uint32 `json:"priceFeedIds"`
	Properties     []string `json:"properties"`
	Formats        []string `json:"formats"`
	Channel        string   `json:"channel"`
	DeliveryFormat string   `json:"deliveryFormat"`
}

// run is one connection lifecycle: connect, subscribe, pump updates into
// the table. Returns nil on a clean server close, an error on any failure
// (-> reconnect).
func run(ctx context.Context, token string, feedIDs []uint32, table *PriceTable) error {
	header := make(map[string][]string)
	header["Authorization"] = []string{"Bearer " + token}

	conn, _, err := websocket.Dial(ctx, LazerURL(), &websocket.DialOptions{
		HTTPHeader: header,
	})
	if err != nil {
		return err
	}
	defer conn.CloseNow()

	sub := subscribeMsg{
		Type:           "subscribe",
		SubscriptionID: 1,
		PriceFeedIDs:   feedIDs,
		Properties:     []string{"price", "exponent"},
		Formats:        []string{},
		Channel:        LazerChannel(),
		DeliveryFormat: "json",
	}
	subBytes, err := json.Marshal(sub)
	if err != nil {
		return err
	}
	if err := conn.Write(ctx, websocket.MessageText, subBytes); err != nil {
		return err
	}

	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			// A normal/clean closure surfaces here as a CloseError with
			// StatusNormalClosure; treat that as a clean close (reconnect
			// promptly, not as a backed-off error).
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				return nil
			}
			return err
		}
		if typ == websocket.MessageText {
			applyUpdate(string(data), table)
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
		lt := lowerASCII(t)
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
	var tsUs uint64
	switch ts := parsed["timestampUs"].(type) {
	case string:
		if n, err := strconv.ParseUint(ts, 10, 64); err == nil {
			tsUs = n
		}
	case float64:
		tsUs = uint64(ts)
	}

	for _, fv := range feeds {
		f, ok := fv.(map[string]any)
		if !ok {
			continue
		}
		idF, ok := f["priceFeedId"].(float64)
		if !ok {
			continue
		}
		// price arrives as a string integer (may also be numeric).
		var raw float64
		var rawOk bool
		switch p := f["price"].(type) {
		case string:
			if n, err := strconv.ParseFloat(p, 64); err == nil {
				raw = n
				rawOk = true
			}
		case float64:
			raw = p
			rawOk = true
		}
		if !rawOk {
			continue
		}
		exp := defaultExponent
		if e, ok := f["exponent"].(float64); ok {
			exp = int(e)
		}
		price := raw * math.Pow(10, float64(exp))
		table.set(uint32(idF), PricePoint{Price: price, TsUs: tsUs})
	}
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
