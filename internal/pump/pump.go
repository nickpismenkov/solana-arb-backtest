// Package pump implements pump.fun / PumpSwap recon + decoders — Phase 1
// (measure-first).
//
// Everything here is derived from CAPTURED REAL MAINNET TRANSACTIONS, never
// from memory (repo doctrine). Program IDs, event discriminators, and byte
// offsets are each verified against >=2 live txs; the Rust source's unit
// tests pin the layouts to real captured bytes so a future pump upgrade that
// shifts a field trips a test instead of silently corrupting the collector.
//
// This package is fully self-contained and shares no state with the
// liquidation engine. It is pure observation: nothing here signs or submits
// a transaction.
//
// ── What was verified (see the original PR body for the exact signatures) ──
//   - Bonding-curve program `6EF8…F6P` — owns every `BondingCurve` PDA and
//     emits the anchor self-CPI event logs (`Program data: …`) we decode.
//   - PumpSwap AMM `pAMM…fXEA` — the graduated venue; a bonding curve
//     migrates into a PumpSwap pool at completion.
//   - The current mainnet program is the Token-2022 variant (mints/curve
//     token accounts live under `TokenzQd…`), and the instruction set has
//     grown V2/V3 variants (Create/CreateV2, Buy/BuyV2, Sell/SellV2,
//     MigrateV2). We therefore decode the anchor event logs, whose layout is
//     stable across those instruction variants, rather than per-instruction
//     discriminators.
//
// ── Anchor event self-CPI logs ──────────────────────────────────────────────
// pump emits structured events as base64 in `Program data:` log lines. The
// first 8 bytes are the anchor event discriminator (sha256("event:<Name>")).
// We match on those to route Create / Trade / Migrate.
package pump

import (
	"encoding/base64"
	"encoding/binary"
	"math/big"

	"github.com/gagliardetto/solana-go"
)

// ── Program IDs (verified: getAccountInfo owner/executable + real txs) ─────

// PumpProgram is the pump.fun bonding-curve program. VERIFIED: it is the
// `owner` of every `BondingCurve` account we fetched, and every
// create/buy/sell/migrate tx we pulled is a top-level invoke of this
// program.
const PumpProgram = "6EF8rrecthR5Dkzon8Nwu78hRvfCKubJ14M5uBEwF6P"

// PumpswapAMM is the PumpSwap AMM program (post-graduation venue). VERIFIED:
// executable=true, owned by the BPF upgradeable loader, and it is account #10
// of the `MigrateV2` instruction (the pool the curve migrates into).
const PumpswapAMM = "pAMMBay6oceH9fJKBRHGP5D4bD4sWpmSwMn52FMfXEA"

// PumpFeeProgram is the pump.fun fee program (`GetFees` CPI seen in every
// trade). Informational.
const PumpFeeProgram = "pfeeUxB6jkeY1Hxd7CsFCAjcbHA9rWtchMGdZ6VojVZ"

// MigrationAuthority is the signer of `MigrateV2`. Its transaction history
// is, in effect, the list of graduations. VERIFIED across multiple migrate
// txs.
const MigrationAuthority = "39azUYFWPz3VHgKCf3VChUwbpURdCHRxjWVowf5jUJjg"

// BondingCurveSeed is the PDA seed for a token's bonding-curve account:
// ["bonding-curve", mint]. VERIFIED: FindProgramAddress(["bonding-curve",
// mint], PumpProgram) equals the `bonding_curve` field of the CreateEvent
// (see the Rust source's test).
var BondingCurveSeed = []byte("bonding-curve")

// PumpTokenDecimals: pump.fun tokens are minted with 6 decimals and a 1e15
// raw total supply (= 1,000,000,000 whole tokens). VERIFIED from
// CreateEvent.token_total_supply.
const PumpTokenDecimals = 6

// ── Anchor event discriminators (first 8 bytes of a `Program data:` blob) ──

// TradeEventDisc is sha256("event:TradeEvent")[..8] — emitted on every buy
// and sell.
var TradeEventDisc = [8]byte{0xbd, 0xdb, 0x7f, 0xd3, 0x4e, 0xe6, 0x61, 0xee}

// CreateEventDisc is sha256("event:CreateEvent")[..8] — emitted on a new
// token launch.
var CreateEventDisc = [8]byte{0x1b, 0x72, 0xa9, 0x4d, 0xde, 0xeb, 0x63, 0x76}

