// Package pump implements pump.fun / PumpSwap recon + decoders —
// **Phase 1 (measure-first)**.
//
// Everything here is derived from CAPTURED REAL MAINNET TRANSACTIONS, never
// from memory (repo doctrine). Program IDs, event discriminators, and byte
// offsets are each verified against >=2 live txs; the unit tests at the
// bottom pin the layouts to real captured bytes so a future pump upgrade
// that shifts a field trips a test instead of silently corrupting the
// collector.
//
// This package is fully self-contained and shares NO state with the
// liquidation engine. It is pure observation: nothing here signs or
// submits a transaction.
//
// It backs three planned commands (Rust: pump_collect, pump_census,
// pump_backtest — not yet ported): a WebSocket collector that calls
// ParseProgramDataB64 on each `Program data:` log line and appends decoded
// events to a JSONL file; a census tool that reads that JSONL back and
// reports launch/graduation/rug statistics; and a backtest tool that
// replays the JSONL chronologically using CurveBuyTokensOut /
// CurveSellSolOut to simulate strategies. The exported surface here (event
// decoders, curve math, program IDs) is shaped to serve all three without
// change.
//
// ── What was verified (see the PR body for the exact signatures) ────────────
//   - Bonding-curve program `6EF8…F6P` — owns every `BondingCurve` PDA and
//     emits the anchor self-CPI event logs (`Program data: …`) we decode.
//   - PumpSwap AMM `pAMM…fXEA` — the graduated venue; a bonding curve
//     migrates into a PumpSwap pool at completion.
//   - The current mainnet program is the **Token-2022 variant** (mints/curve
//     token accounts live under `TokenzQd…`), and the instruction set has
//     grown V2/V3 variants (Create/CreateV2, Buy/BuyV2, Sell/SellV2,
//     MigrateV2). We therefore decode the **anchor event logs**, whose
//     layout is stable across those instruction variants, rather than
//     per-instruction discriminators.
//
// ── Anchor event self-CPI logs ──────────────────────────────────────────────
// pump emits structured events as base64 in `Program data:` log lines. The
// first 8 bytes are the anchor event discriminator (`sha256("event:<Name>")`).
// We match on those to route Create / Trade / Migrate.
package pump

import (
	"encoding/base64"
	"encoding/binary"
	"math"
	"math/big"

	"arbengine/internal/solana"
)

// ── Program IDs (verified: getAccountInfo owner/executable + real txs) ────────

const (
	// PumpProgram is the pump.fun bonding-curve program. VERIFIED: it is the
	// `owner` of every `BondingCurve` account we fetched, and every
	// create/buy/sell/migrate tx we pulled is a top-level invoke of this
	// program.
	PumpProgram = "6EF8rrecthR5Dkzon8Nwu78hRvfCKubJ14M5uBEwF6P"

	// PumpswapAMM is the PumpSwap AMM program (post-graduation venue).
	// VERIFIED: `executable=true`, owned by the BPF upgradeable loader, and
	// it is account #10 of the `MigrateV2` instruction (the pool the curve
	// migrates into).
	PumpswapAMM = "pAMMBay6oceH9fJKBRHGP5D4bD4sWpmSwMn52FMfXEA"

	// PumpFeeProgram is the pump.fun fee program (`GetFees` CPI seen in
	// every trade). Informational.
	PumpFeeProgram = "pfeeUxB6jkeY1Hxd7CsFCAjcbHA9rWtchMGdZ6VojVZ"

	// MigrationAuthority is the signer of `MigrateV2`. Its transaction
	// history is, in effect, the list of graduations. VERIFIED across
	// multiple migrate txs.
	MigrationAuthority = "39azUYFWPz3VHgKCf3VChUwbpURdCHRxjWVowf5jUJjg"

	// PumpTokenDecimals: pump.fun tokens are minted with 6 decimals and a
	// 1e15 raw total supply (= 1,000,000,000 whole tokens). VERIFIED from
	// CreateEvent.TokenTotalSupply.
	PumpTokenDecimals = 6
)

// BondingCurveSeed is the PDA seed for a token's bonding-curve account:
// ["bonding-curve", mint]. VERIFIED: find_program_address(["bonding-curve",
// mint], PumpProgram) equals the `bonding_curve` field of the CreateEvent
// (see test).
var BondingCurveSeed = []byte("bonding-curve")

// ── Anchor event discriminators (first 8 bytes of a `Program data:` blob) ─────

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

