// Package shredstream is the Leg-1 ShredStream trigger feed. In the
// original Rust it wraps the shredstream.com pure-Rust SDK (crate
// `shredstream = "2.0.0"`, per Cargo.toml) and emits a Trigger the instant
// a swap touches one of our pools = how fast WE'd see the dislocating
// swap. The Rust `.transactions()` iterator is blocking, so it runs on its
// own OS thread and sends Triggers over a channel; prices/arb come from
// the gRPC (later: shred-sourced) feed and the harness correlates the two.
//
// NOTE: matches pools via static account keys — v0 txns that reference a
// pool only through an Address Lookup Table won't match until ALT
// resolution lands (watch the pool-hit heartbeat vs txns-seen to gauge the
// gap).
//
// ── Fidelity note (read before touching ShredListener) ──────────────────────
// shredstream.rs itself contains NO shred-reassembly / erasure-coding logic:
// all of that lives inside the external `shredstream` Rust crate (Cargo.toml
// pins `shredstream = "2.0.0"`), whose source is not available in this
// porting session (no vendored copy, no docs.rs mirror fetched). That crate
// is Jito's client for the ShredStream service: it receives raw shred UDP
// packets, reassembles them per-slot using Reed-Solomon erasure coding
// (recovering a full FEC set from a subset of data+coding shreds), and
// yields fully deshredded `(slot, Vec<VersionedTransaction>)` batches via
// its blocking `.transactions()` iterator. Reimplementing that wire format
// and erasure-coding correctly from scratch, with no reference source, would
// produce something that LOOKS like a port but silently decodes garbage —
// worse than being honest about the gap.
//
// So this file is faithful for everything shredstream.rs actually contains
// (the goroutine/thread spawn, the pool-touch match against static keys or
// ALT-resolved keys via decode.AltCache, the DecodeSwaps call, the Trigger
// construction and channel send, and the seen/hits heartbeat), and it is an
// EXPLICIT STUB for exactly one thing: turning received UDP bytes into
// `(slot, []VersionedTransaction)`. ShredListener below binds the real UDP
// port with net.ListenUDP and receives real datagrams (that part is not
// simulated), but RecvTransactions currently cannot deshred them — see its
// doc comment — and returns them as opaque, framing-only placeholders so the
// rest of the pipeline (pool match -> DecodeSwaps -> Trigger send) still
// typechecks and runs end-to-end against a real socket. Replacing
// RecvTransactions' body with a real Reed-Solomon deshredder (once the
// `shredstream` crate's wire format is available to consult) is the only
// change a future implementation needs to make; nothing downstream should
// need to change.
package shredstream

import (
	"fmt"
	"net"
	"os"
	"time"

	"arbengine/internal/decode"
	"arbengine/internal/pools"
	"arbengine/internal/solana"
)

// Trigger is a decoded, pool-touching transaction observed on the shred
// feed, timestamped at receipt.
type Trigger struct {
	Venue string
	Slot  uint64
	TsMs  int64 // stamped at receipt, before downstream work (unix ms)
	Sig   string
	Raw   []byte            // serialized victim tx (for co-bundle [victim, arb])
	Swaps []decode.SwapInfo // decoded direct swaps on our pools (empty if routed/CPI)
}

func nowMs() int64 {
	return time.Now().UnixNano() / int64(time.Millisecond)
}

// poolRef pairs a pool address with its venue label, mirroring the two
// hard-coded `pools` tuples built in run_shredstream_feed.
type poolRef struct {
	Pubkey solana.Pubkey
	Venue  string
}

// watchedPools derives the (pool, venue) pairs from the configured pair,
// exactly like the Rust closure literal `[(orca_pool, "Orca"), (ray_pool,
// "Raydium")]`.
func watchedPools() []poolRef {
	cfg := pools.Pair()
	return []poolRef{
		{Pubkey: solana.MustPubkeyFromBase58(cfg.OrcaPool), Venue: "Orca"},
		{Pubkey: solana.MustPubkeyFromBase58(cfg.RayPool), Venue: "Raydium"},
	}
}

// RunShredstreamFeed spawns the blocking ShredStream listener on its own
// goroutine (mirroring the Rust `std::thread::spawn` — the underlying
// receive loop blocks on the socket, so it must not run on a shared
// executor). Logs a "txns seen / pool-hits" heartbeat every ~10s.
//
// If rpc is non-empty, ALTs are resolved so pool touches referenced via
// lookup tables are caught too (essential — ~all routed swaps use ALTs).
// If empty, falls back to a static-key match (misses routed swaps).
//
// Returns a channel that closes when the listener goroutine exits (bind
// failure or the underlying receive loop ending), so callers can select on
// it exactly like joining the Rust thread::JoinHandle.
func RunShredstreamFeed(port uint16, rpc string, tx chan<- Trigger) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		runListener(port, rpc, tx)
	}()
	return done
}