// MigrateEventDisc is the migrate/complete event discriminator — emitted
// inside the `MigrateV2` tx. Discriminator captured live; only its `mint`
// field (byte offset 50) is decoded, see below.
var MigrateEventDisc = [8]byte{0xb1, 0x31, 0x0c, 0xd2, 0xa0, 0x76, 0xa7, 0x74}

// ── Reserve → price helpers ─────────────────────────────────────────────────

// PriceLamportsPerRawToken is the price of one raw token unit in lamports,
// straight from the constant-product reserves (virtualSol / virtualToken).
func PriceLamportsPerRawToken(virtualSol, virtualToken uint64) float64 {
	if virtualToken == 0 {
		return 0.0
	}
	return float64(virtualSol) / float64(virtualToken)
}

// PriceInSOL is the price of one whole token in SOL, accounting for the 9
// lamport decimals of SOL and tokenDecimals of the token (6 for pump). This
// is the number you compare across launches for the "peak multiple" census.
func PriceInSOL(virtualSol, virtualToken uint64, tokenDecimals int32) float64 {
	if virtualToken == 0 {
		return 0.0
	}
	sol := float64(virtualSol) / 1e9
	tokens := float64(virtualToken) / pow10f(tokenDecimals)
	return sol / tokens
}

func pow10f(n int32) float64 {
	r := 1.0
	if n >= 0 {
		for i := int32(0); i < n; i++ {
			r *= 10
		}
	} else {
		for i := int32(0); i < -n; i++ {
			r /= 10
		}
	}
	return r
}

// CurveBuyTokensOut returns the raw token units received for paying solIn
// lamports INTO the bonding curve (pre-fee, pure constant-product).
// virtualSol/virtualToken are the curve's virtual reserves before the trade.
//
// pump.fun's curve is a constant product k = vsol * vtoken. Paying solIn
// raises vsol to vsol + solIn, so tokens out = vtoken - k/(vsol+solIn) =
// vtoken * solIn / (vsol + solIn). VERIFIED to the lamport against a real
// captured dev-buy (see the Rust source's test
// curve_buy_matches_captured_trade): the pump TradeEvent.sol_amount is
// exactly the SOL that enters the curve, and its token_amount equals this
// function's output. The pump/creator fee is charged separately, on top of
// sol_amount (125 bps in the captured tx) — model it outside this pure curve
// function.
func CurveBuyTokensOut(virtualSol, virtualToken, solIn uint64) uint64 {
	if solIn == 0 {
		return 0
	}
	den := new(big.Int).Add(new(big.Int).SetUint64(virtualSol), new(big.Int).SetUint64(solIn))
	if den.Sign() == 0 {
		return 0
	}
	num := new(big.Int).Mul(new(big.Int).SetUint64(virtualToken), new(big.Int).SetUint64(solIn))
	num.Div(num, den)
	// Result is strictly < virtual_token, so it always fits in u64.
	return num.Uint64()
}

// CurveSellSOLOut returns the lamports of SOL received for selling tokensIn
// raw token units INTO the curve (pre-fee, pure constant-product). Symmetric
// to CurveBuyTokensOut: sol out = vsol * tokensIn / (vtoken + tokensIn). The
// trading fee is charged separately on the SOL received — apply it outside.
func CurveSellSOLOut(virtualSol, virtualToken, tokensIn uint64) uint64 {
	if tokensIn == 0 {
		return 0
	}
	den := new(big.Int).Add(new(big.Int).SetUint64(virtualToken), new(big.Int).SetUint64(tokensIn))
	if den.Sign() == 0 {
		return 0
	}
	num := new(big.Int).Mul(new(big.Int).SetUint64(virtualSol), new(big.Int).SetUint64(tokensIn))
	num.Div(num, den)
	// Result is strictly < virtual_sol, so it always fits in u64.
	return num.Uint64()
}

// BondingCurvePDA derives a token's bonding-curve PDA from its mint.
func BondingCurvePDA(mint solana.PublicKey) solana.PublicKey {
	program := solana.MustPublicKeyFromBase58(PumpProgram)
	pda, _, _ := solana.FindProgramAddress([][]byte{BondingCurveSeed, mint.Bytes()}, program)
	return pda
}

