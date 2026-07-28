// Parse Pyth's Hermes "accumulator update" blob into the pieces the on-chain
// crank consumes: the Wormhole VAA (guardian-signed merkle root) and, per
// feed, the price MESSAGE + its merkle PROOF. This is the data layer of the
// self-crank pipeline (step 3): Hermes gives us the signed update; the VAA
// goes to the Wormhole-verify program, and each {message, proof} goes to the
// Pyth push wrapper which writes the sponsored feed marginfi reads.
//
// Format (AccumulatorUpdateData, VERIFIED against a live SOL update — the
// split matches the ~247B VAA / ~396B per-update seen in a real mainnet
// crank tx):
//
//	magic "PNAU" (0x504e4155) · major u8 · minor u8 ·
//	trailing_hdr_len u8 + that many bytes ·
//	update_type u8 (0 = WormholeMerkle) ·
//	vaa_len u16 (BE) + vaa bytes ·
//	num_updates u8 ·
//	[ message_len u16 (BE) + message · proof (merkle path: u8 count + count×20B) ] × num_updates
//
// The 32-byte feed id lives inside each message (a PriceFeedMessage: variant
// u8=0, then feed_id[32], …) so we can match the update to a bank's feed.
package pyth

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const accumulatorMagic uint32 = 0x504e_4155 // "PNAU"

// MerkleUpdate is one feed's price update inside an accumulator blob.
type MerkleUpdate struct {
	// Message is the price message (PriceFeedMessage bytes) — variant byte
	// then feed_id@1.
	Message []byte
	// Proof is the merkle proof path: a length-prefixed list of 20-byte
	// hashes, kept as the raw wire bytes (count u8 + count×20) so it
	// re-serializes verbatim.
	Proof []byte
}

// FeedID returns the 32-byte Pyth feed id (offset 1: after the 1-byte
// message variant).
func (m *MerkleUpdate) FeedID() ([32]byte, bool) {
	var out [32]byte
	if len(m.Message) < 33 {
		return out, false
	}
	copy(out[:], m.Message[1:33])
	return out, true
}

// PriceUSD returns the USD price from the PriceFeedMessage (big-endian, like
// the rest of the accumulator wire format: variant u8, feed_id[32], price
// i64@33, conf u64@41, expo i32@49). This is EXACTLY the price the crank
// posts on-chain, so health judged at it predicts the liquidate ix outcome.
func (m *MerkleUpdate) PriceUSD() (float64, bool) {
	if len(m.Message) < 53 {
		return 0, false
	}
	px := int64(binary.BigEndian.Uint64(m.Message[33:41]))
	expo := int32(binary.BigEndian.Uint32(m.Message[49:53]))
	return float64(px) * math.Pow(10, float64(expo)), true
}

// AccumulatorUpdate is the parsed accumulator blob.
type AccumulatorUpdate struct {
	// VAA is the guardian-signed Wormhole VAA (goes to the verify program).
	VAA     []byte
	Updates []MerkleUpdate
}

type cursor struct {
	b []byte
	i int
}

func (c *cursor) u8() (byte, error) {
	if c.i >= len(c.b) {
		return 0, fmt.Errorf("eof u8")
	}
	v := c.b[c.i]
	c.i++
	return v, nil
}

func (c *cursor) u16be() (int, error) {
	hi, err := c.u8()
	if err != nil {
		return 0, err
	}
	lo, err := c.u8()
	if err != nil {
		return 0, err
	}
	return (int(hi) << 8) | int(lo), nil
}

func (c *cursor) take(n int) ([]byte, error) {
	if c.i+n > len(c.b) {
		return nil, fmt.Errorf("eof take %d", n)
	}
	s := c.b[c.i : c.i+n]
	c.i += n
	return s, nil
}

// ParseBase64 parses one base64 accumulator blob (as Hermes returns in
// binary.data[0]).
func ParseBase64(b64 string) (*AccumulatorUpdate, error) {
	bs, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	return Parse(bs)
}

