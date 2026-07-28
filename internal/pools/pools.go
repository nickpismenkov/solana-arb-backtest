// Package pools holds pool pair config + on-chain price decode. Byte offsets
// verified against mainnet (see the original Rust project's git history /
// RUN.md). Same Q64.64 sqrt-price math for both venues — Orca Whirlpool and
// Raydium CLMM.
//
// The pair is env-configurable so the same tooling re-points at any pair that
// has a pool on both venues (defaults = liquid SOL/USDC). Env:
//
//	PAIR_LABEL, ORCA_POOL, RAY_CLMM_POOL, BASE_MINT, BASE_DECIMALS,
//	QUOTE_DECIMALS, ORCA_FEE_BPS, RAY_FEE_BPS, RAY_VAULT0
package pools

import (
	"encoding/binary"
	"math"
	"os"
	"strconv"
	"sync"

	"github.com/gagliardetto/solana-go"
)

// Token2022Program is the Token-2022 program id.
const Token2022Program = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"

type PairCfg struct {
	Label    string
	OrcaPool string
	RayPool  string
	// Orientation: prices are quote-per-base on both venues.
	BaseMint string
	// The Whirlpool account doesn't carry decimals (Ray CLMM does).
	BaseDec    int32
	QuoteDec   int32
	OrcaFeeBps float64
	RayFeeBps  float64
	// Ray CLMM token_vault_0 — input vault == this ⇒ base is being sold.
	RayVault0 string
	// Token program owning each mint (classic SPL Token or Token-2022). A
	// Token-2022 leg requires swapV2 + Token-2022 ATAs. Default: both classic.
	BaseTokenProgram  string
	QuoteTokenProgram string
}

// NeedsSwapV2 is true if either side is Token-2022 → must use swapV2 + Token-2022 ATAs.
func (c *PairCfg) NeedsSwapV2() bool {
	return c.BaseTokenProgram == Token2022Program || c.QuoteTokenProgram == Token2022Program
}

func (c *PairCfg) RoundTripFeeBps() float64 {
	return c.OrcaFeeBps + c.RayFeeBps
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envNumF(key string, def float64) float64 {
	if v, ok := os.LookupEnv(key); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envNumI(key string, def int32) int32 {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil {
			return int32(n)
		}
	}
	return def
}

var (
	cfgOnce sync.Once
	cfg     *PairCfg
)

// Pair lazily loads the pair config. Call after env/](.env) has been loaded.
func Pair() *PairCfg {
	cfgOnce.Do(func() {
		cfg = &PairCfg{
			Label:    envOr("PAIR_LABEL", "SOL/USDC"),
			OrcaPool: envOr("ORCA_POOL", "Czfq3xZZDmsdGdUyrNLtRhGc47cXcZtLG4crryfu44zE"),
			// Default Raydium CLMM SOL/USDC — the active 4bp pool; one account
			// holds sqrtPrice so it updates on every swap (no vault-ratio lag).
			RayPool:           envOr("RAY_CLMM_POOL", "3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv"),
			BaseMint:          envOr("BASE_MINT", "So11111111111111111111111111111111111111112"),
			BaseDec:           envNumI("BASE_DECIMALS", 9),
			QuoteDec:          envNumI("QUOTE_DECIMALS", 6),
			OrcaFeeBps:        envNumF("ORCA_FEE_BPS", 4.0),
			RayFeeBps:         envNumF("RAY_FEE_BPS", 4.0),
			RayVault0:         envOr("RAY_VAULT0", "4ct7br2vTPzfdmY3S5HLtTxcGSBfn6pnw98hsS6v359A"),
			BaseTokenProgram:  envOr("BASE_TOKEN_PROGRAM", "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"),
			QuoteTokenProgram: envOr("QUOTE_TOKEN_PROGRAM", "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"),
		}
	})
	return cfg
}

func u128LE(d []byte, o int) *[2]uint64 {
	lo := binary.LittleEndian.Uint64(d[o : o+8])
	hi := binary.LittleEndian.Uint64(d[o+8 : o+16])
	return &[2]uint64{lo, hi}
}

// u128ToFloat converts a little-endian 128-bit unsigned integer (as lo,hi
// uint64 limbs) to float64, matching Rust's `u128::from_le_bytes(..) as f64`.
func u128ToFloat(limbs *[2]uint64) float64 {
	return float64(limbs[1])*18446744073709551616.0 + float64(limbs[0])
}

func mintAt(d []byte, o int) string {
	return solana.PublicKeyFromBytes(d[o : o+32]).String()
}

// OrcaPrice decodes an Orca Whirlpool account: sqrtPrice (Q64.64) @65,
// mintA @101, mintB @181.
func OrcaPrice(d []byte) (float64, bool) {
	if len(d) < 213 {
		return 0, false
	}
	cfg := Pair()
	sqrt := u128ToFloat(u128LE(d, 65)) / math.Pow(2, 64)
	mintA := mintAt(d, 101)
	baseIsA := mintA == cfg.BaseMint
	decA, decB := cfg.QuoteDec, cfg.BaseDec
	if baseIsA {
		decA, decB = cfg.BaseDec, cfg.QuoteDec
	}
	uiBPerA := sqrt * sqrt * math.Pow(10, float64(decA-decB))
	if baseIsA {
		return uiBPerA, true
	}
	return 1.0 / uiBPerA, true
}

// RayClmmPrice decodes a Raydium CLMM PoolState: mint0 @73, mint1 @105,
// decimals @233/234, sqrtPriceX64 @253.
func RayClmmPrice(d []byte) (float64, bool) {
	if len(d) < 269 {
		return 0, false
	}
	mint0 := mintAt(d, 73)
	dec0, dec1 := int32(d[233]), int32(d[234])
	sqrt := u128ToFloat(u128LE(d, 253)) / math.Pow(2, 64)
	uiT1PerT0 := sqrt * sqrt * math.Pow(10, float64(dec0-dec1))
	if mint0 == Pair().BaseMint {
		return uiT1PerT0, true
	}
	return 1.0 / uiT1PerT0, true
}
