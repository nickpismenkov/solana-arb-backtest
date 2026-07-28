// Package liquidation implements the marginfi v2 liquidation engine —
// Stage 1: read-only health finder.
//
// Decodes on-chain Bank and MarginfiAccount state, computes each borrower's
// maintenance health, and flags who is liquidatable (weighted assets <
// weighted liabilities). No money moves here — this is the finder that
// feeds the liquidate-tx builder (Stage 2, fire.go).
//
// Every byte offset below is VERIFIED: the Bank layout is checked
// field-by-field against real mainnet USDC-bank bytes (share values,
// weights, oracle setup all sane), and the MarginfiAccount/Balance layout is
// size-asserted against the marginfi-v2 source (Balance=104,
// MarginfiAccount=2312, Bank=1864) with the head (group@8, authority@40,
// lending_account@72) confirmed on-chain. WrappedI80F48 is a 16-byte LE i128
// fixed-point (value / 2^48), repr(C, align(8)).
package liquidation

import (
	"encoding/binary"
	"math"

	"github.com/gagliardetto/solana-go"
)

// I80F48ToF64 converts a WrappedI80F48 (16-byte LE i128, 48 fractional
// bits) to float64.
func I80F48ToF64(b []byte) float64 {
	if len(b) < 16 {
		return 0
	}
	lo := binary.LittleEndian.Uint64(b[0:8])
	hi := binary.LittleEndian.Uint64(b[8:16])
	// Reconstruct the i128 from its two u64 limbs, then interpret as signed.
	loF := float64(lo)
	hiSigned := int64(hi)
	// value = hi*2^64 + lo, but hi is the signed high limb.
	v := float64(hiSigned)*18446744073709551616.0 + loF
	return v / float64(uint64(1)<<48)
}

func readPubkey(data []byte, off int) solana.PublicKey {
	return solana.PublicKeyFromBytes(data[off : off+32])
}

// ── Bank (VERIFIED against mainnet bytes; total account size 1864) ─────────

// BankDisc is sha256("account:Bank")[..8].
var BankDisc = [8]byte{142, 49, 166, 242, 50, 66, 97, 188}

// EmodeEntry is one emode (elevation-mode) rule on a *liability* bank: when
// this bank is borrowed and the account holds collateral whose bank has
// CollateralTag, the collateral's asset weights are REPLACED by these
// boosted values. VERIFIED against USDC bank 2s37akK2 (tag 619 → maint
// 0.99, tag 871 → maint 0.92) which is exactly what flips ratio-1.30
// accounts healthy per marginfi.
type EmodeEntry struct {
	CollateralTag    uint16
	AssetWeightInit  float64
	AssetWeightMaint float64
}

// Bank is the risk parameters we need from a Bank to price a position's
// health.
type Bank struct {
	Mint                 solana.PublicKey
	MintDecimals         uint8
	AssetShareValue      float64
	LiabilityShareValue  float64
	AssetWeightInit      float64
	AssetWeightMaint     float64
	LiabilityWeightInit  float64
	LiabilityWeightMaint float64
	// OracleSetup enum: 1=PythLegacy, 2=SwitchboardV2, 3=PythPushOracle, …
	OracleSetup uint8
	// OracleKey: oracle_keys[0] — for Pyth Push this is the feed id /
	// PriceUpdateV2 ref.
	OracleKey solana.PublicKey
	// OracleMaxAge is BankConfig.oracle_max_age (seconds) — the chain rejects
	// a price older than this with a stale-oracle error. VERIFIED @800:
	// USDC=300, wSOL=70, BONK=120. 0 = use the program default. We mirror it
	// so the finder doesn't over-flag on prices the chain considers stale
	// (SwitchboardStalePrice 6049).
	OracleMaxAge uint16
	// EmodeTag is this bank's emode class (0 = not emode-eligible as
	// collateral). VERIFIED @920: Amtw3n7G→619, USDC/other stables→57481.
	EmodeTag uint16
	// EmodeEntries are the boosts this bank grants to collateral when
	// borrowed as a liability.
	EmodeEntries []EmodeEntry
}

// Emode layout in the Bank account (VERIFIED against mainnet USDC +
// collateral banks): the bank's own tag is a u16 @920; the boost-entry
// array starts @1264, each entry 40 bytes (collateral_tag u16 @0,
// asset_weight_init @8, asset_weight_maint @24). Unused/garbage slots are
// rejected by weight range.
const (
	bankEmodeTag     = 920
	bankEmodeEntries = 1264
	emodeEntrySize   = 40
	// bankOracleMaxAge is BankConfig.oracle_max_age (u16 seconds) — VERIFIED
	// @800 across banks (USDC=300, wSOL=70, BONK=120). Sits after
	// oracle_keys[5] + the borrow_limit/risk_tier/total_asset_value_init_limit
	// block.
	bankOracleMaxAge = 800
)

