// Command liq_executor is the production marginfi liquidation executor —
// continuous loop, DRY_RUN default.
//
// Detection is simulation-gated (the emode lesson: don't replicate marginfi's
// risk math off-chain — let the chain judge). Pipeline per candidate:
//
//	full scan (RESCAN_SECS) -> watch-set of near-liquidation borrowers
//	fast poll (POLL_MS): fresh watch-set accounts + bank/oracle prices
//	base-weight liquidatable? -> sim-gate [start_fl, liquidate, end_fl]
//	-> SIZE the seize by simulation ladder (largest passing fraction)
//	-> build the atomic fire tx (liquidate->withdraw->Jupiter swap->repay_all)
//	-> profit gate (quoted USDC out vs ~97.5% liability taken + tip)
//	-> FULL fire-tx simulation (ground truth for every leg incl. swap+repay)
//	-> DRY_RUN: log; LIVE: sign + submit via Helius Sender, readback P&L
//
// ── Self-crank mode (the stale-oracle edge) ─────────────────────────────
// marginfi's Pyth feeds lag the true price by 8-44s. When an account is
// underwater at the TRUE (Lazer-blended) price but still healthy at on-chain
// prices, the Sender path can't fire — the chain would judge it healthy. If
// the asset bank's oracle is a shard-0 sponsored feed (permissionless crank),
// we instead fire an atomic Jito bundle:
//
//	[crank_setup, crank_fire (posts the fresh Hermes price), liquidate]
//
// Sizing + ground truth for these run through simulateBundle so the chain
// judges AT the cranked price. The Hermes blob is kept hot by a background
// poll; crank txs are rebuilt from the freshest blob at fire time. The bundle
// is all-or-nothing: a losing fire never lands, pays nothing.
//
// Usage: HELIUS_RPC=<url> [DRY_RUN=1] [KEYPAIR_PATH=~/arb-keypair.json]
//
//	[PYTH_LAZER_TOKEN=... (required for the crank edge)] [CRANK=1]
//	[MIN_COLLATERAL_USD=100] [MIN_PROFIT_USD=0.5] [TIP_FRACTION_BPS=3000]
//	[POLL_MS=5000] [RESCAN_SECS=300] [WATCH_RATIO=0.85] [RUN_DIR=runs]
//	[MAX_BLOB_AGE_MS=3000] [JITO_BLOCK_ENGINE=...]
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"sync"
	"time"

	"arbengine/internal/config"
	"arbengine/internal/jito"
	"arbengine/internal/lazer"
	liqengine "arbengine/internal/liqengine"
	liqfire "arbengine/internal/liqfire"
	liq "arbengine/internal/liquidation"
	"arbengine/internal/marginfi"
	"arbengine/internal/observe"
	"arbengine/internal/pyth"
	"arbengine/internal/pythaccumulator"
	"arbengine/internal/pythcrank"
	"arbengine/internal/rpcclient"
	"arbengine/internal/solana"
)

const marginfiProgram = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA"
const marginfiGroup = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8"
const solMint = "So11111111111111111111111111111111111111112"
const usdtMint = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"
const defaultLiquidatorMA = "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD"
const defaultAuthority = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak"

// isDebtMint reports whether mint is a debt (liability) asset the fire
// path can repay: USDC, USDT, wSOL. The liquidator absorbs the
// liquidatee's liability and repays it by swapping the seized collateral
// into this asset — so it must be a mint Jupiter/direct-DEX routes liquidly
// and the marginfi flashloan can repay.
func isDebtMint(mint solana.Pubkey) bool {
	m := mint.String()
	return m == marginfi.USDCMint || m == usdtMint || m == solMint
}

// sizeLadder: largest -> smallest. Bigger seize = more profit; marginfi
// rejects over-liquidation (post-liq health must stay <= 0), so walk down
// until one passes.
var sizeLadder = []float64{1.0, 0.5, 0.25, 0.1, 0.02}

func nowSecs() uint64   { return uint64(time.Now().Unix()) }
func nowMicros() uint64 { return uint64(time.Now().UnixMicro()) }

