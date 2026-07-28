// Production marginfi liquidation executor — continuous loop, DRY_RUN default.
//
// Detection is simulation-gated (the emode lesson: don't replicate marginfi's
// risk math off-chain — let the chain judge). Pipeline per candidate:
//
//	full scan (RESCAN_SECS) → watch-set of near-liquidation borrowers
//	fast poll (POLL_MS): fresh watch-set accounts + bank/oracle prices
//	base-weight liquidatable? → sim-gate [start_fl, liquidate, end_fl]
//	→ SIZE the seize by simulation ladder (largest passing fraction)
//	→ build the atomic fire tx (liquidate→withdraw→Jupiter swap→repay_all)
//	→ profit gate (quoted USDC out vs ~97.5% liability taken + tip)
//	→ FULL fire-tx simulation (ground truth for every leg incl. swap+repay)
//	→ DRY_RUN: log · LIVE: sign + submit via Helius Sender, readback P&L
//
// ── Self-crank mode (the stale-oracle edge) ─────────────────────────────
// marginfi's Pyth feeds lag the true price by 8–44s. When an account is
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
//	[PYTH_LAZER_TOKEN=… (required for the crank edge)] [CRANK=1]
//	[MIN_COLLATERAL_USD=100] [MIN_PROFIT_USD=0.5] [TIP_FRACTION_BPS=3000]
//	[POLL_MS=5000] [RESCAN_SECS=300] [WATCH_RATIO=0.85] [RUN_DIR=runs]
//	[MAX_BLOB_AGE_MS=3000] [JITO_BLOCK_ENGINE=…]
//	go run ./cmd/liq_executor
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/jito"
	"solana-arb-backtest-go/internal/lazer"
	"solana-arb-backtest-go/internal/liquidation"
	"solana-arb-backtest-go/internal/marginfi"
	"solana-arb-backtest-go/internal/observe"
	"solana-arb-backtest-go/internal/pyth"
)

const (
	marginfiProgram     = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA"
	marginfiGroup       = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8"
	solMint             = "So11111111111111111111111111111111111111112"
	usdtMint            = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"
	defaultLiquidatorMA = "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD"
	defaultAuthority    = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak"
	tokenProgramStr     = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
)

// isDebtMint: debt (liability) assets the fire path can repay: USDC, USDT,
// wSOL. The liquidator absorbs the liquidatee's liability and repays it by
// swapping the seized collateral into this asset — so it must be a mint
// Jupiter routes liquidly and the marginfi flashloan can repay.
func isDebtMint(mint solana.PublicKey) bool {
	m := mint.String()
	return m == marginfi.USDCMint || m == usdtMint || m == solMint
}

// sizeLadder: largest→smallest; bigger seize = more profit; marginfi rejects
// over-liquidation (post-liq health must stay ≤ 0), so walk down until one
// passes.
var sizeLadder = []float64{1.0, 0.5, 0.25, 0.1, 0.02}

func nowSecs() uint64 { return uint64(time.Now().Unix()) }
func nowUs() int64    { return time.Now().UnixMicro() }

// ── tiny RPC/util layer (mirrors the other ported executors) ───────────────

var httpClient = &http.Client{Timeout: 20 * time.Second}

func rpc(endpoint string, body map[string]any) (map[string]any, bool) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, false
	}
	for attempt := 0; attempt < 4; attempt++ {
		resp, err := httpClient.Post(endpoint, "application/json", bytes.NewReader(b))
		if err == nil {
			var v map[string]any
			decErr := json.NewDecoder(resp.Body).Decode(&v)
			resp.Body.Close()
			if decErr == nil {
				return v, true
			}
		}
		time.Sleep(time.Duration(400<<attempt) * time.Millisecond)
	}
	return nil, false
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func asArray(v any) []any        { a, _ := v.([]any); return a }

func b64Decode(data any) ([]byte, bool) {
	arr, ok := data.([]any)
	if !ok || len(arr) == 0 {
		return nil, false
	}
	s, ok := arr[0].(string)
	if !ok {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	return raw, true
}

func getMultiple(endpoint string, keys []solana.PublicKey) map[solana.PublicKey][]byte {
	out := map[solana.PublicKey][]byte{}
	for i := 0; i < len(keys); i += 100 {
		end := i + 100
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[i:end]
		strs := make([]string, len(chunk))
		for j, k := range chunk {
			strs[j] = k.String()
		}
		v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getMultipleAccounts",
			"params": []any{strs, map[string]any{"encoding": "base64"}}})
		if !ok {
			continue
		}
		values := asArray(asMap(v["result"])["value"])
		for j, accV := range values {
			acc := asMap(accV)
			if acc == nil {
				continue
			}
			if raw, ok := b64Decode(acc["data"]); ok {
				out[chunk[j]] = raw
			}
		}
	}
	return out
}

func mintOwner(endpoint string, mint solana.PublicKey) (solana.PublicKey, bool) {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{mint.String(), map[string]any{"encoding": "base64"}}})
	if !ok {
		return solana.PublicKey{}, false
	}
	owner, _ := asMap(asMap(asMap(v["result"])["value"]))["owner"].(string)
	if owner == "" {
		return solana.PublicKey{}, false
	}
	pk, err := solana.PublicKeyFromBase58(owner)
	if err != nil {
		return solana.PublicKey{}, false
	}
	return pk, true
}

func latestBlockhash(endpoint string) (solana.Hash, bool) {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getLatestBlockhash",
		"params": []any{map[string]any{"commitment": "finalized"}}})
	if !ok {
		return solana.Hash{}, false
	}
	bhStr, _ := asMap(asMap(asMap(v["result"])["value"]))["blockhash"].(string)
	if bhStr == "" {
		return solana.Hash{}, false
	}
	bh, err := solana.HashFromBase58(bhStr)
	if err != nil {
		return solana.Hash{}, false
	}
	return bh, true
}

func solBalance(endpoint, owner string) float64 {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getBalance", "params": []any{owner}})
	if !ok {
		return 0
	}
	lamports, ok := asMap(v["result"])["value"].(float64)
	if !ok {
		return 0
	}
	return lamports / 1e9
}

func currentSlot(endpoint string) uint64 {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getSlot",
		"params": []any{map[string]any{"commitment": "confirmed"}}})
	if !ok {
		return 0
	}
	f, _ := v["result"].(float64)
	return uint64(f)
}

func simulateTxB64(endpoint, txB64 string) (map[string]any, bool) {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "simulateTransaction",
		"params": []any{txB64, map[string]any{"sigVerify": false, "replaceRecentBlockhash": true,
			"commitment": "processed", "encoding": "base64"}}})
	if !ok {
		return nil, false
	}
	result := asMap(v["result"])
	if result == nil {
		return nil, false
	}
	value := asMap(result["value"])
	if value == nil {
		return nil, false
	}
	return value, true
}

// bundleSim: how many leading txs in the bundle succeeded. jito-solana stops
// at the first failing tx, so ranOK < n means tx[ranOK] reverted.
type bundleSim struct {
	ranOK    int
	failCode int
	hasCode  bool
}

func simulateBundle(endpoint string, txsB64 []string) (*bundleSim, bool) {
	nulls := make([]any, len(txsB64))
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "simulateBundle",
		"params": []any{
			map[string]any{"encodedTransactions": txsB64},
			map[string]any{"skipSigVerify": true, "replaceRecentBlockhash": true,
				"preExecutionAccountsConfigs": nulls, "postExecutionAccountsConfigs": nulls},
		}})
	if !ok {
		return nil, false
	}
	if e, present := v["error"]; present && e != nil {
		return nil, false
	}
	results := asArray(asMap(asMap(v["result"])["value"])["transactionResults"])
	ranOK := 0
	for _, r := range results {
		if asMap(r)["err"] != nil {
			break
		}
		ranOK++
	}
	sim := &bundleSim{ranOK: ranOK}
	if ranOK < len(results) {
		errV := asMap(results[ranOK])["err"]
		if code, ok := extractCustomCode(errV); ok {
			sim.failCode, sim.hasCode = code, true
		}
	}
	return sim, true
}

