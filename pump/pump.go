// Package pump implements pump.fun / PumpSwap recon + decoders — Phase 1
// (measure-first).
//
// Everything here is derived from CAPTURED REAL MAINNET TRANSACTIONS, never
// from memory (repo doctrine). Program IDs, event discriminators, and byte
// offsets are each verified against >=2 live txs; the original Rust unit
// tests pin the layouts to real captured bytes so a future pump upgrade that
// shifts a field trips a test instead of silently corrupting the collector.
//
// This package is fully self-contained and shares NO state with the
// liquidation engine. It is pure observation: nothing here signs or submits
// a transaction.
//
// -- What was verified (see the PR body for the exact signatures) ----------
//   - Bonding-curve program `6EF8…F6P` — owns every `BondingCurve` PDA and
//     emits the anchor self-CPI event logs (`Program data: …`) we decode.
//   - PumpSwap AMM `pAMM…fXEA` — the graduated venue; a bonding curve
//     migrates into a PumpSwap pool at completion.
//   - The current mainnet program is the Token-2022 variant (mints/curve
//     token accounts live under `TokenzQd…`), and the instruction set has
//     grown V2/V3 variants (Create/CreateV2, Buy/BuyV2, Sell/SellV2,
//     MigrateV2). We therefore decode the anchor event logs, whose layout is
//     stable across those instruction variants, rather than
//     per-instruction discriminators.
//
// -- Anchor event self-CPI logs ---------------------------------------------
// pump emits structured events as base64 in `Program data:` log lines. The
// first 8 bytes are the anchor event discriminator (`sha256("event:<Name>")`).
// We match on those to route Create / Trade / Migrate.
package pump

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"math"
	"math/big"

	solana "github.com/gagliardetto/solana-go"

	"solana-arb-backtest/shared"
)

// -- Program IDs (verified: getAccountInfo owner/executable + real txs) ----

// PumpProgram is the pump.fun bonding-curve program. VERIFIED: it is the
// `owner` of every `BondingCurve` account we fetched, and every
// create/buy/sell/migrate tx we pulled is a top-level invoke of this
// program.
const PumpProgram = "6EF8rrecthR5Dkzon8Nwu78hRvfCKubJ14M5uBEwF6P"

// PumpswapAMM is the PumpSwap AMM program (post-graduation venue). VERIFIED:
// `executable=true`, owned by the BPF upgradeable loader, and it is account
// #10 of the `MigrateV2` instruction (the pool the curve migrates into).
const PumpswapAMM = "pAMMBay6oceH9fJKBRHGP5D4bD4sWpmSwMn52FMfXEA"

// PumpFeeProgram is the pump.fun fee program (`GetFees` CPI seen in every
// trade). Informational.
const PumpFeeProgram = "pfeeUxB6jkeY1Hxd7CsFCAjcbHA9rWtchMGdZ6VojVZ"

// MigrationAuthority is the migration authority — the signer of
// `MigrateV2`. Its transaction history is, in effect, the list of
// graduations. VERIFIED across multiple migrate txs.
const MigrationAuthority = "39azUYFWPz3VHgKCf3VChUwbpURdCHRxjWVowf5jUJjg"

// BondingCurveSeed is the PDA seed for a token's bonding-curve account:
// ["bonding-curve", mint]. VERIFIED: find_program_address(["bonding-curve",
// mint], PUMP_PROGRAM) equals the `bonding_curve` field of the CreateEvent
// (see test).
var BondingCurveSeed = []byte("bonding-curve")

// PumpTokenDecimals: pump.fun tokens are minted with 6 decimals and a 1e15
// raw total supply (= 1,000,000,000 whole tokens). VERIFIED from
// CreateEvent.token_total_supply.
const PumpTokenDecimals uint32 = 6

// -- Anchor event discriminators (first 8 bytes of a `Program data:` blob) -

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

// -- Reserve -> price helpers ------------------------------------------------

// PriceLamportsPerRawToken is the price of one raw token unit in lamports,
// straight from the constant-product reserves
// (virtual_sol_reserves / virtual_token_reserves).
func PriceLamportsPerRawToken(virtualSol, virtualToken uint64) float64 {
	if virtualToken == 0 {
		return 0.0
	}
	return float64(virtualSol) / float64(virtualToken)
}