// DecodeBank decodes a marginfi Bank account. ok=false on a short buffer or
// wrong discriminator.
func DecodeBank(data []byte) (*Bank, bool) {
	if len(data) < 1864 || [8]byte(data[:8]) != BankDisc {
		return nil, false
	}
	// Emode entries: scan slots, keep only sane weight pairs (a real boost
	// has 0 < init ≤ maint ≤ 1.5; leftover/other-field bytes fail this).
	var emodeEntries []EmodeEntry
	for e := 0; bankEmodeEntries+(e+1)*emodeEntrySize <= len(data) && e < 16; e++ {
		base := bankEmodeEntries + e*emodeEntrySize
		tag := binary.LittleEndian.Uint16(data[base : base+2])
		init := I80F48ToF64(data[base+8:])
		maint := I80F48ToF64(data[base+24:])
		if tag != 0 && init > 0.0 && init <= maint && maint <= 1.5 {
			emodeEntries = append(emodeEntries, EmodeEntry{
				CollateralTag: tag, AssetWeightInit: init, AssetWeightMaint: maint,
			})
		}
	}
	return &Bank{
		Mint:         readPubkey(data, 8),
		MintDecimals: data[40],
		// group @41 (verified, unused here)
		AssetShareValue:      I80F48ToF64(data[80:]),
		LiabilityShareValue:  I80F48ToF64(data[96:]),
		AssetWeightInit:      I80F48ToF64(data[296:]), // BankConfig @296
		AssetWeightMaint:     I80F48ToF64(data[312:]),
		LiabilityWeightInit:  I80F48ToF64(data[328:]),
		LiabilityWeightMaint: I80F48ToF64(data[344:]),
		OracleSetup:          data[609],
		OracleKey:            readPubkey(data, 610),
		OracleMaxAge:         binary.LittleEndian.Uint16(data[bankOracleMaxAge : bankOracleMaxAge+2]),
		EmodeTag:             binary.LittleEndian.Uint16(data[bankEmodeTag : bankEmodeTag+2]),
		EmodeEntries:         emodeEntries,
	}, true
}

// emodeBoost returns the boosted maint weight this liability bank grants to
// collateral of tag, if any. Used by the emode intersection rule.
func (b *Bank) emodeBoost(tag uint16) (float64, bool) {
	for _, e := range b.EmodeEntries {
		if e.CollateralTag == tag {
			return e.AssetWeightMaint, true
		}
	}
	return 0, false
}

// EffectiveAssetWeightMaint is the effective maintenance asset weight for
// one collateral bank, applying marginfi's emode rule: emode boosts the
// collateral's weight ONLY if the collateral is emode-tagged AND *every*
// liability bank the account borrows grants a boost for that tag
// (intersection); then the boost is the min across those liabilities (most
// conservative). Otherwise the base maint weight. Falls back to base weight
// whenever emode doesn't cleanly apply, so we over-flag rather than
// under-flag (the sim gate is the fire backstop).
func EffectiveAssetWeightMaint(collateral *Bank, liabilityBanks []*Bank) float64 {
	base := collateral.AssetWeightMaint
	if collateral.EmodeTag == 0 || len(liabilityBanks) == 0 {
		return base
	}
	boost := math.Inf(1)
	for _, l := range liabilityBanks {
		w, ok := l.emodeBoost(collateral.EmodeTag)
		if !ok {
			return base // a borrowed liability doesn't grant emode → no boost
		}
		if w < boost {
			boost = w
		}
	}
	// Emode replaces the base weight; guard against a pathological lower value.
	if !math.IsInf(boost, 0) {
		if boost > base {
			return boost
		}
		return base
	}
	return base
}

// ── MarginfiAccount / Balance (size-asserted; head verified on-chain) ──────

// MarginfiAccountDisc is sha256("account:MarginfiAccount")[..8].
var MarginfiAccountDisc = [8]byte{67, 178, 130, 109, 126, 114, 28, 42}

const (
	MASize         = 2312
	maGroup        = 8
	maAuthority    = 40
	lendingAccount = 72 // balances[0] start
	balanceSize    = 104
	maxBalances    = 16
	balActive      = 0
	balBankPk      = 1
	balAssetShares = 40
	balLiabShares  = 56
)