// extractCustomCode digs err.InstructionError[1].Custom out of a decoded
// simulateTransaction/simulateBundle error value.
func extractCustomCode(errV any) (int, bool) {
	ie := asArray(asMap(errV)["InstructionError"])
	if len(ie) != 2 {
		return 0, false
	}
	custom, ok := asMap(ie[1])["Custom"].(float64)
	if !ok {
		return 0, false
	}
	return int(custom), true
}

// ── gate tx: [start_fl, liquidate(asset_amount), end_fl] as base64 ─────────
// Simulated standalone (Sender path) or as the tail of a crank bundle.

func gateTxB64(
	authority, liquidatorMA, tp, liquidatee solana.PublicKey, acct *liquidation.MarginfiAccount,
	assetBank, liabBank solana.PublicKey, assetAmount uint64, oracleOf map[solana.PublicKey]solana.PublicKey,
) (string, bool) {
	var liquidateeObs solana.AccountMetaSlice
	for _, b := range acct.Balances {
		oc, ok := oracleOf[b.BankPk]
		if !ok {
			return "", false
		}
		liquidateeObs = append(liquidateeObs, solana.NewAccountMeta(b.BankPk, false, false))
		liquidateeObs = append(liquidateeObs, solana.NewAccountMeta(oc, false, false))
	}
	assetOracle, ok := oracleOf[assetBank]
	if !ok {
		return "", false
	}
	liabOracle, ok := oracleOf[liabBank]
	if !ok {
		return "", false
	}
	start := marginfi.StartFlashloan(liquidatorMA, authority, 2)
	liqIx := marginfi.LendingAccountLiquidate(assetBank, liabBank, liquidatorMA, authority, liquidatee, tp,
		assetAmount, assetOracle, liabOracle, liquidateeObs)
	endObs := solana.AccountMetaSlice{
		solana.NewAccountMeta(assetBank, false, false), solana.NewAccountMeta(assetOracle, false, false),
		solana.NewAccountMeta(liabBank, false, false), solana.NewAccountMeta(liabOracle, false, false),
	}
	end := marginfi.EndFlashloan(liquidatorMA, authority, endObs)
	tx, err := solanaCompileV0(authority, []solana.Instruction{start, liqIx, end})
	if err != nil {
		return "", false
	}
	tx.Signatures = []solana.Signature{{}}
	raw, err := tx.MarshalBinary()
	if err != nil {
		return "", false
	}
	return base64.StdEncoding.EncodeToString(raw), true
}

// solanaCompileV0 wraps arb.CompileV0 with no ALTs — local helper so this
// file doesn't need to depend on internal/arb for one call. Kept here (not
// in internal/) since cmd/liq_executor must not touch shared packages.
func solanaCompileV0(payer solana.PublicKey, ixs []solana.Instruction) (*solana.Transaction, error) {
	return solana.NewTransaction(ixs, solana.Hash{}, solana.TransactionPayer(payer))
}

// gateSimOutcome: how a gate simulation came out.
type gateSimOutcome int

const (
	gateFireable gateSimOutcome = iota
	gateReverted
	gateUnusable
)

// simulateGate: cheap sim gate — gateFireable means marginfi accepts the
// liquidation at this size. With crank txs, the gate rides behind them in a
// simulateBundle so the chain judges at the CRANKED price; standalone it
// judges at on-chain prices. Returns (outcome, revertCode, hasCode).
func simulateGate(
	endpoint string, authority, liquidatorMA, tp, liquidatee solana.PublicKey, acct *liquidation.MarginfiAccount,
	assetBank, liabBank solana.PublicKey, assetAmount uint64, oracleOf map[solana.PublicKey]solana.PublicKey,
	crankB64 *[2]string,
) (gateSimOutcome, int, bool) {
	gate, ok := gateTxB64(authority, liquidatorMA, tp, liquidatee, acct, assetBank, liabBank, assetAmount, oracleOf)
	if !ok {
		return gateUnusable, 0, false
	}
	if crankB64 != nil {
		sim, ok := simulateBundle(endpoint, []string{crankB64[0], crankB64[1], gate})
		if !ok {
			return gateUnusable, 0, false
		}
		if sim.ranOK == 3 {
			return gateFireable, 0, false
		}
		// Crank txs must not be the failure — that's a broken crank, not a
		// healthy account; surface as unusable so the caller doesn't cool down.
		if sim.ranOK < 2 {
			return gateUnusable, 0, false
		}
		return gateReverted, sim.failCode, sim.hasCode
	}
	res, ok := simulateTxB64(endpoint, gate)
	if !ok {
		return gateUnusable, 0, false
	}
	errV := res["err"]
	if errV == nil {
		return gateFireable, 0, false
	}
	code, hasCode := extractCustomCode(errV)
	return gateReverted, code, hasCode
}

// revertReason: human-readable reason for a revert code, for the decision
// ledger. Keeps the finder's steady state honest — the old code logged every
// revert as "healthy".
func revertReason(code int, hasCode bool) string {
	if !hasCode {
		return "liquidate reverted (no custom code)"
	}
	switch code {
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
		return fmt.Sprintf("liquidate reverted with marginfi error %d", code)
	}
}

// ── decision / trade logs ───────────────────────────────────────────────

type decisionLog struct {
	T             uint64  `json:"t"`
	Liquidatee    string  `json:"liquidatee"`
	Mode          string  `json:"mode"`
	CollateralUSD float64 `json:"collateral_usd"`
	Ratio         float64 `json:"ratio"`
	SeizeNative   uint64  `json:"seize_native"`
	QuotedUSDCOut float64 `json:"quoted_usdc_out"`
	EstLiabUSDC   float64 `json:"est_liab_usdc"`
	EstProfitUSDC float64 `json:"est_profit_usdc"`
	FireSimOK     bool    `json:"fire_sim_ok"`
	Fired         bool    `json:"fired"`
	Reason        string  `json:"reason"`
}

type tradeLog struct {
	T             uint64   `json:"t"`
	Liquidatee    string   `json:"liquidatee"`
	SeizeNative   uint64   `json:"seize_native"`
	EstProfitUSDC float64  `json:"est_profit_usdc"`
	TipLamports   uint64   `json:"tip_lamports"`
	Signature     *string  `json:"signature"`
	Bundle        *string  `json:"bundle"`
	RealizedUSDC  *float64 `json:"realized_usdc"`
	Error         *string  `json:"error"`
}

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

// ── fire mode ────────────────────────────────────────────────────────────

type fireModeKind int

const (
	fireModeSender fireModeKind = iota
	fireModeCrank
)

// fireMode: how a cached fire gets submitted.
type fireMode struct {
	kind   fireModeKind
	feedID [32]byte // valid iff kind == fireModeCrank
}

func (m fireMode) name() string {
	if m.kind == fireModeCrank {
		return "crank"
	}
	return "sender"
}

// isV1Fireable: true if the fire path can act on at least one LEG of this
// account: it has a collateral position AND a liability whose bank is a
// supported debt asset (USDC/USDT/wSOL). Covers both single- and
// multi-position accounts — the fire path picks the best (collateral, debt)
// leg. Accounts with no wired-debt leg are still skipped (no liquid swap
// route), so the watch-set/engine won't track or rank them.
func isV1Fireable(a *liquidation.MarginfiAccount, banks liquidation.BankMap) bool {
	hasCollateral := false
	for _, b := range a.Balances {
		if b.AssetShares > 0 {
			hasCollateral = true
			break
		}
	}
	if !hasCollateral {
		return false
	}
	for _, b := range a.Balances {
		if b.LiabilityShares > 0 {
			if bk, ok := banks[b.BankPk]; ok && isDebtMint(bk.Mint) {
				return true
			}
		}
	}
	return false
}

// ── crank context ────────────────────────────────────────────────────────

// crankCtx: everything the crank path needs, spun up once at boot.
type crankCtx struct {
	on          bool
	hermes      *pyth.HermesCache
	tips        []solana.PublicKey
	blockEngine string
	maxBlobAge  time.Duration
}

