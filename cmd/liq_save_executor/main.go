// Production Save (Solend) liquidation executor — EVENT-DRIVEN, DRY_RUN default.
//
// The old build polled stored on-chain health every 30s. That lost every race:
// the census found 45 USDC-debt Solend liquidations in 48h and we caught 0,
// because competitors react to the oracle in milliseconds. This rewrite mirrors
// the marginfi executor's architecture — a Lazer WebSocket feeds an in-memory
// health engine (internal/save's Engine) that recomputes every obligation's
// borrowed/unhealthy on each ~ms price tick with ZERO RPC, so a cross is
// noticed in ~ms not ~30s.
//
//	full scan (RESCAN_SECS): v1 (1 collateral / 1 debt, debt ∈ {USDC,USDT,wSOL}) obligations →
//	  save_engine watch-set (stored on-chain health + per-side Lazer anchors)
//	Lazer tick (TICK_POLL_MS in-memory poll): the trigger to RE-CHECK, not the
//	  liquidatable verdict — Lazer leads/diverges from the on-chain Pyth price
//	FIRE tier (TWO-TIER GATING): Lazer NARROWS the watch-set; the ON-CHAIN
//	  oracle price GATES the expensive sim. Only obligations liquidatable at the
//	  on-chain price Solend settles against (stored health from the last rescan,
//	  ZERO Lazer projection) earn a sim, ranked by USD deficit, capped top-K
//	  (MAX_FIRE_PER_CYCLE). Gating on the Lazer-projected ratio instead flooded
//	  ~390 phantoms/cycle through simulateTransaction/Bundle (healthy on-chain).
//	ARM those FIRE-tier candidates: pre-build+size+sim the fire tx → hot cache
//	FIRE on tick: stamp fresh blockhash, sign, submit (no build/quote/sim on
//	  the critical path)
//
// Two fire modes, exactly like marginfi:
//
//	Sender — obligation already liquidatable at ON-CHAIN prices → single tx via
//	  Helius Sender.
//	Crank  — underwater at the true (Lazer) price but Solend hasn't cranked its
//	  Pyth feed yet → atomic Jito bundle [crank_setup, crank_fire, fire] that
//	  posts the fresh price then liquidates. Save reserves read the SAME shard-0
//	  sponsored feeds we crank, so refresh_reserve inside the fire tx picks up
//	  the cranked price. Sizing + ground truth run through simulateBundle.
//
// Profit-or-revert (payback_all fails unless the swap covered the borrow), so a
// losing fire that lands costs only the base fee; a failing bundle never lands.
//
// Usage: HELIUS_RPC=<url> [DRY_RUN=1] [KEYPAIR_PATH=~/arb-keypair.json]
//
//	[PYTH_LAZER_TOKEN=… (required for event-driven + crank)] [CRANK=1]
//	[MIN_DEBT_USD=100] [MIN_PROFIT_USD=0.5] [REPAY_FRACS=0.2,0.1,0.05]
//	[WATCH_RATIO=0.85] [ARM_RATIO=0.97] [RESCAN_SECS=30] [TICK_POLL_MS=1]
//	[MAX_ARM_PER_CYCLE=8] [MAX_FIRE_PER_CYCLE=4] [SLIPPAGE_BPS=100]
//	[MAX_SWAP_ACCOUNTS=18] [MAX_BLOB_AGE_MS=3000] go run ./cmd/liq_save_executor
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
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
	"solana-arb-backtest-go/internal/observe"
	"solana-arb-backtest-go/internal/pyth"
	"solana-arb-backtest-go/internal/save"
)

const (
	defaultAuthority = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak"
	// classicTokenProgram is every Save main-pool debt mint's (USDC/USDT/wSOL) token program.
	classicTokenProgram = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	// lazerUSDT is the Pyth Lazer USDT/USD numeric feed id (verified against
	// the Lazer symbol registry; consistent with SOL=6 / USDC=7). wSOL debt
	// already maps to the SOL feed (6). Added to the executor's local feed
	// set so USDT-debt obligations are subscribed + tracked without editing
	// the shared lazer map.
	lazerUSDT uint32 = 8
)

// armFeeds is the feed ids the executor subscribes/snapshots: the shared
// majors + USDT.
func armFeeds() []uint32 {
	v := lazer.ArmFeedIDs()
	for _, f := range v {
		if f == lazerUSDT {
			return v
		}
	}
	return append(v, lazerUSDT)
}

// mintFeedExt is mint → Lazer feed, the shared map extended with USDT (→
// feed 8) so a USDT debt side is priced by Lazer like USDC is.
func mintFeedExt() map[solana.PublicKey]uint32 {
	m := lazer.MintFeedMap()
	out := make(map[solana.PublicKey]uint32, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out[solana.MustPublicKeyFromBase58(save.USDTMint)] = lazerUSDT
	return out
}

func nowTs() uint64 { return uint64(time.Now().Unix()) }
func nowUs() uint64 { return uint64(time.Now().UnixMicro()) }

// logLatency appends a latency-ledger row — proves whether SPEED is (still)
// the bottleneck. appeared_us is the Lazer PUBLISH timestamp of the tick that
// made the obligation cross; the deltas measure detect + submit lag from that
// instant. → {run_dir}/latency.jsonl
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
	fmt.Fprintln(f, string(b))
}

var httpClient = &http.Client{Timeout: 20 * time.Second}

func rpc(endpoint string, body map[string]any) (map[string]any, bool) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, false
	}
	for attempt := 0; attempt < 4; attempt++ {
		resp, err := httpClient.Post(endpoint, "application/json", bytes.NewReader(b))
		if err == nil {
			raw, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr == nil {
				var v map[string]any
				if json.Unmarshal(raw, &v) == nil {
					return v, true
				}
			}
		}
		time.Sleep(time.Duration(400<<attempt) * time.Millisecond)
	}
	return nil, false
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func asArray(v any) []any        { a, _ := v.([]any); return a }
func asStr(v any) string         { s, _ := v.(string); return s }

func b64(d any) ([]byte, bool) {
	arr := asArray(d)
	if len(arr) == 0 {
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

func b64tx(tx *solana.Transaction) string {
	s, err := tx.ToBase64()
	if err != nil {
		return ""
	}
	return s
}

func getAcct(endpoint string, pk solana.PublicKey) ([]byte, bool) {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{pk.String(), map[string]any{"encoding": "base64"}}})
	if !ok {
		return nil, false
	}
	return b64(asMap(asMap(v["result"])["value"])["data"])
}