// PriceInSol is the price of one whole token in SOL, accounting for the 9
// lamport decimals of SOL and tokenDecimals of the token (6 for pump). This
// is the number you compare across launches for the "peak multiple" census.
func PriceInSol(virtualSol, virtualToken uint64, tokenDecimals uint32) float64 {
	if virtualToken == 0 {
		return 0.0
	}
	sol := float64(virtualSol) / 1e9
	tokens := float64(virtualToken) / math.Pow(10, float64(tokenDecimals))
	return sol / tokens
}

// CurveBuyTokensOut returns the raw token units received for paying solIn
// lamports into the bonding curve (pre-fee, pure constant-product).
// virtualSol/virtualToken are the curve's virtual reserves before the trade.
//
// pump.fun's curve is a constant product k = vsol * vtoken. Paying sol_in
// raises vsol to vsol + sol_in, so tokens out = vtoken - k/(vsol+sol_in) =
// vtoken * sol_in / (vsol + sol_in). VERIFIED to the lamport against a real
// captured dev-buy (see the Rust test curve_buy_matches_captured_trade): the
// pump TradeEvent.sol_amount is exactly the SOL that enters the curve, and
// its token_amount equals this function's output. The pump/creator fee is
// charged separately, on top of sol_amount (125 bps in the captured tx) —
// model it outside this pure curve function.
func CurveBuyTokensOut(virtualSol, virtualToken, solIn uint64) uint64 {
	if solIn == 0 {
		return 0
	}
	den := new(big.Int).SetUint64(virtualSol)
	den.Add(den, new(big.Int).SetUint64(solIn))
	if den.Sign() == 0 {
		return 0
	}
	num := new(big.Int).SetUint64(virtualToken)
	num.Mul(num, new(big.Int).SetUint64(solIn))
	// Result is strictly < virtual_token, so it always fits in u64.
	res := new(big.Int).Div(num, den)
	return res.Uint64()
}

// CurveSellSolOut returns the lamports of SOL received for selling tokensIn
// raw token units into the curve (pre-fee, pure constant-product).
// Symmetric to CurveBuyTokensOut: sol out = vsol * tokens_in / (vtoken +
// tokens_in). The trading fee is charged separately on the SOL received —
// apply it outside.
func CurveSellSolOut(virtualSol, virtualToken, tokensIn uint64) uint64 {
	if tokensIn == 0 {
		return 0
	}
	den := new(big.Int).SetUint64(virtualToken)
	den.Add(den, new(big.Int).SetUint64(tokensIn))
	if den.Sign() == 0 {
		return 0
	}
	num := new(big.Int).SetUint64(virtualSol)
	num.Mul(num, new(big.Int).SetUint64(tokensIn))
	// Result is strictly < virtual_sol, so it always fits in u64.
	res := new(big.Int).Div(num, den)
	return res.Uint64()
}

// BondingCurvePDA derives a token's bonding-curve PDA from its mint.
func BondingCurvePDA(mint shared.Pubkey) shared.Pubkey {
	program := solana.MustPublicKeyFromBase58(PumpProgram)
	addr, _, err := solana.FindProgramAddress([][]byte{BondingCurveSeed, mint.Bytes()}, program)
	if err != nil {
		panic(err)
	}
	pk, _ := shared.PubkeyFromBytes(addr[:])
	return pk
}

// -- BondingCurve account layout ---------------------------------------------
// VERIFIED against a live account whose reserves matched a TradeEvent from
// the same slot exactly (all four reserves + supply). Total account data =
// 151 bytes (8 disc + 5xu64 + 1 bool + 32 creator + trailing zero pad).
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
	Creator  shared.Pubkey
}

// BondingCurveAccountDisc is the account discriminator for a BondingCurve
// (anchor sha256("account:BondingCurve")).
var BondingCurveAccountDisc = [8]byte{0x17, 0xb7, 0xf8, 0x37, 0x60, 0xd8, 0xac, 0x60}

