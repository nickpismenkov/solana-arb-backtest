// Package decode implements the shred/tx swap decoder + Address Lookup
// Table resolution. Turns a raw VersionedTransaction into structured swaps
// on our pools: which venue, direction (sell/buy the base asset), and
// amount. ALT resolution also recovers swaps that reference a pool only via
// a lookup table.
package decode

import (
	"encoding/binary"
	"time"

	"arbengine/internal/pools"
	"arbengine/internal/rpcclient"
	"arbengine/internal/solana"
)

const (
	OrcaProgram    = "whirLbMiicVdio4qvUfM5KAg6Ct8VwpYzGff3uctyCc"
	RayClmmProgram = "CAMMCzo5YL8w4VFF8KVHrK22GGUsp5VTaW7grrKgrWqK"
)

// Anchor "global:<name>" sighashes. Both Orca & Raydium CLMM use `swap` and
// `swap_v2`; venue is disambiguated by program id, not discriminator.
var discSwap = [8]byte{0xf8, 0xc6, 0x9e, 0x91, 0xe1, 0x75, 0x87, 0xc8}
var discSwapV2 = [8]byte{0x2b, 0x04, 0xed, 0x0b, 0x1a, 0xc9, 0x1e, 0x62}

type Dir int

const (
	SellBase Dir = iota // base in -> price down
	BuyBase             // base out -> price up
)

// SwapInfo is a decoded swap instruction touching one of our watched pools.
type SwapInfo struct {
	Venue         string
	Pool          string
	Dir           Dir
	Amount        uint64 // the instruction's `amount` arg (raw)
	AmountIsInput bool   // exact-input vs exact-output
	Kind          string // "swap" | "swapV2"
}

// AltCache caches decoded Address Lookup Tables (pubkey -> its address
// list). ALTs are effectively static, so the cache warms fast and later
// txns are instant.
type AltCache struct {
	rpc       *rpcclient.Client
	tables    map[solana.Pubkey][]solana.Pubkey
	lastFetch time.Time
}

func NewAltCache(rpc string) *AltCache {
	return &AltCache{rpc: rpcclient.New(rpc), tables: make(map[solana.Pubkey][]solana.Pubkey)}
}