func getMultiple(endpoint string, keys []solana.PublicKey) map[solana.PublicKey][]byte {
	out := map[solana.PublicKey][]byte{}
	for start := 0; start < len(keys); start += 100 {
		end := start + 100
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[start:end]
		strs := make([]string, len(chunk))
		for i, k := range chunk {
			strs[i] = k.String()
		}
		v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getMultipleAccounts",
			"params": []any{strs, map[string]any{"encoding": "base64"}}})
		if !ok {
			continue
		}
		values := asArray(asMap(v["result"])["value"])
		for i, accV := range values {
			acc := asMap(accV)
			if acc == nil {
				continue
			}
			if raw, ok := b64(acc["data"]); ok {
				out[chunk[i]] = raw
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
	owner := asStr(asMap(asMap(v["result"])["value"])["owner"])
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
	s := asStr(asMap(asMap(v["result"])["value"])["blockhash"])
	if s == "" {
		return solana.Hash{}, false
	}
	h, err := solana.HashFromBase58(s)
	if err != nil {
		return solana.Hash{}, false
	}
	return h, true
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

func simulateOk(endpoint, txB64 string) bool {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "simulateTransaction",
		"params": []any{txB64, map[string]any{"sigVerify": false, "replaceRecentBlockhash": true, "commitment": "processed", "encoding": "base64"}}})
	if !ok {
		return false
	}
	val := asMap(asMap(v["result"])["value"])
	if val == nil {
		return false
	}
	return val["err"] == nil
}

// simulateBundleRanOk returns how many leading txs of a bundle succeed (jito
// stops at the first revert). For [setup, fire, save_fire] ranOk==3 =
// accepted, <2 = crank broke. ok=false if the RPC rejected the sim outright.
func simulateBundleRanOk(endpoint string, txsB64 []string) (int, bool) {
	nulls := make([]any, len(txsB64))
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "simulateBundle",
		"params": []any{
			map[string]any{"encodedTransactions": txsB64},
			map[string]any{
				"skipSigVerify":                true,
				"replaceRecentBlockhash":       true,
				"preExecutionAccountsConfigs":  nulls,
				"postExecutionAccountsConfigs": nulls,
			},
		}})
	if !ok {
		return 0, false
	}
	if e, present := v["error"]; present && e != nil {
		return 0, false
	}
	results := asArray(asMap(asMap(v["result"])["value"])["transactionResults"])
	ranOk := 0
	for _, r := range results {
		if asMap(r)["err"] != nil {
			break
		}
		ranOk++
	}
	return ranOk, true
}

type decisionLog struct {
	T             uint64  `json:"t"`
	Obligation    string  `json:"obligation"`
	Protocol      string  `json:"protocol"`
	Mode          string  `json:"mode"`
	DebtUSD       float64 `json:"debt_usd"`
	Ratio         float64 `json:"ratio"`
	RepayNative   uint64  `json:"repay_native"`
	QuotedUSDCOut float64 `json:"quoted_usdc_out"`
	EstProfitUSDC float64 `json:"est_profit_usdc"`
	Fired         bool    `json:"fired"`
	Reason        string  `json:"reason"`
}

type tradeLog struct {
	T             uint64   `json:"t"`
	Obligation    string   `json:"obligation"`
	Protocol      string   `json:"protocol"`
	RepayNative   uint64   `json:"repay_native"`
	EstProfitUSDC float64  `json:"est_profit_usdc"`
	TipLamports   uint64   `json:"tip_lamports"`
	Signature     *string  `json:"signature"`
	Bundle        *string  `json:"bundle"`
	RealizedUSDC  *float64 `json:"realized_usdc"`
	Error         *string  `json:"error"`
}

// fireMode describes how a cached fire gets submitted.
type fireMode struct {
	// crank is true for the Jito-bundle [crank_setup, crank_fire, save_fire]
	// path (underwater at the true Lazer price only); false = Sender
	// (already liquidatable at on-chain prices).
	crank  bool
	feedID [32]byte
}

func (m fireMode) name() string {
	if m.crank {
		return "crank"
	}
	return "sender"
}

// crankCtx is everything the crank path needs, spun up once at boot (shared
// with marginfi's design).
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
	return c.tips[nowTs()%uint64(len(c.tips))], true
}

// saveScan is a full scan: v1 accepted-debt (USDC/USDT/wSOL) obligations +
// the reserves/oracle metadata they touch.
type saveScan struct {
	obls      []save.ObligationEntry
	reserves  map[solana.PublicKey]*save.Reserve    // collateral reserves (+ the debt reserves)
	ctpOf     map[solana.PublicKey]solana.PublicKey // collateral liquidity mint → token program
	feedOf    map[solana.PublicKey][32]byte         // collateral reserve → 32-byte Pyth feed id
	crankable map[solana.PublicKey]bool             // collateral reserves whose pyth_oracle is the shard-0 sponsored PDA
}