func (c *crankCtx) pickTip() (solana.PublicKey, bool) {
	if len(c.tips) == 0 {
		return solana.PublicKey{}, false
	}
	return c.tips[nowSecs()%uint64(len(c.tips))], true
}

// ── scan ─────────────────────────────────────────────────────────────────

type scan struct {
	accts    []liquidation.AccountEntry
	banks    liquidation.BankMap
	oracleOf map[solana.PublicKey]solana.PublicKey
	// feedOf: bank → 32-byte Pyth feed id, decoded from the oracle account itself.
	feedOf map[solana.PublicKey][32]byte
	// crankable: banks whose oracle IS the shard-0 sponsored feed PDA — the
	// ones we can permissionlessly crank (write_authority == the feed itself).
	crankable map[solana.PublicKey]struct{}
}

func fullScan(endpoint string) (*scan, bool) {
	resp, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{marginfiProgram, map[string]any{
			"encoding":  "base64",
			"dataSlice": map[string]any{"offset": 0, "length": 1736},
			"filters": []any{
				map[string]any{"dataSize": liquidation.MASize},
				map[string]any{"memcmp": map[string]any{"offset": 8, "bytes": marginfiGroup}},
			},
		}},
	})
	if !ok {
		return nil, false
	}
	entries := asArray(resp["result"])
	var accts []liquidation.AccountEntry
	bankSet := map[solana.PublicKey]struct{}{}
	for _, eAny := range entries {
		e := asMap(eAny)
		pkStr, _ := e["pubkey"].(string)
		pk, err := solana.PublicKeyFromBase58(pkStr)
		if err != nil {
			continue
		}
		raw, ok := b64Decode(asMap(e["account"])["data"])
		if !ok {
			continue
		}
		acct, ok := liquidation.DecodeMarginfiAccount(raw)
		if !ok {
			continue
		}
		hasLiab := false
		for _, b := range acct.Balances {
			if b.LiabilityShares > 0 {
				hasLiab = true
			}
			bankSet[b.BankPk] = struct{}{}
		}
		if !hasLiab {
			continue
		}
		accts = append(accts, liquidation.AccountEntry{Pubkey: pk, Account: acct})
	}
	bankPks := make([]solana.PublicKey, 0, len(bankSet))
	for pk := range bankSet {
		bankPks = append(bankPks, pk)
	}
	banks := liquidation.BankMap{}
	oracleOf := map[solana.PublicKey]solana.PublicKey{}
	for pk, raw := range getMultiple(endpoint, bankPks) {
		if bk, ok := liquidation.DecodeBank(raw); ok {
			oracleOf[pk] = bk.OracleKey
			banks[pk] = bk
		}
	}
	// Crank metadata: decode each oracle's feed id and check whether the
	// oracle is the shard-0 sponsored PDA for that feed (→ crankable).
	oracleSet := map[solana.PublicKey]struct{}{}
	for _, o := range oracleOf {
		oracleSet[o] = struct{}{}
	}
	oraclePks := make([]solana.PublicKey, 0, len(oracleSet))
	for pk := range oracleSet {
		oraclePks = append(oraclePks, pk)
	}
	oracleRaw := getMultiple(endpoint, oraclePks)
	feedOf := map[solana.PublicKey][32]byte{}
	crankable := map[solana.PublicKey]struct{}{}
	for bank, oracle := range oracleOf {
		raw, ok := oracleRaw[oracle]
		if !ok {
			continue
		}
		fid, _, _, ok := liquidation.DecodePriceUpdateV2(raw)
		if !ok {
			continue
		}
		feedOf[bank] = fid
		if pyth.SponsoredFeed(0, fid).Equals(oracle) {
			crankable[bank] = struct{}{}
		}
	}
	return &scan{accts: accts, banks: banks, oracleOf: oracleOf, feedOf: feedOf, crankable: crankable}, true
}

// freshPrices: a stale Switchboard oracle is dropped here (see
// DecodeOraclePriceFresh): the account then reads as `missing` and is never
// trusted as liquidatable, matching the chain's SwitchboardStalePrice(6049)
// gate. The staleness ceiling is PER BANK, from its on-chain oracle_max_age
// (×2 safety) — so we filter exactly what the chain would reject. One
// getSlot per rescan (off the tick path).
func freshPrices(endpoint string, banks liquidation.BankMap, oracleOf map[solana.PublicKey]solana.PublicKey) liquidation.PriceMap {
	slot := currentSlot(endpoint)
	defaultStale := liquidation.DefaultMaxSBStaleSlots
	if v := os.Getenv("MAX_SB_STALE_SLOTS"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			defaultStale = n
		}
	}
	oracleSet := map[solana.PublicKey]struct{}{}
	for _, o := range oracleOf {
		oracleSet[o] = struct{}{}
	}
	oraclePks := make([]solana.PublicKey, 0, len(oracleSet))
	for pk := range oracleSet {
		oraclePks = append(oraclePks, pk)
	}
	raw := getMultiple(endpoint, oraclePks)
	out := liquidation.PriceMap{}
	for bankPk, oraclePk := range oracleOf {
		maxAge := uint16(0)
		if bk, ok := banks[bankPk]; ok {
			maxAge = bk.OracleMaxAge
		}
		maxStale := liquidation.MaxStaleSlotsFor(maxAge, defaultStale)
		r, ok := raw[oraclePk]
		if !ok {
			continue
		}
		usd, ok := liquidation.DecodeOraclePriceFresh(r, slot, maxStale)
		if !ok {
			continue
		}
		out[bankPk] = usd
	}
	return out
}

// ── cfg / cached fire ────────────────────────────────────────────────────

// cfg: copy-able config bundle for the arm/fire helpers.
type cfg struct {
	liquidatorMA   solana.PublicKey
	authority      solana.PublicKey
	tp             solana.PublicKey
	tipAccount     solana.PublicKey
	tipFractionBps uint64
	minTipSol      float64
	minProfit      float64
	slippageBps    uint32
}

// cachedFire: a fully-built, sim-verified fire tx kept hot for an armed
// account. The tx is compiled with a placeholder blockhash (sim uses
// replaceRecentBlockhash); a real blockhash is stamped at fire time. Sending
// it needs only sign+submit — no quote, no sim, no RPC on the critical path.
type cachedFire struct {
	tx          *solana.Transaction
	mode        fireMode
	tipLamports uint64
	tipSol      float64
	estProfit   float64
	seize       uint64
	built       time.Time
}

// fireableLeg: one (collateral bank, debt bank) pair ranked by the smaller
// of the two USD sides.
type fireableLeg struct {
	assetBank, liabBank solana.PublicKey
}

// fireableLegs enumerates the (collateral bank, wired-debt bank) LEG pairs
// the fire path can act on, ranked by the smaller of the two USD sides — the
// bound on how much a single liquidate can seize/repay, so the most valuable
// leg is tried first. A single-position account yields one pair; a
// multi-position account yields up to (#collateral × #wired-debt) pairs.
// marginfi's liquidate is single-leg but carries the full balance list as
// observation accounts, so acting on ONE leg of a multi-position account is
// valid — this is how we reach the ~99% of at-risk collateral the old
// assets==1&&liabs==1 gate skipped.
func fireableLegs(a *liquidation.MarginfiAccount, banks liquidation.BankMap, prices liquidation.PriceMap) []fireableLeg {
	sideUSD := func(b liquidation.Balance, isAsset bool) float64 {
		bk, ok := banks[b.BankPk]
		if !ok {
			return 0
		}
		p, ok := prices[b.BankPk]
		if !ok {
			return 0
		}
		var native float64
		if isAsset {
			native = b.AssetShares * bk.AssetShareValue
		} else {
			native = b.LiabilityShares * bk.LiabilityShareValue
		}
		scale := pow10(int(bk.MintDecimals))
		return native / scale * p
	}
	var assets, debts []liquidation.Balance
	for _, b := range a.Balances {
		if b.AssetShares > 0 {
			assets = append(assets, b)
		}
		if b.LiabilityShares > 0 {
			if bk, ok := banks[b.BankPk]; ok && isDebtMint(bk.Mint) {
				debts = append(debts, b)
			}
		}
	}
	type scored struct {
		leg   fireableLeg
		score float64
	}
	var legs []scored
	for _, c := range assets {
		for _, d := range debts {
			ca, da := sideUSD(c, true), sideUSD(d, false)
			score := ca
			if da < score {
				score = da
			}
			legs = append(legs, scored{fireableLeg{c.BankPk, d.BankPk}, score})
		}
	}
	sort.Slice(legs, func(i, j int) bool { return legs[i].score > legs[j].score })
	out := make([]fireableLeg, len(legs))
	for i, l := range legs {
		out[i] = l.leg
	}
	return out
}