// Balance is one active position slot on a MarginfiAccount (raw shares, not
// yet priced).
type Balance struct {
	BankPk          solana.PublicKey
	AssetShares     float64
	LiabilityShares float64
}

// MarginfiAccount is a decoded borrower: authority + its active balances.
type MarginfiAccount struct {
	Group     solana.PublicKey
	Authority solana.PublicKey
	Balances  []Balance
}

// DecodeMarginfiAccount decodes balances from account data. Accepts either
// the full 2312-byte account or a leading dataSlice that covers all
// balances (>= 1736 bytes).
func DecodeMarginfiAccount(data []byte) (*MarginfiAccount, bool) {
	if len(data) < lendingAccount+maxBalances*balanceSize || [8]byte(data[:8]) != MarginfiAccountDisc {
		return nil, false
	}
	var balances []Balance
	for i := 0; i < maxBalances; i++ {
		base := lendingAccount + i*balanceSize
		// active flag is a u8 bool; skip empty slots.
		if data[base+balActive] == 0 {
			continue
		}
		assetShares := I80F48ToF64(data[base+balAssetShares:])
		liabilityShares := I80F48ToF64(data[base+balLiabShares:])
		if assetShares == 0.0 && liabilityShares == 0.0 {
			continue
		}
		balances = append(balances, Balance{
			BankPk:          readPubkey(data, base+balBankPk),
			AssetShares:     assetShares,
			LiabilityShares: liabilityShares,
		})
	}
	return &MarginfiAccount{
		Group:     readPubkey(data, maGroup),
		Authority: readPubkey(data, maAuthority),
		Balances:  balances,
	}, true
}

// ActiveBankPks returns bank pubkeys of ALL active-flag balances, in slot
// order — INCLUDING zero-share ones. marginfi's liquidate health-check
// requires an oracle for every active balance (not just the funded ones),
// so the observation list must cover all of these or it fails
// WrongNumberOfOracleAccounts (6051). DecodeMarginfiAccount drops
// zero-share balances (fine for health/selection, wrong for the obs list).
func ActiveBankPks(data []byte) []solana.PublicKey {
	var v []solana.PublicKey
	if len(data) < lendingAccount+maxBalances*balanceSize {
		return v
	}
	for i := 0; i < maxBalances; i++ {
		base := lendingAccount + i*balanceSize
		if data[base+balActive] == 0 {
			continue
		}
		v = append(v, readPubkey(data, base+balBankPk))
	}
	return v
}

// ── Pyth PriceUpdateV2 (on-chain pull oracle — what marginfi reads) ────────
// disc(8) · write_authority(32)@8 · verification_level@40 · price_message · …
// verification_level is a Borsh enum: tag@40 is 1 for Full (1 byte total) or
// 0 for Partial (2 bytes: tag + num_signatures). So price_message starts at
// 41 (Full) or 42 (Partial). VERIFIED against the USDC oracle (Full →
// $0.9998). price_message: feed_id(32) · price:i64 · conf:u64 ·
// exponent:i32 · publish_time:i64.

// PriceUpdateV2Disc is sha256("account:PriceUpdateV2")[..8].
var PriceUpdateV2Disc = [8]byte{34, 241, 35, 99, 157, 126, 244, 205}

// DecodePriceUpdateV2 decodes a Pyth PriceUpdateV2 account → (feed_id,
// usd_price, publish_time). Price is scaled by its exponent to whole-token
// USD.
func DecodePriceUpdateV2(data []byte) (feedID [32]byte, usdPrice float64, publishTime int64, ok bool) {
	if len(data) < 134 || [8]byte(data[:8]) != PriceUpdateV2Disc {
		return feedID, 0, 0, false
	}
	// Branch on verification_level tag to locate price_message.
	var pm int
	switch data[40] {
	case 1:
		pm = 41 // Full
	case 0:
		pm = 42 // Partial (tag + num_signatures)
	default:
		return feedID, 0, 0, false
	}
	if len(data) < pm+60 {
		return feedID, 0, 0, false
	}
	copy(feedID[:], data[pm:pm+32])
	price := int64(binary.LittleEndian.Uint64(data[pm+32 : pm+40]))
	exponent := int32(binary.LittleEndian.Uint32(data[pm+48 : pm+52]))
	publishTime = int64(binary.LittleEndian.Uint64(data[pm+52 : pm+60]))
	usd := float64(price) * math.Pow(10, float64(exponent))
	return feedID, usd, publishTime, true
}