// fullScanSave scans obligations (full), keeps v1 / debt in
// {USDC,USDT,wSOL} / ≥ minDebt, then loads their collateral reserves +
// oracle crank metadata. The debt reserves are passed in pre-decoded
// (stable accounts).
func fullScanSave(endpoint string, debtReserves map[solana.PublicKey]*save.Reserve, minDebt float64, ctpCache map[solana.PublicKey]solana.PublicKey) *saveScan {
	// One getProgramAccounts per scanned pool (memcmp matches a single
	// value), merged. The obligation's own lending_market flows through to
	// the fire tx, so multi-pool needs no fire-path change — just these
	// obligations + the pools' debt reserves in debtReserves.
	var entries []any
	for _, pool := range save.ScanPools {
		resp, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
			"params": []any{save.SolendProgram, map[string]any{"encoding": "base64", "dataSize": 1300,
				"filters": []any{
					map[string]any{"dataSize": 1300},
					map[string]any{"memcmp": map[string]any{"offset": 10, "bytes": pool}},
				}}}})
		if !ok {
			continue
		}
		entries = append(entries, asArray(resp["result"])...)
	}
	if len(entries) == 0 {
		return nil
	}
	var obls []save.ObligationEntry
	for _, ev := range entries {
		e := asMap(ev)
		pk, err := solana.PublicKeyFromBase58(asStr(e["pubkey"]))
		if err != nil {
			continue
		}
		d, ok := b64(asMap(e["account"])["data"])
		if !ok {
			continue
		}
		o, ok := save.DecodeObligation(d)
		if !ok {
			continue
		}
		if len(o.Deposits) != 1 || len(o.Borrows) != 1 { // v1 shape (fire path)
			continue
		}
		if _, ok := debtReserves[o.Borrows[0].Reserve]; !ok { // accepted debt only
			continue
		}
		if o.BorrowedValue < minDebt {
			continue
		}
		obls = append(obls, save.ObligationEntry{Pubkey: pk, Obligation: o})
	}

	// Load the distinct collateral reserves referenced.
	seen := map[solana.PublicKey]bool{}
	var collPks []solana.PublicKey
	for _, e := range obls {
		r := e.Obligation.Deposits[0].Reserve
		if !seen[r] {
			seen[r] = true
			collPks = append(collPks, r)
		}
	}
	reserves := make(map[solana.PublicKey]*save.Reserve, len(debtReserves)+len(collPks))
	for k, v := range debtReserves {
		reserves[k] = v
	}
	for pk, raw := range getMultiple(endpoint, collPks) {
		if r, ok := save.DecodeReserve(pk, raw); ok {
			reserves[pk] = r
		}
	}

	// Collateral-mint → token program (for the redeem ATA).
	ctpOf := map[solana.PublicKey]solana.PublicKey{}
	for _, pk := range collPks {
		r, ok := reserves[pk]
		if !ok {
			continue
		}
		tp, ok := ctpCache[r.LiquidityMint]
		if !ok {
			tp, ok = mintOwner(endpoint, r.LiquidityMint)
			if !ok {
				continue
			}
			ctpCache[r.LiquidityMint] = tp
		}
		ctpOf[r.LiquidityMint] = tp
	}

	// Oracle crank metadata: decode each collateral reserve's pyth_oracle →
	// feed id, and mark crankable when the oracle IS that feed's shard-0
	// sponsored PDA.
	oracleSeen := map[solana.PublicKey]bool{}
	var oraclePks []solana.PublicKey
	for _, pk := range collPks {
		if r, ok := reserves[pk]; ok && !oracleSeen[r.PythOracle] {
			oracleSeen[r.PythOracle] = true
			oraclePks = append(oraclePks, r.PythOracle)
		}
	}
	oracleRaw := getMultiple(endpoint, oraclePks)
	feedOf := map[solana.PublicKey][32]byte{}
	crankable := map[solana.PublicKey]bool{}
	for _, pk := range collPks {
		r, ok := reserves[pk]
		if !ok {
			continue
		}
		raw, ok := oracleRaw[r.PythOracle]
		if !ok {
			continue
		}
		fid, _, _, ok := liquidation.DecodePriceUpdateV2(raw)
		if !ok {
			continue
		}
		feedOf[pk] = fid
		if pyth.SponsoredFeed(0, fid).Equals(r.PythOracle) {
			crankable[pk] = true
		}
	}
	return &saveScan{obls: obls, reserves: reserves, ctpOf: ctpOf, feedOf: feedOf, crankable: crankable}
}

type cfg struct {
	authority       solana.PublicKey
	tipAccount      solana.PublicKey
	tipFractionBps  uint64
	minTipSol       float64
	minProfit       float64
	slippageBps     uint32
	maxSwapAccounts int
}

// cachedFire is a sim-verified fire tx kept hot for an armed obligation.
// Compiled with a placeholder blockhash (sim replaces it); the real hash is
// stamped at fire.
type cachedFire struct {
	tx          *solana.Transaction
	mode        fireMode
	tipLamports uint64
	tipSol      float64
	estProfit   float64
	repay       uint64
	debtUSD     float64
	ratio       float64
	built       time.Time
}

func logSkip(runDir string, pk solana.PublicKey, mode string, debt, ratio float64, reason string) {
	observe.LogDecision(runDir, decisionLog{
		T: nowTs(), Obligation: pk.String(), Protocol: "save", Mode: mode,
		DebtUSD: debt, Ratio: ratio, Reason: reason,
	})
}

