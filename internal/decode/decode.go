// Package decode implements the shred swap decoder + Address Lookup Table
// resolution. Turns a raw VersionedTransaction (from a shred) into structured
// swaps on our pools: which venue, direction (sell/buy the base asset), and
// amount. ALT resolution also recovers swaps that reference a pool only via a
// lookup table — the ones a static-key match in the ShredStream feed misses.
package decode

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/pools"
)

const (
	OrcaProgram    = "whirLbMiicVdio4qvUfM5KAg6Ct8VwpYzGff3uctyCc"
	RayClmmProgram = "CAMMCzo5YL8w4VFF8KVHrK22GGUsp5VTaW7grrKgrWqK"
)

// Anchor "global:<name>" sighashes. Both Orca & Raydium CLMM use `swap` and
// `swap_v2`; venue is disambiguated by program id, not discriminator.
var (
	discSwap   = [8]byte{0xf8, 0xc6, 0x9e, 0x91, 0xe1, 0x75, 0x87, 0xc8}
	discSwapV2 = [8]byte{0x2b, 0x04, 0xed, 0x0b, 0x1a, 0xc9, 0x1e, 0x62}
)

type Dir int

const (
	SellBase Dir = iota // base in → price down
	BuyBase             // base out → price up
)

type SwapInfo struct {
	Venue         string
	Pool          string
	Dir           Dir
	Amount        uint64 // the instruction's `amount` arg (raw)
	AmountIsInput bool   // exact-input vs exact-output
	Kind          string // "swap" | "swapV2"
}

// AltCache caches decoded Address Lookup Tables (pubkey → its address list).
// ALTs are effectively static, so the cache warms fast and later txns are instant.
type AltCache struct {
	rpc       string
	mu        sync.Mutex
	tables    map[solana.PublicKey][]solana.PublicKey
	lastFetch time.Time
}

func NewAltCache(rpc string) *AltCache {
	return &AltCache{rpc: rpc, tables: make(map[solana.PublicKey][]solana.PublicKey)}
}

// ResolveKeys returns the fully resolved account keys for a message: static
// keys, then ALT writables (per lookup order), then ALT readonlys — matching
// Solana's account index layout. Returns nil,false if any referenced ALT
// can't be fetched/decoded.
func (a *AltCache) ResolveKeys(msg *solana.Message) ([]solana.PublicKey, bool) {
	keys := append([]solana.PublicKey{}, msg.AccountKeys...)
	if msg.GetVersion() != solana.MessageVersionV0 {
		return keys, true // no ALTs
	}
	var writable, readonly []solana.PublicKey
	for _, lookup := range msg.AddressTableLookups {
		table, ok := a.getTable(lookup.AccountKey)
		if !ok {
			return nil, false
		}
		for _, i := range lookup.WritableIndexes {
			if int(i) >= len(table) {
				return nil, false
			}
			writable = append(writable, table[i])
		}
		for _, i := range lookup.ReadonlyIndexes {
			if int(i) >= len(table) {
				return nil, false
			}
			readonly = append(readonly, table[i])
		}
	}
	keys = append(keys, writable...)
	keys = append(keys, readonly...)
	return keys, true
}

// PoolRef is a pool pubkey paired with its venue label.
type PoolRef struct {
	Pool  solana.PublicKey
	Venue string
}

// TouchesPool is a lightweight pool-touch check for the trigger feed: static
// keys first (no RPC), then ALT-referenced addresses by index (cached table,
// no big alloc). Returns the matching venue. NOTE: cold ALTs block on an RPC
// fetch — fine for measurement (cache warms fast); production would pre-load
// the hot ALTs instead of fetching in the hot path.
func (a *AltCache) TouchesPool(msg *solana.Message, poolsList []PoolRef) (string, bool) {
	for _, k := range msg.AccountKeys {
		for _, p := range poolsList {
			if k.Equals(p.Pool) {
				return p.Venue, true
			}
		}
	}
	if msg.GetVersion() == solana.MessageVersionV0 {
		for _, lookup := range msg.AddressTableLookups {
			if !a.ensureTable(lookup.AccountKey) {
				continue
			}
			a.mu.Lock()
			table := a.tables[lookup.AccountKey]
			a.mu.Unlock()
			for _, i := range append(append([]uint8{}, lookup.WritableIndexes...), lookup.ReadonlyIndexes...) {
				if int(i) >= len(table) {
					continue
				}
				addr := table[i]
				for _, p := range poolsList {
					if addr.Equals(p.Pool) {
						return p.Venue, true
					}
				}
			}
		}
	}
	return "", false
}