func runListener(port uint16, rpc string, tx chan<- Trigger) {
	watched := watchedPools()

	var alt *decode.AltCache
	if rpc != "" {
		alt = decode.NewAltCache(rpc)
	}

	listener, err := BindShredListener(port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[shredstream] bind udp/%d failed: %v\n", port, err)
		return
	}
	defer listener.Close()

	altStatus := "off (static-key only)"
	if alt != nil {
		altStatus = "on"
	}
	fmt.Fprintf(os.Stderr, "[shredstream] listening on udp/%d (ALT resolution: %s)\n", port, altStatus)

	var seen, hits uint64
	lastHB := time.Now()

	poolRefs := make([]decode.PoolRef, len(watched))
	for i, p := range watched {
		poolRefs[i] = decode.PoolRef{Pool: p.Pubkey, Venue: p.Venue}
	}

	for {
		slot, txns, err := listener.RecvTransactions()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[shredstream] recv error: %v\n", err)
			return
		}
		tsMs := nowMs()
		for _, txn := range txns {
			seen++
			var venue string
			if alt != nil {
				venue = alt.TouchesPool(txn.Message, poolRefs)
			} else {
				keys := txn.Message.StaticAccountKeys()
				for _, p := range watched {
					if containsPubkey(keys, p.Pubkey) {
						venue = p.Venue
						break
					}
				}
			}
			if venue == "" {
				continue
			}
			hits++
			// Pool-hit (rare vs total txns) -> do the expensive work here:
			// fully resolve keys and decode direct swaps to attach to the
			// trigger, so the executor's hot path does zero RPC/resolution.
			// Routed/CPI swaps decode to empty (not co-bundlable).
			var swaps []decode.SwapInfo
			if alt != nil {
				if keys, ok := alt.ResolveKeys(txn.Message); ok {
					swaps = decode.DecodeSwaps(txn, keys)
				}
			} else {
				swaps = decode.DecodeSwaps(txn, txn.Message.StaticAccountKeys())
			}
			var sig string
			if len(txn.Signatures) > 0 {
				sig = txn.Signatures[0].String()
			}
			raw, err := txn.MarshalBinary()
			if err != nil {
				raw = nil
			}
			select {
			case tx <- Trigger{
				Venue: venue,
				Slot:  slot,
				TsMs:  tsMs,
				Sig:   sig,
				Raw:   raw,
				Swaps: swaps,
			}:
			default:
				// Mirrors the Rust `let _ = tx.send(...)`: never block the
				// listener on a full/closed downstream channel.
			}
		}
		if time.Since(lastHB) >= 10*time.Second {
			fmt.Fprintf(os.Stderr, "[shredstream] txns seen=%d pool-hits=%d\n", seen, hits)
			lastHB = time.Now()
		}
	}
}

func containsPubkey(keys []solana.Pubkey, want solana.Pubkey) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}

// ── UDP listener ─────────────────────────────────────────────────────────────
//
// ShredListener owns the real UDP socket. Binding and receiving raw
// datagrams is faithfully implemented with net.ListenUDP; turning those
// datagrams into deshredded transactions is NOT (see the package doc
// comment and RecvTransactions below).

// maxDatagramSize is large enough for any single UDP datagram Solana shreds
// are packetized into (shreds are well under the classic 1232-byte MTU
// budget, but this leaves headroom rather than silently truncating).
const maxDatagramSize = 2048

// ShredListener binds a UDP socket and (once a real deshredder is plugged
// into RecvTransactions) yields reassembled per-slot transaction batches,
// mirroring the Rust `shredstream::ShredListener` handle returned by
// `ShredListener::bind`.
type ShredListener struct {
	conn *net.UDPConn
}

// BindShredListener opens the UDP socket the ShredStream service will
// forward shreds to. This part has a real implementation (no stubbing): it
// is a plain net.ListenUDP, exactly mirroring what `shredstream::
// ShredListener::bind(port)` does at the transport layer.
func BindShredListener(port uint16) (*ShredListener, error) {
	addr := &net.UDPAddr{Port: int(port)}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, err
	}
	return &ShredListener{conn: conn}, nil
}

// Close releases the UDP socket.
func (l *ShredListener) Close() error {
	return l.conn.Close()
}

// RecvTransactions blocks for the next inbound UDP datagram and reports the
// slot + decoded transactions it represents.
//
// STUB: a real shred is one Reed-Solomon-coded fragment of a slot's entries,
// not a full transaction — the shredstream crate buffers many shreds per
// slot across two matrices (data shreds + coding shreds), reconstructs any
// missing data shreds once enough of the FEC set has arrived, deserializes
// the recovered entry bytes into `Vec<Entry>`, and only then hands back the
// `VersionedTransaction`s embedded in those entries. None of that
// deshredding/erasure-coding logic lives in shredstream.rs — it is entirely
// inside the external crate, whose source this session has no access to
// (see the package doc comment for why it is not safe to guess at).
//
// So this function reads one real datagram off the real socket (proving the
// transport half of the port is faithful) but cannot deshred it: it returns
// (0, nil, nil) for every datagram — a no-op batch — rather than fabricate a
// wire format that would silently corrupt downstream decoding. Swapping in
// a real deshredder here (parse shred header -> group by (slot, fec_set) ->
// Reed-Solomon recover -> reassemble entries -> extract transactions) is the
// only change RunShredstreamFeed's caller-visible contract needs; the
// pool-match / DecodeSwaps / Trigger-send pipeline above already consumes
// exactly the (slot, []VersionedTransaction) shape a real implementation
// would produce.
func (l *ShredListener) RecvTransactions() (uint64, []solana.VersionedTransaction, error) {
	buf := make([]byte, maxDatagramSize)
	n, _, err := l.conn.ReadFromUDP(buf)
	if err != nil {
		return 0, nil, err
	}
	_ = buf[:n] // raw shred bytes: real deshredding belongs here (see doc comment)
	return 0, nil, nil
}