// tryArm builds + sizes + profit-gates + full-sim-gates one obligation →
// *cachedFire. Mirrors the marginfi try_arm: mode from on-chain vs Lazer
// health, size by a sim ladder (bundle sim in crank mode so the chain judges
// at the cranked price), profit gate, ground-truth sim. This is the only
// place a fire tx is built; the sim lives here (arm time), off the fire
// critical path.
func tryArm(
	endpoint, runDir string, c *cfg, crank *crankCtx, scan *saveScan,
	pk solana.PublicKey, repayFracs []float64, engineRatio float64,
) *cachedFire {
	// Fresh obligation (health may have moved since scan) + its collateral reserve.
	raw, ok := getAcct(endpoint, pk)
	if !ok {
		return nil
	}
	o, ok := save.DecodeObligation(raw)
	if !ok || len(o.Deposits) != 1 || len(o.Borrows) != 1 {
		return nil
	}
	collPk := o.Deposits[0].Reserve
	coll, ok := scan.reserves[collPk]
	if !ok {
		return nil
	}
	ctp, ok := scan.ctpOf[coll.LiquidityMint]
	if !ok {
		return nil
	}
	// The obligation's actual debt reserve (USDC/USDT/wSOL) — prices the
	// repay and is the flash-borrow/swap-target asset.
	debtReserve, ok := scan.reserves[o.Borrows[0].Reserve]
	if !ok {
		return nil
	}
	debtDec := pow10(int(debtReserve.MintDecimals))
	debtTp := solana.MustPublicKeyFromBase58(classicTokenProgram)
	debtUSD := o.BorrowedValue

	// Mode: liquidatable at FRESH on-chain prices (the value Solend's
	// `liquidate` recomputes at settle time — same cToken-exchange-rate math
	// the fire tier gates on, so routing stays consistent and never drops a
	// genuine fire) → Sender. Else the Lazer engine flagged it but Solend
	// hasn't cranked yet → crank + liquidate bundle (needs a crankable
	// oracle + a fresh Hermes blob).
	var mode fireMode
	if o.FreshLiquidatable(scan.reserves) {
		mode = fireMode{crank: false}
	} else {
		if !crank.on {
			return nil
		}
		if !scan.crankable[collPk] {
			logSkip(runDir, pk, "crank", debtUSD, engineRatio, "flagged at Lazer price but healthy on-chain and collateral oracle not crankable — cannot act")
			return nil
		}
		feedID, ok := scan.feedOf[collPk]
		if !ok {
			logSkip(runDir, pk, "crank", debtUSD, engineRatio, "crankable but feed id missing")
			return nil
		}
		if _, _, _, ok := crank.hermes.UpdateFor(feedID); !ok {
			logSkip(runDir, pk, "crank", debtUSD, engineRatio, "crankable but no fresh Hermes blob for feed yet")
			return nil
		}
		mode = fireMode{crank: true, feedID: feedID}
	}

	// Crank txs for the sizing/ground-truth bundle (placeholder blockhash —
	// sims replace it; the LIVE fire rebuilds from the freshest blob).
	var crankSetupB64, crankFireB64 string
	haveCrankTxs := false
	if mode.crank {
		update, vaa, _, ok := crank.hermes.UpdateFor(mode.feedID)
		if !ok {
			return nil
		}
		txs, err := pyth.BuildCrankTxs(c.authority, vaa, []pyth.MerkleUpdate{update}, 0, 0, solana.Hash{})
		if err != nil {
			return nil
		}
		crankSetupB64, crankFireB64, err = txs.ToB64()
		if err != nil {
			return nil
		}
		haveCrankTxs = true
	}

	// The in-tx tip destination differs by mode: a Sender fire tips a
	// Helius Sender wallet; a crank fire rides a Jito bundle and must tip a
	// Jito account.
	tipTo := c.tipAccount
	if mode.crank {
		t, ok := crank.pickTip()
		if !ok {
			logSkip(runDir, pk, "crank", debtUSD, engineRatio, "no Jito tip accounts")
			return nil
		}
		tipTo = t
	}

	mk := func(repay, seize, tip uint64, bh solana.Hash) *save.FireTx {
		cand := &save.FireCandidate{
			Obligation: pk, RepayReserve: debtReserve, WithdrawReserve: coll,
			CollateralTokenProgram: ctp, DebtTokenProgram: debtTp,
			RepayAmount: repay, SeizeUnderlying: seize,
			DepositReserves: []solana.PublicKey{coll.Reserve},
			BorrowReserves:  []solana.PublicKey{debtReserve.Reserve},
		}
		fire, err := save.BuildFireTx(endpoint, cand, c.authority, &tipTo, tip, 100_000, c.slippageBps, c.maxSwapAccounts, bh)
		if err != nil {
			return nil
		}
		return fire
	}
	// gate: standalone sim (Sender) or bundle sim (crank) so the chain
	// judges at the actionable price.
	gate := func(fire *solana.Transaction) bool {
		if !haveCrankTxs {
			return simulateOk(endpoint, b64tx(fire))
		}
		ranOk, ok := simulateBundleRanOk(endpoint, []string{crankSetupB64, crankFireB64, b64tx(fire)})
		return ok && ranOk == 3
	}

	// Size by simulation ladder — largest repay fraction Solend accepts.
	ph := solana.Hash{}
	var chosenRepay uint64
	var chosenFire *save.FireTx
	for _, frac := range repayFracs {
		repayUSD := debtUSD * frac
		mp := debtReserve.MarketPrice
		if mp < 1e-9 {
			mp = 1e-9
		}
		repayF := repayUSD / mp * debtDec
		if repayF < 1.0 {
			repayF = 1.0
		}
		repay := uint64(repayF)
		seizedUSD := repayUSD * (1.0 + float64(coll.LiquidationBonusPct)/100.0)
		wmp := coll.MarketPrice
		if wmp < 1e-9 {
			wmp = 1e-9
		}
		seize := uint64(seizedUSD / wmp * pow10(int(coll.MintDecimals)))
		fire := mk(repay, seize, 0, ph)
		if fire == nil {
			continue
		}
		if gate(fire.Tx) {
			chosenRepay, chosenFire = repay, fire
			break
		}
	}
	if chosenFire == nil {
		logSkip(runDir, pk, mode.name(), debtUSD, engineRatio, "no repay fraction passed sim (healthy at actionable price / too small)")
		return nil
	}

	// Profit gate — price both legs in the debt asset's decimals + market price.
	repayUSD := float64(chosenRepay) / debtDec * debtReserve.MarketPrice
	usdcOut := float64(chosenFire.QuotedDebtOut) / debtDec * debtReserve.MarketPrice
	estProfit := usdcOut - repayUSD
	const solUsd = 150.0 // conservative; tip is tiny vs profit
	tipSol := estProfit * float64(c.tipFractionBps) / 10_000.0 / solUsd
	if tipSol < c.minTipSol {
		tipSol = c.minTipSol
	}
	tipLamports := uint64(tipSol * 1e9)
	log := decisionLog{
		T: nowTs(), Obligation: pk.String(), Protocol: "save", Mode: mode.name(),
		DebtUSD: debtUSD, Ratio: engineRatio, RepayNative: chosenRepay, QuotedUSDCOut: usdcOut,
		EstProfitUSDC: estProfit,
	}
	if estProfit < c.minProfit+tipSol*solUsd {
		log.Reason = fmt.Sprintf("below min profit (est $%.2f)", estProfit)
		observe.LogDecision(runDir, log)
		return nil
	}

	// Final build WITH the tip, ground-truth sim gate.
	seizedUSD := repayUSD * (1.0 + float64(coll.LiquidationBonusPct)/100.0)
	wmp := coll.MarketPrice
	if wmp < 1e-9 {
		wmp = 1e-9
	}
	seize := uint64(seizedUSD / wmp * pow10(int(coll.MintDecimals)))
	fire := mk(chosenRepay, seize, tipLamports, ph)
	if fire == nil {
		log.Reason = "final build failed"
		observe.LogDecision(runDir, log)
		return nil
	}
	if !gate(fire.Tx) {
		log.Reason = "final fire sim revert (swap/repay would not cover the borrow)"
		observe.LogDecision(runDir, log)
		return nil
	}
	return &cachedFire{
		tx: fire.Tx, mode: mode, tipLamports: tipLamports, tipSol: tipSol,
		estProfit: estProfit, repay: chosenRepay, debtUSD: debtUSD, ratio: engineRatio,
		built: time.Now(),
	}
}