// BondingCurveAccountDisc is the account discriminator for a `BondingCurve`
// (anchor sha256("account:BondingCurve")).
var BondingCurveAccountDisc = [8]byte{0x17, 0xb7, 0xf8, 0x37, 0x60, 0xd8, 0xac, 0x60}

// ── Reserve -> price helpers ──────────────────────────────────────────────────

// PriceLamportsPerRawToken is the price of one **raw** token unit in
// **lamports**, straight from the constant-product reserves
// (virtualSol / virtualToken).
func PriceLamportsPerRawToken(virtualSol, virtualToken uint64) float64 {
	if virtualToken == 0 {
		return 0.0
	}
	return float64(virtualSol) / float64(virtualToken)
}

// PriceInSol is the price of one **whole** token in **SOL**, accounting for
// the 9 lamport decimals of SOL and tokenDecimals of the token (6 for
// pump). This is the number you compare across launches for the "peak
// multiple" census.
func PriceInSol(virtualSol, virtualToken uint64, tokenDecimals uint32) float64 {
	if virtualToken == 0 {
		return 0.0
	}
	sol := float64(virtualSol) / 1e9
	tokens := float64(virtualToken) / math.Pow(10, float64(tokenDecimals))
	return sol / tokens
}

// CurveBuyTokensOut is the raw token units received for paying solIn
// lamports **into** the bonding curve (pre-fee, pure constant-product).
// virtualSol/virtualToken are the curve's virtual reserves *before* the
// trade.
//
// pump.fun's curve is a constant product k = vsol * vtoken. Paying solIn
// raises vsol to vsol+solIn, so tokens out = vtoken - k/(vsol+solIn) =
// vtoken * solIn / (vsol + solIn). VERIFIED to the lamport against a real
// captured dev-buy (see the Rust test curve_buy_matches_captured_trade):
// the pump TradeEvent.sol_amount is exactly the SOL that enters the curve,
// and its token_amount equals this function's output. The pump/creator fee
// is charged **separately, on top** of sol_amount (125 bps in the captured
// tx) — model it outside this pure curve function.
func CurveBuyTokensOut(virtualSol, virtualToken, solIn uint64) uint64 {
	if solIn == 0 {
		return 0
	}
	den := new(big.Int).Add(bigU64(virtualSol), bigU64(solIn))
	if den.Sign() == 0 {
		return 0
	}
	// Result is strictly < virtualToken, so it always fits in u64.
	num := new(big.Int).Mul(bigU64(virtualToken), bigU64(solIn))
	return num.Div(num, den).Uint64()
}

// CurveSellSolOut is the lamports of SOL received for selling tokensIn raw
// token units **into** the curve (pre-fee, pure constant-product).
// Symmetric to CurveBuyTokensOut: sol out = vsol * tokensIn / (vtoken +
// tokensIn). The trading fee is charged separately on the SOL received —
// apply it outside.
func CurveSellSolOut(virtualSol, virtualToken, tokensIn uint64) uint64 {
	if tokensIn == 0 {
		return 0
	}
	den := new(big.Int).Add(bigU64(virtualToken), bigU64(tokensIn))
	if den.Sign() == 0 {
		return 0
	}
	// Result is strictly < virtualSol, so it always fits in u64.
	num := new(big.Int).Mul(bigU64(virtualSol), bigU64(tokensIn))
	return num.Div(num, den).Uint64()
}

func bigU64(v uint64) *big.Int {
	return new(big.Int).SetUint64(v)
}

// BondingCurvePDA derives a token's bonding-curve PDA from its mint.
func BondingCurvePDA(mint solana.Pubkey) solana.Pubkey {
	program := solana.MustPubkeyFromBase58(PumpProgram)
	pda, _ := solana.FindProgramAddress([][]byte{BondingCurveSeed, mint.Bytes()}, program)
	return pda
}

// ── BondingCurve account layout ──────────────────────────────────────────────
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

// BondingCurve is a decoded `BondingCurve` account — the on-chain
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
	Creator  solana.Pubkey
}

// DecodeBondingCurve decodes raw account data. Returns (zero, false) if the
// discriminator does not match or the buffer is short (i.e. it is not a
// BondingCurve account).
func DecodeBondingCurve(data []byte) (BondingCurve, bool) {
	if len(data) < 81 || !discMatches(data, BondingCurveAccountDisc) {
		return BondingCurve{}, false
	}
	creator, ok := pubkeyAt(data, 49)
	if !ok {
		return BondingCurve{}, false
	}
	return BondingCurve{
		VirtualTokenReserves: u64LE(data, 8),
		VirtualSolReserves:   u64LE(data, 16),
		RealTokenReserves:    u64LE(data, 24),
		RealSolReserves:      u64LE(data, 32),
		TokenTotalSupply:     u64LE(data, 40),
		Complete:             data[48] != 0,
		Creator:              creator,
	}, true
}

