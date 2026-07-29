// Package shared holds cross-cutting type definitions used by more than one
// ported package (e.g. detector, pools, grpc, pump). Keep this package small:
// it exists to avoid duplicate/incompatible redefinitions across chunks, not
// to accumulate general-purpose utilities.
package shared

import (
	"github.com/mr-tron/base58"
)

// Pubkey mirrors solana_pubkey::Pubkey from the Rust source: a 32-byte
// Solana address. Used by pump.rs (bonding-curve PDA + event decoding) and
// any future module that touches Solana account addresses.
type Pubkey [32]byte

func (p Pubkey) String() string {
	return base58.Encode(p[:])
}

func (p Pubkey) Bytes() []byte {
	return p[:]
}

func PubkeyFromBytes(b []byte) (Pubkey, error) {
	var p Pubkey
	copy(p[:], b)
	return p, nil
}
