// Package shredstream implements the leg-1 ShredStream trigger feed. Emits a
// Trigger the instant a swap touches one of our pools = how fast WE'd see the
// dislocating swap.
//
// PORTING NOTE: the original Rust used the third-party `shredstream` crate
// (shredstream.com's pure-Rust SDK), which does the actual Solana Turbine
// shred reassembly (UDP receipt + Reed-Solomon FEC decode + entry parsing) —
// that reconstruction logic lives inside the external crate, not in this
// repository, so there is nothing of *this* codebase's logic to port for it.
// This package faithfully ports everything that IS this repo's code: the UDP
// listener lifecycle, the pool-touch match, ALT resolution, swap decode, the
// Trigger channel, and the seen/hits heartbeat. The raw-shred → transaction
// reconstruction is left as a pluggable Decoder so this package is a drop-in
// once wired to a real shred source (e.g. a local shredstream-proxy speaking
// its actual framing).
package shredstream

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/decode"
	"solana-arb-backtest-go/internal/pools"
)

// Trigger is stamped at receipt, before downstream work.
type Trigger struct {
	Venue string
	Slot  uint64
	TsMs  uint64
	Sig   string
	Raw   []byte            // serialized victim tx (for co-bundle [victim, arb])
	Swaps []decode.SwapInfo // decoded direct swaps on our pools (empty if routed/CPI)
}

// Decoder turns one raw UDP datagram from the shred source into zero or more
// reassembled (slot, message) pairs. The default decoder (see NewUDPListener)
// is a stub that reports no transactions — swap in a real Turbine/shred-proxy
// decoder to make this feed live.
type Decoder func(packet []byte) (slot uint64, msgs []*solana.Message, sigs []string, ok bool)

func nowMs() uint64 { return uint64(time.Now().UnixMilli()) }

// PoolRef pairs a pool pubkey with its venue label.
type PoolRef = decode.PoolRef

// RunFeed spawns the (blocking) ShredStream listener on its own goroutine and
// pushes Triggers to tx. Logs a `txns seen / pool-hits` heartbeat every ~10s.
//
// If rpc is non-empty, ALTs are resolved so pool touches referenced via
// lookup tables are caught too (essential — ~all routed swaps use ALTs). If
// empty, falls back to a static-key match (misses routed swaps).
func RunFeed(port uint16, rpc string, decoder Decoder, tx chan<- Trigger) error {
	pair := pools.Pair()
	orcaPool, err := solana.PublicKeyFromBase58(pair.OrcaPool)
	if err != nil {
		return fmt.Errorf("bad ORCA_POOL: %w", err)
	}
	rayPool, err := solana.PublicKeyFromBase58(pair.RayPool)
	if err != nil {
		return fmt.Errorf("bad RAY_CLMM_POOL: %w", err)
	}
	poolsList := []PoolRef{{Pool: orcaPool, Venue: "Orca"}, {Pool: rayPool, Venue: "Raydium"}}

	var alt *decode.AltCache
	if rpc != "" {
		alt = decode.NewAltCache(rpc)
	}

	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: int(port)})
	if err != nil {
		return fmt.Errorf("bind udp/%d failed: %w", port, err)
	}
	defer conn.Close()

	fmt.Fprintf(os.Stderr, "[shredstream] listening on udp/%d (ALT resolution: %s)\n",
		port, onOff(alt != nil))

	if decoder == nil {
		decoder = stubDecoder
	}

	var seen, hits uint64
	lastHb := time.Now()
	buf := make([]byte, 64*1024)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return err
		}
		tsMs := nowMs()
		slot, msgs, sigs, ok := decoder(buf[:n])
		if !ok {
			continue
		}
		for i, msg := range msgs {
			seen++
			var venue string
			var found bool
			if alt != nil {
				venue, found = alt.TouchesPool(msg, poolsList)
			} else {
				for _, k := range msg.AccountKeys {
					for _, p := range poolsList {
						if k.Equals(p.Pool) {
							venue, found = p.Venue, true
							break
						}
					}
					if found {
						break
					}
				}
			}
			if !found {
				continue
			}
			hits++
			// Pool-hit (rare vs total txns) → do the expensive work here: fully
			// resolve keys and decode direct swaps to attach to the trigger, so
			// the executor's hot path does zero RPC/resolution.
			var swaps []decode.SwapInfo
			if alt != nil {
				if keys, ok := alt.ResolveKeys(msg); ok {
					swaps = decode.DecodeSwaps(msg, keys)
				}
			} else {
				swaps = decode.DecodeSwaps(msg, msg.AccountKeys)
			}
			var sig string
			if i < len(sigs) {
				sig = sigs[i]
			}
			select {
			case tx <- Trigger{Venue: venue, Slot: slot, TsMs: tsMs, Sig: sig, Swaps: swaps}:
			default:
			}
		}
		if time.Since(lastHb) >= 10*time.Second {
			fmt.Fprintf(os.Stderr, "[shredstream] txns seen=%d pool-hits=%d\n", seen, hits)
			lastHb = time.Now()
		}
	}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off (static-key only)"
}

// stubDecoder never reassembles a transaction — see the package doc.
func stubDecoder([]byte) (uint64, []*solana.Message, []string, bool) {
	return 0, nil, nil, false
}
