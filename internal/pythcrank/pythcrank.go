// Package pythcrank builds self-crank instruction sequences (step 4 of the
// pipeline): turn a parsed Hermes update (pythaccumulator) into the on-chain
// ix sequence that posts a fresh price to the SHARED sponsored PriceUpdateV2
// feed marginfi reads.
//
// Derived from a REAL mainnet sponsored-feed crank (the marginfi/Kamino
// lesson) — setup tx 2hkWPh62… + fire tx 4vYXYqdN…, same buffer keypair:
//
//	SETUP tx: system createAccount(space = 46 + vaa_len, owner = Wormhole-verify)
//	          · init_encoded_vaa · write_encoded_vaa(index 0, first ~720B)
//	FIRE  tx: write_encoded_vaa(tail) · verify_encoded_vaa_v1(guardian_set)
//	          · push-wrapper update_price_feed{PostUpdateParams, shard, feed_id}
//	            (CPIs receiver post_update → writes the sponsored feed PDA,
//	             whose write_authority is itself — the permissionless pattern)
//	          · close_encoded_vaa (rent back)
//
// The two-tx split exists because the guardian-signed VAA (~952B for one
// Hermes blob) can't fit a single 1232B tx next to the 396B update ix. One
// encoded-VAA buffer can serve several update ixs (the VAA commits to the
// merkle root of ALL feeds in the blob); only the last fire tx should close.
//
// Observed rent for space=998 was 7,836,960 lamports = (space+128)*6960 —
// reclaimed by close_encoded_vaa.
package pythcrank

import (
	"fmt"

	"arbengine/internal/pythaccumulator"
	"arbengine/internal/solana"
)

// WORMHOLE_VERIFY is the Wormhole verification program the Pyth receiver
// trusts (encoded-VAA flow).
const WormholeVerify = "HDwcJBJXjL9FpJ7UBsYBtaDjsBUhuLCUYoz3zr8SWWaQ"

// PythReceiver is the Pyth pull-oracle receiver (post_update lives here;
// wrapper CPIs into it).
const PythReceiver = "rec5EKMGg6MxZYaMdyBfgwp4d5rB9T1VQH5pJv5LtFJ"

// PushOracle is the Pyth push wrapper — the ONLY writer of the shared
// sponsored feeds.
const PushOracle = "pythWSnswVUd12oZpeFP8e9CVaEqJg25g1Vtc2biRsT"

// ReceiverConfig is the receiver config PDA (seed "config"), pinned from the
// captured tx.
const ReceiverConfig = "DaWUKXCyXsnzcvLUyeJRWou8KTn7XtadgTsdhJ6RHS7b"

const system = "11111111111111111111111111111111"

// VERIFIED discriminators (sha256("global:<name>")[..8], matched against the
// captured crank — see pyth_crank_decode).
var (
	discInitEncodedVaa     = [8]byte{0xd1, 0xc1, 0xad, 0x19, 0x5b, 0xca, 0xb5, 0xda}
	discWriteEncodedVaa    = [8]byte{0xc7, 0xd0, 0x6e, 0xb1, 0x96, 0x4c, 0x76, 0x2a}
	discVerifyEncodedVaaV1 = [8]byte{0x67, 0x38, 0xb1, 0xe5, 0xf0, 0x67, 0x44, 0x49}
	discCloseEncodedVaa    = [8]byte{0x30, 0xdd, 0xae, 0xc6, 0xe7, 0x07, 0x98, 0x26}
	discUpdatePriceFeed    = [8]byte{0x1c, 0x09, 0x5d, 0x96, 0x56, 0x99, 0xbc, 0x73}
)

// SetupChunk is the first-chunk size for the setup tx (observed cranker used
// ~720; the tail + verify + update + close must fit the 1232B fire tx).
const SetupChunk = 720

func pk(s string) solana.Pubkey { return solana.MustPubkeyFromBase58(s) }

// EncodedVaaSpace returns the EncodedVaa account size: 46B header (disc +
// status + write_authority + version + len) + the raw VAA. Observed: 952B
// VAA → space 998.
func EncodedVaaSpace(vaaLen int) uint64 { return 46 + uint64(vaaLen) }