// fireCached fires a cached tx: stamp fresh blockhash, sign, submit (Sender
// or a Jito bundle with freshly-built crank txs), log, spawn P&L readback.
func fireCached(
	endpoint, runDir, senderURL string, c *cfg, crank *crankCtx, dryRun bool,
	pk solana.PublicKey, cached *cachedFire, freshBh solana.Hash, kp *solana.PrivateKey,
	dailyTip *tipCounter, maxDailyTip, walletMin float64, webhook *string,
) {
	mode := cached.mode.name()
	log := decisionLog{
		T: nowTs(), Obligation: pk.String(), Protocol: "save", Mode: mode, DebtUSD: cached.debtUSD,
		Ratio: cached.ratio, RepayNative: cached.repay, EstProfitUSDC: cached.estProfit,
	}
	fmt.Printf("★ SAVE LIQUIDATABLE [%s]  %s  debt $%.0f  repay %d  est profit $%.2f  tip %.5f SOL  (armed %s ago)\n",
		mode, shortStr(pk.String(), 8), cached.debtUSD, cached.repay, cached.estProfit, cached.tipSol, time.Since(cached.built))
	if dryRun {
		log.Reason = fmt.Sprintf("dry-run: would fire (%s, armed)", mode)
		observe.LogDecision(runDir, log)
		observe.Alert(webhook, "save-dry", fmt.Sprintf("DRY-RUN Save %s liquidation %s est profit $%.2f", mode, pk, cached.estProfit))
		return
	}
	if dailyTip.get()+cached.tipSol > maxDailyTip {
		log.Reason = "daily tip cap"
		observe.LogDecision(runDir, log)
		observe.Alert(webhook, "save-cap", "daily tip cap reached")
		return
	}
	if solBalance(endpoint, c.authority.String()) < walletMin {
		log.Reason = "wallet below floor"
		observe.LogDecision(runDir, log)
		observe.Alert(webhook, "save-floor", "wallet below floor — not firing")
		return
	}
	tx := *cached.tx
	tx.Message.RecentBlockhash = freshBh
	kpv := *kp
	kpPub := kpv.PublicKey()
	if _, err := tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(kpPub) {
			return &kpv
		}
		return nil
	}); err != nil {
		log.Reason = "sign failed"
		observe.LogDecision(runDir, log)
		return
	}
	sig := tx.Signatures[0].String()
	txB64 := b64tx(&tx)
	repay, estProfit, tipLamports, tipSol := cached.repay, cached.estProfit, cached.tipLamports, cached.tipSol

	var submitBundleID string
	var submitErr error
	if !cached.mode.crank {
		_, submitErr = jito.SendSender(senderURL, txB64)
	} else {
		submitBundleID, submitErr = func() (string, error) {
			update, vaa, age, ok := crank.hermes.UpdateFor(cached.mode.feedID)
			if !ok {
				return "", fmt.Errorf("no Hermes blob for feed")
			}
			if age > crank.maxBlobAge {
				return "", fmt.Errorf("Hermes blob stale (%v) — not bundling", age)
			}
			ctxs, err := pyth.BuildCrankTxs(c.authority, vaa, []pyth.MerkleUpdate{update}, 0, 0, freshBh)
			if err != nil {
				return "", err
			}
			if err := ctxs.StampAndSign(kpv, freshBh); err != nil {
				return "", err
			}
			setupB64, fireB64, err := ctxs.ToB64()
			if err != nil {
				return "", err
			}
			var last error
			for attempt := 0; attempt < 3; attempt++ {
				id, err := jito.SendBundle(crank.blockEngine, []string{setupB64, fireB64, txB64})
				if err == nil {
					return id, nil
				}
				last = err
				if strings.Contains(err.Error(), "429") && attempt < 2 {
					time.Sleep(250 * time.Millisecond)
					continue
				}
				return "", err
			}
			return "", last
		}()
	}

	log.Fired = submitErr == nil
	log.Reason = fmt.Sprintf("fired (%s, armed cache)", mode)
	observe.LogDecision(runDir, log)
	if submitErr == nil {
		var bundlePtr *string
		bundleMsg := ""
		if submitBundleID != "" {
			bundlePtr = &submitBundleID
			bundleMsg = fmt.Sprintf(" bundle %s", submitBundleID)
		}
		fmt.Fprintf(os.Stderr, "[save] FIRED [%s] %s%s\n", mode, sig, bundleMsg)
		sigPtr := sig
		observe.LogTrade(runDir, tradeLog{
			T: nowTs(), Obligation: pk.String(), Protocol: "save", RepayNative: repay,
			EstProfitUSDC: estProfit, TipLamports: tipLamports, Signature: &sigPtr, Bundle: bundlePtr,
		})
		ep, rd, owner, s, wh := endpoint, runDir, c.authority.String(), sig, webhook
		be, bid, tc := crank.blockEngine, submitBundleID, dailyTip
		go func() {
			for _, wait := range []int{5, 15, 45} {
				time.Sleep(time.Duration(wait) * time.Second)
				if pnl, ok := observe.RealizedUSDC(ep, s, owner); ok {
					tc.add(tipSol)
					sigPtr2 := s
					pnlPtr := pnl
					observe.LogTrade(rd, tradeLog{T: nowTs(), Protocol: "save", Signature: &sigPtr2, RealizedUSDC: &pnlPtr})
					observe.Alert(wh, "save-landed", fmt.Sprintf("Save liquidation landed %s: realized $%.2f", s, pnl))
					return
				}
			}
			status := ""
			if bid != "" {
				st, _ := jito.BundleStatus(be, bid)
				status = st
			}
			observe.Alert(wh, "save-miss", fmt.Sprintf("Save liquidation %s never confirmed (bundle status: %s)", s, status))
		}()
	} else {
		fmt.Fprintf(os.Stderr, "[save] send failed: %v\n", submitErr)
		errStr := submitErr.Error()
		observe.LogTrade(runDir, tradeLog{
			T: nowTs(), Obligation: pk.String(), Protocol: "save", RepayNative: repay,
			EstProfitUSDC: estProfit, TipLamports: tipLamports, Error: &errStr,
		})
	}
}