// logLatency is the latency ledger: proves whether SPEED is the
// bottleneck. appearedUs is the Lazer PUBLISH timestamp of the price that
// made the account cross (the moment the opportunity truly exists); the
// deltas measure how long WE take from that instant to detect and to
// submit. Appended to {run_dir}/latency.jsonl.
func logLatency(runDir string, v map[string]any) {
	_ = os.MkdirAll(runDir, 0o755)
	f, err := os.OpenFile(runDir+"/latency.jsonl", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	f.Write(b)
	f.Write([]byte("\n"))
}

func rpcCallRetry(endpoint string, method string, params any, attempts int) (map[string]any, bool) {
	c := rpcclient.New(endpoint)
	for attempt := 0; attempt < attempts; attempt++ {
		raw, err := c.Call(method, params)
		if err == nil {
			var v map[string]any
			if json.Unmarshal(raw, &v) == nil {
				return v, true
			}
		}
		time.Sleep(time.Duration(400<<uint(attempt)) * time.Millisecond)
	}
	return nil, false
}

func getMultiple(endpoint string, keys []solana.Pubkey) map[solana.Pubkey][]byte {
	out := make(map[solana.Pubkey][]byte)
	if len(keys) == 0 {
		return out
	}
	c := rpcclient.New(endpoint)
	for i := 0; i < len(keys); i += 100 {
		end := i + 100
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[i:end]
		infos, err := c.GetMultipleAccounts(chunk)
		if err != nil {
			continue
		}
		for j, info := range infos {
			if info != nil {
				out[chunk[j]] = info.Data
			}
		}
	}
	return out
}

func mintOwner(endpoint string, mint solana.Pubkey) (solana.Pubkey, bool) {
	info, err := rpcclient.New(endpoint).GetAccountInfo(mint)
	if err != nil || info == nil {
		return solana.Pubkey{}, false
	}
	pk, err := solana.PubkeyFromBase58(info.Owner)
	if err != nil {
		return solana.Pubkey{}, false
	}
	return pk, true
}

func latestBlockhash(endpoint string) (solana.Hash, bool) {
	h, err := rpcclient.New(endpoint).GetLatestBlockhash()
	if err != nil {
		return solana.Hash{}, false
	}
	return h, true
}

func solBalance(endpoint, owner string) float64 {
	pk, err := solana.PubkeyFromBase58(owner)
	if err != nil {
		return 0
	}
	lamports, err := rpcclient.New(endpoint).GetBalance(pk)
	if err != nil {
		return 0
	}
	return float64(lamports) / 1e9
}

func currentSlot(endpoint string) uint64 {
	slot, err := rpcclient.New(endpoint).GetSlot()
	if err != nil {
		return 0
	}
	return slot
}

func simulateTxB64(endpoint, b64tx string) (map[string]any, bool) {
	raw, err := rpcclient.New(endpoint).SimulateTransaction(b64tx)
	if err != nil || raw == nil {
		return nil, false
	}
	var v map[string]any
	if json.Unmarshal(raw, &v) != nil {
		return nil, false
	}
	return v, true
}

// gateTxB64 builds the sizing-gate tx [start_fl, liquidate(asset_amount),
// end_fl] as base64 — simulated standalone (Sender path) or as the tail of
// a crank bundle.
func gateTxB64(
	authority, liquidatorMA, tp, liquidatee solana.Pubkey, acct *liq.MarginfiAccount,
	assetBank, liabBank solana.Pubkey, assetAmount uint64, oracleOf map[solana.Pubkey]solana.Pubkey,
) (string, bool) {
	var liquidateeObs []solana.AccountMeta
	for _, b := range acct.Balances {
		oc, ok := oracleOf[b.BankPk]
		if !ok {
			return "", false
		}
		liquidateeObs = append(liquidateeObs, solana.ReadonlyMeta(b.BankPk), solana.ReadonlyMeta(oc))
	}
	start := marginfi.StartFlashloan(liquidatorMA, authority, 2)
	assetOracle, ok := oracleOf[assetBank]
	if !ok {
		return "", false
	}
	liabOracle, ok := oracleOf[liabBank]
	if !ok {
		return "", false
	}
	liqIx := marginfi.LendingAccountLiquidate(assetBank, liabBank, liquidatorMA, authority, liquidatee, tp,
		assetAmount, assetOracle, liabOracle, liquidateeObs)
	endObs := []solana.AccountMeta{
		solana.ReadonlyMeta(assetBank), solana.ReadonlyMeta(assetOracle),
		solana.ReadonlyMeta(liabBank), solana.ReadonlyMeta(liabOracle),
	}
	end := marginfi.EndFlashloan(liquidatorMA, authority, endObs)
	msg, err := solana.CompileV0(authority, []solana.Instruction{start, liqIx, end}, nil, solana.Hash{})
	if err != nil {
		return "", false
	}
	tx := solana.NewUnsignedVersionedTransaction(msg)
	b, err := tx.Base64()
	if err != nil {
		return "", false
	}
	return b, true
}

// gateSimKind mirrors Rust's GateSim enum: Fireable / Reverted(code) / Unusable.
type gateSimKind int

const (
	gateFireable gateSimKind = iota
	gateReverted
	gateUnusable
)

type gateSim struct {
	kind     gateSimKind
	failCode *uint32
}

// revertReason gives a human-readable reason for a revert code, for the
// decision ledger. Keeps the finder's steady state honest — the old code
// logged every revert as "healthy".
func revertReason(code *uint32) string {
	if code == nil {
		return "liquidate reverted (no custom code)"
	}
	switch *code {
	case 6068:
		return "chain says healthy at the actionable price (not truly liquidatable)"
	case 6049:
		return "collateral oracle stale on-chain (SwitchboardStalePrice) — not actionable"
	case 6009:
		return "risk engine rejected: bad health or stale oracle"
	case 6012:
		return "liquidation amount rounded to zero (position too small)"
	case 6210:
		return "Kamino-integrated collateral: reserve validation failed"
	default:
		return fmt.Sprintf("liquidate reverted with marginfi error %d", *code)
	}
}

type crankB64Pair struct {
	setup string
	fire  string
}

func simulateGate(
	endpoint string, authority, liquidatorMA, tp, liquidatee solana.Pubkey, acct *liq.MarginfiAccount,
	assetBank, liabBank solana.Pubkey, assetAmount uint64, oracleOf map[solana.Pubkey]solana.Pubkey,
	crankB64 *crankB64Pair,
) gateSim {
	gate, ok := gateTxB64(authority, liquidatorMA, tp, liquidatee, acct, assetBank, liabBank, assetAmount, oracleOf)
	if !ok {
		return gateSim{kind: gateUnusable}
	}
	if crankB64 != nil {
		sim, ok := simulateBundle(endpoint, []string{crankB64.setup, crankB64.fire, gate})
		if !ok {
			return gateSim{kind: gateUnusable}
		}
		if sim.ranOk == 3 {
			return gateSim{kind: gateFireable}
		}
		// Crank txs must not be the failure — that's a broken crank, not a
		// healthy account; surface as Unusable so the caller doesn't cool down.
		if sim.ranOk < 2 {
			return gateSim{kind: gateUnusable}
		}
		return gateSim{kind: gateReverted, failCode: sim.failCode}
	}
	res, ok := simulateTxB64(endpoint, gate)
	if !ok {
		return gateSim{kind: gateUnusable}
	}
	errv, hasErr := res["err"]
	if !hasErr || errv == nil {
		return gateSim{kind: gateFireable}
	}
	code := extractCustomCode(errv)
	return gateSim{kind: gateReverted, failCode: code}
}

func extractCustomCode(errv any) *uint32 {
	m, ok := errv.(map[string]any)
	if !ok {
		return nil
	}
	ie, ok := m["InstructionError"].([]any)
	if !ok || len(ie) < 2 {
		return nil
	}
	cm, ok := ie[1].(map[string]any)
	if !ok {
		return nil
	}
	cv, ok := cm["Custom"]
	if !ok {
		return nil
	}
	switch n := cv.(type) {
	case float64:
		c := uint32(n)
		return &c
	}
	return nil
}

type decisionLog struct {
	T             uint64  `json:"t"`
	Liquidatee    string  `json:"liquidatee"`
	Mode          string  `json:"mode"`
	CollateralUsd float64 `json:"collateral_usd"`
	Ratio         float64 `json:"ratio"`
	SeizeNative   uint64  `json:"seize_native"`
	QuotedUsdcOut float64 `json:"quoted_usdc_out"`
	EstLiabUsdc   float64 `json:"est_liab_usdc"`
	EstProfitUsdc float64 `json:"est_profit_usdc"`
	FireSimOk     bool    `json:"fire_sim_ok"`
	Fired         bool    `json:"fired"`
	Reason        string  `json:"reason"`
}

type tradeLog struct {
	T             uint64   `json:"t"`
	Liquidatee    string   `json:"liquidatee"`
	SeizeNative   uint64   `json:"seize_native"`
	EstProfitUsdc float64  `json:"est_profit_usdc"`
	TipLamports   uint64   `json:"tip_lamports"`
	Signature     *string  `json:"signature"`
	Bundle        *string  `json:"bundle"`
	RealizedUsdc  *float64 `json:"realized_usdc"`
	Error         *string  `json:"error"`
}

// fireModeKind mirrors Rust's FireMode enum.
type fireModeKind int

const (
	fireSender fireModeKind = iota
	fireCrank
)

type fireMode struct {
	kind   fireModeKind
	feedID [32]byte // only meaningful when kind == fireCrank
}

func (m fireMode) name() string {
	if m.kind == fireCrank {
		return "crank"
	}
	return "sender"
}

// isV1Fireable is true if the fire path can act on at least one LEG of this
// account: it has a collateral position AND a liability whose bank is a
// supported debt asset (USDC/USDT/wSOL). Covers both single- and
// multi-position accounts — the fire path picks the best (collateral,
// debt) leg (see fireableLegs/tryArm). Accounts with no wired-debt leg are
// still skipped (no liquid swap route), so the watch-set/engine won't
// track or rank them.
func isV1Fireable(a *liq.MarginfiAccount, banks liq.BankMap) bool {
	hasCollateral := false
	for _, b := range a.Balances {
		if b.AssetShares > 0.0 {
			hasCollateral = true
			break
		}
	}
	if !hasCollateral {
		return false
	}
	for _, b := range a.Balances {
		if b.LiabilityShares > 0.0 {
			if bk, ok := banks[b.BankPk]; ok && isDebtMint(bk.Mint) {
				return true
			}
		}
	}
	return false
}

// crankCtx holds everything the crank path needs, spun up once at boot.
type crankCtx struct {
	on          bool
	hermes      *pythaccumulator.HermesCache
	tips        []solana.Pubkey
	blockEngine string
	maxBlobAge  time.Duration
}

func (c *crankCtx) pickTip() (solana.Pubkey, bool) {
	if len(c.tips) == 0 {
		return solana.Pubkey{}, false
	}
	return c.tips[nowSecs()%uint64(len(c.tips))], true
}

type scan struct {
	accts     []acctEntry
	banks     liq.BankMap
	oracleOf  map[solana.Pubkey]solana.Pubkey
	feedOf    map[solana.Pubkey][32]byte
	crankable map[solana.Pubkey]struct{}
}

type acctEntry struct {
	pk  solana.Pubkey
	acc liq.MarginfiAccount
}

func fullScan(endpoint string) (*scan, bool) {
	c := rpcclient.New(endpoint)
	prog := solana.MustPubkeyFromBase58(marginfiProgram)
	groupBytes := solana.MustPubkeyFromBase58(marginfiGroup).Bytes()
	opts := rpcclient.GetProgramAccountsOpts{
		Filters: []any{
			map[string]any{"dataSize": liq.MASize},
			map[string]any{"memcmp": map[string]any{"offset": 8, "bytes": b58EncodeForFilter(groupBytes)}},
		},
		DataSlice: &struct {
			Offset int `json:"offset"`
			Length int `json:"length"`
		}{Offset: 0, Length: 1736},
	}
	entries, err := c.GetProgramAccounts(prog, opts)
	if err != nil {
		return nil, false
	}
	var accts []acctEntry
	for _, e := range entries {
		acc, ok := liq.DecodeMarginfiAccount(e.Account.Data)
		if !ok {
			continue
		}
		hasLiab := false
		for _, b := range acc.Balances {
			if b.LiabilityShares > 0.0 {
				hasLiab = true
				break
			}
		}
		if hasLiab {
			accts = append(accts, acctEntry{pk: e.Pubkey, acc: *acc})
		}
	}

	bankSet := map[solana.Pubkey]struct{}{}
	for _, a := range accts {
		for _, b := range a.acc.Balances {
			bankSet[b.BankPk] = struct{}{}
		}
	}
	bankPks := make([]solana.Pubkey, 0, len(bankSet))
	for pk := range bankSet {
		bankPks = append(bankPks, pk)
	}
	banks := make(liq.BankMap)
	oracleOf := make(map[solana.Pubkey]solana.Pubkey)
	for pk, raw := range getMultiple(endpoint, bankPks) {
		if bk, ok := liq.DecodeBank(raw); ok {
			oracleOf[pk] = bk.OracleKey
			banks[pk] = bk
		}
	}

	// Crank metadata: decode each oracle's feed id and check whether the
	// oracle is the shard-0 sponsored PDA for that feed (-> crankable).
	oracleSet := map[solana.Pubkey]struct{}{}
	for _, oc := range oracleOf {
		oracleSet[oc] = struct{}{}
	}
	oraclePks := make([]solana.Pubkey, 0, len(oracleSet))
	for pk := range oracleSet {
		oraclePks = append(oraclePks, pk)
	}
	oracleRaw := getMultiple(endpoint, oraclePks)
	feedOf := make(map[solana.Pubkey][32]byte)
	crankable := make(map[solana.Pubkey]struct{})
	for bank, oracle := range oracleOf {
		raw, ok := oracleRaw[oracle]
		if !ok {
			continue
		}
		fid, _, _, ok := liq.DecodePriceUpdateV2(raw)
		if !ok {
			continue
		}
		feedOf[bank] = fid
		if pythcrank.SponsoredFeed(0, fid) == oracle {
			crankable[bank] = struct{}{}
		}
	}
	return &scan{accts: accts, banks: banks, oracleOf: oracleOf, feedOf: feedOf, crankable: crankable}, true
}

// b58EncodeForFilter base58-encodes raw bytes for a getProgramAccounts
// memcmp filter (the JSON-RPC filter value, not a Pubkey string method).
func b58EncodeForFilter(b []byte) string {
	pk, err := solana.PubkeyFromBytes(b)
	if err != nil {
		return ""
	}
	return pk.String()
}

// ── simulateBundle plumbing (crank mode judges through the fresh price) ────

// bundleSim reports how many leading txs in the bundle succeeded.
// jito-solana stops at the first failing tx, so ranOk < n means tx[ranOk]
// reverted. For the crank bundle [setup, fire, gate], ranOk == 3 =
// accepted; 2 = crank landed but the liquidate reverted; < 2 = the crank
// itself failed.
type bundleSim struct {
	ranOk    int
	failCode *uint32
}

func simulateBundle(endpoint string, txsB64 []string) (bundleSim, bool) {
	nulls := make([]any, len(txsB64))
	params := []any{
		map[string]any{"encodedTransactions": txsB64},
		map[string]any{
			"skipSigVerify":                true,
			"replaceRecentBlockhash":       true,
			"preExecutionAccountsConfigs":  nulls,
			"postExecutionAccountsConfigs": nulls,
		},
	}
	v, ok := rpcCallRetry(endpoint, "simulateBundle", params, 1)
	if !ok {
		return bundleSim{}, false
	}
	if e, has := v["error"]; has && e != nil {
		return bundleSim{}, false
	}
	result, _ := v["result"].(map[string]any)
	value, _ := result["value"].(map[string]any)
	results, _ := value["transactionResults"].([]any)
	ranOk := 0
	for _, r := range results {
		rm, ok := r.(map[string]any)
		if !ok {
			break
		}
		if rm["err"] != nil {
			break
		}
		ranOk++
	}
	var failCode *uint32
	if ranOk < len(results) {
		if rm, ok := results[ranOk].(map[string]any); ok {
			failCode = extractCustomCode(rm["err"])
		}
	}
	return bundleSim{ranOk: ranOk, failCode: failCode}, true
}

func freshPrices(endpoint string, banks liq.BankMap, oracleOf map[solana.Pubkey]solana.Pubkey) liq.PriceMap {
	// A stale Switchboard oracle is dropped here (see
	// liq.DecodeOraclePriceFresh): the account then reads as `missing` and
	// is never trusted as liquidatable, matching the chain's
	// SwitchboardStalePrice(6049) gate. The staleness ceiling is PER BANK,
	// from its on-chain oracle_max_age (x2 safety) — so we filter exactly
	// what the chain would reject, no more.
	slot := currentSlot(endpoint)
	defaultStale := liq.DefaultMaxSBStaleSlots
	if v := config.EnvUint64("MAX_SB_STALE_SLOTS", 0); v != 0 {
		defaultStale = v
	}
	oracleSet := map[solana.Pubkey]struct{}{}
	for _, oc := range oracleOf {
		oracleSet[oc] = struct{}{}
	}
	oraclePks := make([]solana.Pubkey, 0, len(oracleSet))
	for pk := range oracleSet {
		oraclePks = append(oraclePks, pk)
	}
	raw := getMultiple(endpoint, oraclePks)
	out := make(liq.PriceMap)
	for bankPk, oraclePk := range oracleOf {
		var maxAge uint16
		if b, ok := banks[bankPk]; ok {
			maxAge = b.OracleMaxAge
		}
		maxStale := liq.MaxStaleSlotsFor(maxAge, defaultStale)
		rd, ok := raw[oraclePk]
		if !ok {
			continue
		}
		usd, ok := liq.DecodeOraclePriceFresh(rd, slot, maxStale)
		if !ok {
			continue
		}
		out[bankPk] = usd
	}
	return out
}

// cfgBundle is a copy-able config bundle for the arm/fire helpers.
type cfgBundle struct {
	liquidatorMA   solana.Pubkey
	authority      solana.Pubkey
	tp             solana.Pubkey
	tipAccount     solana.Pubkey
	tipFractionBps uint64
	minTipSol      float64
	minProfit      float64
	slippageBps    uint32
}

// cachedFire is a fully-built, sim-verified fire tx kept hot for an armed
// account. The tx is compiled with a placeholder blockhash (sim uses
// replaceRecentBlockhash); a real blockhash is stamped at fire time.
// Sending it needs only sign+submit — no quote, no sim, no RPC on the
// critical path.
type cachedFire struct {
	tx          solana.VersionedTransaction
	mode        fireMode
	tipLamports uint64
	tipSol      float64
	estProfit   float64
	seize       uint64
	built       time.Time
}

// legPair is a (collateral bank, debt bank) leg with its ranking score.
type legPair struct {
	assetBank solana.Pubkey
	liabBank  solana.Pubkey
	score     float64
}

// fireableLegs enumerates the (collateral bank, wired-debt bank) LEG pairs
// the fire path can act on, ranked by the smaller of the two USD sides —
// the bound on how much a single liquidate can seize/repay, so the most
// valuable leg is tried first. A single-position account yields one pair;
// a multi-position account yields up to (#collateral x #wired-debt) pairs.
// marginfi's liquidate is single-leg but carries the full balance list as
// observation accounts, so acting on ONE leg of a multi-position account
// is valid — this is how we reach the ~99% of at-risk collateral the old
// assets==1&&liabs==1 gate skipped.
func fireableLegs(a *liq.MarginfiAccount, banks liq.BankMap, prices liq.PriceMap) []legPair {
	sideUSD := func(b liq.Balance, isAsset bool) float64 {
		bk, ok := banks[b.BankPk]
		if !ok {
			return 0.0
		}
		p, ok := prices[b.BankPk]
		if !ok {
			return 0.0
		}
		var native float64
		if isAsset {
			native = b.AssetShares * bk.AssetShareValue
		} else {
			native = b.LiabilityShares * bk.LiabilityShareValue
		}
		return native / math.Pow(10, float64(bk.MintDecimals)) * p
	}
	var assets, debts []liq.Balance
	for _, b := range a.Balances {
		if b.AssetShares > 0.0 {
			assets = append(assets, b)
		}
	}
	for _, b := range a.Balances {
		if b.LiabilityShares > 0.0 {
			if bk, ok := banks[b.BankPk]; ok && isDebtMint(bk.Mint) {
				debts = append(debts, b)
			}
		}
	}
	var legs []legPair
	for _, c := range assets {
		for _, d := range debts {
			ca := sideUSD(c, true)
			da := sideUSD(d, false)
			score := math.Min(ca, da)
			legs = append(legs, legPair{assetBank: c.BankPk, liabBank: d.BankPk, score: score})
		}
	}
	sort.Slice(legs, func(i, j int) bool { return legs[i].score > legs[j].score })
	return legs
}

// tryArm arms an account: tries its ranked fireable legs (capped) and
// returns the first that builds + simulates + clears the profit gate. For
// a single-position account this is exactly one leg (unchanged behavior);
// for a multi-position account it walks the most-valuable legs first.
func tryArm(
	endpoint, runDir string, cfg *cfgBundle, crank *crankCtx, sc *scan,
	a *liq.MarginfiAccount, pk solana.Pubkey, prices, base liq.PriceMap,
	mintTP map[solana.Pubkey]solana.Pubkey,
) *cachedFire {
	r := liq.MaintenanceHealth(a, sc.banks, prices)
	legs := fireableLegs(a, sc.banks, prices)
	if len(legs) == 0 {
		return nil
	}
	maxLegs := config.EnvInt("MAX_LEGS_PER_ARM", 3)
	if maxLegs > len(legs) {
		maxLegs = len(legs)
	}
	for _, leg := range legs[:maxLegs] {
		if c := tryArmLeg(endpoint, runDir, cfg, crank, sc, a, pk, prices, base, mintTP, r, leg.assetBank, leg.liabBank); c != nil {
			return c
		}
	}
	return nil
}

// tryArmLeg arms ONE (collateral, debt) leg of an account. Every safety
// gate (on-chain liquidatable, sim, profit) is per-leg.
func tryArmLeg(
	endpoint, runDir string, cfg *cfgBundle, crank *crankCtx, sc *scan,
	a *liq.MarginfiAccount, pk solana.Pubkey, prices, base liq.PriceMap,
	mintTP map[solana.Pubkey]solana.Pubkey, r liq.HealthResult,
	assetBank, liabBank solana.Pubkey,
) *cachedFire {
	liabBankInfo, ok := sc.banks[liabBank]
	if !ok || !isDebtMint(liabBankInfo.Mint) {
		return nil
	}
	bank, ok := sc.banks[assetBank]
	if !ok {
		return nil
	}
	var assetBal *liq.Balance
	for i := range a.Balances {
		if a.Balances[i].BankPk == assetBank && a.Balances[i].AssetShares > 0.0 {
			assetBal = &a.Balances[i]
			break
		}
	}
	if assetBal == nil {
		return nil
	}
	nativeTotal := assetBal.AssetShares * bank.AssetShareValue

	// Record why a flagged account did NOT fire (so the steady state is
	// observable — otherwise these rejects are silent). Gated by the
	// caller's handle/sim cooldowns, so it's ~a row per account per
	// cooldown, not spam.
	logSkip := func(mode, reason string) {
		observe.LogDecision(runDir, &decisionLog{
			T: nowSecs(), Liquidatee: pk.String(), Mode: mode,
			CollateralUsd: r.Health.WeightedAssets, Ratio: r.Health.Ratio(),
			SeizeNative: 0, QuotedUsdcOut: 0.0, EstLiabUsdc: 0.0, EstProfitUsdc: 0.0,
			FireSimOk: false, Fired: false, Reason: reason,
		})
	}

	// Mode: already underwater at ON-CHAIN prices -> plain Sender tx.
	// Healthy on-chain but underwater at the true (blended) price -> the
	// stale-window edge: crank + liquidate as one bundle. Requires a
	// crankable oracle and a Hermes blob covering its feed.
	onchain := liq.MaintenanceHealth(a, sc.banks, base)
	var mode fireMode
	if onchain.Missing == 0 && onchain.Health.Liquidatable() {
		mode = fireMode{kind: fireSender}
	} else {
		if !crank.on {
			return nil
		}
		// Below the true-price threshold the chain refuses even WITH a
		// fresh crank — don't burn bundle sims; the fire phase re-arms on
		// the cross.
		if r.Missing > 0 || !r.Health.Liquidatable() {
			return nil
		}
		// Crankable check FIRST: it covers non-Pyth (Switchboard)
		// collateral, whose feedOf lookup would otherwise silently
		// short-circuit. Crankable => shard-0 sponsored Pyth => feedOf is
		// present.
		if _, ok := sc.crankable[assetBank]; !ok {
			logSkip("crank", "flagged at Lazer price but healthy on-chain and oracle not crankable (non-Pyth/non-sponsored) — cannot act")
			return nil
		}
		feedID, ok := sc.feedOf[assetBank]
		if !ok {
			logSkip("crank", "crankable but feed id missing — cannot build crank")
			return nil
		}
		if _, _, _, ok := crank.hermes.UpdateFor(feedID); !ok {
			logSkip("crank", "crankable but no fresh Hermes blob for feed yet")
			return nil
		}
		mode = fireMode{kind: fireCrank, feedID: feedID}
	}

	// Crank txs for the sizing/ground-truth bundles (placeholder blockhash
	// — sims replace it; the LIVE fire rebuilds from the freshest blob
	// anyway).
	var crankB64 *crankB64Pair
	if mode.kind == fireCrank {
		mu, vaa, _, ok := crank.hermes.UpdateFor(mode.feedID)
		if !ok {
			return nil
		}
		ctxs, err := pythcrank.BuildCrankTxs(cfg.authority, vaa, []pythaccumulator.MerkleUpdate{mu}, 0, 0, solana.Hash{})
		if err != nil {
			return nil
		}
		setupB64, fireB64, err := ctxs.ToB64()
		if err != nil {
			return nil
		}
		crankB64 = &crankB64Pair{setup: setupB64, fire: fireB64}
	}

	// Size by simulation ladder, largest passing fraction first. Track the
	// last revert code so a full miss logs the TRUE reason (6068 healthy
	// vs 6049 stale-oracle vs ...), not a blanket "healthy".
	var seize uint64
	var lastRevert *uint32
	for _, frac := range sizeLadder {
		amount := uint64(nativeTotal * frac)
		if amount == 0 {
			continue
		}
		gs := simulateGate(endpoint, cfg.authority, cfg.liquidatorMA, cfg.tp, pk, a, assetBank, liabBank, amount, sc.oracleOf, crankB64)
		switch gs.kind {
		case gateFireable:
			seize = amount
		case gateReverted:
			lastRevert = gs.failCode
		case gateUnusable:
			// couldn't judge (rpc/crank) — try another rung
		}
		if seize != 0 {
			break
		}
	}
	if seize == 0 {
		// No rung passed. The reason is now specific: healthy at the
		// actionable price (Lazer led), a stale on-chain oracle the chain
		// won't act on, etc.
		logSkip(mode.name(), revertReason(lastRevert))
		return nil
	}

	assetTP, ok := mintTP[bank.Mint]
	if !ok {
		t, ok := mintOwner(endpoint, bank.Mint)
		if !ok {
			return nil
		}
		mintTP[bank.Mint] = t
		assetTP = t
	}
	debtMint := liabBankInfo.Mint
	debtTP, ok := mintTP[debtMint]
	if !ok {
		t, ok := mintOwner(endpoint, debtMint)
		if !ok {
			return nil
		}
		mintTP[debtMint] = t
		debtTP = t
	}
	var obs []solana.AccountMeta
	for _, b := range a.Balances {
		oc, ok := sc.oracleOf[b.BankPk]
		if !ok {
			return nil
		}
		obs = append(obs, solana.ReadonlyMeta(b.BankPk), solana.ReadonlyMeta(oc))
	}
	cand := liqfire.FireCandidate{
		Liquidatee: pk, AssetBank: assetBank, AssetMint: bank.Mint, AssetTokenProgram: assetTP,
		AssetAmount: seize, LiabBank: liabBank,
		DebtMint: debtMint, DebtTokenProgram: debtTP,
		AssetOracle: sc.oracleOf[assetBank], LiabOracle: sc.oracleOf[liabBank],
		LiquidateeObs: obs,
	}
	price := prices[assetBank]
	seizedUSD := float64(seize) / math.Pow(10, float64(bank.MintDecimals)) * price
	estLiab := seizedUSD * 0.975
	// Debt asset USD conversion: the swap output is native debt units, so
	// a non-USDC debt (USDT ~= $1, wSOL ~= $150) must be priced to compare
	// against the (USD) liability estimate. Fall back to $1 for a
	// stablecoin debt bank with no live price rather than mis-valuing it
	// as free.
	debtDec := int(liabBankInfo.MintDecimals)
	debtPrice, ok := prices[liabBank]
	if !ok {
		if debtMint.String() == solMint {
			debtPrice = 150.0
		} else {
			debtPrice = 1.0
		}
	}
	debtOutUSD := func(native uint64) float64 {
		return float64(native) / math.Pow(10, float64(debtDec)) * debtPrice
	}
	solUSD := 150.0
	for bk, b := range sc.banks {
		if b.Mint.String() == solMint {
			if p, ok := prices[bk]; ok {
				solUSD = p
			}
			break
		}
	}

	log := &decisionLog{
		T: nowSecs(), Liquidatee: pk.String(), Mode: mode.name(),
		CollateralUsd: r.Health.WeightedAssets, Ratio: r.Health.Ratio(),
		SeizeNative: seize, QuotedUsdcOut: 0.0, EstLiabUsdc: estLiab, EstProfitUsdc: 0.0,
		FireSimOk: false, Fired: false, Reason: "",
	}
	// Sender tips a Helius Sender wallet; a bundle must tip a Jito account.
	var tipTo solana.Pubkey
	if mode.kind == fireSender {
		tipTo = cfg.tipAccount
	} else {
		t, ok := crank.pickTip()
		if !ok {
			log.Reason = "no Jito tip accounts"
			observe.LogDecision(runDir, log)
			return nil
		}
		tipTo = t
	}
	// Build with a placeholder blockhash (sim replaces it; fire stamps a
	// real one).
	ph := solana.Hash{}
	fire, err := liqfire.BuildFireTx(endpoint, &cand, cfg.liquidatorMA, cfg.authority, &tipTo, 0, 100_000, cfg.slippageBps, 20, ph)
	if err != nil {
		log.Reason = fmt.Sprintf("build: %v", err)
		observe.LogDecision(runDir, log)
		return nil
	}
	log.QuotedUsdcOut = debtOutUSD(fire.QuotedUsdcOut)
	estProfit := debtOutUSD(fire.QuotedUsdcOut) - estLiab
	log.EstProfitUsdc = estProfit
	tipSol := math.Max(estProfit*float64(cfg.tipFractionBps)/10_000.0/solUSD, cfg.minTipSol)
	tipLamports := uint64(tipSol * 1e9)
	if estProfit < cfg.minProfit+tipSol*solUSD {
		log.Reason = fmt.Sprintf("below min profit (est $%.2f, tip $%.2f)", estProfit, tipSol*solUSD)
		observe.LogDecision(runDir, log)
		return nil
	}
	fire, err = liqfire.BuildFireTx(endpoint, &cand, cfg.liquidatorMA, cfg.authority, &tipTo, tipLamports, 100_000, cfg.slippageBps, 20, ph)
	if err != nil {
		log.Reason = fmt.Sprintf("rebuild: %v", err)
		observe.LogDecision(runDir, log)
		return nil
	}
	// Ground-truth gate lives HERE (arm time), off the fire critical path.
	// In crank mode the whole bundle is the ground truth — the liquidate
	// must succeed AT the cranked price.
	b64tx, err := fire.Tx.Base64()
	if err != nil {
		log.Reason = fmt.Sprintf("encode: %v", err)
		observe.LogDecision(runDir, log)
		return nil
	}
	var simOk bool
	if crankB64 == nil {
		res, ok := simulateTxB64(endpoint, b64tx)
		simOk = ok && (res["err"] == nil)
	} else {
		bs, ok := simulateBundle(endpoint, []string{crankB64.setup, crankB64.fire, b64tx})
		simOk = ok && bs.ranOk == 3
	}
	log.FireSimOk = simOk
	if !simOk {
		log.Reason = "fire sim revert (swap/repay would not cover liability)"
		observe.LogDecision(runDir, log)
		return nil
	}
	return &cachedFire{tx: fire.Tx, mode: mode, tipLamports: tipLamports, tipSol: tipSol, estProfit: estProfit, seize: seize, built: time.Now()}
}

// fireCached fires a cached tx: stamp the fresh blockhash, sign, submit
// (Sender for a plain fire; a Jito bundle with freshly-built crank txs for
// crank mode), log, spawn the realized-P&L readback. The profit-or-revert
// guard makes this safe without re-simulating — a stale/unprofitable
// Sender fire reverts for the base fee, and a failing bundle never lands
// at all.
func fireCached(
	endpoint, runDir, senderURL string, cfg *cfgBundle, crank *crankCtx, dryRun bool,
	pk solana.Pubkey, cached *cachedFire, freshBH solana.Hash, kp *solana.Keypair,
	dailyTip *sync.Mutex, dailyTipSol *float64, maxDailyTip, walletMin float64, webhook string,
) {
	mode := cached.mode.name()
	log := &decisionLog{
		T: nowSecs(), Liquidatee: pk.String(), Mode: mode, CollateralUsd: 0.0, Ratio: 0.0,
		SeizeNative: cached.seize, QuotedUsdcOut: 0.0, EstLiabUsdc: 0.0, EstProfitUsdc: cached.estProfit,
		FireSimOk: true, Fired: false, Reason: "",
	}
	pkStr := pk.String()
	fmt.Printf("★ LIQUIDATABLE [%s]  %s  seize %d  est profit $%.2f  tip %.5f SOL  (armed %v ago)\n",
		mode, pkStr[:min8(len(pkStr))], cached.seize, cached.estProfit, cached.tipSol, time.Since(cached.built))
	if dryRun {
		log.Reason = fmt.Sprintf("dry-run: would fire (%s, armed)", mode)
		observe.LogDecision(runDir, log)
		observe.Alert(webhook, "liq-dry", fmt.Sprintf("DRY-RUN %s liquidation: %s est profit $%.2f", mode, pkStr, cached.estProfit))
		return
	}
	dailyTip.Lock()
	over := *dailyTipSol+cached.tipSol > maxDailyTip
	dailyTip.Unlock()
	if over {
		log.Reason = "daily tip cap"
		observe.LogDecision(runDir, log)
		observe.Alert(webhook, "liq-cap", "daily tip cap reached")
		return
	}
	if solBalance(endpoint, cfg.authority.String()) < walletMin {
		log.Reason = "wallet below floor"
		observe.LogDecision(runDir, log)
		observe.Alert(webhook, "liq-floor", "wallet below floor — not firing")
		return
	}
	tx := cached.tx
	tx.Message.V0.RecentBlockhash = freshBH
	if err := tx.Sign([]solana.Keypair{*kp}); err != nil {
		log.Reason = fmt.Sprintf("sign: %v", err)
		observe.LogDecision(runDir, log)
		return
	}
	sig := tx.Signatures[0].String()
	txB64, err := tx.Base64()
	if err != nil {
		log.Reason = fmt.Sprintf("encode: %v", err)
		observe.LogDecision(runDir, log)
		return
	}
	seize, estProfit, tipLamports, tipSol := cached.seize, cached.estProfit, cached.tipLamports, cached.tipSol

	// Submit: Sender for a plain fire, Jito bundle for crank mode.
	var bundleID *string
	var submitErr error
	switch cached.mode.kind {
	case fireSender:
		_, submitErr = jito.SendSender(senderURL, txB64)
	case fireCrank:
		mu, vaa, age, ok := crank.hermes.UpdateFor(cached.mode.feedID)
		if !ok {
			submitErr = fmt.Errorf("no Hermes blob for feed")
			break
		}
		if age > crank.maxBlobAge {
			submitErr = fmt.Errorf("Hermes blob stale (%v) — not bundling", age)
			break
		}
		ctxs, err := pythcrank.BuildCrankTxs(cfg.authority, vaa, []pythaccumulator.MerkleUpdate{mu}, 0, 0, freshBH)
		if err != nil {
			submitErr = err
			break
		}
		if err := ctxs.StampAndSign(*kp, freshBH); err != nil {
			submitErr = err
			break
		}
		setupB64, crankFireB64, err := ctxs.ToB64()
		if err != nil {
			submitErr = err
			break
		}
		var last error
		for attempt := 0; attempt < 3; attempt++ {
			id, err := jito.SendBundle(crank.blockEngine, []string{setupB64, crankFireB64, txB64})
			if err == nil {
				bundleID = &id
				break
			}
			last = err
			if attempt < 2 && containsStr(err.Error(), "429") {
				time.Sleep(250 * time.Millisecond)
				continue
			}
			break
		}
		if bundleID == nil {
			submitErr = last
		}
	}

	log.Fired = submitErr == nil
	log.Reason = fmt.Sprintf("fired (%s, armed cache)", mode)
	observe.LogDecision(runDir, log)
	if submitErr == nil {
		bundleTag := ""
		if bundleID != nil {
			bundleTag = " bundle " + *bundleID
		}
		fmt.Fprintf(os.Stderr, "[exec] FIRED [%s] %s%s\n", mode, sig, bundleTag)
		sigCopy := sig
		observe.LogTrade(runDir, &tradeLog{
			T: nowSecs(), Liquidatee: pkStr, SeizeNative: seize,
			EstProfitUsdc: estProfit, TipLamports: tipLamports, Signature: &sigCopy,
			Bundle: bundleID,
		})
		owner := cfg.authority.String()
		be := crank.blockEngine
		bid := bundleID
		go func(s string) {
			for _, wait := range []int{5, 15, 45} {
				time.Sleep(time.Duration(wait) * time.Second)
				if pnl, ok := observe.RealizedUSDC(endpoint, s, owner); ok {
					dailyTip.Lock()
					*dailyTipSol += tipSol
					dailyTip.Unlock()
					sCopy := s
					pnlCopy := pnl
					observe.LogTrade(runDir, &tradeLog{
						T: nowSecs(), Liquidatee: "", SeizeNative: 0,
						EstProfitUsdc: 0.0, TipLamports: 0, Signature: &sCopy,
						RealizedUsdc: &pnlCopy,
					})
					observe.Alert(webhook, "liq-landed", fmt.Sprintf("liquidation landed %s: realized $%.2f", s, pnl))
					return
				}
			}
			status := ""
			if bid != nil {
				status = jito.BundleStatus(be, *bid)
			}
			observe.Alert(webhook, "liq-miss", fmt.Sprintf("liquidation %s never confirmed (bundle status: %s)", s, status))
		}(sig)
	} else {
		fmt.Fprintf(os.Stderr, "[exec] send failed: %v\n", submitErr)
		errStr := submitErr.Error()
		observe.LogTrade(runDir, &tradeLog{
			T: nowSecs(), Liquidatee: pkStr, SeizeNative: seize,
			EstProfitUsdc: estProfit, TipLamports: tipLamports, Error: &errStr,
		})
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func min8(n int) int {
	if n < 8 {
		return n
	}
	return 8
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func main() {
	config.LoadDotenv()

	endpoint, ok := config.EnvOptional("HELIUS_RPC")
	if !ok {
		endpoint, ok = config.EnvOptional("RPC_HTTP")
	}
	if !ok {
		fatalf("HELIUS_RPC")
	}
	dryRun := config.EnvOr("DRY_RUN", "1") != "0"
	runDir := config.EnvOr("RUN_DIR", "runs")
	minCollateral := config.EnvFloat("MIN_COLLATERAL_USD", 100.0)
	minProfit := config.EnvFloat("MIN_PROFIT_USD", 0.5)
	tipFractionBps := config.EnvUint64("TIP_FRACTION_BPS", 3000)
	minTipSol := config.EnvFloat("MIN_TIP_SOL", 0.0002)
	maxDailyTipSol := config.EnvFloat("MAX_DAILY_TIP_SOL", 0.05)
	walletMinSol := config.EnvFloat("WALLET_MIN_SOL", 0.02)
	poll := time.Duration(config.EnvInt("POLL_MS", 5000)) * time.Millisecond
	rescan := time.Duration(config.EnvInt("RESCAN_SECS", 300)) * time.Second
	watchRatio := config.EnvFloat("WATCH_RATIO", 0.85)
	slippageBps := uint32(config.EnvInt("SLIPPAGE_BPS", 100))
	senderURL := config.EnvOr("SENDER_URL", "http://ams-sender.helius-rpc.com/fast")
	// Helius Sender requires the tip go to one of ITS tip wallets.
	tipAccount := solana.MustPubkeyFromBase58(config.EnvOr("SENDER_TIP_ACCOUNT", "2nyhqdwKcJZR2vcqCyrYsaPVdAnFoJjiksCXJ7hfEYgD"))
	webhook := config.EnvOr("ALERT_WEBHOOK", "")
	liquidatorMA := solana.MustPubkeyFromBase58(config.EnvOr("LIQUIDATOR_MA", defaultLiquidatorMA))
	tp := solana.MustPubkeyFromBase58("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA")

	var kp *solana.Keypair
	if kpPath, ok := config.EnvOptional("KEYPAIR_PATH"); ok {
		raw, err := os.ReadFile(kpPath)
		if err != nil {
			fatalf("read keypair: %v", err)
		}
		var bytes []byte
		if err := json.Unmarshal(raw, &bytes); err != nil {
			fatalf("parse keypair: %v", err)
		}
		k, err := solana.KeypairFromBytes(bytes)
		if err != nil {
			fatalf("keypair: %v", err)
		}
		kp = &k
	}
	if kp == nil && !dryRun {
		fatalf("LIVE needs KEYPAIR_PATH")
	}
	var authority solana.Pubkey
	if kp != nil {
		authority = kp.Public
	} else {
		authority = solana.MustPubkeyFromBase58(config.EnvOr("AUTHORITY", defaultAuthority))
	}

	// Optional Pyth Lazer pre-positioning: when PYTH_LAZER_TOKEN is set,
	// blend Lazer's ms-latency major prices over the on-chain oracle in
	// the watch-set recompute so the loop ARMS accounts about to cross the
	// threshold ahead of the on-chain crank. The FIRE decision stays gated
	// by full on-chain sim — Lazer only steers which accounts we spend
	// sim budget on.
	lazerTable := pyth.NewTable()
	lazerMap := lazer.MintFeedMap()
	lazerToken, hasLazerToken := config.EnvOptional("PYTH_LAZER_TOKEN")
	lazerOn := hasLazerToken
	if lazerOn {
		lazer.SpawnLazerThread(lazerToken, lazer.ArmFeedIDs(), lazerTable)
		fmt.Fprintln(os.Stderr, "[exec] Pyth Lazer pre-positioning ENABLED")
	}

	// Self-crank context: hot Hermes blob + Jito tip accounts. The edge
	// only triggers with Lazer on (that's what detects the true-price
	// cross); the fallback tip list keeps DRY_RUN sims working if the
	// fetch fails — DttWaMu... was observed live as the tip destination in
	// the captured crank.
	crankOn := config.EnvOr("CRANK", "1") != "0"
	blockEngine := jito.DefaultBlockEngine()
	var tips []solana.Pubkey
	if crankOn {
		if t, err := jito.GetTipAccounts(blockEngine); err == nil {
			tips = t
		}
	}
	if crankOn && len(tips) == 0 {
		fmt.Fprintln(os.Stderr, "[exec] getTipAccounts failed — using fallback Jito tip list")
		tips = []solana.Pubkey{solana.MustPubkeyFromBase58("DttWaMuVvTiduZRnguLF7jNxTgiMBZ1hyAumKUiL2KRL")}
	}
	hermesURL := config.EnvOr("HERMES", "https://hermes.pyth.network")
	maxBlobMs := config.EnvInt("MAX_BLOB_AGE_MS", 3000)
	crank := &crankCtx{
		on:          crankOn,
		hermes:      pythaccumulator.SpawnHermesCache(hermesURL, nil, 400*time.Millisecond),
		tips:        tips,
		blockEngine: blockEngine,
		maxBlobAge:  time.Duration(maxBlobMs) * time.Millisecond,
	}
	onOff := "off"
	if crank.on {
		onOff = "ENABLED"
	}
	fmt.Fprintf(os.Stderr, "[exec] self-crank mode: %s\n", onOff)

	dryTag := "[DRY RUN]"
	if !dryRun {
		dryTag = "[LIVE]"
	}
	fmt.Fprintf(os.Stderr, "[exec] marginfi liquidation executor %s  authority=%s  min_profit=$%v  poll=%v rescan=%v  lazer=%v\n",
		dryTag, authority.String(), minProfit, poll, rescan, lazerOn)
	if !dryRun {
		bal := solBalance(endpoint, authority.String())
		fmt.Fprintf(os.Stderr, "[exec] wallet balance: %v SOL\n", bal)
		if bal < walletMinSol {
			fatalf("wallet below floor %v", walletMinSol)
		}
	}

	mintFeed := lazer.MintFeedMap()
	lazerDirect := lazer.OneToOneMints()
	sc, ok := fullScan(endpoint)
	if !ok {
		fatalf("initial scan")
	}
	lastScan := time.Now()
	var watch []solana.Pubkey
	engine := liqengine.NewEngine(minCollateral)
	// Counts only LANDED tips (a guard-reverted tx pays no tip — the ix
	// reverts with it), incremented by the readback goroutine.
	var dailyTipMu sync.Mutex
	dailyTipSol := 0.0
	tipDay := nowSecs() / 86_400
	mintTPCache := make(map[solana.Pubkey]solana.Pubkey)
	// Ladder-rejected candidates (emode phantoms) re-sim at most once per
	// cooldown — they'd otherwise burn 5 gate sims every poll, forever.
	simCooldown := time.Duration(config.EnvInt("SIM_COOLDOWN_SECS", 60)) * time.Second
	// Refused accounts carry a strike count; the cooldown DOUBLES per
	// strike (capped at 1h). One flat cooldown let structurally-unfireable
	// accounts (healthy on-chain + uncrankable oracle, LST phantoms)
	// re-enter the ranked top-K forever and starve the per-cycle fire
	// slots.
	type rejectEntry struct {
		t       time.Time
		strikes uint32
	}
	simRejected := make(map[solana.Pubkey]rejectEntry)
	simBackoff := func(strikes uint32) time.Duration {
		s := strikes
		if s > 0 {
			s--
		}
		if s > 6 {
			s = 6
		}
		mult := time.Duration(1 << s)
		d := simCooldown * mult
		if d > time.Hour {
			d = time.Hour
		}
		return d
	}
	// After handling a crossed account (fired or gated) don't re-process
	// it for this long — a persistently-crossed account would otherwise
	// spin every tick.
	handleCooldown := time.Duration(config.EnvInt("HANDLE_COOLDOWN_SECS", 20)) * time.Second
	handled := make(map[solana.Pubkey]time.Time)
	var lastTickUs uint64
	// Lazer-table poll granularity (ms). 1ms ~= instant detection at
	// negligible CPU; raise it only to save CPU on a shared box.
	tickPollMs := config.EnvInt("TICK_POLL_MS", 1)
	first := true

	cfg := &cfgBundle{
		liquidatorMA: liquidatorMA, authority: authority, tp: tp, tipAccount: tipAccount,
		tipFractionBps: tipFractionBps, minTipSol: minTipSol, minProfit: minProfit, slippageBps: slippageBps,
	}
	// Pre-built fire-tx cache: armed accounts (ratio >= ARM_RATIO) get a
	// hot, sim-verified tx so a cross -> sign+send with no build/quote/sim
	// on the critical path. ARM_RATIO < 1.0 so the tx is ready BEFORE the
	// cross.
	armRatio := config.EnvFloat("ARM_RATIO", 0.97)
	armTTL := time.Duration(config.EnvInt("ARM_TTL_SECS", 20)) * time.Second
	// Per-cycle sim caps (bound + prioritize): with emode-aware health the
	// crossed sets are small, but a real crash could flag many at once —
	// cap the arm/fire work to the top-K by USD deficit so the sim budget
	// always reaches the biggest real opportunities first and never
	// floods RPC or starves.
	maxArm := config.EnvInt("MAX_ARM_PER_CYCLE", 8)
	maxFire := config.EnvInt("MAX_FIRE_PER_CYCLE", 4)
	cache := make(map[solana.Pubkey]*cachedFire)
	var freshBH solana.Hash
	lastBH := time.Now().Add(-9999 * time.Second)
	// Heartbeat cadence: the event-driven loop is otherwise silent between
	// the 5-min rescans, so a healthy-but-calm bot looks identical to a
	// hung one or a dead Lazer feed. HEARTBEAT_SECS=0 disables.
	hbEvery := config.EnvInt("HEARTBEAT_SECS", 30)
	lastHB := time.Now().Add(-9999 * time.Second)
	// How many crossed/arm-set accounts were deferred past the per-cycle
	// cap (surfaced in the heartbeat so a persistent backlog is visible).
	fireDeferred := 0
	armDeferred := 0

	for {
		// Refresh the watch-set + engine coefficients from a full scan.
		if first || time.Since(lastScan) >= rescan {
			if !first {
				if s, ok := fullScan(endpoint); ok {
					sc = s
				}
			}
			lastScan = time.Now()
			base := freshPrices(endpoint, sc.banks, sc.oracleOf)
			prices, _ := lazer.Blend(sc.banks, base, lazerTable, lazerMap)
			// Only track accounts the fire path can act on (1 collateral /
			// 1 USDC/USDT/wSOL debt); non-fireable shapes would otherwise
			// inflate the counts and starve deficit-ranking. Matches
			// tryArm.
			var fireable []struct {
				Pubkey  solana.Pubkey
				Account liq.MarginfiAccount
			}
			for _, a := range sc.accts {
				if isV1Fireable(&a.acc, sc.banks) {
					fireable = append(fireable, struct {
						Pubkey  solana.Pubkey
						Account liq.MarginfiAccount
					}{a.pk, a.acc})
				}
			}
			watch = watch[:0]
			for _, f := range fireable {
				r := liq.MaintenanceHealth(&f.Account, sc.banks, prices)
				if r.Missing == 0 && r.Health.Ratio() >= watchRatio && r.Health.WeightedAssets >= minCollateral {
					watch = append(watch, f.Pubkey)
				}
			}
			// Engine (event-driven trigger): coefficients over the
			// on-chain baseline; Lazer feeds move health between rescans
			// with no RPC.
			lazerSnapshot := make(map[uint32]float64)
			for _, feedID := range lazer.ArmFeedIDs() {
				if p, ok := pyth.Get(lazerTable, feedID); ok {
					lazerSnapshot[feedID] = p.Price
				}
			}
			armed := engine.Rebuild(fireable, sc.banks, base, mintFeed, lazerDirect, lazerSnapshot, watchRatio)
			fmt.Fprintf(os.Stderr, "[exec] scan: %d borrowers -> %d fireable-shaped -> watch-set %d (ratio >= %v), engine armed %d\n",
				len(sc.accts), len(fireable), len(watch), watchRatio, armed)
			// Point the Hermes cache at the feeds we could actually need
			// to crank: crankable asset banks held by watch-set accounts.
			if crank.on {
				watchSet := make(map[solana.Pubkey]struct{}, len(watch))
				for _, w := range watch {
					watchSet[w] = struct{}{}
				}
				feedSet := make(map[[32]byte]struct{})
				for _, a := range sc.accts {
					if _, ok := watchSet[a.pk]; !ok {
						continue
					}
					for _, b := range a.acc.Balances {
						if b.AssetShares <= 0.0 {
							continue
						}
						if _, ok := sc.crankable[b.BankPk]; !ok {
							continue
						}
						if fid, ok := sc.feedOf[b.BankPk]; ok {
							feedSet[fid] = struct{}{}
						}
					}
				}
				hex := make([]string, 0, len(feedSet))
				for f := range feedSet {
					hex = append(hex, hexEncode(f[:]))
				}
				fmt.Fprintf(os.Stderr, "[exec] crank: %d crankable banks, %d feeds in Hermes cache\n", len(sc.crankable), len(hex))
				wantBlob := len(hex) > 0
				crank.hermes.SetFeeds(hex)
				// Boot warm-up: the poll goroutine starts with an empty
				// feed set, so the first blob only lands one
				// poll-interval AFTER this SetFeeds. Ticks evaluated in
				// that gap skip crankable candidates with "no fresh
				// Hermes blob yet". Block briefly (bounded) for the first
				// blob so the engine never evaluates blind at startup;
				// later rescans don't wait (the cache already holds a
				// recent blob).
				if first && wantBlob {
					warmStart := time.Now()
					for {
						if _, _, ok := crank.hermes.Latest(); ok || time.Since(warmStart) >= 5*time.Second {
							ready := "still pending (continuing)"
							if ok {
								ready = "READY"
							}
							fmt.Fprintf(os.Stderr, "[exec] hermes warm-up: blob %s after %v\n", ready, time.Since(warmStart))
							break
						}
						time.Sleep(50 * time.Millisecond)
					}
				}
			}
			first = false
		}

		day := nowSecs() / 86_400
		if day != tipDay {
			tipDay = day
			dailyTipMu.Lock()
			dailyTipSol = 0.0
			dailyTipMu.Unlock()
		}

		// Keep a recent blockhash hot so a fire stamps it without an RPC
		// on the critical path (refresh off the hot path, ~2s cadence).
		if time.Since(lastBH) >= 2*time.Second {
			if bh, ok := latestBlockhash(endpoint); ok {
				freshBH = bh
				lastBH = time.Now()
			}
		}

		// Trigger: event-driven on a Lazer tick (in-memory, no RPC) when
		// the feed is live; else fall back to the on-chain poll over the
		// watch-set.
		var toEval []solana.Pubkey
		var snap map[uint32]float64
		if lazerOn {
			deadline := time.Now().Add(poll)
			for {
				var cur uint64
				for _, f := range lazer.ArmFeedIDs() {
					if p, ok := pyth.Get(lazerTable, f); ok && p.TsUs > cur {
						cur = p.TsUs
					}
				}
				if cur > lastTickUs {
					lastTickUs = cur
					break
				}
				if time.Now().After(deadline) || time.Now().Equal(deadline) {
					break
				}
				// Tight poll of the in-memory Lazer table (checking it is
				// a few us). 1ms default cuts the tick->notice latency.
				time.Sleep(time.Duration(tickPollMs) * time.Millisecond)
			}
			snap = make(map[uint32]float64)
			for _, f := range lazer.ArmFeedIDs() {
				if p, ok := pyth.Get(lazerTable, f); ok {
					snap[f] = p.Price
				}
			}
			// Rank crossed accounts by USD deficit, fire only the top
			// MAX_FIRE this cycle (deferred ones ride the next tick —
			// deepest-underwater first, so the biggest real opportunity
			// is never starved). Cooldown filters run BEFORE the top-K
			// cap: a cooled-down account must not occupy a fire slot, or
			// a handful of standing phantoms blocks every real candidate
			// below them in the ranking.
			ranked := engine.CrossedRanked(snap, 1.0)
			var filtered []solana.Pubkey
			for _, rk := range ranked {
				if t, ok := handled[rk.Pubkey]; ok && time.Since(t) < handleCooldown {
					continue
				}
				if e, ok := simRejected[rk.Pubkey]; ok && time.Since(e.t) < simBackoff(e.strikes) {
					continue
				}
				filtered = append(filtered, rk.Pubkey)
			}
			fireDeferred = len(filtered) - maxFire
			if fireDeferred < 0 {
				fireDeferred = 0
			}
			if len(filtered) > maxFire {
				toEval = filtered[:maxFire]
			} else {
				toEval = filtered
			}
		} else {
			time.Sleep(poll)
			toEval = append([]solana.Pubkey{}, watch...)
			snap = make(map[uint32]float64)
		}

		// Heartbeat: prove liveness + show how close the market is. "feeds
		// live" is the tell — if it's 0/N the Lazer WS is dead and the
		// bot is inert (every crossed returns empty), which otherwise
		// looks just like calm.
		if lazerOn && hbEvery > 0 && time.Since(lastHB) >= time.Duration(hbEvery)*time.Second {
			totalFeeds := len(lazer.ArmFeedIDs())
			near := len(engine.Crossed(snap, armRatio))
			crossing := len(engine.Crossed(snap, 1.0))
			defer_ := ""
			if fireDeferred+armDeferred > 0 {
				defer_ = fmt.Sprintf(" | DEFERRED fire %d/arm %d (raise MAX_*_PER_CYCLE)", fireDeferred, armDeferred)
			}
			// Detection freshness: how far behind the latest Lazer
			// publish we are.
			var freshest uint64
			for _, f := range lazer.ArmFeedIDs() {
				if p, ok := pyth.Get(lazerTable, f); ok && p.TsUs > freshest {
					freshest = p.TsUs
				}
			}
			lagMs := (nowMicros() - freshest) / 1000
			fmt.Fprintf(os.Stderr, "[hb] lazer feeds %d/%d live | detect_lag %dms | %d within arm(%v) | %d liquidatable now | cache %d%s | %s\n",
				len(snap), totalFeeds, lagMs, near, armRatio, crossing, len(cache), defer_, lazer.Status(lazerTable))
			lastHB = time.Now()
		}

		// ── ARM phase (lazer mode only): keep a hot, sim-verified fire
		// tx for accounts near the threshold (ratio >= armRatio) so the
		// cross -> send is instant. Prune stale/no-longer-armed entries.
		// Costs Jupiter quotes + sims, but only for the small arm-set, and
		// off the fire critical path.
		if lazerOn {
			// Ranked by USD deficit (closest-to-crossing first for the
			// arm-set).
			armRanked := engine.CrossedRanked(snap, armRatio)
			armKeys := make(map[solana.Pubkey]struct{}, len(armRanked))
			for _, rk := range armRanked {
				armKeys[rk.Pubkey] = struct{}{}
			}
			// Drop cache entries that left the arm-set or went stale.
			for pk, c := range cache {
				if _, ok := armKeys[pk]; !ok || time.Since(c.built) >= armTTL {
					delete(cache, pk)
				}
			}
			var candidates []solana.Pubkey
			for _, rk := range armRanked {
				if _, cached := cache[rk.Pubkey]; cached {
					continue
				}
				if e, ok := simRejected[rk.Pubkey]; ok && time.Since(e.t) < simBackoff(e.strikes) {
					continue
				}
				candidates = append(candidates, rk.Pubkey)
			}
			// Cap the per-cycle arm work; the rest ride the next tick.
			armDeferred = len(candidates) - maxArm
			if armDeferred < 0 {
				armDeferred = 0
			}
			need := candidates
			if len(need) > maxArm {
				need = need[:maxArm]
			}
			if len(need) > 0 {
				raw := getMultiple(endpoint, need)
				base := freshPrices(endpoint, sc.banks, sc.oracleOf)
				prices, _ := lazer.Blend(sc.banks, base, lazerTable, lazerMap)
				for _, pk := range need {
					rd, ok := raw[pk]
					if !ok {
						continue
					}
					a, ok := liq.DecodeMarginfiAccount(rd)
					if !ok {
						continue
					}
					if c := tryArm(endpoint, runDir, cfg, crank, sc, a, pk, prices, base, mintTPCache); c != nil {
						delete(simRejected, pk)
						cache[pk] = c
					} else {
						e := simRejected[pk]
						e.t = time.Now()
						e.strikes++
						simRejected[pk] = e
					}
				}
			}
		}

		// Drop accounts handled recently (avoid per-tick spin on a
		// standing cross).
		var filteredEval []solana.Pubkey
		for _, pk := range toEval {
			if t, ok := handled[pk]; ok && time.Since(t) < handleCooldown {
				continue
			}
			filteredEval = append(filteredEval, pk)
		}
		toEval = filteredEval
		if len(toEval) == 0 {
			continue
		}

		// ── FIRE phase: for each crossed account, prefer the armed cache
		// (instant); else arm it inline now (covers a cross that outran
		// the arm pass, and the whole poll-mode path). Then send.
		freshRaw := getMultiple(endpoint, toEval)
		base := freshPrices(endpoint, sc.banks, sc.oracleOf)
		prices, _ := lazer.Blend(sc.banks, base, lazerTable, lazerMap)
		for _, pk := range toEval {
			handled[pk] = time.Now()
			var cached *cachedFire
			if c, ok := cache[pk]; ok && time.Since(c.built) < armTTL {
				cached = c
				delete(cache, pk)
			} else {
				// Not armed (or stale) — build inline now.
				rd, ok := freshRaw[pk]
				if !ok {
					continue
				}
				a, ok := liq.DecodeMarginfiAccount(rd)
				if !ok {
					continue
				}
				r := liq.MaintenanceHealth(a, sc.banks, prices)
				if r.Missing > 0 || !r.Health.Liquidatable() || r.Health.WeightedAssets < minCollateral {
					continue
				}
				if c := tryArm(endpoint, runDir, cfg, crank, sc, a, pk, prices, base, mintTPCache); c != nil {
					delete(simRejected, pk)
					cached = c
				} else {
					e := simRejected[pk]
					e.t = time.Now()
					e.strikes++
					simRejected[pk] = e
					continue
				}
			}
			armedFromCache := time.Since(cached.built) < armTTL && time.Since(cached.built) > 0
			fireStart := nowMicros()
			fireCached(endpoint, runDir, senderURL, cfg, crank, dryRun, pk, cached, freshBH, kp,
				&dailyTipMu, &dailyTipSol, maxDailyTipSol, walletMinSol, webhook)
			// Latency ledger: from the Lazer publish that made this cross
			// (lastTickUs) to detection (loop) to fire submit. Proves
			// whether we can act inside the liquidation window.
			done := nowMicros()
			logLatency(runDir, map[string]any{
				"t": nowSecs(), "account": pk.String(), "mode": cached.mode.name(),
				"appeared_us":     lastTickUs,
				"detected_lag_ms": (fireStart - lastTickUs) / 1000,
				"submit_lag_ms":   (done - lastTickUs) / 1000,
				"fire_submit_ms":  (done - fireStart) / 1000,
				"armed":           armedFromCache, "dry_run": dryRun,
			})
		}
	}
}

func hexEncode(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hexdigits[c>>4]
		out[i*2+1] = hexdigits[c&0xf]
	}
	return string(out)
}
