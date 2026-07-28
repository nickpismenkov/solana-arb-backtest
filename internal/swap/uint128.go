package swap

import "math/big"

// Uint128 is a raw little-endian 128-bit value, exactly as it's serialized
// into instruction data (Solana programs use u128 for sqrt-price bounds).
type Uint128 [16]byte

func (v Uint128) Bytes() [16]byte { return v }

// Uint128FromU64 packs a u64 into the low 8 bytes (high 8 bytes zero).
func Uint128FromU64(v uint64) Uint128 {
	var out Uint128
	for i := 0; i < 8; i++ {
		out[i] = byte(v >> (8 * i))
	}
	return out
}

// Uint128Max is the all-ones sentinel (u64::MAX widened to u128 in the
// original code — used as an unenforced max-in threshold).
var Uint128Max = Uint128FromU64(^uint64(0))

// Uint128FromString parses a base-10 string (arbitrary size up to 128 bits)
// into its little-endian byte representation — used for the two hard-coded
// sqrt-price sentinels.
func Uint128FromString(s string) Uint128 {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("swap: invalid uint128 literal " + s)
	}
	be := n.Bytes() // big-endian, minimal length
	var out Uint128
	for i, b := range be {
		out[len(be)-1-i] = b
	}
	return out
}
