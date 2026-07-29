// Package pools holds the (env-configurable) pool pair config plus on-chain
// price decode for the two venues this engine watches — Orca Whirlpool and
// Raydium CLMM. Both use the same Q64.64 sqrt-price representation. Byte
// offsets are verified against mainnet (see the original Rust project's git
// history / RUN.md).
package pools

import (
	"math"
	"sync"

	"github.com/mr-tron/base58"

	"arbengine/internal/config"
)

// Token-2022 program id — a pair needing swapV2 has either leg owned by this.
const token2022Program = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"

// PairCfg is the arbitraged pair's on-chain identity and economics.
type PairCfg struct {
	Label    string
	OrcaPool string
	RayPool  string
	// Orientation: prices are quote-per-base on both venues.
	BaseMint string
	// The Whirlpool account doesn't carry decimals (Ray CLMM does).
	BaseDec    int
	QuoteDec   int
	OrcaFeeBps float64
	RayFeeBps  float64
	// Ray CLMM token_vault_0 — input vault == this => base is being sold.
	RayVault0 string
	// Token program owning each mint (classic SPL Token or Token-2022). A
	// Token-2022 leg requires swapV2 + Token-2022 ATAs. Default: both classic.
	BaseTokenProgram  string
	QuoteTokenProgram string
}

// NeedsSwapV2 reports whether either side is Token-2022, requiring swapV2 +
// Token-2022 ATAs.
func (c *PairCfg) NeedsSwapV2() bool {
	return c.BaseTokenProgram == token2022Program || c.QuoteTokenProgram == token2022Program
}

// RoundTripFeeBps is the combined round-trip venue fee.
func (c *PairCfg) RoundTripFeeBps() float64 {
	return c.OrcaFeeBps + c.RayFeeBps
}

var (
	pairOnce sync.Once
	pairCfg  *PairCfg
)

// Pair lazily loads the pair config from the environment. Call after dotenv
// has run (config.LoadDotenv, as every command does at startup).
func Pair() *PairCfg {
	pairOnce.Do(func() {
		pairCfg = &PairCfg{
			Label:    config.EnvOr("PAIR_LABEL", "SOL/USDC"),
			OrcaPool: config.EnvOr("ORCA_POOL", "Czfq3xZZDmsdGdUyrNLtRhGc47cXcZtLG4crryfu44zE"),
			// Default Raydium CLMM SOL/USDC — the active 4bp pool; one account
			// holds sqrtPrice so it updates on every swap (no vault-ratio lag).
			RayPool:           config.EnvOr("RAY_CLMM_POOL", "3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv"),
			BaseMint:          config.EnvOr("BASE_MINT", "So11111111111111111111111111111111111111112"),
			BaseDec:           config.EnvInt("BASE_DECIMALS", 9),
			QuoteDec:          config.EnvInt("QUOTE_DECIMALS", 6),
			OrcaFeeBps:        config.EnvFloat("ORCA_FEE_BPS", 4.0),
			RayFeeBps:         config.EnvFloat("RAY_FEE_BPS", 4.0),
			RayVault0:         config.EnvOr("RAY_VAULT0", "4ct7br2vTPzfdmY3S5HLtTxcGSBfn6pnw98hsS6v359A"),
			BaseTokenProgram:  config.EnvOr("BASE_TOKEN_PROGRAM", "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"),
			QuoteTokenProgram: config.EnvOr("QUOTE_TOKEN_PROGRAM", "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"),
		}
	})
	return pairCfg
}

// ResetForTest clears the memoized pair config — test-only helper so cases
// can exercise different env combinations.
func ResetForTest() {
	pairOnce = sync.Once{}
	pairCfg = nil
}

func u128LE(d []byte, o int) uint64hi128 {
	return uint64hi128{lo: leU64(d, o), hi: leU64(d, o+8)}
}

// uint64hi128 is a minimal little-endian u128 (as two u64 halves) — enough to
// convert to float64 without pulling in math/big for the hot path.
type uint64hi128 struct{ lo, hi uint64 }

func (v uint64hi128) Float64() float64 {
	return float64(v.hi)*18446744073709551616.0 + float64(v.lo) // hi * 2^64 + lo
}

func leU64(d []byte, o int) uint64 {
	var v uint64
	for i := 0; i < 8; i++ {
		v |= uint64(d[o+i]) << (8 * i)
	}
	return v
}

func mintAt(d []byte, o int) string {
	return base58.Encode(d[o : o+32])
}

// OrcaPrice decodes an Orca Whirlpool account: sqrtPrice (Q64.64) @65,
// mintA @101, mintB @181. Returns (price, ok).
func OrcaPrice(d []byte) (float64, bool) {
	if len(d) < 213 {
		return 0, false
	}
	cfg := Pair()
	sqrt := u128LE(d, 65).Float64() / math.Pow(2, 64)
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

// RayClmmPrice decodes a Raydium CLMM PoolState account: mint0 @73,
// mint1 @105, decimals @233/234, sqrtPriceX64 @253.
func RayClmmPrice(d []byte) (float64, bool) {
	if len(d) < 269 {
		return 0, false
	}
	mint0 := mintAt(d, 73)
	dec0, dec1 := int(d[233]), int(d[234])
	sqrt := u128LE(d, 253).Float64() / math.Pow(2, 64)
	uiT1PerT0 := sqrt * sqrt * math.Pow(10, float64(dec0-dec1))
	if mint0 == Pair().BaseMint {
		return uiT1PerT0, true
	}
	return 1.0 / uiT1PerT0, true
}