// Switchboard On-Demand PullFeed (owner SBondMDrcV3K…, 3208 bytes). marginfi
// oracle_setup 4 (SwitchboardPull) and 7 use these. The feed result is an
// i128 fixed-point (÷ 1e18) at offset 56 — VERIFIED on-chain: SOL feed reads
// $80.98, a USDS feed $0.9999. disc = sha256("account:PullFeedAccountData")[..8].

// SwitchboardPullDisc is sha256("account:PullFeedAccountData")[..8].
var SwitchboardPullDisc = [8]byte{0xc4, 0x1b, 0x6c, 0xc4, 0x0a, 0xd7, 0xdb, 0x28}

const sbPullResult = 56

// DecodeSwitchboardPull decodes the USD price from a Switchboard On-Demand
// PullFeed account.
func DecodeSwitchboardPull(data []byte) (float64, bool) {
	if len(data) < sbPullResult+16 || [8]byte(data[:8]) != SwitchboardPullDisc {
		return 0, false
	}
	lo := binary.LittleEndian.Uint64(data[sbPullResult : sbPullResult+8])
	hi := int64(binary.LittleEndian.Uint64(data[sbPullResult+8 : sbPullResult+16]))
	v := float64(hi)*18446744073709551616.0 + float64(lo)
	usd := v / 1e18
	if math.IsInf(usd, 0) || math.IsNaN(usd) || usd <= 0.0 {
		return 0, false
	}
	return usd, true
}

// sbPullResultSlot is the Solana slot the current Switchboard result was
// produced at, offset 40 in the PullFeedAccountData (VERIFIED empirically
// across all 1508 live feeds 2026-07-14: fresh feeds read ~350 slots behind
// head, dead feeds read 0/behind millions). marginfi gates liquidation on
// this via the bank's oracle_max_age; a result too far behind head reverts
// the liquidate ix with SwitchboardStalePrice (6049) — which is exactly
// what made ~6.3k accounts show as "liquidatable" to a finder that read the
// price but not its age.
const sbPullResultSlot = 40

// DecodeSwitchboardPullSlot decodes the result slot of a Switchboard
// On-Demand PullFeed account.
func DecodeSwitchboardPullSlot(data []byte) (uint64, bool) {
	if len(data) < sbPullResultSlot+8 || [8]byte(data[:8]) != SwitchboardPullDisc {
		return 0, false
	}
	return binary.LittleEndian.Uint64(data[sbPullResultSlot : sbPullResultSlot+8]), true
}

// DefaultMaxSBStaleSlots is the fallback staleness ceiling (in slots) for a
// Switchboard oracle whose bank reports oracle_max_age = 0 (meaning "use
// the program default"). Generous by design — see the asymmetry note on
// MaxStaleSlotsFor. Override with MAX_SB_STALE_SLOTS.
const DefaultMaxSBStaleSlots uint64 = 5000

// SlotsPerSec is the approximate Solana slot rate. Real cadence is
// ~2.3–2.5 slots/s; using the high end makes the slot budget slightly
// LARGER (more lenient), which is the safe direction here.
const SlotsPerSec float64 = 2.5

// StaleSafetyFactor is extra leniency over the chain's own threshold. The
// failure asymmetry is one-sided: gating TIGHTER than the chain would make
// us SKIP an account the chain would accept (missed money); gating LOOSER
// only lets a stale account reach the sim gate, which rejects it (6049) and
// the caller backs it off. So we deliberately allow ~2× the chain's max-age
// before filtering — tight enough to kill the over-flagging (the old fixed
// 5000 was ~30× the chain's wSOL max-age), loose enough that a slot-rate
// mis-estimate never costs a fire.
const StaleSafetyFactor float64 = 2.0

// MaxStaleSlotsFor is the per-bank Switchboard staleness ceiling in SLOTS,
// derived from the bank's on-chain oracle_max_age (seconds) so we mirror
// exactly what the program will accept — plus the safety factor.
// oracleMaxAgeSecs == 0 → program default → the generous fallback
// (env-overridable by the caller). Matching the chain's own threshold is
// what guarantees we never skip a genuinely fireable account: a real
// liquidation REQUIRES the chain to accept the price, which requires it
// within max_age, which is inside this (larger) budget.
func MaxStaleSlotsFor(oracleMaxAgeSecs uint16, defaultSlots uint64) uint64 {
	if oracleMaxAgeSecs == 0 {
		return defaultSlots
	}
	return uint64(float64(oracleMaxAgeSecs) * SlotsPerSec * StaleSafetyFactor)
}