func Parse(data []byte) (*AccumulatorUpdate, error) {
	c := &cursor{b: data}
	magicBytes, err := c.take(4)
	if err != nil {
		return nil, err
	}
	magic := binary.BigEndian.Uint32(magicBytes)
	if magic != accumulatorMagic {
		return nil, fmt.Errorf("bad magic %#x", magic)
	}
	if _, err := c.u8(); err != nil { // major
		return nil, err
	}
	if _, err := c.u8(); err != nil { // minor
		return nil, err
	}
	trailingByte, err := c.u8()
	if err != nil {
		return nil, err
	}
	if _, err := c.take(int(trailingByte)); err != nil { // skip trailing header
		return nil, err
	}
	updateType, err := c.u8()
	if err != nil {
		return nil, err
	}
	if updateType != 0 {
		return nil, fmt.Errorf("unsupported update_type %d", updateType)
	}
	vaaLen, err := c.u16be()
	if err != nil {
		return nil, err
	}
	vaaBytes, err := c.take(vaaLen)
	if err != nil {
		return nil, err
	}
	vaa := append([]byte(nil), vaaBytes...)
	numByte, err := c.u8()
	if err != nil {
		return nil, err
	}
	num := int(numByte)
	updates := make([]MerkleUpdate, 0, num)
	for i := 0; i < num; i++ {
		msgLen, err := c.u16be()
		if err != nil {
			return nil, err
		}
		msgBytes, err := c.take(msgLen)
		if err != nil {
			return nil, err
		}
		message := append([]byte(nil), msgBytes...)
		// proof = count u8 + count x 20-byte hashes (kept as raw wire bytes).
		proofStart := c.i
		count, err := c.u8()
		if err != nil {
			return nil, err
		}
		if _, err := c.take(int(count) * 20); err != nil {
			return nil, err
		}
		proof := append([]byte(nil), data[proofStart:c.i]...)
		updates = append(updates, MerkleUpdate{Message: message, Proof: proof})
	}
	return &AccumulatorUpdate{VAA: vaa, Updates: updates}, nil
}

// httpClient is the shared client used for Hermes fetches (poll + stream).
var httpClient = &http.Client{Timeout: 10 * time.Second}

// FetchHermes fetches the latest signed update for a set of hex feed ids
// from Hermes.
func FetchHermes(hermes string, feedIDsHex []string) (*AccumulatorUpdate, error) {
	var ids strings.Builder
	for _, f := range feedIDsHex {
		fmt.Fprintf(&ids, "&ids[]=%s", f)
	}
	url := fmt.Sprintf("%s/v2/updates/price/latest?encoding=base64%s", hermes, ids.String())
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	b64, ok := hermesBinaryData0(v)
	if !ok {
		return nil, fmt.Errorf("no binary.data: %s", string(raw))
	}
	return ParseBase64(b64)
}

func hermesBinaryData0(v map[string]any) (string, bool) {
	binary_, ok := v["binary"].(map[string]any)
	if !ok {
		return "", false
	}
	data, ok := binary_["data"].([]any)
	if !ok || len(data) == 0 {
		return "", false
	}
	s, ok := data[0].(string)
	return s, ok
}

// ── Hermes cache: keep the latest signed blob hot for the fire path ─────────

// HermesCache holds the latest Hermes blob for a (settable) feed set,
// refreshed by a background goroutine so the crank fire path never waits on
// an HTTP round-trip. One blob covers the whole set: a single VAA proves
// every update in it.
type HermesCache struct {
	mu     sync.RWMutex
	latest *cachedUpdate
	feeds  []string
}

type cachedUpdate struct {
	update *AccumulatorUpdate
	at     time.Time
}

// Latest returns the latest parsed blob and its age. ok=false until the
// first successful fetch.
func (c *HermesCache) Latest() (*AccumulatorUpdate, time.Duration, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.latest == nil {
		return nil, 0, false
	}
	return c.latest.update, time.Since(c.latest.at), true
}

// UpdateFor returns the update for one feed from the latest blob, with the
// blob's age.
func (c *HermesCache) UpdateFor(feedID [32]byte) (MerkleUpdate, []byte, time.Duration, bool) {
	blob, age, ok := c.Latest()
	if !ok {
		return MerkleUpdate{}, nil, 0, false
	}
	for _, u := range blob.Updates {
		if id, ok := u.FeedID(); ok && id == feedID {
			return u, blob.VAA, age, true
		}
	}
	return MerkleUpdate{}, nil, 0, false
}