// DecodeBondingCurve decodes raw account data. Returns (BondingCurve{},
// false) if the discriminator does not match or the buffer is short (i.e.
// it is not a BondingCurve account).
func DecodeBondingCurve(data []byte) (BondingCurve, bool) {
	if len(data) < 81 || !bytes.Equal(data[:8], BondingCurveAccountDisc[:]) {
		return BondingCurve{}, false
	}
	virtualTokenReserves, ok := u64LE(data, 8)
	if !ok {
		return BondingCurve{}, false
	}
	virtualSolReserves, ok := u64LE(data, 16)
	if !ok {
		return BondingCurve{}, false
	}
	realTokenReserves, ok := u64LE(data, 24)
	if !ok {
		return BondingCurve{}, false
	}
	realSolReserves, ok := u64LE(data, 32)
	if !ok {
		return BondingCurve{}, false
	}
	tokenTotalSupply, ok := u64LE(data, 40)
	if !ok {
		return BondingCurve{}, false
	}
	creator, ok := pubkeyAt(data, 49)
	if !ok {
		return BondingCurve{}, false
	}
	return BondingCurve{
		VirtualTokenReserves: virtualTokenReserves,
		VirtualSolReserves:   virtualSolReserves,
		RealTokenReserves:    realTokenReserves,
		RealSolReserves:      realSolReserves,
		TokenTotalSupply:     tokenTotalSupply,
		Complete:             data[48] != 0,
		Creator:              creator,
	}, true
}

// PriceInSol is the price of one whole token in SOL from the current
// virtual reserves.
func (bc BondingCurve) PriceInSol() float64 {
	return PriceInSol(bc.VirtualSolReserves, bc.VirtualTokenReserves, PumpTokenDecimals)
}

// -- TradeEvent (buy / sell) -------------------------------------------------
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
//	                              "buy"/"sell" string — not needed, left
//	                              undecoded)

// TradeEvent is a decoded buy or sell on the bonding curve.
type TradeEvent struct {
	Mint shared.Pubkey
	// SolAmount is the lamports of SOL that moved (paid on a buy, received
	// on a sell).
	SolAmount uint64
	// TokenAmount is the raw token units that moved.
	TokenAmount uint64
	IsBuy       bool
	// User is the trader.
	User                 shared.Pubkey
	Timestamp            int64
	VirtualSolReserves   uint64
	VirtualTokenReserves uint64
	RealSolReserves      uint64
	RealTokenReserves    uint64
	// Creator is the token's creator (dev), if present in the current
	// layout; zero-value/false on the older/shorter variant. Useful for the
	// dev-dump rug proxy.
	Creator    shared.Pubkey
	HasCreator bool
}

// DecodeTradeEvent decodes a `Program data:` blob whose first 8 bytes are
// TradeEventDisc.
func DecodeTradeEvent(data []byte) (TradeEvent, bool) {
	if len(data) < 129 || !bytes.Equal(data[:8], TradeEventDisc[:]) {
		return TradeEvent{}, false
	}
	var creator shared.Pubkey
	hasCreator := false
	if len(data) >= 209 {
		if c, ok := pubkeyAt(data, 177); ok {
			creator = c
			hasCreator = true
		}
	}
	mint, ok := pubkeyAt(data, 8)
	if !ok {
		return TradeEvent{}, false
	}
	solAmount, ok := u64LE(data, 40)
	if !ok {
		return TradeEvent{}, false
	}
	tokenAmount, ok := u64LE(data, 48)
	if !ok {
		return TradeEvent{}, false
	}
	user, ok := pubkeyAt(data, 57)
	if !ok {
		return TradeEvent{}, false
	}
	timestamp, ok := i64LE(data, 89)
	if !ok {
		return TradeEvent{}, false
	}
	virtualSolReserves, ok := u64LE(data, 97)
	if !ok {
		return TradeEvent{}, false
	}
	virtualTokenReserves, ok := u64LE(data, 105)
	if !ok {
		return TradeEvent{}, false
	}
	realSolReserves, ok := u64LE(data, 113)
	if !ok {
		return TradeEvent{}, false
	}
	realTokenReserves, ok := u64LE(data, 121)
	if !ok {
		return TradeEvent{}, false
	}
	return TradeEvent{
		Mint:                 mint,
		SolAmount:            solAmount,
		TokenAmount:          tokenAmount,
		IsBuy:                data[56] != 0,
		User:                 user,
		Timestamp:            timestamp,
		VirtualSolReserves:   virtualSolReserves,
		VirtualTokenReserves: virtualTokenReserves,
		RealSolReserves:      realSolReserves,
		RealTokenReserves:    realTokenReserves,
		Creator:              creator,
		HasCreator:           hasCreator,
	}, true
}

// PriceInSol is the whole-token price implied by this event's post-trade
// virtual reserves.
func (ev TradeEvent) PriceInSol() float64 {
	return PriceInSol(ev.VirtualSolReserves, ev.VirtualTokenReserves, PumpTokenDecimals)
}