// DecodeOraclePriceFresh returns the USD price from an oracle, treating a
// Switchboard feed whose result slot is more than maxStaleSlots behind
// currentSlot as UNAVAILABLE (ok=false → the health calc counts it missing
// and never trusts the account). Pyth feeds pass through unchanged (Pyth
// staleness is handled at the sponsored-feed/crank layer). currentSlot == 0
// disables the gate (falls back to price-only).
func DecodeOraclePriceFresh(data []byte, currentSlot, maxStaleSlots uint64) (float64, bool) {
	if _, usd, _, ok := DecodePriceUpdateV2(data); ok {
		return usd, true
	}
	usd, ok := DecodeSwitchboardPull(data)
	if !ok {
		return 0, false
	}
	if currentSlot > 0 {
		slot, ok := DecodeSwitchboardPullSlot(data)
		if !ok {
			return 0, false
		}
		var behind uint64
		if currentSlot > slot {
			behind = currentSlot - slot
		}
		if behind > maxStaleSlots {
			return 0, false // stale — the chain would revert with 6049
		}
	}
	return usd, true
}

// DecodeOraclePrice returns the USD price from any oracle account we can
// decode, dispatching on disc: Pyth PriceUpdateV2 (setups 1/3/5/6) or
// Switchboard On-Demand PullFeed (setups 4/7). Staked-SOL setups (5)
// resolve to the SOL Pyth feed, which under-values the LST slightly — safe
// here because the finder only builds the watch-set; the fire decision is
// gated by full on-chain simulation, which uses marginfi's exact pricing.
func DecodeOraclePrice(data []byte) (float64, bool) {
	if _, usd, _, ok := DecodePriceUpdateV2(data); ok {
		return usd, true
	}
	return DecodeSwitchboardPull(data)
}

// ── Health ───────────────────────────────────────────────────────────────

// PriceMap is the USD price of one whole token for a bank's mint (mid;
// confidence bounds are a later refinement). Keyed by bank pubkey.
type PriceMap map[solana.PublicKey]float64

// BankMap is bank pubkey -> Bank.
type BankMap map[solana.PublicKey]*Bank

// Health holds an account's weighted-asset/liability totals.
type Health struct {
	WeightedAssets      float64
	WeightedLiabilities float64
}

// Value is the maintenance health value. Liquidatable when < 0.
func (h Health) Value() float64 {
	return h.WeightedAssets - h.WeightedLiabilities
}

func (h Health) Liquidatable() bool {
	return h.Value() < 0.0
}

// Ratio is weighted_liabs / weighted_assets — ratio >= 1.0 means underwater.
func (h Health) Ratio() float64 {
	if h.WeightedAssets == 0.0 {
		return math.Inf(1)
	}
	return h.WeightedLiabilities / h.WeightedAssets
}

// HealthResult is the computed maintenance-weighted health for an account.
// Missing counts balances whose bank or price we couldn't resolve — if it's
// > 0 the health is INCOMPLETE and must NOT be trusted for a liquidation
// decision (skipping an unpriced collateral bank makes a healthy account
// look underwater).
type HealthResult struct {
	Health  Health
	Missing int
}

// MaintenanceHealth computes maintenance-weighted health for an account.
func MaintenanceHealth(acct *MarginfiAccount, banks BankMap, prices PriceMap) HealthResult {
	// Liability banks of this account (needed for the emode intersection rule).
	var liabBanks []*Bank
	for _, b := range acct.Balances {
		if b.LiabilityShares > 0.0 {
			if bank, ok := banks[b.BankPk]; ok {
				liabBanks = append(liabBanks, bank)
			}
		}
	}
	var wa, wl float64
	missing := 0
	for _, b := range acct.Balances {
		bank, bankOK := banks[b.BankPk]
		price, priceOK := prices[b.BankPk]
		if !bankOK || !priceOK {
			missing++
			continue
		}
		scale := math.Pow(10, float64(bank.MintDecimals))
		if b.AssetShares > 0.0 {
			ui := b.AssetShares * bank.AssetShareValue / scale
			// Emode: collateral weight may be boosted by the borrowed liabilities.
			w := EffectiveAssetWeightMaint(bank, liabBanks)
			wa += ui * price * w
		}
		if b.LiabilityShares > 0.0 {
			ui := b.LiabilityShares * bank.LiabilityShareValue / scale
			wl += ui * price * bank.LiabilityWeightMaint
		}
	}
	return HealthResult{Health: Health{WeightedAssets: wa, WeightedLiabilities: wl}, Missing: missing}
}
