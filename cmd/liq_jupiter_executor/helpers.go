package main

import (
	"encoding/base64"
	"math/big"
)

func base64StdEncode(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

// leU128ToDecimalString renders a raw little-endian u128 (as stored in
// VaultState.AbsorbedDebtAmount) as a decimal string, matching Rust's u128
// Display formatting used in the original log lines.
func leU128ToDecimalString(raw [16]byte) string {
	be := make([]byte, 16)
	for i, b := range raw {
		be[15-i] = b
	}
	return new(big.Int).SetBytes(be).String()
}