// ── BondingCurve account layout ─────────────────────────────────────────────
// VERIFIED against a live account whose reserves matched a TradeEvent from
// the same slot exactly (all four reserves + supply). Total account data =
// 151 bytes (8 disc + 5x u64 + 1 bool + 32 creator + trailing zero pad).
//
//	offset  field
//	0       8-byte account discriminator (17 b7 f8 37 60 d8 ac 60)
//	8       virtual_token_reserves : u64
//	16      virtual_sol_reserves   : u64
//	24      real_token_reserves    : u64
//	32      real_sol_reserves      : u64
//	40      token_total_supply     : u64
//	48      complete               : bool  (1 = graduated / migrating)
//	49      creator                : pubkey (32)

// BondingCurve is a decoded BondingCurve account — the on-chain
// price/graduation state.
type BondingCurve struct {
	VirtualTokenReserves uint64
	VirtualSolReserves   uint64
	RealTokenReserves    uint64
	RealSolReserves      uint64
	TokenTotalSupply     uint64
	// Complete is true once the curve has filled and is graduating to
	// PumpSwap.
	Complete bool
	Creator  solana.PublicKey
}

// BondingCurveAccountDisc is the account discriminator for a BondingCurve
// (anchor sha256("account:BondingCurve")).
var BondingCurveAccountDisc = [8]byte{0x17, 0xb7, 0xf8, 0x37, 0x60, 0xd8, 0xac, 0x60}

// DecodeBondingCurve decodes raw account data. Returns ok=false if the
// discriminator does not match or the buffer is short (i.e. it is not a
// BondingCurve account).
func DecodeBondingCurve(data []byte) (*BondingCurve, bool) {
	if len(data) < 81 || !hasDisc(data, BondingCurveAccountDisc) {
		return nil, false
	}
	return &BondingCurve{
		VirtualTokenReserves: u64LE(data, 8),
		VirtualSolReserves:   u64LE(data, 16),
		RealTokenReserves:    u64LE(data, 24),
		RealSolReserves:      u64LE(data, 32),
		TokenTotalSupply:     u64LE(data, 40),
		Complete:             data[48] != 0,
		Creator:              pubkeyAt(data, 49),
	}, true
}

// PriceInSOL is the price of one whole token in SOL from the current virtual
// reserves.
func (bc *BondingCurve) PriceInSOL() float64 {
	return PriceInSOL(bc.VirtualSolReserves, bc.VirtualTokenReserves, PumpTokenDecimals)
}

// ── TradeEvent (buy / sell) ─────────────────────────────────────────────────
// VERIFIED against >=2 real events (a Sell and a dev Buy). Core layout:
//
//	0    disc(8)                 40  sol_amount:u64
//	8    mint:pubkey(32)         48  token_amount:u64
//	56   is_buy:bool             57  user:pubkey(32)
//	89   timestamp:i64           97  virtual_sol_reserves:u64
//	105  virtual_token_reserves  113 real_sol_reserves:u64
//	121  real_token_reserves:u64 129 fee_recipient:pubkey(32)
//	161  fee_basis_points:u64    169 fee:u64
//	177  creator:pubkey(32)      209 creator_fee_basis_points:u64
//	217  creator_fee:u64         (further fields: volume accounting + a
//	                             "buy"/"sell" string — not needed, left
//	                             undecoded)

// TradeEvent is a decoded buy or sell on the bonding curve.
type TradeEvent struct {
	Mint solana.PublicKey
	// SolAmount is lamports of SOL that moved (paid on a buy, received on a
	// sell).
	SolAmount uint64
	// TokenAmount is raw token units that moved.
	TokenAmount          uint64
	IsBuy                bool
	User                 solana.PublicKey
	Timestamp            int64
	VirtualSolReserves   uint64
	VirtualTokenReserves uint64
	RealSolReserves      uint64
	RealTokenReserves    uint64
	// Creator is the token's creator (dev), present in the current layout;
	// ok=false on the older/shorter variant. Useful for the dev-dump rug
	// proxy.
	Creator    solana.PublicKey
	HasCreator bool
}