// RentLamports returns the rent-exempt minimum, matching the observed
// createAccount exactly.
func RentLamports(space uint64) uint64 { return (space + 128) * 6960 }

// GuardianSetIndex returns the guardian-set index a VAA was signed under (u32
// BE right after the version).
func GuardianSetIndex(vaa []byte) (uint32, bool) {
	if len(vaa) < 5 {
		return 0, false
	}
	b := vaa[1:5]
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]), true
}

// GuardianSet returns the guardian-set PDA of the Wormhole-verify program.
func GuardianSet(index uint32) solana.Pubkey {
	idxBytes := []byte{byte(index >> 24), byte(index >> 16), byte(index >> 8), byte(index)}
	addr, _ := solana.FindProgramAddress([][]byte{[]byte("GuardianSet"), idxBytes}, pk(WormholeVerify))
	return addr
}

// Treasury returns the receiver treasury PDA (any id 0..=255 works; ids
// spread write contention).
func Treasury(id uint8) solana.Pubkey {
	addr, _ := solana.FindProgramAddress([][]byte{[]byte("treasury"), {id}}, pk(PythReceiver))
	return addr
}

// SponsoredFeed returns the sponsored-feed PDA the push wrapper writes:
// seeds [shard_le, feed_id].
func SponsoredFeed(shard uint16, feedID [32]byte) solana.Pubkey {
	shardLE := []byte{byte(shard), byte(shard >> 8)}
	addr, _ := solana.FindProgramAddress([][]byte{shardLE, feedID[:]}, pk(PushOracle))
	return addr
}

// CreateEncodedVaaAccountIx is the system createAccount for the encoded-VAA
// buffer (buffer must co-sign).
func CreateEncodedVaaAccountIx(payer, buffer solana.Pubkey, vaaLen int) solana.Instruction {
	space := EncodedVaaSpace(vaaLen)
	data := make([]byte, 0, 52)
	data = append(data, 0, 0, 0, 0) // CreateAccount tag (u32 LE)
	data = appendU64LE(data, RentLamports(space))
	data = appendU64LE(data, space)
	data = append(data, pk(WormholeVerify).Bytes()...)
	return solana.Instruction{
		ProgramID: pk(system),
		Accounts: []solana.AccountMeta{
			solana.WritableSigner(payer),
			solana.WritableSigner(buffer),
		},
		Data: data,
	}
}

// InitEncodedVaaIx builds the init_encoded_vaa instruction.
func InitEncodedVaaIx(writeAuthority, buffer solana.Pubkey) solana.Instruction {
	return solana.Instruction{
		ProgramID: pk(WormholeVerify),
		Accounts: []solana.AccountMeta{
			solana.SignerMeta(writeAuthority),
			solana.Writable(buffer),
		},
		Data: discInitEncodedVaa[:],
	}
}

// WriteEncodedVaaIx builds write_encoded_vaa { index: u32 (byte offset),
// data: Vec<u8> }.
func WriteEncodedVaaIx(writeAuthority, buffer solana.Pubkey, index uint32, chunk []byte) solana.Instruction {
	data := make([]byte, 0, 16+len(chunk))
	data = append(data, discWriteEncodedVaa[:]...)
	data = appendU32LE(data, index)
	data = appendU32LE(data, uint32(len(chunk)))
	data = append(data, chunk...)
	return solana.Instruction{
		ProgramID: pk(WormholeVerify),
		Accounts: []solana.AccountMeta{
			solana.SignerMeta(writeAuthority),
			solana.Writable(buffer),
		},
		Data: data,
	}
}

// VerifyEncodedVaaV1Ix builds the verify_encoded_vaa_v1 instruction.
func VerifyEncodedVaaV1Ix(writeAuthority, buffer, guardianSet solana.Pubkey) solana.Instruction {
	return solana.Instruction{
		ProgramID: pk(WormholeVerify),
		Accounts: []solana.AccountMeta{
			solana.SignerMeta(writeAuthority),
			solana.Writable(buffer),
			solana.ReadonlyMeta(guardianSet),
		},
		Data: discVerifyEncodedVaaV1[:],
	}
}