// ResolveKeys fully resolves account keys for a message: static keys, then
// ALT writables (per lookup order), then ALT readonlys — matching Solana's
// account index layout. Returns (nil, false) if any referenced ALT can't be
// fetched/decoded.
func (c *AltCache) ResolveKeys(msg solana.VersionedMessage) ([]solana.Pubkey, bool) {
	keys := append([]solana.Pubkey{}, msg.StaticAccountKeys()...)
	if !msg.IsV0 {
		return keys, true // legacy: no ALTs
	}
	var writable, readonly []solana.Pubkey
	for _, lookup := range msg.V0.AddressTableLookups {
		table, ok := c.getTable(lookup.AccountKey)
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

// PoolRef pairs a pool address with its venue label for TouchesPool.
type PoolRef struct {
	Pool  solana.Pubkey
	Venue string
}

// TouchesPool is a lightweight pool-touch check for the trigger feed: static
// keys first (no RPC), then ALT-referenced addresses by index (cached
// table, no big alloc). Returns the matching venue, or "" if none. NOTE:
// cold ALTs block on an RPC fetch — fine for measurement (cache warms
// fast); production would pre-load the hot ALTs instead of fetching in the
// hot path.
func (c *AltCache) TouchesPool(msg solana.VersionedMessage, pools []PoolRef) string {
	for _, k := range msg.StaticAccountKeys() {
		for _, p := range pools {
			if k == p.Pool {
				return p.Venue
			}
		}
	}
	if msg.IsV0 {
		for _, lookup := range msg.V0.AddressTableLookups {
			if !c.ensureTable(lookup.AccountKey) {
				continue
			}
			table := c.tables[lookup.AccountKey]
			indexes := append(append([]uint8{}, lookup.WritableIndexes...), lookup.ReadonlyIndexes...)
			for _, i := range indexes {
				if int(i) >= len(table) {
					continue
				}
				addr := table[i]
				for _, p := range pools {
					if addr == p.Pool {
						return p.Venue
					}
				}
			}
		}
	}
	return ""
}

func (c *AltCache) ensureTable(key solana.Pubkey) bool {
	if _, ok := c.tables[key]; ok {
		return true
	}
	// Cap cold-ALT fetches at ~10/s. Failed fetches aren't cached, so
	// without this a rate-limited RPC triggers a self-sustaining retry
	// flood off the txn firehose, starving every other consumer of the
	// endpoint (e.g. the executor's blockhash refresh). Hot ALTs recur
	// constantly, so the cache still warms within seconds.
	if !c.lastFetch.IsZero() && time.Since(c.lastFetch) < 100*time.Millisecond {
		return false
	}
	c.lastFetch = time.Now()
	data, err := c.rpc.GetAccountData(key)
	if err != nil || data == nil {
		return false
	}
	// ALT: LookupTableMeta is 56 bytes; addresses follow, 32 bytes each.
	if len(data) < 56 {
		return false
	}
	var addrs []solana.Pubkey
	for o := 56; o+32 <= len(data); o += 32 {
		pk, err := solana.PubkeyFromBytes(data[o : o+32])
		if err != nil {
			return false
		}
		addrs = append(addrs, pk)
	}
	c.tables[key] = addrs
	return true
}

func (c *AltCache) getTable(key solana.Pubkey) ([]solana.Pubkey, bool) {
	if !c.ensureTable(key) {
		return nil, false
	}
	return c.tables[key], true
}

// DecodeSwaps decodes all swaps on our pools in a transaction, given its
// resolved keys.
func DecodeSwaps(txn solana.VersionedTransaction, keys []solana.Pubkey) []SwapInfo {
	cfg := pools.Pair()
	orcaPool := solana.MustPubkeyFromBase58(cfg.OrcaPool)
	rayPool := solana.MustPubkeyFromBase58(cfg.RayPool)
	orcaProg := solana.MustPubkeyFromBase58(OrcaProgram)
	rayProg := solana.MustPubkeyFromBase58(RayClmmProgram)
	rayVault0 := solana.MustPubkeyFromBase58(cfg.RayVault0)

	var out []SwapInfo
	for _, ix := range txn.Message.Instructions() {
		if int(ix.ProgramIDIndex) >= len(keys) {
			continue
		}
		program := keys[ix.ProgramIDIndex]
		var venue string
		switch program {
		case orcaProg:
			venue = "Orca"
		case rayProg:
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

		// Resolve this instruction's accounts to pubkeys.
		ixKeys := make([]solana.Pubkey, 0, len(ix.Accounts))
		for _, a := range ix.Accounts {
			if int(a) < len(keys) {
				ixKeys = append(ixKeys, keys[a])
			}
		}

		var pool string
		var dir Dir
		if venue == "Orca" {
			if !containsPubkey(ixKeys, orcaPool) {
				continue
			}
			// swap args: amount u64, thresh u64, sqrtLimit u128, amtSpecIsInput bool, aToB bool
			if len(ix.Data) < 42 {
				continue
			}
			// NOTE: assumes mintA is the base asset (true for the default
			// SOL/USDC pool; verify per pair) -> A->B = sell base.
			aToB := ix.Data[41] != 0
			pool = cfg.OrcaPool
			if aToB {
				dir = SellBase
			} else {
				dir = BuyBase
			}
		} else {
			if !containsPubkey(ixKeys, rayPool) {
				continue
			}
			// Raydium CLMM swap accounts: input_vault at index 5.
			if len(ixKeys) <= 5 {
				continue
			}
			inputVault := ixKeys[5]
			pool = cfg.RayPool
			if inputVault == rayVault0 {
				dir = SellBase
			} else {
				dir = BuyBase
			}
		}

		amount := binary.LittleEndian.Uint64(ix.Data[8:16])
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

func containsPubkey(list []solana.Pubkey, pk solana.Pubkey) bool {
	for _, k := range list {
		if k == pk {
			return true
		}
	}
	return false
}