func (a *AltCache) ensureTable(key solana.PublicKey) bool {
	a.mu.Lock()
	if _, ok := a.tables[key]; ok {
		a.mu.Unlock()
		return true
	}
	// Cap cold-ALT fetches at ~10/s. Failed fetches aren't cached, so without
	// this a rate-limited RPC triggers a self-sustaining retry flood off the
	// txn firehose, starving every other consumer of the endpoint (e.g. the
	// executor's blockhash refresh). Hot ALTs recur constantly, so the cache
	// still warms within seconds.
	if !a.lastFetch.IsZero() && time.Since(a.lastFetch) < 100*time.Millisecond {
		a.mu.Unlock()
		return false
	}
	a.lastFetch = time.Now()
	a.mu.Unlock()

	data, ok := fetchAccountData(a.rpc, key)
	if !ok || len(data) < 56 {
		return false
	}
	// ALT: LookupTableMeta is 56 bytes; addresses follow, 32 bytes each.
	var addrs []solana.PublicKey
	for o := 56; o+32 <= len(data); o += 32 {
		addrs = append(addrs, solana.PublicKeyFromBytes(data[o:o+32]))
	}
	a.mu.Lock()
	a.tables[key] = addrs
	a.mu.Unlock()
	return true
}

func (a *AltCache) getTable(key solana.PublicKey) ([]solana.PublicKey, bool) {
	if !a.ensureTable(key) {
		return nil, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tables[key], true
}

// DecodeSwaps decodes all swaps on our pools in a transaction, given its
// resolved keys.
func DecodeSwaps(msg *solana.Message, keys []solana.PublicKey) []SwapInfo {
	cfg := pools.Pair()
	orcaPool := solana.MustPublicKeyFromBase58(cfg.OrcaPool)
	rayPool := solana.MustPublicKeyFromBase58(cfg.RayPool)
	orcaProg := solana.MustPublicKeyFromBase58(OrcaProgram)
	rayProg := solana.MustPublicKeyFromBase58(RayClmmProgram)
	rayVault0 := solana.MustPublicKeyFromBase58(cfg.RayVault0)

	var out []SwapInfo
	for _, ix := range msg.Instructions {
		if int(ix.ProgramIDIndex) >= len(keys) {
			continue
		}
		program := keys[ix.ProgramIDIndex]
		var venue string
		switch {
		case program.Equals(orcaProg):
			venue = "Orca"
		case program.Equals(rayProg):
			venue = "Raydium"
		default:
			continue
		}
		if len(ix.Data) < 8 {
			continue
		}
		var disc [8]byte
		copy(disc[:], ix.Data[:8])
		var kind string
		switch disc {
		case discSwap:
			kind = "swap"
		case discSwapV2:
			kind = "swapV2"
		default:
			continue
		}

		ixKeys := make([]solana.PublicKey, 0, len(ix.Accounts))
		for _, a := range ix.Accounts {
			if int(a) < len(keys) {
				ixKeys = append(ixKeys, keys[a])
			}
		}

		var pool string
		var dir Dir
		if venue == "Orca" {
			if !containsKey(ixKeys, orcaPool) {
				continue
			}
			// swap args: amount u64, thresh u64, sqrtLimit u128, amtSpecIsInput bool, aToB bool
			if len(ix.Data) < 42 {
				continue
			}
			// NOTE: assumes mintA is the base asset (true for the default
			// SOL/USDC pool; verify per pair) → A→B = sell base.
			aToB := ix.Data[41] != 0
			pool = cfg.OrcaPool
			if aToB {
				dir = SellBase
			} else {
				dir = BuyBase
			}
		} else {
			if !containsKey(ixKeys, rayPool) {
				continue
			}
			// Raydium CLMM swap accounts: input_vault at index 5.
			if len(ixKeys) <= 5 {
				continue
			}
			inputVault := ixKeys[5]
			pool = cfg.RayPool
			if inputVault.Equals(rayVault0) {
				dir = SellBase
			} else {
				dir = BuyBase
			}
		}

		amount := leUint64(ix.Data[8:16])
		// Orca: amountSpecifiedIsInput @40; Raydium CLMM: isBaseInput @40.
		amountIsInput := true
		if len(ix.Data) > 40 {
			amountIsInput = ix.Data[40] != 0
		}
		out = append(out, SwapInfo{
			Venue:         venue,
			Pool:          pool,
			Dir:           dir,
			Amount:        amount,
			AmountIsInput: amountIsInput,
			Kind:          kind,
		})
	}
	return out
}

func containsKey(keys []solana.PublicKey, k solana.PublicKey) bool {
	for _, x := range keys {
		if x.Equals(k) {
			return true
		}
	}
	return false
}

func leUint64(b []byte) uint64 {
	var v uint64
	for i := 7; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	return v
}

func fetchAccountData(rpc string, key solana.PublicKey) ([]byte, bool) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{key.String(), map[string]string{"encoding": "base64"}},
	})
	resp, err := http.Post(rpc, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	var v struct {
		Result struct {
			Value struct {
				Data []string `json:"data"`
			} `json:"value"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, false
	}
	if len(v.Result.Value.Data) == 0 {
		return nil, false
	}
	data, err := base64.StdEncoding.DecodeString(v.Result.Value.Data[0])
	if err != nil {
		return nil, false
	}
	return data, true
}