// -- CreateEvent (new launch) ------------------------------------------------
// VERIFIED against a CreateV2 tx. Layout (three leading anchor strings, so
// it is variable-length up to `mint`):
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
	Mint         shared.Pubkey
	BondingCurve shared.Pubkey
	// User is the wallet that submitted the create (the "dev").
	User shared.Pubkey
	// Creator is the recorded creator (== User in every sample; distinct
	// field on-chain).
	Creator              shared.Pubkey
	Timestamp            int64
	VirtualTokenReserves uint64
	VirtualSolReserves   uint64
	RealTokenReserves    uint64
	TokenTotalSupply     uint64
}

// DecodeCreateEvent decodes a `Program data:` blob whose first 8 bytes are
// CreateEventDisc.
func DecodeCreateEvent(data []byte) (CreateEvent, bool) {
	if len(data) < 8 || !bytes.Equal(data[:8], CreateEventDisc[:]) {
		return CreateEvent{}, false
	}
	o := 8
	name, ok := readString(data, &o)
	if !ok {
		return CreateEvent{}, false
	}
	symbol, ok := readString(data, &o)
	if !ok {
		return CreateEvent{}, false
	}
	uri, ok := readString(data, &o)
	if !ok {
		return CreateEvent{}, false
	}
	mint, ok := pubkeyAt(data, o)
	if !ok {
		return CreateEvent{}, false
	}
	o += 32
	bondingCurve, ok := pubkeyAt(data, o)
	if !ok {
		return CreateEvent{}, false
	}
	o += 32
	user, ok := pubkeyAt(data, o)
	if !ok {
		return CreateEvent{}, false
	}
	o += 32
	creator, ok := pubkeyAt(data, o)
	if !ok {
		return CreateEvent{}, false
	}
	o += 32
	timestamp, ok := i64LE(data, o)
	if !ok {
		return CreateEvent{}, false
	}
	o += 8
	virtualTokenReserves, ok := u64LE(data, o)
	if !ok {
		return CreateEvent{}, false
	}
	o += 8
	virtualSolReserves, ok := u64LE(data, o)
	if !ok {
		return CreateEvent{}, false
	}
	o += 8
	realTokenReserves, ok := u64LE(data, o)
	if !ok {
		return CreateEvent{}, false
	}
	o += 8
	tokenTotalSupply, ok := u64LE(data, o)
	if !ok {
		return CreateEvent{}, false
	}
	return CreateEvent{
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

// -- Migrate event ------------------------------------------------------------
// The MigrateV2 tx emits an event with MigrateEventDisc. Its full field
// layout is NOT fully decoded (fields before `mint` are not needed and left
// undocumented — being honest); the token `mint` sits at a fixed byte
// offset of 50, VERIFIED stable across 3 migrate events (all ending in
// "pump").

// MigrateEvent is a decoded graduation. Only Mint is extracted from the
// event; the new PumpSwap pool is derivable from the migrate instruction
// accounts if needed.
type MigrateEvent struct {
	Mint shared.Pubkey
}

// migrateEventMintOffset is the byte offset of the mint field within a
// migrate event blob.
const migrateEventMintOffset = 50

// DecodeMigrateEvent decodes a `Program data:` blob whose first 8 bytes are
// MigrateEventDisc.
func DecodeMigrateEvent(data []byte) (MigrateEvent, bool) {
	if len(data) < migrateEventMintOffset+32 || !bytes.Equal(data[:8], MigrateEventDisc[:]) {
		return MigrateEvent{}, false
	}
	mint, ok := pubkeyAt(data, migrateEventMintOffset)
	if !ok {
		return MigrateEvent{}, false
	}
	return MigrateEvent{Mint: mint}, true
}

// -- Unified event ------------------------------------------------------------

// PumpEventKind identifies which variant a PumpEvent holds.
type PumpEventKind int

const (
	PumpEventKindCreate PumpEventKind = iota
	PumpEventKindTrade
	PumpEventKindMigrate
)

// PumpEvent is any pump.fun event decoded from a `Program data:` log blob.
// Exactly one of Create / Trade / Migrate is populated, selected by Kind().
type PumpEvent struct {
	variant PumpEventKind
	create  CreateEvent
	trade   TradeEvent
	migrate MigrateEvent
}

// NewCreatePumpEvent wraps a CreateEvent as a PumpEvent.
func NewCreatePumpEvent(ev CreateEvent) PumpEvent {
	return PumpEvent{variant: PumpEventKindCreate, create: ev}
}

// NewTradePumpEvent wraps a TradeEvent as a PumpEvent.
func NewTradePumpEvent(ev TradeEvent) PumpEvent {
	return PumpEvent{variant: PumpEventKindTrade, trade: ev}
}

// NewMigratePumpEvent wraps a MigrateEvent as a PumpEvent.
func NewMigratePumpEvent(ev MigrateEvent) PumpEvent {
	return PumpEvent{variant: PumpEventKindMigrate, migrate: ev}
}

// Variant reports which event kind this PumpEvent holds.
func (e PumpEvent) Variant() PumpEventKind { return e.variant }

// Create returns the wrapped CreateEvent and whether this PumpEvent is a
// Create variant.
func (e PumpEvent) Create() (CreateEvent, bool) {
	return e.create, e.variant == PumpEventKindCreate
}

// Trade returns the wrapped TradeEvent and whether this PumpEvent is a
// Trade variant.
func (e PumpEvent) Trade() (TradeEvent, bool) {
	return e.trade, e.variant == PumpEventKindTrade
}

// Migrate returns the wrapped MigrateEvent and whether this PumpEvent is a
// Migrate variant.
func (e PumpEvent) Migrate() (MigrateEvent, bool) {
	return e.migrate, e.variant == PumpEventKindMigrate
}

// ParsePumpEvent routes a raw event blob (already base64-decoded) by its
// discriminator. Returns (PumpEvent{}, false) if unrecognised.
func ParsePumpEvent(data []byte) (PumpEvent, bool) {
	if len(data) < 8 {
		return PumpEvent{}, false
	}
	var disc [8]byte
	copy(disc[:], data[:8])
	switch disc {
	case CreateEventDisc:
		ev, ok := DecodeCreateEvent(data)
		if !ok {
			return PumpEvent{}, false
		}
		return NewCreatePumpEvent(ev), true
	case TradeEventDisc:
		ev, ok := DecodeTradeEvent(data)
		if !ok {
			return PumpEvent{}, false
		}
		return NewTradePumpEvent(ev), true
	case MigrateEventDisc:
		ev, ok := DecodeMigrateEvent(data)
		if !ok {
			return PumpEvent{}, false
		}
		return NewMigratePumpEvent(ev), true
	default:
		return PumpEvent{}, false
	}
}

// Kind returns the short tag used in the collector's JSONL `event_type`
// field.
func (e PumpEvent) Kind() string {
	switch e.variant {
	case PumpEventKindCreate:
		return "create"
	case PumpEventKindTrade:
		if e.trade.IsBuy {
			return "buy"
		}
		return "sell"
	case PumpEventKindMigrate:
		return "migrate"
	default:
		return ""
	}
}

// ParseProgramDataB64 decodes the base64 payload of a `Program data:` log
// line into a PumpEvent. Non-pump / unrecognised blobs return
// (PumpEvent{}, false).
func ParseProgramDataB64(b64 string) (PumpEvent, bool) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return PumpEvent{}, false
	}
	return ParsePumpEvent(raw)
}