// CloseEncodedVaaIx closes the encoded-VAA buffer; rent goes back to the
// write authority.
func CloseEncodedVaaIx(writeAuthority, buffer solana.Pubkey) solana.Instruction {
	return solana.Instruction{
		ProgramID: pk(WormholeVerify),
		Accounts: []solana.AccountMeta{
			solana.Writable(writeAuthority),
			solana.Writable(buffer),
		},
		Data: discCloseEncodedVaa[:],
	}
}

// UpdatePriceFeedIx builds the push-wrapper update_price_feed(PostUpdateParams
// { merkle_price_update { message: Vec<u8>, proof: Vec<[u8;20]> },
// treasury_id: u8 }, shard: u16, feed_id: [u8;32]) — 396B observed for an 85B
// message + 13-hash proof. Converts the accumulator's wire proof (u8 count +
// count×20B) to borsh (u32 count + hashes). Returns ok=false on a malformed
// proof.
func UpdatePriceFeedIx(payer, encodedVaa solana.Pubkey, update pythaccumulator.MerkleUpdate, shard uint16, treasuryID uint8) (solana.Instruction, bool) {
	feedID, ok := update.FeedID()
	if !ok {
		return solana.Instruction{}, false
	}
	if len(update.Proof) < 1 {
		return solana.Instruction{}, false
	}
	n := int(update.Proof[0])
	if len(update.Proof) < 1+n*20 {
		return solana.Instruction{}, false
	}
	hashes := update.Proof[1 : 1+n*20]

	data := make([]byte, 0, 8+4+len(update.Message)+4+len(hashes)+35)
	data = append(data, discUpdatePriceFeed[:]...)
	data = appendU32LE(data, uint32(len(update.Message)))
	data = append(data, update.Message...)
	data = appendU32LE(data, uint32(n))
	data = append(data, hashes...)
	data = append(data, treasuryID)
	data = appendU16LE(data, shard)
	data = append(data, feedID[:]...)

	return solana.Instruction{
		ProgramID: pk(PushOracle),
		Accounts: []solana.AccountMeta{
			solana.WritableSigner(payer),
			solana.ReadonlyMeta(pk(PythReceiver)),
			solana.ReadonlyMeta(encodedVaa),
			solana.ReadonlyMeta(pk(ReceiverConfig)),
			solana.Writable(Treasury(treasuryID)),
			solana.Writable(SponsoredFeed(shard, feedID)),
			solana.ReadonlyMeta(pk(system)),
		},
		Data: data,
	}, true
}

// CrankIxs is the full two-tx crank for one Hermes blob: setup
// (create+init+first chunk) and fire (tail+verify+update per feed+close).
// `updates` may hold several feeds — one VAA proves them all; each gets its
// own update ix in the fire tx if it fits (each adds ~430B; more than 2 feeds
// needs splitting upstream).
type CrankIxs struct {
	Setup []solana.Instruction
	Fire  []solana.Instruction
}

// BuildCrankIxs builds the setup/fire instruction lists. Returns ok=false on
// a malformed VAA/update.
func BuildCrankIxs(payer, buffer solana.Pubkey, vaa []byte, updates []pythaccumulator.MerkleUpdate, shard uint16, treasuryID uint8) (CrankIxs, bool) {
	gsIndex, ok := GuardianSetIndex(vaa)
	if !ok {
		return CrankIxs{}, false
	}
	gs := GuardianSet(gsIndex)

	headLen := SetupChunk
	if headLen > len(vaa) {
		headLen = len(vaa)
	}
	head := vaa[:headLen]

	setup := []solana.Instruction{
		CreateEncodedVaaAccountIx(payer, buffer, len(vaa)),
		InitEncodedVaaIx(payer, buffer),
		WriteEncodedVaaIx(payer, buffer, 0, head),
	}

	fire := make([]solana.Instruction, 0, 3+len(updates))
	if len(vaa) > len(head) {
		fire = append(fire, WriteEncodedVaaIx(payer, buffer, uint32(len(head)), vaa[len(head):]))
	}
	fire = append(fire, VerifyEncodedVaaV1Ix(payer, buffer, gs))
	for _, u := range updates {
		ix, ok := UpdatePriceFeedIx(payer, buffer, u, shard, treasuryID)
		if !ok {
			return CrankIxs{}, false
		}
		fire = append(fire, ix)
	}
	fire = append(fire, CloseEncodedVaaIx(payer, buffer))

	return CrankIxs{Setup: setup, Fire: fire}, true
}