// PriceInSol is the price of one whole token in SOL from the current
// virtual reserves.
func (bc BondingCurve) PriceInSol() float64 {
	return PriceInSol(bc.VirtualSolReserves, bc.VirtualTokenReserves, PumpTokenDecimals)
}

// ── TradeEvent (buy / sell) ──────────────────────────────────────────────────
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
	Mint solana.Pubkey
	// SolAmount is the lamports of SOL that moved (paid on a buy, received
	// on a sell).
	SolAmount uint64
	// TokenAmount is the raw token units that moved.
	TokenAmount uint64
	IsBuy       bool
	// User is the trader.
	User                 solana.Pubkey
	Timestamp            int64
	VirtualSolReserves   uint64
	VirtualTokenReserves uint64
	RealSolReserves      uint64
	RealTokenReserves    uint64
	// Creator is the token's creator (dev), present in the current layout;
	// zero-value/HasCreator=false on the older/shorter variant. Useful for
	// the dev-dump rug proxy.
	Creator    solana.Pubkey
	HasCreator bool
}

// DecodeTradeEvent decodes a `Program data:` blob whose first 8 bytes are
// TradeEventDisc. Returns (zero, false) on mismatch/short buffer.
func DecodeTradeEvent(data []byte) (TradeEvent, bool) {
	if len(data) < 129 || !discMatches(data, TradeEventDisc) {
		return TradeEvent{}, false
	}
	var creator solana.Pubkey
	var hasCreator bool
	if len(data) >= 209 {
		if c, ok := pubkeyAt(data, 177); ok {
			creator, hasCreator = c, true
		}
	}
	mint, ok := pubkeyAt(data, 8)
	if !ok {
		return TradeEvent{}, false
	}
	user, ok := pubkeyAt(data, 57)
	if !ok {
		return TradeEvent{}, false
	}
	return TradeEvent{
		Mint:                 mint,
		SolAmount:            u64LE(data, 40),
		TokenAmount:          u64LE(data, 48),
		IsBuy:                data[56] != 0,
		User:                 user,
		Timestamp:            i64LE(data, 89),
		VirtualSolReserves:   u64LE(data, 97),
		VirtualTokenReserves: u64LE(data, 105),
		RealSolReserves:      u64LE(data, 113),
		RealTokenReserves:    u64LE(data, 121),
		Creator:              creator,
		HasCreator:           hasCreator,
	}, true
}

// PriceInSol is the whole-token price implied by this event's post-trade
// virtual reserves.
func (t TradeEvent) PriceInSol() float64 {
	return PriceInSol(t.VirtualSolReserves, t.VirtualTokenReserves, PumpTokenDecimals)
}

// ── CreateEvent (new launch) ─────────────────────────────────────────────────
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
	Mint         solana.Pubkey
	BondingCurve solana.Pubkey
	// User is the wallet that submitted the create (the "dev").
	User solana.Pubkey
	// Creator is the recorded creator (== User in every sample; distinct
	// field on-chain).
	Creator              solana.Pubkey
	Timestamp            int64
	VirtualTokenReserves uint64
	VirtualSolReserves   uint64
	RealTokenReserves    uint64
	TokenTotalSupply     uint64
}