// DecodeTradeEvent decodes a `Program data:` blob whose first 8 bytes are
// TradeEventDisc.
func DecodeTradeEvent(data []byte) (*TradeEvent, bool) {
	if len(data) < 129 || !hasDisc(data, TradeEventDisc) {
		return nil, false
	}
	var creator solana.PublicKey
	hasCreator := false
	if len(data) >= 209 {
		creator = pubkeyAt(data, 177)
		hasCreator = true
	}
	return &TradeEvent{
		Mint:                 pubkeyAt(data, 8),
		SolAmount:            u64LE(data, 40),
		TokenAmount:          u64LE(data, 48),
		IsBuy:                data[56] != 0,
		User:                 pubkeyAt(data, 57),
		Timestamp:            i64LE(data, 89),
		VirtualSolReserves:   u64LE(data, 97),
		VirtualTokenReserves: u64LE(data, 105),
		RealSolReserves:      u64LE(data, 113),
		RealTokenReserves:    u64LE(data, 121),
		Creator:              creator,
		HasCreator:           hasCreator,
	}, true
}

// PriceInSOL is the whole-token price implied by this event's post-trade
// virtual reserves.
func (e *TradeEvent) PriceInSOL() float64 {
	return PriceInSOL(e.VirtualSolReserves, e.VirtualTokenReserves, PumpTokenDecimals)
}

// ── CreateEvent (new launch) ─────────────────────────────────────────────────
// VERIFIED against a CreateV2 tx. Layout (three leading anchor strings, so it
// is variable-length up to `mint`):
//
//	0   disc(8)
//	8   name:string  symbol:string  uri:string    (each u32 len + bytes)
//	..  mint:pubkey  bonding_curve:pubkey  user:pubkey(dev)  creator:pubkey
//	..  timestamp:i64
//	..  virtual_token_reserves:u64  virtual_sol_reserves:u64
//	..  real_token_reserves:u64     token_total_supply:u64

// CreateEvent is a decoded new-token launch.
type CreateEvent struct {
	Name         string
	Symbol       string
	URI          string
	Mint         solana.PublicKey
	BondingCurve solana.PublicKey
	// User is the wallet that submitted the create (the "dev").
	User solana.PublicKey
	// Creator is the recorded creator (== User in every sample; distinct
	// field on-chain).
	Creator              solana.PublicKey
	Timestamp            int64
	VirtualTokenReserves uint64
	VirtualSolReserves   uint64
	RealTokenReserves    uint64
	TokenTotalSupply     uint64
}

// DecodeCreateEvent decodes a `Program data:` blob whose first 8 bytes are
// CreateEventDisc.
func DecodeCreateEvent(data []byte) (*CreateEvent, bool) {
	if len(data) < 8 || !hasDisc(data, CreateEventDisc) {
		return nil, false
	}
	o := 8
	name, ok := readString(data, &o)
	if !ok {
		return nil, false
	}
	symbol, ok := readString(data, &o)
	if !ok {
		return nil, false
	}
	uri, ok := readString(data, &o)
	if !ok {
		return nil, false
	}
	if !hasBytes(data, o, 32*4+8*4) {
		return nil, false
	}
	mint := pubkeyAt(data, o)
	o += 32
	bondingCurve := pubkeyAt(data, o)
	o += 32
	user := pubkeyAt(data, o)
	o += 32
	creator := pubkeyAt(data, o)
	o += 32
	timestamp := i64LE(data, o)
	o += 8
	virtualTokenReserves := u64LE(data, o)
	o += 8
	virtualSolReserves := u64LE(data, o)
	o += 8
	realTokenReserves := u64LE(data, o)
	o += 8
	tokenTotalSupply := u64LE(data, o)
	return &CreateEvent{
		Name:                 name,
		Symbol:               symbol,
		URI:                  uri,
		Mint:                 mint,
		BondingCurve:         bondingCurve,
		User:                 user,
		Creator:              creator,
		Timestamp:            timestamp,
		VirtualTokenReserves: virtualTokenReserves,
		VirtualSolReserves:   virtualSolReserves,
		RealTokenReserves:    realTokenReserves,
		TokenTotalSupply:     tokenTotalSupply,
	}, true
}

// ── Migrate event ────────────────────────────────────────────────────────────
// The MigrateV2 tx emits an event with MigrateEventDisc. Its full field
// layout is NOT fully decoded (fields before `mint` are not needed and left
// undocumented — being honest); the token `mint` sits at a fixed byte offset
// of 50, VERIFIED stable across 3 migrate events (all ending in "pump").