// tipCounter is a mutex-guarded daily-tip accumulator (stand-in for Rust's
// Arc<Mutex<f64>>).
type tipCounter struct {
	mu  sync.Mutex
	val float64
}

func (t *tipCounter) get() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.val
}
func (t *tipCounter) add(v float64) {
	t.mu.Lock()
	t.val += v
	t.mu.Unlock()
}
func (t *tipCounter) reset() {
	t.mu.Lock()
	t.val = 0
	t.mu.Unlock()
}

func pow10(n int) float64 {
	r := 1.0
	if n >= 0 {
		for i := 0; i < n; i++ {
			r *= 10
		}
	} else {
		for i := 0; i < -n; i++ {
			r /= 10
		}
	}
	return r
}

func shortStr(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func envStr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
func envBool(name string, def bool) bool {
	if v, ok := os.LookupEnv(name); ok {
		return v != "0"
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
func envU64(name string, def uint64) uint64 {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}
func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
func envFloats(name string, def []float64) []float64 {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	var out []float64
	for _, s := range strings.Split(v, ",") {
		if n, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func main() {
	envfile.LoadDotEnv()

	endpoint := os.Getenv("HELIUS_RPC")
	if endpoint == "" {
		endpoint = os.Getenv("RPC_HTTP")
	}
	if endpoint == "" {
		fail("HELIUS_RPC required")
	}
	dryRun := envBool("DRY_RUN", true)
	runDir := envStr("RUN_DIR", "runs")
	minDebt := envF64("MIN_DEBT_USD", 100.0)
	ratioCap := envF64("RATIO_CAP", 3.0)
	minProfit := envF64("MIN_PROFIT_USD", 0.5)
	rescan := time.Duration(envU64("RESCAN_SECS", 30)) * time.Second
	watchRatio := envF64("WATCH_RATIO", 0.85)
	armRatio := envF64("ARM_RATIO", 0.97)
	armTTL := time.Duration(envU64("ARM_TTL_SECS", 20)) * time.Second
	maxArm := envInt("MAX_ARM_PER_CYCLE", 8)
	maxFire := envInt("MAX_FIRE_PER_CYCLE", 4)
	tickPollMs := envU64("TICK_POLL_MS", 1)
	poll := time.Duration(envU64("POLL_MS", 5000)) * time.Millisecond
	simCooldown := time.Duration(envU64("SIM_COOLDOWN_SECS", 60)) * time.Second
	handleCooldown := time.Duration(envU64("HANDLE_COOLDOWN_SECS", 20)) * time.Second
	hbEvery := envU64("HEARTBEAT_SECS", 30)
	senderURL := envStr("SENDER_URL", "http://ams-sender.helius-rpc.com/fast")
	var webhook *string
	if v := os.Getenv("ALERT_WEBHOOK"); v != "" {
		webhook = &v
	}
	repayFracs := envFloats("REPAY_FRACS", []float64{0.2, 0.1, 0.05})
	maxDailyTipSol := envF64("MAX_DAILY_TIP_SOL", 0.05)
	walletMinSol := envF64("WALLET_MIN_SOL", 0.02)

	c := &cfg{
		tipAccount:      solana.MustPublicKeyFromBase58(envStr("SENDER_TIP_ACCOUNT", "2nyhqdwKcJZR2vcqCyrYsaPVdAnFoJjiksCXJ7hfEYgD")),
		tipFractionBps:  envU64("TIP_FRACTION_BPS", 3000),
		minTipSol:       envF64("MIN_TIP_SOL", 0.0002),
		minProfit:       minProfit,
		slippageBps:     uint32(envU64("SLIPPAGE_BPS", 100)),
		maxSwapAccounts: envInt("MAX_SWAP_ACCOUNTS", 18),
	}

	var kp *solana.PrivateKey
	if p := os.Getenv("KEYPAIR_PATH"); p != "" {
		k, err := solana.PrivateKeyFromSolanaKeygenFile(p)
		if err != nil {
			fail("read keypair: %v", err)
		}
		kp = &k
	}
	if kp == nil && !dryRun {
		fail("LIVE needs KEYPAIR_PATH")
	}
	var authority solana.PublicKey
	if kp != nil {
		authority = kp.PublicKey()
	} else {
		authority = solana.MustPublicKeyFromBase58(envStr("AUTHORITY", defaultAuthority))
	}
	c.authority = authority

	// Lazer WebSocket: the event-driven trigger. Without a token the loop
	// still runs but only on the slow poll fallback — warn loudly, since
	// that's the exact 30s-poll regression this rewrite exists to kill.
	lazerTable := lazer.NewPriceTable()
	mintFeed := mintFeedExt()
	lazerOn := false
	lazerCtx, lazerCancel := context.WithCancel(context.Background())
	defer lazerCancel()
	if token := os.Getenv("PYTH_LAZER_TOKEN"); token != "" {
		lazer.SpawnLazerThread(lazerCtx, token, armFeeds(), lazerTable, nil)
		fmt.Fprintln(os.Stderr, "[save] Pyth Lazer event-driven trigger ENABLED")
		lazerOn = true
	}
	if !lazerOn {
		fmt.Fprintln(os.Stderr, "[save] WARNING: no PYTH_LAZER_TOKEN — falling back to slow poll (the 30s regression). Set the token for ms detection.")
	}

	// Crank context (front-run Solend's own cranker on stale feeds).
	crankOn := envBool("CRANK", true) && lazerOn
	blockEngine := jito.DefaultBlockEngine()
	var tips []solana.PublicKey
	if crankOn {
		tips, _ = jito.GetTipAccounts(blockEngine)
	}
	if crankOn && len(tips) == 0 {
		fmt.Fprintln(os.Stderr, "[save] getTipAccounts failed — using fallback Jito tip list")
		tips = []solana.PublicKey{solana.MustPublicKeyFromBase58("DttWaMuVvTiduZRnguLF7jNxTgiMBZ1hyAumKUiL2KRL")}
	}
	hermesURL := envStr("HERMES", "https://hermes.pyth.network")
	maxBlobMs := envU64("MAX_BLOB_AGE_MS", 3000)
	crank := &crankCtx{
		on:          crankOn,
		hermes:      pyth.SpawnHermesCache(hermesURL, nil, 400*time.Millisecond),
		tips:        tips,
		blockEngine: blockEngine,
		maxBlobAge:  time.Duration(maxBlobMs) * time.Millisecond,
	}

	// Debt reserves decoded once (stable accounts). Each has a wired JupLend
	// flash market and is what the fire path repays: USDC/USDT/wSOL.
	debtReserves := map[solana.PublicKey]*save.Reserve{}
	for _, res := range save.DebtReserves {
		pk := solana.MustPublicKeyFromBase58(res)
		raw, ok := getAcct(endpoint, pk)
		if !ok {
			fail("fetch debt reserve %s", res)
		}
		r, ok := save.DecodeReserve(pk, raw)
		if !ok {
			fail("decode debt reserve %s", res)
		}
		debtReserves[pk] = r
	}

	dryTag := "[LIVE]"
	if dryRun {
		dryTag = "[DRY RUN]"
	}
	fmt.Fprintf(os.Stderr, "[save] Solend liquidation executor %s  authority=%s  min_debt=$%v rescan=%v tick_poll=%dms lazer=%v crank=%v\n",
		dryTag, authority, minDebt, rescan, tickPollMs, lazerOn, crank.on)
	if !dryRun {
		bal := solBalance(endpoint, authority.String())
		fmt.Fprintf(os.Stderr, "[save] wallet balance: %v SOL\n", bal)
		if bal < walletMinSol {
			fail("wallet below floor")
		}
	}

	engine := save.NewEngine(minDebt, ratioCap)
	ctpCache := map[solana.PublicKey]solana.PublicKey{}
	scan := fullScanSave(endpoint, debtReserves, minDebt, ctpCache)
	if scan == nil {
		fail("initial scan failed")
	}
	lastScan := time.Now()

	dailyTip := &tipCounter{}
	tipDay := nowTs() / 86_400
	var freshBh solana.Hash
	lastBh := time.Now().Add(-9999 * time.Second)
	handled := map[solana.PublicKey]time.Time{}
	simRejected := map[solana.PublicKey]time.Time{}
	cache := map[solana.PublicKey]*cachedFire{}
	var lastTickUs uint64
	lastHb := time.Now().Add(-9999 * time.Second)
	armDeferred := 0
	first := true

	lazerSnapshot := func() map[uint32]float64 {
		out := map[uint32]float64{}
		for _, f := range armFeeds() {
			if p, ok := lazerTable.Get(f); ok {
				out[f] = p.Price
			}
		}
		return out
	}

	for {
		// Rebuild the watch-set + engine from a full scan.
		if first || time.Since(lastScan) >= rescan {
			if !first {
				if s := fullScanSave(endpoint, debtReserves, minDebt, ctpCache); s != nil {
					scan = s
				}
			}
			lastScan = time.Now()
			snap := lazerSnapshot()
			var obls []save.ObligationEntry
			obls = append(obls, scan.obls...)
			armed := engine.Rebuild(obls, scan.reserves, mintFeed, watchRatio, snap)
			fmt.Fprintf(os.Stderr, "[save] scan: %d v1 USDC/USDT/wSOL-debt obligations (≥ $%v) → engine watch-set %d (ratio ≥ %v)\n",
				len(scan.obls), minDebt, armed, watchRatio)
			if crank.on {
				feedSet := map[[32]byte]bool{}
				for _, w := range engine.Accounts {
					if !scan.crankable[w.CollReserve] {
						continue
					}
					if f, ok := scan.feedOf[w.CollReserve]; ok {
						feedSet[f] = true
					}
				}
				hex := make([]string, 0, len(feedSet))
				for f := range feedSet {
					hex = append(hex, fmt.Sprintf("%x", f[:]))
				}
				sort.Strings(hex)
				fmt.Fprintf(os.Stderr, "[save] crank: %d crankable collateral reserves, %d feeds in Hermes cache\n", len(scan.crankable), len(hex))
				crank.hermes.SetFeeds(hex)
			}
			first = false
		}

		day := nowTs() / 86_400
		if day != tipDay {
			tipDay = day
			dailyTip.reset()
		}
		if time.Since(lastBh) >= 2*time.Second {
			if bh, ok := latestBlockhash(endpoint); ok {
				freshBh = bh
				lastBh = time.Now()
			}
		}

		// Trigger: event-driven on a Lazer tick (in-memory, no RPC) when
		// live, else the slow poll fallback. Lazer is the trigger to
		// RE-CHECK, not the liquidatable verdict — see the FIRE tier below.
		var snap map[uint32]float64
		if lazerOn {
			deadline := time.Now().Add(poll)
			for {
				var cur uint64
				for _, f := range armFeeds() {
					if p, ok := lazerTable.Get(f); ok && p.TsUs > cur {
						cur = p.TsUs
					}
				}
				if cur > lastTickUs {
					lastTickUs = cur
					break
				}
				if time.Now().After(deadline) {
					break
				}
				time.Sleep(time.Duration(tickPollMs) * time.Millisecond)
			}
			snap = lazerSnapshot()
		} else {
			time.Sleep(poll)
			snap = lazerSnapshot()
		}

		// FIRE tier: TWO-TIER GATING. Lazer NARROWS the watch-set (below);
		// the ON-CHAIN oracle price GATES the expensive sim/submit work.
		// Only obligations liquidatable at the on-chain price Solend
		// settles against (Solend's authoritative stored health captured
		// fresh at the last rescan — ZERO Lazer projection) earn a sim,
		// ranked by USD deficit and capped top-K so the biggest real
		// opportunity wins. Gating on the Lazer-projected ratio instead
		// flooded ~390 phantoms/cycle through simulateTransaction/Bundle
		// (healthy on-chain), starving a genuine opportunity's sim budget.
		//
		// Solend refreshes obligation health lazily, so some read
		// stored-liquidatable while a fresh sim shows them healthy
		// ("healthy at fresh price"). Once one sim-rejects we SUPPRESS it
		// for the cooldown, so a learned phantom can't keep occupying the
		// capped top-K and crowd out a real opportunity — the fire set
		// converges onto genuine/untested obligations.
		var liveFire []save.RankedEntry
		for _, e := range engine.OnChainLiquidatableRanked() {
			if t, ok := simRejected[e.Obligation]; ok && time.Since(t) < simCooldown {
				continue
			}
			liveFire = append(liveFire, e)
		}
		fireDeferred := len(liveFire) - maxFire
		if fireDeferred < 0 {
			fireDeferred = 0
		}
		var crossed []solana.PublicKey
		for i, e := range liveFire {
			if i >= maxFire {
				break
			}
			crossed = append(crossed, e.Obligation)
		}

		// Heartbeat: liveness + detect_lag (the tell this rewrite worked —
		// it must read milliseconds, not the old 30s).
		if lazerOn && hbEvery > 0 && time.Since(lastHb) >= time.Duration(hbEvery)*time.Second {
			totalFeeds := len(armFeeds())
			// Report the tiers DISTINCTLY: `lazer-flagged` is the projected
			// set (leads/diverges — expect hundreds in a moving market);
			// `on-chain liquidatable` is Solend's authoritative stored
			// verdict; `live fire` is that minus the sim-rejected
			// (learned-phantom) cooldown set — the obligations actually
			// eligible to sim this cycle. Only `live fire` (capped) earns
			// sim work. In a calm market `live fire` converges toward 0 as
			// phantoms are learned; if it stays high, real opportunities or
			// a stored-health issue are worth investigating.
			lazerNear := len(engine.Crossed(snap, armRatio))
			lazerFlagged := len(engine.Crossed(snap, 1.0))
			onchainLiq := engine.OnChainLiquidatableCount()
			liveFireCt := 0
			for _, e := range engine.OnChainLiquidatableRanked() {
				if t, ok := simRejected[e.Obligation]; ok && time.Since(t) < simCooldown {
					continue
				}
				liveFireCt++
			}
			var freshest uint64
			for _, f := range armFeeds() {
				if p, ok := lazerTable.Get(f); ok && p.TsUs > freshest {
					freshest = p.TsUs
				}
			}
			lagMs := int64(0)
			if now := nowUs(); now > freshest {
				lagMs = int64((now - freshest) / 1000)
			}
			defer_ := ""
			if fireDeferred+armDeferred > 0 {
				defer_ = fmt.Sprintf(" | DEFERRED fire %d/arm %d", fireDeferred, armDeferred)
			}
			fmt.Fprintf(os.Stderr, "[hb] lazer feeds %d/%d live | detect_lag %dms | watch %d | lazer-flagged %d (≥arm(%v) %d) | on-chain liquidatable %d | LIVE fire %d (cap %d) | cache %d%s | %s\n",
				len(snap), totalFeeds, lagMs, len(engine.Accounts), lazerFlagged, armRatio, lazerNear,
				onchainLiq, liveFireCt, maxFire, len(cache), defer_, lazer.Status(lazerTable))
			lastHb = time.Now()
		}

		// ARM phase: keep a hot, sim-verified fire tx for the FIRE tier (the
		// on-chain-liquidatable set, top-K) so a tick → sign+send is
		// instant. This is the ONLY place a sim runs, and it is bounded by
		// that small set — the broad Lazer near-threshold set is WATCHED
		// but NEVER simulated (that was the phantom flood). sim_rejected
		// suppresses re-simming an obligation that just sim-rejected
		// ("healthy at fresh price / too small") for a cooldown.
		if lazerOn {
			fireKeys := map[solana.PublicKey]bool{}
			for _, pk := range crossed {
				fireKeys[pk] = true
			}
			for pk, c2 := range cache {
				if !fireKeys[pk] || time.Since(c2.built) >= armTTL {
					delete(cache, pk)
				}
			}
			var candidates []solana.PublicKey
			for _, pk := range crossed {
				if _, ok := cache[pk]; ok {
					continue
				}
				if t, ok := simRejected[pk]; ok && time.Since(t) < simCooldown {
					continue
				}
				candidates = append(candidates, pk)
			}
			armDeferred = len(candidates) - maxArm
			if armDeferred < 0 {
				armDeferred = 0
			}
			for i, pk := range candidates {
				if i >= maxArm {
					break
				}
				ratio, _ := engine.OnChainRatioOf(pk)
				if cf := tryArm(endpoint, runDir, c, crank, scan, pk, repayFracs, ratio); cf != nil {
					cache[pk] = cf
				} else {
					simRejected[pk] = time.Now()
				}
			}
		}

		// Drop recently-handled obligations (avoid per-tick spin on a
		// standing cross).
		var toFire []solana.PublicKey
		for _, pk := range crossed {
			if t, ok := handled[pk]; ok && time.Since(t) < handleCooldown {
				continue
			}
			toFire = append(toFire, pk)
		}
		if len(toFire) == 0 {
			continue
		}

		// FIRE phase: prefer the armed cache (instant); else arm inline now.
		for _, pk := range toFire {
			handled[pk] = time.Now()
			ratio, ok := engine.OnChainRatioOf(pk)
			if !ok {
				ratio = 1.0
			}
			var cached *cachedFire
			if cc, ok := cache[pk]; ok && time.Since(cc.built) < armTTL {
				cached = cc
				delete(cache, pk)
			} else if t, ok := simRejected[pk]; ok && time.Since(t) < simCooldown {
				// Respect the sim cooldown here too: a just-rejected
				// obligation stays on-chain-liquidatable (stored health is
				// fixed until the next rescan), so without this guard the
				// fire path would re-sim the same phantom every cycle — the
				// exact flood we're killing.
				cached = nil
			} else {
				cached = tryArm(endpoint, runDir, c, crank, scan, pk, repayFracs, ratio)
				if cached == nil {
					simRejected[pk] = time.Now()
				}
			}
			if cached != nil {
				armedFromCache := time.Since(cached.built) > 0
				fireStart := nowUs()
				fireCached(endpoint, runDir, senderURL, c, crank, dryRun, pk, cached, freshBh,
					kp, dailyTip, maxDailyTipSol, walletMinSol, webhook)
				done := nowUs()
				logLatency(runDir, map[string]any{
					"t": nowTs(), "obligation": pk.String(), "protocol": "save", "mode": cached.mode.name(),
					"appeared_us":     lastTickUs,
					"detected_lag_ms": satSubUs(fireStart, lastTickUs) / 1000,
					"submit_lag_ms":   satSubUs(done, lastTickUs) / 1000,
					"fire_submit_ms":  satSubUs(done, fireStart) / 1000,
					"armed":           armedFromCache,
					"dry_run":         dryRun,
				})
			}
		}
	}
}

func satSubUs(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}