// DecodeCreateEvent decodes a `Program data:` blob whose first 8 bytes are
// CreateEventDisc. Returns (zero, false) on mismatch/short buffer.
func DecodeCreateEvent(data []byte) (CreateEvent, bool) {
	if len(data) < 8 || !discMatches(data, CreateEventDisc) {
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
	if len(data) < o+8*5 {
		return CreateEvent{}, false
	}
	timestamp := i64LE(data, o)
	o += 8
	virtualTokenReserves := u64LE(data, o)
	o += 8
	virtualSolReserves := u64LE(data, o)
	o += 8
	realTokenReserves := u64LE(data, o)
	o += 8
	tokenTotalSupply := u64LE(data, o)
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

// ── Migrate event ────────────────────────────────────────────────────────────
// The MigrateV2 tx emits an event with MigrateEventDisc. Its full field
// layout is NOT fully decoded (fields before `mint` are not needed and left
// undocumented — being honest); the token `mint` sits at a fixed byte
// offset of 50, VERIFIED stable across 3 migrate events (all ending in
// "pump").

// migrateMintOffset is the byte offset of `mint` within a MigrateEvent blob.
const migrateMintOffset = 50

// MigrateEvent is a decoded graduation. Only Mint is extracted from the
// event; the new PumpSwap pool is derivable from the migrate instruction
// accounts if needed.
type MigrateEvent struct {
	Mint solana.Pubkey
}

// DecodeMigrateEvent decodes a `Program data:` blob whose first 8 bytes are
// MigrateEventDisc.
func DecodeMigrateEvent(data []byte) (MigrateEvent, bool) {
	if len(data) < migrateMintOffset+32 || !discMatches(data, MigrateEventDisc) {
		return MigrateEvent{}, false
	}
	mint, ok := pubkeyAt(data, migrateMintOffset)
	if !ok {
		return MigrateEvent{}, false
	}
	return MigrateEvent{Mint: mint}, true
}

// ── Unified event ────────────────────────────────────────────────────────────

// EventKind tags which variant a PumpEvent holds.
type EventKind int

const (
	// EventCreate marks a PumpEvent holding a CreateEvent.
	EventCreate EventKind = iota
	// EventTrade marks a PumpEvent holding a TradeEvent.
	EventTrade
	// EventMigrate marks a PumpEvent holding a MigrateEvent.
	EventMigrate
)

// PumpEvent is any pump.fun event decoded from a `Program data:` log blob.
// Exactly one of Create / Trade / Migrate is populated, per Kind.
type PumpEvent struct {
	Kind    EventKind
	Create  CreateEvent
	Trade   TradeEvent
	Migrate MigrateEvent
}

// ParsePumpEvent routes a raw event blob (already base64-decoded) by its
// discriminator. Returns (zero, false) if unrecognised or undecodable.
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
		return PumpEvent{Kind: EventCreate, Create: ev}, true
	case TradeEventDisc:
		ev, ok := DecodeTradeEvent(data)
		if !ok {
			return PumpEvent{}, false
		}
		return PumpEvent{Kind: EventTrade, Trade: ev}, true
	case MigrateEventDisc:
		ev, ok := DecodeMigrateEvent(data)
		if !ok {
			return PumpEvent{}, false
		}
		return PumpEvent{Kind: EventMigrate, Migrate: ev}, true
	default:
		return PumpEvent{}, false
	}
}

// KindTag is the short tag used in the collector's JSONL `event_type`
// field: "create", "buy", "sell", or "migrate".
func (e PumpEvent) KindTag() string {
	switch e.Kind {
	case EventCreate:
		return "create"
	case EventTrade:
		if e.Trade.IsBuy {
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
// line into a PumpEvent. Non-pump / unrecognised blobs return (zero,
// false).
func ParseProgramDataB64(b64 string) (PumpEvent, bool) {
	bytes, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return PumpEvent{}, false
	}
	return ParsePumpEvent(bytes)
}

// ── little-endian read helpers ───────────────────────────────────────────────

func u64LE(d []byte, o int) uint64 {
	return binary.LittleEndian.Uint64(d[o : o+8])
}

func i64LE(d []byte, o int) int64 {
	return int64(binary.LittleEndian.Uint64(d[o : o+8]))
}

func discMatches(d []byte, want [8]byte) bool {
	for i := 0; i < 8; i++ {
		if d[i] != want[i] {
			return false
		}
	}
	return true
}

func pubkeyAt(d []byte, o int) (solana.Pubkey, bool) {
	if o+32 > len(d) {
		return solana.Pubkey{}, false
	}
	pk, err := solana.PubkeyFromBytes(d[o : o+32])
	if err != nil {
		return solana.Pubkey{}, false
	}
	return pk, true
}

// readString reads an anchor `String` (u32 little-endian length prefix +
// UTF-8 bytes), advancing *o. Invalid UTF-8 is preserved as-is via Go's
// native (lossy on print, but byte-preserving) string conversion — token
// names are arbitrary user input, matching the Rust
// String::from_utf8_lossy behavior closely enough for observational use.
func readString(d []byte, o *int) (string, bool) {
	if *o+4 > len(d) {
		return "", false
	}
	length := int(binary.LittleEndian.Uint32(d[*o : *o+4]))
	*o += 4
	if length < 0 || *o+length > len(d) {
		return "", false
	}
	s := string(d[*o : *o+length])
	*o += length
	return s, true
}