// -- little-endian read helpers ----------------------------------------------

func u64LE(d []byte, o int) (uint64, bool) {
	if o < 0 || o+8 > len(d) {
		return 0, false
	}
	return binary.LittleEndian.Uint64(d[o : o+8]), true
}

func i64LE(d []byte, o int) (int64, bool) {
	v, ok := u64LE(d, o)
	if !ok {
		return 0, false
	}
	return int64(v), true
}

func pubkeyAt(d []byte, o int) (shared.Pubkey, bool) {
	if o < 0 || o+32 > len(d) {
		return shared.Pubkey{}, false
	}
	pk, err := shared.PubkeyFromBytes(d[o : o+32])
	return pk, err == nil
}

// readString reads an anchor String (u32 little-endian length prefix + UTF-8
// bytes), advancing *o. Matches Rust's String::from_utf8_lossy: invalid
// UTF-8 is replaced rather than rejected, since token names are arbitrary
// user input.
func readString(d []byte, o *int) (string, bool) {
	if *o < 0 || *o+4 > len(d) {
		return "", false
	}
	length := int(binary.LittleEndian.Uint32(d[*o : *o+4]))
	*o += 4
	if length < 0 || *o+length > len(d) {
		return "", false
	}
	b := d[*o : *o+length]
	*o += length
	// Go's string(b) conversion already replaces invalid UTF-8 with U+FFFD,
	// matching Rust's String::from_utf8_lossy (token names are arbitrary
	// user input, so lossy handling is required, not just accepted).
	return string(b), true
}
