// Package solana provides a minimal, dependency-light re-implementation of
// the pieces of the Rust solana-* SDK crates this project relies on: Pubkey
// (base58 + PDA derivation), Instruction/AccountMeta, Hash, Signature,
// Keypair, and versioned message/transaction (de)serialization. It exists so
// the ported business logic (arb, swap, flashloan, kamino, save, ...) has the
// same primitives the Rust code used, without pulling in a full third-party
// Solana client SDK.
package solana

import (
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"

	"filippo.io/edwards25519"
	"github.com/mr-tron/base58"
)

// Pubkey is a 32-byte Solana public key / program-derived address.
type Pubkey [32]byte

// ZeroPubkey is the all-zero (system program / default) key.
var ZeroPubkey = Pubkey{}

// PubkeyFromBase58 decodes a base58-encoded 32-byte public key.
func PubkeyFromBase58(s string) (Pubkey, error) {
	b, err := base58.Decode(s)
	if err != nil {
		return Pubkey{}, fmt.Errorf("pubkey: bad base58 %q: %w", s, err)
	}
	return PubkeyFromBytes(b)
}

// MustPubkeyFromBase58 is PubkeyFromBase58 but panics on error — for
// hard-coded well-known program/mint addresses (mirrors the Rust code's
// liberal use of `.unwrap()` / `from_str_const` on constant addresses).
func MustPubkeyFromBase58(s string) Pubkey {
	pk, err := PubkeyFromBase58(s)
	if err != nil {
		panic(err)
	}
	return pk
}

// PubkeyFromBytes copies a 32-byte slice into a Pubkey.
func PubkeyFromBytes(b []byte) (Pubkey, error) {
	if len(b) != 32 {
		return Pubkey{}, fmt.Errorf("pubkey: expected 32 bytes, got %d", len(b))
	}
	var pk Pubkey
	copy(pk[:], b)
	return pk, nil
}

// Bytes returns the key's 32 raw bytes.
func (p Pubkey) Bytes() []byte { return p[:] }

// String base58-encodes the key (Solana's canonical text form).
func (p Pubkey) String() string { return base58.Encode(p[:]) }

// IsZero reports whether this is the all-zero key.
func (p Pubkey) IsZero() bool { return p == Pubkey{} }

const maxSeedLen = 32
const maxSeeds = 16

// pdaMarker is appended by CreateProgramAddress, matching Solana's
// "ProgramDerivedAddress" domain separator.
var pdaMarker = []byte("ProgramDerivedAddress")

// CreateProgramAddress derives a PDA candidate from seeds + program id, and
// rejects it if the resulting point lies ON the ed25519 curve (a real PDA
// must be off-curve, i.e. have no known private key).
func CreateProgramAddress(seeds [][]byte, programID Pubkey) (Pubkey, error) {
	if len(seeds) > maxSeeds {
		return Pubkey{}, errors.New("solana: too many seeds")
	}
	h := sha256.New()
	for _, s := range seeds {
		if len(s) > maxSeedLen {
			return Pubkey{}, errors.New("solana: seed too long")
		}
		h.Write(s)
	}
	h.Write(programID[:])
	h.Write(pdaMarker)
	sum := h.Sum(nil)

	var pk Pubkey
	copy(pk[:], sum)
	if isOnCurve(pk) {
		return Pubkey{}, errors.New("solana: invalid seeds; address on curve")
	}
	return pk, nil
}

// FindProgramAddress finds the canonical (highest valid bump) PDA for seeds
// + program id, exactly like Rust's Pubkey::find_program_address.
func FindProgramAddress(seeds [][]byte, programID Pubkey) (Pubkey, uint8) {
	for bump := 255; bump >= 0; bump-- {
		withBump := make([][]byte, len(seeds)+1)
		copy(withBump, seeds)
		withBump[len(seeds)] = []byte{byte(bump)}
		if pk, err := CreateProgramAddress(withBump, programID); err == nil {
			return pk, uint8(bump)
		}
	}
	panic("solana: unable to find a viable program address bump seed")
}

// isOnCurve reports whether b decodes to a valid point on ed25519's curve.
func isOnCurve(pk Pubkey) bool {
	_, err := new(edwards25519.Point).SetBytes(pk[:])
	return err == nil
}

// Keypair mirrors Rust's Keypair: an ed25519 signing key plus its public key.
type Keypair struct {
	Public  Pubkey
	Private ed25519.PrivateKey
}

// NewKeypair generates a fresh random keypair.
func NewKeypair() (Keypair, error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return Keypair{}, err
	}
	pk, _ := PubkeyFromBytes(pub)
	return Keypair{Public: pk, Private: priv}, nil
}

// KeypairFromBytes loads a keypair from the 64-byte Solana CLI wire format
// (32-byte seed followed by 32-byte public key), as found in `solana-keygen`
// JSON files ([]byte arrays).
func KeypairFromBytes(b []byte) (Keypair, error) {
	if len(b) != 64 {
		return Keypair{}, fmt.Errorf("solana: keypair must be 64 bytes, got %d", len(b))
	}
	priv := ed25519.NewKeyFromSeed(b[:32])
	pk, err := PubkeyFromBytes(b[32:64])
	if err != nil {
		return Keypair{}, err
	}
	if !ed25519.PublicKey(pk[:]).Equal(priv.Public().(ed25519.PublicKey)) {
		return Keypair{}, errors.New("solana: keypair public key does not match seed")
	}
	return Keypair{Public: pk, Private: priv}, nil
}

// Sign signs a message and returns a 64-byte Signature.
func (k Keypair) Sign(message []byte) Signature {
	var sig Signature
	copy(sig[:], ed25519.Sign(k.Private, message))
	return sig
}