// SetFeeds replaces the polled feed set (hex ids); takes effect next poll.
func (c *HermesCache) SetFeeds(feedIDsHex []string) {
	c.mu.Lock()
	c.feeds = feedIDsHex
	c.mu.Unlock()
}

func (c *HermesCache) getFeeds() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]string(nil), c.feeds...)
}

func (c *HermesCache) store(u *AccumulatorUpdate) {
	c.mu.Lock()
	c.latest = &cachedUpdate{update: u, at: time.Now()}
	c.mu.Unlock()
}

func feedsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// SpawnHermesStream fetches the latest signed update for a set of hex feed
// ids by holding ONE SSE connection open and updating the cache on every
// pushed price (latency ~10-30ms vs the 400ms poll), so the blob we crank
// stays in lockstep with the Lazer trigger. Reconnects on drop / feed
// change. Feeds are fixed at spawn (our crankable set); SetFeeds triggers a
// reconnect.
func SpawnHermesStream(hermes string, feedIDsHex []string) *HermesCache {
	cache := &HermesCache{feeds: feedIDsHex}
	go func() {
		// Read timeout catches a stalled stream (Hermes pushes many/sec, so
		// 20s idle = dead) → reconnect.
		client := &http.Client{Timeout: 0}
		for {
			feeds := cache.getFeeds()
			if len(feeds) == 0 {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			var ids strings.Builder
			for _, f := range feeds {
				fmt.Fprintf(&ids, "&ids[]=%s", f)
			}
			url := fmt.Sprintf("%s/v2/updates/price/stream?encoding=base64%s", hermes, ids.String())

			req, err := http.NewRequest(http.MethodGet, url, nil)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[hermes-stream] disconnected (%v); reconnecting\n", err)
				time.Sleep(300 * time.Millisecond)
				continue
			}
			resp, err := client.Do(req)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[hermes-stream] disconnected (%v); reconnecting\n", err)
				time.Sleep(300 * time.Millisecond)
				continue
			}
			reader := bufio.NewReader(resp.Body)
			for {
				line, err := readLineTimeout(reader, 20*time.Second)
				if err != nil {
					break
				}
				if payload, ok := strings.CutPrefix(line, "data:"); ok {
					payload = strings.TrimSpace(payload)
					if payload == "" {
						continue
					}
					var v map[string]any
					if err := json.Unmarshal([]byte(payload), &v); err == nil {
						if b64, ok := hermesBinaryData0(v); ok {
							if u, err := ParseBase64(b64); err == nil {
								cache.store(u)
							}
						}
					}
				}
				// feeds changed → break to reconnect with the new set.
				if !feedsEqual(cache.getFeeds(), feeds) {
					break
				}
			}
			resp.Body.Close()
			time.Sleep(300 * time.Millisecond) // reconnect backoff
		}
	}()
	return cache
}

// readLineTimeout reads one line, treating a stall longer than d as an
// error (equivalent to ureq's timeout_read).
func readLineTimeout(r *bufio.Reader, d time.Duration) (string, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := r.ReadString('\n')
		ch <- result{line, err}
	}()
	select {
	case res := <-ch:
		if res.err != nil && res.line == "" {
			return "", res.err
		}
		return strings.TrimRight(res.line, "\r\n"), nil
	case <-time.After(d):
		return "", fmt.Errorf("read timeout")
	}
}

// SpawnHermesCache spawns the poll thread. Errors are retried on the next
// tick; the cache keeps the last good blob (callers gate on age).
func SpawnHermesCache(hermes string, feedIDsHex []string, interval time.Duration) *HermesCache {
	cache := &HermesCache{feeds: feedIDsHex}
	go func() {
		for {
			feeds := cache.getFeeds()
			if len(feeds) > 0 {
				u, err := FetchHermes(hermes, feeds)
				if err != nil {
					fmt.Fprintf(os.Stderr, "[hermes] fetch failed (will retry): %v\n", err)
				} else {
					cache.store(u)
				}
			}
			time.Sleep(interval)
		}
	}()
	return cache
}
