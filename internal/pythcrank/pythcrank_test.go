package pythcrank

import (
	"encoding/hex"
	"testing"

	"arbengine/internal/solana"
)

// Every PDA derivation is asserted against addresses captured from the real
// mainnet crank (setup 2hkWPh62…, fire 4vYXYqdN…).
func TestPDAsMatchObservedCrank(t *testing.T) {
	// Guardian set 7 (the captured VAA's index) — account in verify ix.
	if got, want := GuardianSet(7).String(), "6GaHgiaQg9Pg346xHq9m7vQ9rJtnH83gQKqJoiAxQa7D"; got != want {
		t.Fatalf("GuardianSet(7) = %s, want %s", got, want)
	}

	// Treasury observed in the update ix — some id must derive it.
	var treasuryID uint8
	found := false
	for id := 0; id <= 255; id++ {
		if Treasury(uint8(id)).String() == "8hQfT7SVhkCrzUSgBq6u2wYEt1sH3xmofZ5ss3YaydZW" {
			treasuryID = uint8(id)
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no treasury id derives the observed treasury")
	}

	// Receiver config PDA (seed "config").
	cfg, _ := solana.FindProgramAddress([][]byte{[]byte("config")}, pk(PythReceiver))
	if cfg.String() != ReceiverConfig {
		t.Fatalf("receiver config PDA = %s, want %s", cfg.String(), ReceiverConfig)
	}

	// Sponsored feeds: SOL (ef0d8b6f…) → 7UVimff…, PENGU-ish (d82183dd…) →
	// F2VfCy… — both from the captured cranks; find the shard.
	sol := mustHex32(t, "ef0d8b6fda2ceba41da15d4095d1da392a0d2f8ed0c6c7bc0f4cfac8c280b56d")
	other := mustHex32(t, "d82183dd487bef3208a227bb25d748930db58862c5121198e723ed0976eb92b7")

	var shard uint16
	shardFound := false
	for s := 0; s < 64; s++ {
		if SponsoredFeed(uint16(s), sol).String() == "7UVimffxr9ow1uXYxsr4LHAcV58mLzhmwaeKvJ1pjLiE" {
			shard = uint16(s)
			shardFound = true
			break
		}
	}
	if !shardFound {
		t.Fatalf("no shard derives the observed SOL sponsored feed")
	}
	if got, want := SponsoredFeed(shard, other).String(), "F2VfCymdNQiCa8Vyg5E7BwEv9UPwfm8cVN6eqQLqXiGo"; got != want {
		t.Fatalf("second observed feed disagrees on shard %d: got %s, want %s", shard, got, want)
	}
	t.Logf("treasury_id=%d shard=%d", treasuryID, shard)
}

// Rent formula reproduces the observed createAccount lamports exactly.
func TestRentMatchesObserved(t *testing.T) {
	if got := EncodedVaaSpace(952); got != 998 {
		t.Fatalf("EncodedVaaSpace(952) = %d, want 998", got)
	}
	if got := RentLamports(998); got != 7_836_960 {
		t.Fatalf("RentLamports(998) = %d, want 7836960", got)
	}
}

func mustHex32(t *testing.T, s string) [32]byte {
	t.Helper()
	// The Rust test's `hex` helper reads 32 bytes (64 hex chars) from the
	// front of a longer string, mirroring `&s[2*i..2*i+2]` for i in 0..32.
	raw, err := hex.DecodeString(s[:64])
	if err != nil {
		t.Fatalf("bad hex: %v", err)
	}
	var out [32]byte
	copy(out[:], raw)
	return out
}