func pow10(n int) float64 {
	r := 1.0
	if n >= 0 {
		for i := 0; i < n; i++ {
			r *= 10
		}
		return r
	}
	for i := 0; i < -n; i++ {
		r /= 10
	}
	return r
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
func envU64(name string, def uint64) uint64 {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}
func envF64(name string, def float64) float64 {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n
		}
	}
	return def
}
func envDurSecs(name string, def time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Duration(n) * time.Second
		}
	}
	return def
}
func envDurMs(name string, def time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Duration(n) * time.Millisecond
		}
	}
	return def
}
func short(pk solana.PublicKey) string {
	s := pk.String()
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// ── try_arm / try_arm_leg ───────────────────────────────────────────────

// executor bundles the shared state try_arm/try_arm_leg/fireCached need.
// Fields correspond 1:1 to the Rust free-function parameters.
type executor struct {
	endpoint  string
	runDir    string
	cfg       cfg
	crank     *crankCtx
	senderURL string
	mintTP    map[solana.PublicKey]solana.PublicKey
}

// tryArm arms an account: try its ranked fireable legs (capped) and return
// the first that builds + simulates + clears the profit gate. For a
// single-position account this is exactly one leg (unchanged behavior); for
// a multi-position account it walks the most-valuable legs first.
func (ex *executor) tryArm(sc *scan, a *liquidation.MarginfiAccount, pk solana.PublicKey, prices, base liquidation.PriceMap) *cachedFire {
	r := liquidation.MaintenanceHealth(a, sc.banks, prices)
	legs := fireableLegs(a, sc.banks, prices)
	if len(legs) == 0 {
		return nil
	}
	maxLegs := envInt("MAX_LEGS_PER_ARM", 3)
	if maxLegs > len(legs) {
		maxLegs = len(legs)
	}
	for _, leg := range legs[:maxLegs] {
		if c := ex.tryArmLeg(sc, a, pk, prices, base, r, leg.assetBank, leg.liabBank); c != nil {
			return c
		}
	}
	return nil
}

// tryArmLeg arms ONE (collateral, debt) leg of an account. Every safety gate
// (on-chain liquidatable, sim, profit) is per-leg.
func (ex *executor) tryArmLeg(
	sc *scan, a *liquidation.MarginfiAccount, pk solana.PublicKey, prices, base liquidation.PriceMap,
	r liquidation.HealthResult, assetBank, liabBank solana.PublicKey,
) *cachedFire {
	liabBankInfo, ok := sc.banks[liabBank]
	if !ok || !isDebtMint(liabBankInfo.Mint) {
		return nil
	}
	bank, ok := sc.banks[assetBank]
	if !ok {
		return nil
	}
	var assetBal *liquidation.Balance
	for i := range a.Balances {
		if a.Balances[i].BankPk.Equals(assetBank) && a.Balances[i].AssetShares > 0 {
			assetBal = &a.Balances[i]
			break
		}
	}
	if assetBal == nil {
		return nil
	}
	nativeTotal := assetBal.AssetShares * bank.AssetShareValue

	// Record why a flagged account did NOT fire (so the steady state is
	// observable — otherwise these rejects are silent).
	logSkip := func(mode, reason string) {
		observe.LogDecision(ex.runDir, decisionLog{
			T: nowSecs(), Liquidatee: pk.String(), Mode: mode,
			CollateralUSD: r.Health.WeightedAssets, Ratio: r.Health.Ratio(),
			Reason: reason,
		})
	}

	// Mode: already underwater at ON-CHAIN prices → plain Sender tx. Healthy
	// on-chain but underwater at the true (blended) price → the stale-window
	// edge: crank + liquidate as one bundle. Requires a crankable oracle and
	// a Hermes blob covering its feed.
	onchain := liquidation.MaintenanceHealth(a, sc.banks, base)
	var mode fireMode
	if onchain.Missing == 0 && onchain.Health.Liquidatable() {
		mode = fireMode{kind: fireModeSender}
	} else {
		if !ex.crank.on {
			return nil
		}
		// Below the true-price threshold the chain refuses even WITH a fresh
		// crank — don't burn bundle sims; the fire phase re-arms on the cross.
		if r.Missing > 0 || !r.Health.Liquidatable() {
			return nil
		}
		// Crankable check FIRST: it covers non-Pyth (Switchboard) collateral,
		// whose feedOf lookup would otherwise silently short-circuit.
		if _, ok := sc.crankable[assetBank]; !ok {
			logSkip("crank", "flagged at Lazer price but healthy on-chain and oracle not crankable (non-Pyth/non-sponsored) — cannot act")
			return nil
		}
		feedID, ok := sc.feedOf[assetBank]
		if !ok {
			logSkip("crank", "crankable but feed id missing — cannot build crank")
			return nil
		}
		if _, _, _, ok := ex.crank.hermes.UpdateFor(feedID); !ok {
			logSkip("crank", "crankable but no fresh Hermes blob for feed yet")
			return nil
		}
		mode = fireMode{kind: fireModeCrank, feedID: feedID}
	}

	// Crank txs for the sizing/ground-truth bundles (placeholder blockhash —
	// sims replace it; the LIVE fire rebuilds from the freshest blob anyway).
	var crankB64 *[2]string
	if mode.kind == fireModeCrank {
		mu, vaa, _, ok := ex.crank.hermes.UpdateFor(mode.feedID)
		if !ok {
			return nil
		}
		txs, err := pyth.BuildCrankTxs(ex.cfg.authority, vaa, []pyth.MerkleUpdate{mu}, 0, 0, solana.Hash{})
		if err != nil {
			return nil
		}
		setupB64, fireB64, err := txs.ToB64()
		if err != nil {
			return nil
		}
		crankB64 = &[2]string{setupB64, fireB64}
	}

	// Size by simulation ladder, largest passing fraction first. Track the
	// last revert code so a full miss logs the TRUE reason (6068 healthy vs
	// 6049 stale-oracle vs …), not a blanket "healthy".
	var seize uint64
	var lastRevertCode int
	var lastRevertHasCode bool
	for _, frac := range sizeLadder {
		amount := uint64(nativeTotal * frac)
		if amount == 0 {
			continue
		}
		outcome, code, hasCode := simulateGate(ex.endpoint, ex.cfg.authority, ex.cfg.liquidatorMA, ex.cfg.tp,
			pk, a, assetBank, liabBank, amount, sc.oracleOf, crankB64)
		switch outcome {
		case gateFireable:
			seize = amount
		case gateReverted:
			lastRevertCode, lastRevertHasCode = code, hasCode
		case gateUnusable:
			// couldn't judge (rpc/crank) — try another rung
		}
		if seize != 0 {
			break
		}
	}
	if seize == 0 {
		logSkip(mode.name(), revertReason(lastRevertCode, lastRevertHasCode))
		return nil
	}

	assetTP, ok := ex.mintTP[bank.Mint]
	if !ok {
		assetTP, ok = mintOwner(ex.endpoint, bank.Mint)
		if !ok {
			return nil
		}
		ex.mintTP[bank.Mint] = assetTP
	}
	debtMint := liabBankInfo.Mint
	debtTP, ok := ex.mintTP[debtMint]
	if !ok {
		debtTP, ok = mintOwner(ex.endpoint, debtMint)
		if !ok {
			return nil
		}
		ex.mintTP[debtMint] = debtTP
	}
	var obs solana.AccountMetaSlice
	for _, b := range a.Balances {
		oc, ok := sc.oracleOf[b.BankPk]
		if !ok {
			return nil
		}
		obs = append(obs, solana.NewAccountMeta(b.BankPk, false, false))
		obs = append(obs, solana.NewAccountMeta(oc, false, false))
	}
	cand := &liquidation.FireCandidate{
		Liquidatee: pk, AssetBank: assetBank, AssetMint: bank.Mint, AssetTokenProgram: assetTP,
		AssetAmount: seize, LiabBank: liabBank,
		DebtMint: debtMint, DebtTokenProgram: debtTP,
		AssetOracle: sc.oracleOf[assetBank], LiabOracle: sc.oracleOf[liabBank],
		LiquidateeObs: obs,
	}
	price := prices[assetBank]
	seizedUSD := float64(seize) / pow10(int(bank.MintDecimals)) * price
	estLiab := seizedUSD * 0.975
	// Debt asset USD conversion: the swap output is native debt units, so a
	// non-USDC debt (USDT ≈ $1, wSOL ≈ $150) must be priced to compare
	// against the (USD) liability estimate.
	debtDec := int(liabBankInfo.MintDecimals)
	debtPrice, hasDebtPrice := prices[liabBank]
	if !hasDebtPrice {
		if debtMint.String() == solMint {
			debtPrice = 150.0
		} else {
			debtPrice = 1.0
		}
	}
	debtOutUSD := func(native uint64) float64 { return float64(native) / pow10(debtDec) * debtPrice }
	solUSD := 150.0
	for bk, b := range sc.banks {
		if b.Mint.String() == solMint {
			if p, ok := prices[bk]; ok {
				solUSD = p
			}
			break
		}
	}

	log := decisionLog{
		T: nowSecs(), Liquidatee: pk.String(), Mode: mode.name(),
		CollateralUSD: r.Health.WeightedAssets, Ratio: r.Health.Ratio(),
		SeizeNative: seize, EstLiabUSDC: estLiab,
	}
	// Sender tips a Helius Sender wallet; a bundle must tip a Jito account.
	var tipTo solana.PublicKey
	if mode.kind == fireModeSender {
		tipTo = ex.cfg.tipAccount
	} else {
		t, ok := ex.crank.pickTip()
		if !ok {
			log.Reason = "no Jito tip accounts"
			observe.LogDecision(ex.runDir, log)
			return nil
		}
		tipTo = t
	}
	// Build with a placeholder blockhash (sim replaces it; fire stamps a real one).
	fire, err := liquidation.BuildFireTx(ex.endpoint, cand, ex.cfg.liquidatorMA, ex.cfg.authority,
		&tipTo, 0, 100_000, ex.cfg.slippageBps, 20, solana.Hash{})
	if err != nil {
		log.Reason = "build: " + err.Error()
		observe.LogDecision(ex.runDir, log)
		return nil
	}
	log.QuotedUSDCOut = debtOutUSD(fire.QuotedUSDCOut)
	estProfit := debtOutUSD(fire.QuotedUSDCOut) - estLiab
	log.EstProfitUSDC = estProfit
	tipSol := estProfit * float64(ex.cfg.tipFractionBps) / 10_000.0 / solUSD
	if tipSol < ex.cfg.minTipSol {
		tipSol = ex.cfg.minTipSol
	}
	tipLamports := uint64(tipSol * 1e9)
	if estProfit < ex.cfg.minProfit+tipSol*solUSD {
		log.Reason = fmt.Sprintf("below min profit (est $%.2f, tip $%.2f)", estProfit, tipSol*solUSD)
		observe.LogDecision(ex.runDir, log)
		return nil
	}
	fire, err = liquidation.BuildFireTx(ex.endpoint, cand, ex.cfg.liquidatorMA, ex.cfg.authority,
		&tipTo, tipLamports, 100_000, ex.cfg.slippageBps, 20, solana.Hash{})
	if err != nil {
		log.Reason = "rebuild: " + err.Error()
		observe.LogDecision(ex.runDir, log)
		return nil
	}
	// Ground-truth gate lives HERE (arm time), off the fire critical path. In
	// crank mode the whole bundle is the ground truth — the liquidate must
	// succeed AT the cranked price.
	fire.Tx.Signatures = []solana.Signature{{}}
	rawTx, err := fire.Tx.MarshalBinary()
	if err != nil {
		log.Reason = "marshal: " + err.Error()
		observe.LogDecision(ex.runDir, log)
		return nil
	}
	txB64 := base64.StdEncoding.EncodeToString(rawTx)
	var simOK bool
	if crankB64 == nil {
		res, ok := simulateTxB64(ex.endpoint, txB64)
		simOK = ok && res["err"] == nil
	} else {
		sim, ok := simulateBundle(ex.endpoint, []string{crankB64[0], crankB64[1], txB64})
		simOK = ok && sim.ranOK == 3
	}
	log.FireSimOK = simOK
	if !simOK {
		log.Reason = "fire sim revert (swap/repay would not cover liability)"
		observe.LogDecision(ex.runDir, log)
		return nil
	}
	return &cachedFire{tx: fire.Tx, mode: mode, tipLamports: tipLamports, tipSol: tipSol,
		estProfit: estProfit, seize: seize, built: time.Now()}
}

// fireCached: fire a cached tx: stamp the fresh blockhash, sign, submit
// (Sender for a plain fire; a Jito bundle with freshly-built crank txs for
// crank mode), log, spawn the realized-P&L readback. The profit-or-revert
// guard makes this safe without re-simulating — a stale/unprofitable Sender
// fire reverts for the base fee, and a failing bundle never lands at all.
func (ex *executor) fireCached(
	dryRun bool, pk solana.PublicKey, cached *cachedFire, freshBH solana.Hash, kp *solana.PrivateKey,
	dailyTip *dailyTipCounter, maxDailyTip, walletMin float64, webhook *string,
) {
	mode := cached.mode.name()
	log := decisionLog{
		T: nowSecs(), Liquidatee: pk.String(), Mode: mode,
		SeizeNative: cached.seize, EstProfitUSDC: cached.estProfit, FireSimOK: true,
	}
	fmt.Printf("★ LIQUIDATABLE [%s]  %s  seize %d  est profit $%.2f  tip %.5f SOL  (armed %s ago)\n",
		mode, short(pk), cached.seize, cached.estProfit, cached.tipSol, time.Since(cached.built))
	if dryRun {
		log.Reason = fmt.Sprintf("dry-run: would fire (%s, armed)", mode)
		observe.LogDecision(ex.runDir, log)
		observe.Alert(webhook, "liq-dry", fmt.Sprintf("DRY-RUN %s liquidation: %s est profit $%.2f", mode, pk, cached.estProfit))
		return
	}
	if dailyTip.get()+cached.tipSol > maxDailyTip {
		log.Reason = "daily tip cap"
		observe.LogDecision(ex.runDir, log)
		observe.Alert(webhook, "liq-cap", "daily tip cap reached")
		return
	}
	if solBalance(ex.endpoint, ex.cfg.authority.String()) < walletMin {
		log.Reason = "wallet below floor"
		observe.LogDecision(ex.runDir, log)
		observe.Alert(webhook, "liq-floor", "wallet below floor — not firing")
		return
	}
	// Shallow-copy the tx struct, then deep-copy Signatures — Sign() mutates
	// Signatures[i] in place, and a shallow copy would share the cached tx's
	// backing array, corrupting the hot cache entry on every fire attempt.
	txCopy := *cached.tx
	txCopy.Signatures = append([]solana.Signature(nil), cached.tx.Signatures...)
	txCopy.Message.RecentBlockhash = freshBH
	if _, err := txCopy.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(kp.PublicKey()) {
			return kp
		}
		return nil
	}); err != nil {
		log.Reason = "sign: " + err.Error()
		observe.LogDecision(ex.runDir, log)
		return
	}
	sig := ""
	if len(txCopy.Signatures) > 0 {
		sig = txCopy.Signatures[0].String()
	}
	rawTx, err := txCopy.MarshalBinary()
	if err != nil {
		log.Reason = "marshal: " + err.Error()
		observe.LogDecision(ex.runDir, log)
		return
	}
	txB64 := base64.StdEncoding.EncodeToString(rawTx)
	seize, estProfit, tipLamports, tipSol := cached.seize, cached.estProfit, cached.tipLamports, cached.tipSol

	// Submit: Sender for a plain fire, Jito bundle for crank mode.
	var bundleID *string
	var submitErr error
	switch cached.mode.kind {
	case fireModeSender:
		_, submitErr = jito.SendSender(ex.senderURL, txB64)
	case fireModeCrank:
		// Freshest blob → crank txs; the whole point is the newest price.
		mu, vaa, age, ok := ex.crank.hermes.UpdateFor(cached.mode.feedID)
		if !ok {
			submitErr = fmt.Errorf("no Hermes blob for feed")
			break
		}
		if age > ex.crank.maxBlobAge {
			submitErr = fmt.Errorf("Hermes blob stale (%s) — not bundling", age)
			break
		}
		ctxs, err := pyth.BuildCrankTxs(ex.cfg.authority, vaa, []pyth.MerkleUpdate{mu}, 0, 0, freshBH)
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
		var id string
		for attempt := 0; attempt < 3; attempt++ {
			id, submitErr = jito.SendBundle(ex.crank.blockEngine, []string{setupB64, crankFireB64, txB64})
			if submitErr == nil {
				bundleID = &id
				break
			}
			if strings.Contains(submitErr.Error(), "429") && attempt < 2 {
				time.Sleep(250 * time.Millisecond)
				continue
			}
			break
		}
	}

	log.Fired = submitErr == nil
	log.Reason = fmt.Sprintf("fired (%s, armed cache)", mode)
	observe.LogDecision(ex.runDir, log)
	if submitErr == nil {
		bundleSuffix := ""
		if bundleID != nil {
			bundleSuffix = " bundle " + *bundleID
		}
		fmt.Printf("[exec] FIRED [%s] %s%s\n", mode, sig, bundleSuffix)
		sigCopy := sig
		observe.LogTrade(ex.runDir, tradeLog{T: nowSecs(), Liquidatee: pk.String(), SeizeNative: seize,
			EstProfitUSDC: estProfit, TipLamports: tipLamports, Signature: &sigCopy, Bundle: bundleID})

		ep, rd, owner := ex.endpoint, ex.runDir, ex.cfg.authority.String()
		be, bid, wh := ex.crank.blockEngine, bundleID, webhook
		go func() {
			for _, wait := range []time.Duration{5 * time.Second, 15 * time.Second, 45 * time.Second} {
				time.Sleep(wait)
				if pnl, ok := observe.RealizedUSDC(ep, sigCopy, owner); ok {
					dailyTip.add(tipSol)
					pnlCopy := pnl
					observe.LogTrade(rd, tradeLog{T: nowSecs(), Signature: &sigCopy, RealizedUSDC: &pnlCopy})
					observe.Alert(wh, "liq-landed", fmt.Sprintf("liquidation landed %s: realized $%.2f", sigCopy, pnl))
					return
				}
			}
			status := ""
			if bid != nil {
				if s, ok := jito.BundleStatus(be, *bid); ok {
					status = s
				}
			}
			observe.Alert(wh, "liq-miss", fmt.Sprintf("liquidation %s never confirmed (bundle status: %s)", sigCopy, status))
		}()
	} else {
		errStr := submitErr.Error()
		fmt.Printf("[exec] send failed: %s\n", errStr)
		observe.LogTrade(ex.runDir, tradeLog{T: nowSecs(), Liquidatee: pk.String(), SeizeNative: seize,
			EstProfitUSDC: estProfit, TipLamports: tipLamports, Error: &errStr})
	}
}

