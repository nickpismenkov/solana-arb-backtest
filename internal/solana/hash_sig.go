package solana

import (
	"fmt"

	"github.com/mr-tron/base58"
)

// Hash is a 32-byte blockhash.
type Hash [32]byte

func HashFromBase58(s string) (Hash, error) {
	b, err := base58.Decode(s)
	if err != nil {
		return Hash{}, err
	}
	if len(b) != 32 {
		return Hash{}, fmt.Errorf("solana: hash expected 32 bytes, got %d", len(b))
	}
	var h Hash
	copy(h[:], b)
	return h, nil
}

func (h Hash) String() string { return base58.Encode(h[:]) }
func (h Hash) IsZero() bool   { return h == Hash{} }

// Signature is a 64-byte ed25519 signature.
type Signature [64]byte

func SignatureFromBase58(s string) (Signature, error) {
	b, err := base58.Decode(s)
	if err != nil {
		return Signature{}, err
	}
	if len(b) != 64 {
		return Signature{}, fmt.Errorf("solana: signature expected 64 bytes, got %d", len(b))
	}
	var sig Signature
	copy(sig[:], b)
	return sig, nil
}

func (s Signature) String() string { return base58.Encode(s[:]) }
func (s Signature) IsZero() bool   { return s == Signature{} }