// ── Transaction-level assembly (what the executor bundles ahead of the
//    liquidate tx) ──────────────────────────────────────────────────────────

// SetupCU and FireCU are CU ceilings from the captured crank: setup ran in
// ~5.4k, fire in ~402k (verify_encoded_vaa_v1 does 13 secp recoveries).
const (
	SetupCU uint32 = 30_000
	FireCU  uint32 = 500_000
)

func cuLimitIx(units uint32) solana.Instruction {
	data := make([]byte, 0, 5)
	data = append(data, 2)
	data = appendU32LE(data, units)
	return solana.Instruction{
		ProgramID: pk("ComputeBudget111111111111111111111111111111"),
		Accounts:  nil,
		Data:      data,
	}
}

// CrankTxs is the two crank txs plus the ephemeral buffer keypair that must
// co-sign the setup (createAccount). Build fresh per fire — the VAA inside
// is only as fresh as the Hermes blob it came from.
type CrankTxs struct {
	Setup  solana.VersionedTransaction
	Fire   solana.VersionedTransaction
	Buffer solana.Keypair
}

// StampAndSign stamps a recent blockhash and signs: setup = [payer, buffer],
// fire = [payer].
func (tx *CrankTxs) StampAndSign(payer solana.Keypair, blockhash solana.Hash) error {
	tx.Setup.Message.V0.RecentBlockhash = blockhash
	tx.Fire.Message.V0.RecentBlockhash = blockhash
	if err := tx.Setup.Sign([]solana.Keypair{payer, tx.Buffer}); err != nil {
		return err
	}
	if err := tx.Fire.Sign([]solana.Keypair{payer}); err != nil {
		return err
	}
	return nil
}

// ToB64 returns (setup, fire) as base64 — the encoding sendBundle/
// simulateBundle take.
func (tx *CrankTxs) ToB64() (string, string, error) {
	setupB64, err := tx.Setup.Base64()
	if err != nil {
		return "", "", err
	}
	fireB64, err := tx.Fire.Base64()
	if err != nil {
		return "", "", err
	}
	return setupB64, fireB64, nil
}

// BuildCrankTxs compiles the two-tx crank for `updates` (usually one feed)
// with placeholder signatures. `blockhash` may be default for
// replace-blockhash simulation; StampAndSign before a live send.
func BuildCrankTxs(payer solana.Pubkey, vaa []byte, updates []pythaccumulator.MerkleUpdate, shard uint16, treasuryID uint8, blockhash solana.Hash) (*CrankTxs, error) {
	buffer, err := solana.NewKeypair()
	if err != nil {
		return nil, fmt.Errorf("crank buffer keypair: %w", err)
	}
	ixs, ok := BuildCrankIxs(payer, buffer.Public, vaa, updates, shard, treasuryID)
	if !ok {
		return nil, fmt.Errorf("crank ix build failed (malformed VAA/update)")
	}

	compile := func(cu uint32, body []solana.Instruction, nSigs int) (solana.VersionedTransaction, error) {
		all := append([]solana.Instruction{cuLimitIx(cu)}, body...)
		msg, err := solana.CompileV0(payer, all, nil, blockhash)
		if err != nil {
			return solana.VersionedTransaction{}, fmt.Errorf("compile crank tx: %w", err)
		}
		return solana.VersionedTransaction{
			Signatures: make([]solana.Signature, nSigs),
			Message:    solana.VersionedMessage{IsV0: true, V0: msg},
		}, nil
	}

	setupTx, err := compile(SetupCU, ixs.Setup, 2)
	if err != nil {
		return nil, err
	}
	fireTx, err := compile(FireCU, ixs.Fire, 1)
	if err != nil {
		return nil, err
	}

	return &CrankTxs{Setup: setupTx, Fire: fireTx, Buffer: buffer}, nil
}

func appendU16LE(b []byte, v uint16) []byte {
	return append(b, byte(v), byte(v>>8))
}

func appendU32LE(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

func appendU64LE(b []byte, v uint64) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24),
		byte(v>>32), byte(v>>40), byte(v>>48), byte(v>>56))
}