// dailyTipCounter counts only LANDED tips (a guard-reverted tx pays no tip —
// the ix reverts with it), incremented by the readback goroutine.
type dailyTipCounter struct {
	mu  sync.Mutex
	sol float64
}

func (d *dailyTipCounter) get() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sol
}
func (d *dailyTipCounter) add(v float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sol += v
}
func (d *dailyTipCounter) reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sol = 0
}

func main() {
	envfile.LoadDotEnv()

	endpoint := os.Getenv("HELIUS_RPC")
	if endpoint == "" {
		endpoint = os.Getenv("RPC_HTTP")
	}
	if endpoint == "" {
		fmt.Fprintln(os.Stderr, "HELIUS_RPC in .env")
		os.Exit(1)
	}
	dryRun := true
	if v := os.Getenv("DRY_RUN"); v != "" {
		dryRun = v != "0"
	}
	runDir := os.Getenv("RUN_DIR")
	if runDir == "" {
		runDir = "runs"
	}
	minCollateral := envF64("MIN_COLLATERAL_USD", 100.0)
	minProfit := envF64("MIN_PROFIT_USD", 0.5)
	tipFractionBps := envU64("TIP_FRACTION_BPS", 3000)
	minTipSol := envF64("MIN_TIP_SOL", 0.0002)
	maxDailyTipSol := envF64("MAX_DAILY_TIP_SOL", 0.05)
	walletMinSol := envF64("WALLET_MIN_SOL", 0.02)
	poll := envDurMs("POLL_MS", 5000*time.Millisecond)
	rescan := envDurSecs("RESCAN_SECS", 300*time.Second)
	watchRatio := envF64("WATCH_RATIO", 0.85)
	slippageBps := uint32(envInt("SLIPPAGE_BPS", 100))
	senderURL := os.Getenv("SENDER_URL")
	if senderURL == "" {
		senderURL = "http://ams-sender.helius-rpc.com/fast"
	}
	tipAccountStr := os.Getenv("SENDER_TIP_ACCOUNT")
	if tipAccountStr == "" {
		tipAccountStr = "2nyhqdwKcJZR2vcqCyrYsaPVdAnFoJjiksCXJ7hfEYgD"
	}
	tipAccount := solana.MustPublicKeyFromBase58(tipAccountStr)
	var webhook *string
	if w := os.Getenv("ALERT_WEBHOOK"); w != "" {
		webhook = &w
	}
	liquidatorMAStr := os.Getenv("LIQUIDATOR_MA")
	if liquidatorMAStr == "" {
		liquidatorMAStr = defaultLiquidatorMA
	}
	liquidatorMA := solana.MustPublicKeyFromBase58(liquidatorMAStr)
	tp := solana.MustPublicKeyFromBase58(tokenProgramStr)

	var kp *solana.PrivateKey
	if p := os.Getenv("KEYPAIR_PATH"); p != "" {
		raw, err := os.ReadFile(p)
		if err != nil {
			panic("read keypair: " + err.Error())
		}
		var bytesArr []byte
		if err := json.Unmarshal(raw, &bytesArr); err != nil {
			panic("parse keypair: " + err.Error())
		}
		k := solana.PrivateKey(bytesArr)
		kp = &k
	}
	if kp == nil && !dryRun {
		panic("LIVE needs KEYPAIR_PATH")
	}
	var authority solana.PublicKey
	if kp != nil {
		authority = kp.PublicKey()
	} else {
		authorityStr := os.Getenv("AUTHORITY")
		if authorityStr == "" {
			authorityStr = defaultAuthority
		}
		authority = solana.MustPublicKeyFromBase58(authorityStr)
	}

	// Optional Pyth Lazer pre-positioning: when PYTH_LAZER_TOKEN is set, blend
	// Lazer's ms-latency major prices over the on-chain oracle in the
	// watch-set recompute so the loop ARMS accounts about to cross the
	// threshold ahead of the on-chain crank. The FIRE decision stays gated by
	// full on-chain sim — Lazer only steers which accounts we spend sim
	// budget on.
	lazerTable := lazer.NewPriceTable()
	lazerMintMap := lazer.MintFeedMap()
	lazerOn := false
	ctx := context.Background()
	if token := os.Getenv("PYTH_LAZER_TOKEN"); token != "" {
		lazer.SpawnLazerThread(ctx, token, lazer.ArmFeedIDs(), lazerTable, nil)
		lazerOn = true
		fmt.Fprintln(os.Stderr, "[exec] Pyth Lazer pre-positioning ENABLED")
	}

	// Self-crank context: hot Hermes blob + Jito tip accounts. The edge only
	// triggers with Lazer on (that's what detects the true-price cross); the
	// fallback tip list keeps DRY_RUN sims working if the fetch fails.
	crankOn := true
	if v := os.Getenv("CRANK"); v != "" {
		crankOn = v != "0"
	}
	blockEngine := jito.DefaultBlockEngine()
	var tips []solana.PublicKey
	if crankOn {
		if t, err := jito.GetTipAccounts(blockEngine); err == nil {
			tips = t
		}
	}
	if crankOn && len(tips) == 0 {
		fmt.Fprintln(os.Stderr, "[exec] getTipAccounts failed — using fallback Jito tip list")
		tips = []solana.PublicKey{solana.MustPublicKeyFromBase58("DttWaMuVvTiduZRnguLF7jNxTgiMBZ1hyAumKUiL2KRL")}
	}
	hermesURL := os.Getenv("HERMES")
	if hermesURL == "" {
		hermesURL = "https://hermes.pyth.network"
	}
	maxBlobMs := envDurMs("MAX_BLOB_AGE_MS", 3000*time.Millisecond)
	crank := &crankCtx{
		on:          crankOn,
		hermes:      pyth.SpawnHermesCache(hermesURL, nil, 400*time.Millisecond),
		tips:        tips,
		blockEngine: blockEngine,
		maxBlobAge:  maxBlobMs,
	}
	fmt.Fprintf(os.Stderr, "[exec] self-crank mode: %s\n", onOff(crank.on))

	fmt.Fprintf(os.Stderr, "[exec] marginfi liquidation executor %s  authority=%s  min_profit=$%g  poll=%s rescan=%s  lazer=%v\n",
		dryRunTag(dryRun), authority, minProfit, poll, rescan, lazerOn)
	if !dryRun {
		bal := solBalance(endpoint, authority.String())
		fmt.Fprintf(os.Stderr, "[exec] wallet balance: %g SOL\n", bal)
		if bal < walletMinSol {
			panic(fmt.Sprintf("wallet below floor %g", walletMinSol))
		}
	}

	mintFeed := lazer.MintFeedMap()
	lazerDirect := lazer.OneToOneMints()
	sc, ok := fullScan(endpoint)
	if !ok {
		panic("initial scan failed")
	}
	lastScan := time.Now()
	var watch []solana.PublicKey
	engine := liquidation.NewEngine(minCollateral)
	dailyTipSol := &dailyTipCounter{}
	tipDay := nowSecs() / 86_400
	mintTPCache := map[solana.PublicKey]solana.PublicKey{}
	simCooldown := envDurSecs("SIM_COOLDOWN_SECS", 60*time.Second)

	ex := &executor{
		endpoint: endpoint, runDir: runDir, crank: crank, senderURL: senderURL, mintTP: mintTPCache,
		cfg: cfg{
			liquidatorMA: liquidatorMA, authority: authority, tp: tp, tipAccount: tipAccount,
			tipFractionBps: tipFractionBps, minTipSol: minTipSol, minProfit: minProfit, slippageBps: slippageBps,
		},
	}

	// Ladder-rejected candidates (emode phantoms) re-sim at most once per
	// cooldown — they'd otherwise burn 5 gate sims every poll, forever.
	// Refused accounts carry a strike count; the cooldown DOUBLES per strike
	// (capped at 1h).
	type rejectEntry struct {
		at      time.Time
		strikes uint32
	}
	simRejected := map[solana.PublicKey]rejectEntry{}
	simBackoff := func(strikes uint32) time.Duration {
		mult := uint32(1)
		shift := strikes
		if shift == 0 {
			shift = 0
		} else {
			shift--
		}
		if shift > 6 {
			shift = 6
		}
		mult <<= shift
		d := simCooldown * time.Duration(mult)
		if d > time.Hour {
			d = time.Hour
		}
		return d
	}
	coolingDown := func(pk solana.PublicKey) bool {
		e, ok := simRejected[pk]
		return ok && time.Since(e.at) < simBackoff(e.strikes)
	}
	markRejected := func(pk solana.PublicKey) {
		e := simRejected[pk]
		simRejected[pk] = rejectEntry{at: time.Now(), strikes: e.strikes + 1}
	}

	// After handling a crossed account (fired or gated) don't re-process it
	// for this long — a persistently-crossed account would otherwise spin
	// every tick.
	handleCooldown := envDurSecs("HANDLE_COOLDOWN_SECS", 20*time.Second)
	handled := map[solana.PublicKey]time.Time{}
	var lastTickUs int64
	tickPollMs := time.Duration(envInt("TICK_POLL_MS", 1)) * time.Millisecond
	first := true

	// Pre-built fire-tx cache: armed accounts (ratio ≥ ARM_RATIO) get a hot,
	// sim-verified tx so a cross → sign+send with no build/quote/sim on the
	// critical path. ARM_RATIO < 1.0 so the tx is ready BEFORE the cross.
	armRatio := envF64("ARM_RATIO", 0.97)
	armTTL := envDurSecs("ARM_TTL_SECS", 20*time.Second)
	// Per-cycle sim caps: cap the arm/fire work to the top-K by USD deficit
	// so the sim budget always reaches the biggest real opportunities first
	// and never floods RPC or starves.
	maxArm := envInt("MAX_ARM_PER_CYCLE", 8)
	maxFire := envInt("MAX_FIRE_PER_CYCLE", 4)
	cache := map[solana.PublicKey]*cachedFire{}
	var freshBH solana.Hash
	lastBH := time.Now().Add(-9999 * time.Second)
	// Heartbeat cadence: the event-driven loop is otherwise silent between
	// the 5-min rescans, so a healthy-but-calm bot looks identical to a hung
	// one or a dead Lazer feed. HEARTBEAT_SECS=0 disables.
	hbEvery := envDurSecs("HEARTBEAT_SECS", 30*time.Second)
	lastHB := time.Now().Add(-9999 * time.Second)
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
			prices, _ := lazer.Blend(sc.banks, base, lazerTable, lazerMintMap)
			// Only track accounts the fire path can act on; non-fireable
			// shapes would otherwise inflate the counts and starve
			// deficit-ranking.
			var fireable []liquidation.AccountEntry
			for _, e := range sc.accts {
				if isV1Fireable(e.Account, sc.banks) {
					fireable = append(fireable, e)
				}
			}
			watch = watch[:0]
			for _, e := range fireable {
				r := liquidation.MaintenanceHealth(e.Account, sc.banks, prices)
				if r.Missing == 0 && r.Health.Ratio() >= watchRatio && r.Health.WeightedAssets >= minCollateral {
					watch = append(watch, e.Pubkey)
				}
			}
			// Engine (event-driven trigger): coefficients over the on-chain
			// baseline; Lazer feeds move health between rescans with no RPC.
			lazerSnapshot := map[uint32]float64{}
			for _, f := range lazer.ArmFeedIDs() {
				if p, ok := lazerTable.Get(f); ok {
					lazerSnapshot[f] = p.Price
				}
			}
			armed := engine.Rebuild(fireable, sc.banks, base, mintFeed, lazerDirect, lazerSnapshot, watchRatio)
			fmt.Fprintf(os.Stderr, "[exec] scan: %d borrowers → %d fireable-shaped → watch-set %d (ratio ≥ %g), engine armed %d\n",
				len(sc.accts), len(fireable), len(watch), watchRatio, armed)
			// Point the Hermes cache at the feeds we could actually need to
			// crank: crankable asset banks held by watch-set accounts.
			if crank.on {
				watchSet := map[solana.PublicKey]struct{}{}
				for _, pk := range watch {
					watchSet[pk] = struct{}{}
				}
				feedSet := map[[32]byte]struct{}{}
				for _, e := range sc.accts {
					if _, in := watchSet[e.Pubkey]; !in {
						continue
					}
					for _, b := range e.Account.Balances {
						if b.AssetShares <= 0 {
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
				// Boot warm-up: block briefly (bounded) for the first blob so
				// the engine never evaluates blind at startup.
				if first && wantBlob {
					warmStart := time.Now()
					for {
						if _, _, ok := crank.hermes.Latest(); ok {
							break
						}
						if time.Since(warmStart) >= 5*time.Second {
							break
						}
						time.Sleep(50 * time.Millisecond)
					}
					_, _, ready := crank.hermes.Latest()
					fmt.Fprintf(os.Stderr, "[exec] hermes warm-up: blob %s after %s\n",
						readyTag(ready), time.Since(warmStart))
				}
			}
			first = false
		}

		day := nowSecs() / 86_400
		if day != tipDay {
			tipDay = day
			dailyTipSol.reset()
		}

		// Keep a recent blockhash hot so a fire stamps it without an RPC on
		// the critical path (refresh off the hot path, ~2s cadence).
		if time.Since(lastBH) >= 2*time.Second {
			if bh, ok := latestBlockhash(endpoint); ok {
				freshBH = bh
				lastBH = time.Now()
			}
		}

		// Trigger: event-driven on a Lazer tick (in-memory, no RPC) when the
		// feed is live; else fall back to the on-chain poll over the
		// watch-set.
		var toEval []solana.PublicKey
		snap := map[uint32]float64{}
		if lazerOn {
			deadline := time.Now().Add(poll)
			for {
				var cur int64
				for _, f := range lazer.ArmFeedIDs() {
					if p, ok := lazerTable.Get(f); ok && int64(p.TsUs) > cur {
						cur = int64(p.TsUs)
					}
				}
				if cur > lastTickUs {
					lastTickUs = cur
					break
				}
				if time.Now().After(deadline) {
					break
				}
				time.Sleep(tickPollMs)
			}
			for _, f := range lazer.ArmFeedIDs() {
				if p, ok := lazerTable.Get(f); ok {
					snap[f] = p.Price
				}
			}
			// Rank crossed accounts by USD deficit, fire only the top
			// MAX_FIRE this cycle (deepest-underwater first).
			ranked := engine.CrossedRanked(snap, 1.0)
			var filtered []solana.PublicKey
			for _, r := range ranked {
				if t, ok := handled[r.Pubkey]; ok && time.Since(t) < handleCooldown {
					continue
				}
				if coolingDown(r.Pubkey) {
					continue
				}
				filtered = append(filtered, r.Pubkey)
			}
			if len(filtered) > maxFire {
				fireDeferred = len(filtered) - maxFire
				toEval = filtered[:maxFire]
			} else {
				fireDeferred = 0
				toEval = filtered
			}
		} else {
			time.Sleep(poll)
			toEval = append(toEval, watch...)
		}

		// Heartbeat: prove liveness + show how close the market is.
		if lazerOn && hbEvery > 0 && time.Since(lastHB) >= hbEvery {
			totalFeeds := len(lazer.ArmFeedIDs())
			near := len(engine.Crossed(snap, armRatio))
			crossing := len(engine.Crossed(snap, 1.0))
			defer_ := ""
			if fireDeferred+armDeferred > 0 {
				defer_ = fmt.Sprintf(" | DEFERRED fire %d/arm %d (raise MAX_*_PER_CYCLE)", fireDeferred, armDeferred)
			}
			var freshest int64
			for _, f := range lazer.ArmFeedIDs() {
				if p, ok := lazerTable.Get(f); ok && int64(p.TsUs) > freshest {
					freshest = int64(p.TsUs)
				}
			}
			lagMs := (nowUs() - freshest) / 1000
			fmt.Fprintf(os.Stderr, "[hb] lazer feeds %d/%d live | detect_lag %dms | %d within arm(%g) | %d liquidatable now | cache %d%s | %s\n",
				len(snap), totalFeeds, lagMs, near, armRatio, crossing, len(cache), defer_, lazer.Status(lazerTable))
			lastHB = time.Now()
		}

		// ── ARM phase (lazer mode only): keep a hot, sim-verified fire tx
		// for accounts near the threshold (ratio ≥ armRatio) so the cross →
		// send is instant.
		if lazerOn {
			armRanked := engine.CrossedRanked(snap, armRatio)
			armKeys := map[solana.PublicKey]struct{}{}
			for _, r := range armRanked {
				armKeys[r.Pubkey] = struct{}{}
			}
			for pk, c := range cache {
				if _, ok := armKeys[pk]; !ok || time.Since(c.built) >= armTTL {
					delete(cache, pk)
				}
			}
			var candidates []solana.PublicKey
			for _, r := range armRanked {
				if _, ok := cache[r.Pubkey]; ok {
					continue
				}
				if coolingDown(r.Pubkey) {
					continue
				}
				candidates = append(candidates, r.Pubkey)
			}
			var need []solana.PublicKey
			if len(candidates) > maxArm {
				armDeferred = len(candidates) - maxArm
				need = candidates[:maxArm]
			} else {
				armDeferred = 0
				need = candidates
			}
			if len(need) > 0 {
				raw := getMultiple(endpoint, need)
				base := freshPrices(endpoint, sc.banks, sc.oracleOf)
				prices, _ := lazer.Blend(sc.banks, base, lazerTable, lazerMintMap)
				for _, pk := range need {
					r, ok := raw[pk]
					if !ok {
						continue
					}
					a, ok := liquidation.DecodeMarginfiAccount(r)
					if !ok {
						continue
					}
					if c := ex.tryArm(sc, a, pk, prices, base); c != nil {
						delete(simRejected, pk)
						cache[pk] = c
					} else {
						markRejected(pk)
					}
				}
			}
		}

		// Drop accounts handled recently (avoid per-tick spin on a standing cross).
		var filteredEval []solana.PublicKey
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
		// (instant); else arm it inline now.
		freshRaw := getMultiple(endpoint, toEval)
		base := freshPrices(endpoint, sc.banks, sc.oracleOf)
		prices, _ := lazer.Blend(sc.banks, base, lazerTable, lazerMintMap)
		for _, pk := range toEval {
			handled[pk] = time.Now()
			var cached *cachedFire
			if c, ok := cache[pk]; ok && time.Since(c.built) < armTTL {
				delete(cache, pk)
				cached = c
			} else {
				r, ok := freshRaw[pk]
				if !ok {
					continue
				}
				a, ok := liquidation.DecodeMarginfiAccount(r)
				if !ok {
					continue
				}
				hr := liquidation.MaintenanceHealth(a, sc.banks, prices)
				if hr.Missing > 0 || !hr.Health.Liquidatable() || hr.Health.WeightedAssets < minCollateral {
					continue
				}
				if c := ex.tryArm(sc, a, pk, prices, base); c != nil {
					delete(simRejected, pk)
					cached = c
				} else {
					markRejected(pk)
					continue
				}
			}
			// True iff the fire tx was armed AHEAD of time by the ARM phase
			// (built some nonzero time ago, within TTL) rather than inline
			// just now by this FIRE phase (elapsed ~0).
			builtElapsed := time.Since(cached.built)
			armedFromCache := builtElapsed < armTTL && builtElapsed > 0
			fireStart := nowUs()
			ex.fireCached(dryRun, pk, cached, freshBH, kp, dailyTipSol, maxDailyTipSol, walletMinSol, webhook)
			// Latency ledger: from the Lazer publish that made this cross
			// (lastTickUs) to detection (loop) to fire submit.
			done := nowUs()
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

func onOff(b bool) string {
	if b {
		return "ENABLED"
	}
	return "off"
}
func dryRunTag(b bool) string {
	if b {
		return "[DRY RUN]"
	}
	return "[LIVE]"
}
func readyTag(b bool) string {
	if b {
		return "READY"
	}
	return "still pending (continuing)"
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
