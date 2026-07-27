package pools

import (
	"encoding/binary"
	"math"
	"os"
	"strconv"
	"sync"

	"github.com/mr-tron/base58"
)

type PairCfg struct {
	Label             string
	OrcaPool          string
	RayPool           string
	BaseMint          string
	BaseDec           int32
	QuoteDec          int32
	OrcaFeeBps        float64
	RayFeeBps         float64
	RayVault0         string
	BaseTokenProgram  string
	QuoteTokenProgram string
}

func (p *PairCfg) NeedsSwapV2() bool {
	const T22 = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"
	return p.BaseTokenProgram == T22 || p.QuoteTokenProgram == T22
}

func (p *PairCfg) RoundTripFeeBps() float64 {
	return p.OrcaFeeBps + p.RayFeeBps
}

var (
	cfg  *PairCfg
	once sync.Once
)

func envOr(key, defaultValue string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultValue
}

func envNum(key string, defaultValue float64) float64 {
	if val, ok := os.LookupEnv(key); ok {
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			return f
		}
	}
	return defaultValue
}

func Pair() *PairCfg {
	once.Do(func() {
		cfg = &PairCfg{
			Label:             envOr("PAIR_LABEL", "SOL/USDC"),
			OrcaPool:          envOr("ORCA_POOL", "Czfq3xZZDmsdGdUyrNLtRhGc47cXcZtLG4crryfu44zE"),
			RayPool:           envOr("RAY_CLMM_POOL", "3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv"),
			BaseMint:          envOr("BASE_MINT", "So11111111111111111111111111111111111111112"),
			BaseDec:           int32(envNum("BASE_DECIMALS", 9)),
			QuoteDec:          int32(envNum("QUOTE_DECIMALS", 6)),
			OrcaFeeBps:        envNum("ORCA_FEE_BPS", 4.0),
			RayFeeBps:         envNum("RAY_FEE_BPS", 4.0),
			RayVault0:         envOr("RAY_VAULT0", "4ct7br2vTPzfdmY3S5HLtTxcGSBfn6pnw98hsS6v359A"),
			BaseTokenProgram:  envOr("BASE_TOKEN_PROGRAM", "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"),
			QuoteTokenProgram: envOr("QUOTE_TOKEN_PROGRAM", "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"),
		}
	})
	return cfg
}

func SqrtPriceToFloat(d []byte, o int) float64 {
	low := binary.LittleEndian.Uint64(d[o : o+8])
	high := binary.LittleEndian.Uint64(d[o+8 : o+16])
	return float64(high) + float64(low)/math.Pow(2, 64)
}

func mintAt(d []byte, o int) string {
	return base58.Encode(d[o : o+32])
}

func OrcaPrice(d []byte) (float64, bool) {
	if len(d) < 213 {
		return 0, false
	}
	c := Pair()
	sqrt := SqrtPriceToFloat(d, 65)
	mintA := mintAt(d, 101)
	baseIsA := mintA == c.BaseMint
	var decA, decB int32
	if baseIsA {
		decA, decB = c.BaseDec, c.QuoteDec
	} else {
		decA, decB = c.QuoteDec, c.BaseDec
	}
	uiBPerA := sqrt * sqrt * math.Pow(10, float64(decA-decB))
	if baseIsA {
		return uiBPerA, true
	}
	return 1.0 / uiBPerA, true
}

func RayClmmPrice(d []byte) (float64, bool) {
	if len(d) < 269 {
		return 0, false
	}
	mint0 := mintAt(d, 73)
	dec0 := int32(d[233])
	dec1 := int32(d[234])
	sqrt := SqrtPriceToFloat(d, 253)
	uiT1PerT0 := sqrt * sqrt * math.Pow(10, float64(dec0-dec1))
	if mint0 == Pair().BaseMint {
		return uiT1PerT0, true
	}
	return 1.0 / uiT1PerT0, true
}