// MigrateEvent is a decoded graduation. Only Mint is extracted from the
// event; the new PumpSwap pool is derivable from the migrate instruction
// accounts if needed.
type MigrateEvent struct {
	Mint solana.PublicKey
}

const migrateEventMintOffset = 50

// DecodeMigrateEvent decodes a `Program data:` blob whose first 8 bytes are
// MigrateEventDisc.
func DecodeMigrateEvent(data []byte) (*MigrateEvent, bool) {
	if len(data) < migrateEventMintOffset+32 || !hasDisc(data, MigrateEventDisc) {
		return nil, false
	}
	return &MigrateEvent{Mint: pubkeyAt(data, migrateEventMintOffset)}, true
}

// ── Unified event ────────────────────────────────────────────────────────────

// EventKind tags which variant a PumpEvent holds.
type EventKind int

const (
	EventCreate EventKind = iota
	EventTrade
	EventMigrate
)

// PumpEvent is any pump.fun event decoded from a `Program data:` log blob.
// Exactly one of Create/Trade/Migrate is populated, per Kind.
type PumpEvent struct {
	Kind    EventKind
	Create  *CreateEvent
	Trade   *TradeEvent
	Migrate *MigrateEvent
}

// ParsePumpEvent routes a raw event blob (already base64-decoded) by its
// discriminator.
func ParsePumpEvent(data []byte) (*PumpEvent, bool) {
	if len(data) < 8 {
		return nil, false
	}
	var disc [8]byte
	copy(disc[:], data[:8])
	switch disc {
	case CreateEventDisc:
		ev, ok := DecodeCreateEvent(data)
		if !ok {
			return nil, false
		}
		return &PumpEvent{Kind: EventCreate, Create: ev}, true
	case TradeEventDisc:
		ev, ok := DecodeTradeEvent(data)
		if !ok {
			return nil, false
		}
		return &PumpEvent{Kind: EventTrade, Trade: ev}, true
	case MigrateEventDisc:
		ev, ok := DecodeMigrateEvent(data)
		if !ok {
			return nil, false
		}
		return &PumpEvent{Kind: EventMigrate, Migrate: ev}, true
	default:
		return nil, false
	}
}

// KindTag returns the short tag used in the collector's JSONL event_type
// field.
func (e *PumpEvent) KindTag() string {
	switch e.Kind {
	case EventCreate:
		return "create"
	case EventTrade:
		if e.Trade != nil && e.Trade.IsBuy {
			return "buy"
		}
		return "sell"
	case EventMigrate:
		return "migrate"
	default:
		return ""
	}
}

// ParseProgramDataB64 decodes the base64 payload of a `Program data:` log
// line into a PumpEvent. Non-pump / unrecognised blobs return ok=false.
func ParseProgramDataB64(b64 string) (*PumpEvent, bool) {
	bytes, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, false
	}
	return ParsePumpEvent(bytes)
}

// ── little-endian read helpers ───────────────────────────────────────────────

func hasBytes(d []byte, o, n int) bool {
	return o >= 0 && n >= 0 && o+n <= len(d)
}

func u64LE(d []byte, o int) uint64 {
	return binary.LittleEndian.Uint64(d[o : o+8])
}

func i64LE(d []byte, o int) int64 {
	return int64(binary.LittleEndian.Uint64(d[o : o+8]))
}

func pubkeyAt(d []byte, o int) solana.PublicKey {
	return solana.PublicKeyFromBytes(d[o : o+32])
}

func hasDisc(d []byte, disc [8]byte) bool {
	if len(d) < 8 {
		return false
	}
	var got [8]byte
	copy(got[:], d[:8])
	return got == disc
}

// readString reads an anchor String (u32 little-endian length prefix + UTF-8
// bytes), advancing *o. Go's string() conversion from bytes accepts invalid
// UTF-8 as-is (token names are arbitrary user input), matching the Rust
// source's use of String::from_utf8_lossy.
func readString(d []byte, o *int) (string, bool) {
	if !hasBytes(d, *o, 4) {
		return "", false
	}
	length := int(binary.LittleEndian.Uint32(d[*o : *o+4]))
	*o += 4
	if !hasBytes(d, *o, length) {
		return "", false
	}
	s := string(d[*o : *o+length])
	*o += length
	return s, true
}
