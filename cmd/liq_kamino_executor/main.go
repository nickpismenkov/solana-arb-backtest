// Production Kamino (KLend) liquidation executor — EVENT-DRIVEN, DRY_RUN default.
//
// The old build polled stored on-chain health every 30s (RESCAN_SECS) / re-read
// the watch-set every 5s (POLL_MS). That loses the race: Kamino's Scope oracle
// updates on-chain and whoever submits a liquidate in the same/next slot wins,
// while a 5-30s poll shows up long after. This mirrors the marginfi/Save
// executors — a Lazer WebSocket feeds an in-memory health engine
// (internal/kamino Engine) that recomputes every obligation's bf_debt/threshold
// on each ~ms price tick with ZERO RPC, so a cross is noticed in ms not seconds.
//
// TWO-TIER gating (the overflag fix): Lazer NARROWS the set; the ON-CHAIN Scope
// price GATES the expensive work. KLend liquidations settle at the on-chain
// Scope oracle, and Lazer LEADS/diverges from Scope, so the Lazer-projected
// "liquidatable" set is mostly phantoms that are healthy on-chain. Building a
// quote+sim fire tx for each hammers Jupiter into a 429 storm and starves real
// opportunities. So:
//
//	full scan (RESCAN_SECS): v1 (1 deposit / 1 wired-debt borrow, non-elevation)
//	  obligations + their reserves -> kamino Engine watch-set (stored on-chain
//	  health + per-side Lazer anchors)
//	ARM tier (cheap, Lazer): the near-threshold watch-set - recomputed per tick
//	  with ZERO RPC, NO Jupiter, NO sim. Only narrows who's worth watching.
//	FIRE tier (expensive): ONLY obligations liquidatable at the on-chain Scope
//	  price (engine.OnChainLiquidatableRanked - stored health, not the Lazer
//	  projection), ranked by USD deficit, capped to MAX_FIRE_PER_CYCLE. These
//	  get the Jupiter quote + sim + submit; a quote/sim reject -> cooldown so the
//	  same candidate isn't re-hammered every cycle.
//
// Kamino prices via Scope (its own oracle) which we cannot crank ourselves, so
// unlike Save there is no crank/bundle mode - a single Sender tx. Safety is
// profit-or-revert: the JupLend fixed-amount payback fails unless the
// seized-collateral swap covered the flash-borrow, so a premature or losing fire
// that lands costs only the base fee; the fire sim is a clean full-execution OR a
// revert only at Kamino's own liquidate/health gate.
//
// v1.5 debt scope (preserved from PR #67): any debt with a wired JupLend flash
// market - USDC / USDT / wSOL.
//
// Usage: HELIUS_RPC=<url> [DRY_RUN=1] [KEYPAIR_PATH=~/arb-keypair.json]
//
//	[PYTH_LAZER_TOKEN=... (required for event-driven)] [MIN_DEBT_USD=100]
//	[MIN_PROFIT_USD=0.5] [CLOSE_FACTOR=0.2] [MAX_BORROW_USD=5000]
//	[WATCH_RATIO=0.9] [ARM_RATIO=0.97] [RATIO_CAP=3] [RESCAN_SECS=30]
//	[TICK_POLL_MS=1] [POLL_MS=5000] [MAX_FIRE_PER_CYCLE=4]
//	[SIM_COOLDOWN_SECS=60] [HANDLE_COOLDOWN_SECS=20] [JUP_API_BASE=...]
//	[SLIPPAGE_BPS=100] [MAX_SWAP_ACCOUNTS=20]
//	go run ./cmd/liq_kamino_executor
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/flashloan"
	"solana-arb-backtest-go/internal/jito"
	"solana-arb-backtest-go/internal/kamino"
	"solana-arb-backtest-go/internal/lazer"
	"solana-arb-backtest-go/internal/observe"
)

const (
	klendProgram     = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"
	mainMarket       = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"
	tokenProgramID   = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	obligationSize   = kamino.ObligationSize
	defaultAuthority = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak"
	// [cu, cu_price, ata, ata, ata, borrow, refresh, refresh, refresh_ob, LIQUIDATE, ...]
	liquidateIxIndex = uint64(9)
)

func nowSecs() uint64 { return uint64(time.Now().Unix()) }
func nowUs() int64    { return time.Now().UnixMicro() }

// log_latency: proves whether SPEED is (still) the bottleneck. appearedUs is
// the Lazer PUBLISH timestamp of the tick that made the obligation cross; the
// deltas measure detect + submit lag from that instant. -> {run_dir}/latency.jsonl
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
	_, _ = f.Write(append(b, '\n'))
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

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

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}
func asArray(v any) []any {
	a, _ := v.([]any)
	return a
}
func asStr(v any) string {
	s, _ := v.(string)
	return s
}
func b64(data any) ([]byte, bool) {
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

func mintOwner(endpoint string, mint solana.PublicKey, cache map[solana.PublicKey]solana.PublicKey) solana.PublicKey {
	if p, ok := cache[mint]; ok {
		return p
	}
	p := solana.MustPublicKeyFromBase58(tokenProgramID)
	if v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{mint.String(), map[string]any{"encoding": "base64"}}}); ok {
		value := asMap(asMap(v["result"])["value"])
		if s := asStr(value["owner"]); s != "" {
			if pk, err := solana.PublicKeyFromBase58(s); err == nil {
				p = pk
			}
		}
	}
	cache[mint] = p
	return p
}

func latestBlockhash(endpoint string) (solana.Hash, bool) {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getLatestBlockhash",
		"params": []any{map[string]any{"commitment": "finalized"}}})
	if !ok {
		return solana.Hash{}, false
	}
	value := asMap(asMap(v["result"])["value"])
	bh, err := solana.HashFromBase58(asStr(value["blockhash"]))
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
	value := asMap(v["result"])["value"]
	f, ok := value.(float64)
	if !ok {
		return 0
	}
	return f / 1e9
}

// simClass: full-tx sim outcome, classified by where it stopped (mirrors kamino_fire_probe).
type simClass int

const (
	// simClean: err null — whole flashloan-wrapped tx executes (on-chain liquidatable + profitable).
	simClean simClass = iota
	// simLiquidateGate: reverts only at Kamino's own liquidate/health/close-factor gate (ix 9) —
	// wiring OK, armed AHEAD of the on-chain cross.
	simLiquidateGate
	// simOtherRevert: reverts at some other ix — a wiring problem; don't arm.
	simOtherRevert
	// simReject: RPC rejected the sim (no value) — treat as unusable.
	simReject
)

func simulate(endpoint, txB64 string) (simClass, uint64) {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "simulateTransaction",
		"params": []any{txB64, map[string]any{"sigVerify": false, "replaceRecentBlockhash": true, "commitment": "processed", "encoding": "base64"}}})
	if !ok {
		return simReject, 0
	}
	result := asMap(v["result"])
	if result == nil || result["value"] == nil {
		return simReject, 0
	}
	res := asMap(result["value"])
	if res["err"] == nil {
		return simClean, 0
	}
	errMap := asMap(res["err"])
	if ie, ok := errMap["InstructionError"].([]any); ok && len(ie) >= 1 {
		if f, ok := ie[0].(float64); ok {
			idx := uint64(f)
			if idx == liquidateIxIndex {
				return simLiquidateGate, idx
			}
			return simOtherRevert, idx
		}
	}
	return simReject, 0
}

type decisionLog struct {
	T             uint64  `json:"t"`
	Obligation    string  `json:"obligation"`
	Protocol      string  `json:"protocol"`
	Ratio         float64 `json:"ratio"`
	DebtUSD       float64 `json:"debt_usd"`
	RepayUSD      float64 `json:"repay_usd"`
	QuotedUSDCOut float64 `json:"quoted_usdc_out"`
	EstProfitUSDC float64 `json:"est_profit_usdc"`
	FireSimOk     bool    `json:"fire_sim_ok"`
	Fired         bool    `json:"fired"`
	Reason        string  `json:"reason"`
}

type tradeLog struct {
	T             uint64   `json:"t"`
	Obligation    string   `json:"obligation"`
	Protocol      string   `json:"protocol"`
	RepayUSD      float64  `json:"repay_usd"`
	EstProfitUSDC float64  `json:"est_profit_usdc"`
	TipLamports   uint64   `json:"tip_lamports"`
	Signature     *string  `json:"signature"`
	RealizedUSDC  *float64 `json:"realized_usdc"`
	Error         *string  `json:"error"`
}

// kaminoScan: a full scan - v1 obligations + the reserve -> Lazer-feed map they
// resolve to. (Fresh reserve prices/wiring are re-fetched at arm time; only the
// stable reserve->feed mapping is kept here for the engine's ratio anchoring.)
type kaminoScan struct {
	obls        []kamino.ObligationEntry
	obIndex     map[solana.PublicKey]*kamino.Obligation
	reserveFeed map[solana.PublicKey]uint32           // reserve pk -> Lazer feed id
	reserveMint map[solana.PublicKey]solana.PublicKey // reserve pk -> liquidity mint (wired-flash-market gate)
}

func scanObligations(endpoint string) []kamino.ObligationEntry {
	resp, _ := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{klendProgram, map[string]any{"encoding": "base64", "dataSlice": map[string]any{"offset": 0, "length": 2288},
			"filters": []any{map[string]any{"dataSize": obligationSize}, map[string]any{"memcmp": map[string]any{"offset": 32, "bytes": mainMarket}}}}}})
	entries := asArray(resp["result"])
	var out []kamino.ObligationEntry
	for _, ev := range entries {
		e := asMap(ev)
		pk, err := solana.PublicKeyFromBase58(asStr(e["pubkey"]))
		if err != nil {
			continue
		}
		raw, ok := b64(asMap(e["account"])["data"])
		if !ok {
			continue
		}
		o, ok := kamino.DecodeObligation(raw)
		if !ok {
			continue
		}
		if len(o.Deposits) == 1 && len(o.Borrows) == 1 && o.ElevationGroup == 0 {
			out = append(out, kamino.ObligationEntry{Pubkey: pk, Obligation: o})
		}
	}
	return out
}

func pow10(n int) float64 { return math.Pow(10, float64(n)) }

// fullScanKamino scans obligations, keeps v1 shape, loads their reserves
// (price + wiring), and builds the reserve -> Lazer-feed map (via each
// reserve's liquidity mint).
func fullScanKamino(endpoint string, minDebt float64, mintFeed map[solana.PublicKey]uint32) *kaminoScan {
	obls := scanObligations(endpoint)
	if len(obls) == 0 {
		return nil
	}
	seen := map[solana.PublicKey]bool{}
	var reservePks []solana.PublicKey
	for _, e := range obls {
		for _, d := range e.Obligation.Deposits {
			if !seen[d.Reserve] {
				seen[d.Reserve] = true
				reservePks = append(reservePks, d.Reserve)
			}
		}
		for _, bpos := range e.Obligation.Borrows {
			if !seen[bpos.Reserve] {
				seen[bpos.Reserve] = true
				reservePks = append(reservePks, bpos.Reserve)
			}
		}
	}
	raw := getMultiple(endpoint, reservePks)
	reserveFeed := map[solana.PublicKey]uint32{}
	reserveMint := map[solana.PublicKey]solana.PublicKey{}
	reserves := map[solana.PublicKey]*kamino.Reserve{}
	for _, pk := range reservePks {
		d, ok := raw[pk]
		if !ok {
			continue
		}
		// The liquidity mint drives both the Lazer feed (ratio anchor) and the
		// wired-flash-market gate.
		if ra, ok := kamino.DecodeReserveAccounts(pk, d); ok {
			reserveMint[pk] = ra.LiquidityMint
			if f, ok := mintFeed[ra.LiquidityMint]; ok {
				reserveFeed[pk] = f
			}
		}
		// The reserve's cached Scope price + config -> recompute CURRENT health.
		if r, ok := kamino.DecodeReserve(d); ok {
			reserves[pk] = r
		}
	}

	// Anchor on CURRENT on-chain (Scope) health, NOT the obligation's stored
	// health. The stored bf_adjusted_debt/unhealthy_borrow_value are only as
	// fresh as the obligation's last refresh - a position that WAS underwater
	// but has since been priced healthy still reads "liquidatable" from its
	// stale stored values, which over-flags the fire tier. Recompute reprices
	// every position from the reserves' fresh Scope prices, so the engine
	// anchors on what KLend will actually settle at. Interest isn't
	// re-accrued (conservative -> no false-positive).
	var filtered []kamino.ObligationEntry
	for _, e := range obls {
		o := e.Obligation
		rc := kamino.Recompute(o, reserves)
		if rc.Trustworthy() {
			o.BfAdjustedDebt = rc.BfAdjustedDebt
			o.UnhealthyBorrowValue = rc.UnhealthyBorrowValue
			o.AllowedBorrowValue = rc.AllowedBorrowValue
			o.DepositedValue = rc.DepositedValue
			var borrowed float64
			for _, bpos := range o.Borrows {
				r, ok := reserves[bpos.Reserve]
				if !ok {
					continue
				}
				borrowed += (bpos.Amount / pow10(int(r.MintDecimals))) * r.MarketPrice
			}
			o.BorrowedValue = borrowed
		}
		if o.BorrowedValue >= minDebt {
			filtered = append(filtered, e)
		}
	}
	obIndex := make(map[solana.PublicKey]*kamino.Obligation, len(filtered))
	for _, e := range filtered {
		obIndex[e.Pubkey] = e.Obligation
	}
	return &kaminoScan{obls: filtered, obIndex: obIndex, reserveFeed: reserveFeed, reserveMint: reserveMint}
}

type cfg struct {
	authority       solana.PublicKey
	tipAccount      solana.PublicKey
	tipFractionBps  uint64
	minTipSol       float64
	minProfit       float64
	closeFactor     float64
	maxBorrowUsd    float64
	slippageBps     uint32
	maxSwapAccounts int
}

// cachedFire: a sim-verified fire tx kept hot for an armed obligation.
// Compiled with a placeholder blockhash (sim replaces it); the real hash is
// stamped at fire.
type cachedFire struct {
	tx          *solana.Transaction
	tipLamports uint64
	tipSol      float64
	estProfit   float64
	repayUsd    float64
	debtUsd     float64
	ratio       float64
	// clean == true: sim ran fully CLEAN (already liquidatable on-chain);
	// false: armed ahead of the on-chain cross (sim reverted only at the
	// liquidate gate).
	clean bool
	built time.Time
}

func skipDecision(runDir string, log *decisionLog, reason string) {
	log.Reason = reason
	observe.LogDecision(runDir, log)
}

// tryArm builds + sizes + profit-gates + sim-gates one obligation -> cachedFire.
// This is the only place a fire tx is built (build + Jupiter quote + sim), all
// off the fire critical path. Accepts a sim that is CLEAN or reverts only at
// Kamino's own liquidate gate (armed ahead of the Scope cross).
func tryArm(endpoint, runDir string, c *cfg, scan *kaminoScan, pk solana.PublicKey, engineRatio float64, tpCache map[solana.PublicKey]solana.PublicKey) *cachedFire {
	market := solana.MustPublicKeyFromBase58(mainMarket)
	o, ok := scan.obIndex[pk]
	if !ok {
		return nil
	}
	if len(o.Deposits) != 1 || len(o.Borrows) != 1 || o.ElevationGroup != 0 {
		return nil
	}
	withdrawPk := o.Deposits[0].Reserve
	repayPk := o.Borrows[0].Reserve

	log := &decisionLog{
		T: nowSecs(), Obligation: pk.String(), Protocol: "kamino", Ratio: engineRatio,
	}

	// Fresh reserve data (prices move; obligation reserves are stable).
	raw := getMultiple(endpoint, []solana.PublicKey{withdrawPk, repayPk})
	wrData, wrOk := raw[withdrawPk]
	rrData, rrOk := raw[repayPk]
	if !wrOk || !rrOk {
		skipDecision(runDir, log, "reserve fetch failed")
		return nil
	}
	wr, wrDecOk := kamino.DecodeReserveAccounts(withdrawPk, wrData)
	rr, rrDecOk := kamino.DecodeReserveAccounts(repayPk, rrData)
	if !wrDecOk || !rrDecOk {
		skipDecision(runDir, log, "reserve accounts decode failed")
		return nil
	}
	wrRes, wrResOk := kamino.DecodeReserve(wrData)
	rrRes, rrResOk := kamino.DecodeReserve(rrData)
	if !wrResOk || !rrResOk {
		skipDecision(runDir, log, "reserve decode failed")
		return nil
	}
	// v1.5: any debt with a wired JupLend flash market (USDC/USDT/wSOL). Preserved.
	if !flashloan.HasMarket(rr.LiquidityMint) {
		skipDecision(runDir, log, "debt mint has no wired flash market")
		return nil
	}

	debtDec := int(rrRes.MintDecimals)
	debtPrice := math.Max(rrRes.MarketPrice, 1e-9)
	debtUsd := (o.Borrows[0].Amount / pow10(debtDec)) * rrRes.MarketPrice
	repayUsd := math.Max(math.Min(debtUsd*c.closeFactor, c.maxBorrowUsd), 1.0)
	repayAmount := uint64(repayUsd / debtPrice * pow10(debtDec))
	bonus := 1.05
	seizedNative := repayUsd * bonus / math.Max(wrRes.MarketPrice, 1e-9) * pow10(int(wrRes.MintDecimals))
	swapInAmount := uint64(seizedNative * 0.995)
	log.DebtUSD = debtUsd
	log.RepayUSD = repayUsd

	cand := &kamino.FireCandidate{
		Obligation: pk, LendingMarket: market, RepayReserve: rr, WithdrawReserve: wr,
		ObligationReserves:             []solana.PublicKey{withdrawPk, repayPk},
		WithdrawLiquidityMint:          wr.LiquidityMint,
		WithdrawLiquidityTokenProgram:  mintOwner(endpoint, wr.LiquidityMint, tpCache),
		WithdrawCollateralTokenProgram: mintOwner(endpoint, wr.CollateralMint, tpCache),
		RepayLiquidityTokenProgram:     mintOwner(endpoint, rr.LiquidityMint, tpCache),
		RepayAmount:                    repayAmount,
		SwapInAmount:                   swapInAmount,
	}
	// Placeholder blockhash - sim replaces it; the live fire stamps the fresh hash.
	ph := solana.Hash{}

	// First build (no tip) to get the Jupiter quote for the profit gate.
	fire, err := kamino.BuildFireTx(endpoint, cand, c.authority, nil, 0, 100_000, c.slippageBps, c.maxSwapAccounts, ph)
	if err != nil {
		skipDecision(runDir, log, fmt.Sprintf("build: %v", err))
		return nil
	}
	quotedUsd := float64(fire.QuotedUSDCOut) / pow10(debtDec) * debtPrice
	estProfit := quotedUsd - repayUsd
	log.QuotedUSDCOut = quotedUsd
	log.EstProfitUSDC = estProfit
	solUsd := 150.0 // conservative; tip is tiny vs profit
	tipSol := math.Max(estProfit*float64(c.tipFractionBps)/10_000.0/solUsd, c.minTipSol)
	tipLamports := uint64(tipSol * 1e9)
	if estProfit < c.minProfit+tipSol*solUsd {
		skipDecision(runDir, log, fmt.Sprintf("below min profit (est $%.2f, tip $%.2f)", estProfit, tipSol*solUsd))
		return nil
	}

	// Final build WITH the tip, sim-gate.
	fire, err = kamino.BuildFireTx(endpoint, cand, c.authority, &c.tipAccount, tipLamports, 100_000, c.slippageBps, c.maxSwapAccounts, ph)
	if err != nil {
		skipDecision(runDir, log, fmt.Sprintf("rebuild: %v", err))
		return nil
	}
	txB64, err := fire.Tx.ToBase64()
	if err != nil {
		skipDecision(runDir, log, fmt.Sprintf("serialize: %v", err))
		return nil
	}
	class, ixIdx := simulate(endpoint, txB64)
	clean := class == simClean
	log.FireSimOk = class == simClean || class == simLiquidateGate
	switch class {
	case simClean, simLiquidateGate:
	case simOtherRevert:
		skipDecision(runDir, log, fmt.Sprintf("sim revert at ix %d (wiring) - not arming", ixIdx))
		return nil
	default:
		skipDecision(runDir, log, "sim rejected by RPC")
		return nil
	}
	if clean {
		log.Reason = "armed (clean - liquidatable on-chain now)"
	} else {
		log.Reason = "armed (ahead - reverts at liquidate gate until Scope crosses)"
	}
	observe.LogDecision(runDir, log)
	return &cachedFire{
		tx: fire.Tx, tipLamports: tipLamports, tipSol: tipSol, estProfit: estProfit,
		repayUsd: repayUsd, debtUsd: debtUsd, ratio: engineRatio, clean: clean, built: time.Now(),
	}
}

// fireCached fires a cached tx: stamps a fresh blockhash, signs, submits via
// Helius Sender, logs, spawns P&L readback. No build/quote/sim here - the hot
// path is submit-only.
func fireCached(
	endpoint, runDir, senderURL string, c *cfg, dryRun bool,
	pk solana.PublicKey, cached *cachedFire, freshBh solana.Hash, kp *solana.PrivateKey,
	dailyTip *dailyTipCounter, maxDailyTip, walletMin float64, webhook *string,
) {
	log := &decisionLog{
		T: nowSecs(), Obligation: pk.String(), Protocol: "kamino", Ratio: cached.ratio, DebtUSD: cached.debtUsd,
		RepayUSD: cached.repayUsd, EstProfitUSDC: cached.estProfit, FireSimOk: true,
	}
	cleanTag := "ahead"
	if cached.clean {
		cleanTag = "clean"
	}
	fmt.Printf("★ KAMINO LIQUIDATABLE %s  debt $%.0f  repay $%.2f  est profit $%.2f  tip %.5f SOL  (%s armed %s ago)\n",
		shortStr(pk.String(), 8), cached.debtUsd, cached.repayUsd, cached.estProfit, cached.tipSol, cleanTag, time.Since(cached.built))
	if dryRun {
		log.Reason = fmt.Sprintf("dry-run: would fire (armed, %s)", cleanTag)
		observe.LogDecision(runDir, log)
		observe.Alert(webhook, "kliq-dry", fmt.Sprintf("DRY-RUN Kamino liq %s est profit $%.2f", pk, cached.estProfit))
		return
	}
	if dailyTip.get()+cached.tipSol > maxDailyTip {
		log.Reason = "daily tip cap"
		observe.LogDecision(runDir, log)
		observe.Alert(webhook, "kliq-cap", "daily tip cap reached")
		return
	}
	if solBalance(endpoint, c.authority.String()) < walletMin {
		log.Reason = "wallet below floor"
		observe.LogDecision(runDir, log)
		observe.Alert(webhook, "kliq-floor", "wallet below floor - not firing")
		return
	}
	tx := *cached.tx
	sigs := make([]solana.Signature, len(cached.tx.Signatures))
	copy(sigs, cached.tx.Signatures)
	tx.Signatures = sigs
	tx.Message.RecentBlockhash = freshBh
	if _, err := tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(kp.PublicKey()) {
			return kp
		}
		return nil
	}); err != nil {
		eprintln(fmt.Sprintf("[kexec] sign failed: %v", err))
		return
	}
	sig := tx.Signatures[0].String()
	raw, err := tx.MarshalBinary()
	if err != nil {
		eprintln(fmt.Sprintf("[kexec] serialize failed: %v", err))
		return
	}
	txB64 := base64.StdEncoding.EncodeToString(raw)
	repayUsd, estProfit, tipLamports, tipSol := cached.repayUsd, cached.estProfit, cached.tipLamports, cached.tipSol
	log.Fired = true
	log.Reason = "fired (armed cache)"
	observe.LogDecision(runDir, log)
	if _, err := jito.SendSender(senderURL, txB64); err != nil {
		eprintln(fmt.Sprintf("[kexec] send failed: %v", err))
		errS := err.Error()
		observe.LogTrade(runDir, &tradeLog{T: nowSecs(), Obligation: pk.String(), Protocol: "kamino",
			RepayUSD: repayUsd, EstProfitUSDC: estProfit, TipLamports: tipLamports, Error: &errS})
		return
	}
	eprintln(fmt.Sprintf("[kexec] FIRED %s", sig))
	sigCopy := sig
	observe.LogTrade(runDir, &tradeLog{T: nowSecs(), Obligation: pk.String(), Protocol: "kamino",
		RepayUSD: repayUsd, EstProfitUSDC: estProfit, TipLamports: tipLamports, Signature: &sigCopy})
	go func() {
		owner := c.authority.String()
		for _, wait := range []time.Duration{5 * time.Second, 15 * time.Second, 45 * time.Second} {
			time.Sleep(wait)
			if pnl, ok := observe.RealizedUSDC(endpoint, sig, owner); ok {
				dailyTip.add(tipSol)
				sCopy := sig
				pnlCopy := pnl
				observe.LogTrade(runDir, &tradeLog{T: nowSecs(), Protocol: "kamino", Signature: &sCopy, RealizedUSDC: &pnlCopy})
				observe.Alert(webhook, "kliq-landed", fmt.Sprintf("Kamino liq landed %s: realized $%.2f", sig, pnl))
				return
			}
		}
		observe.Alert(webhook, "kliq-miss", fmt.Sprintf("Kamino liq %s never confirmed", sig))
	}()
}

// dailyTipCounter is a mutex-guarded running total of tip SOL spent today.
type dailyTipCounter struct {
	mu  sync.Mutex
	val float64
}

func (d *dailyTipCounter) get() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.val
}
func (d *dailyTipCounter) add(v float64) {
	d.mu.Lock()
	d.val += v
	d.mu.Unlock()
}
func (d *dailyTipCounter) reset() {
	d.mu.Lock()
	d.val = 0
	d.mu.Unlock()
}

func eprintln(s string) { fmt.Fprintln(os.Stderr, s) }

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

func main() {
	envfile.LoadDotEnv()

	endpoint := os.Getenv("HELIUS_RPC")
	if endpoint == "" {
		endpoint = os.Getenv("RPC_HTTP")
	}
	if endpoint == "" {
		eprintln("HELIUS_RPC")
		os.Exit(1)
	}
	dryRun := os.Getenv("DRY_RUN") != "0"
	runDir := envStr("RUN_DIR", "runs")
	minDebt := envF64("MIN_DEBT_USD", 100.0)
	ratioCap := envF64("RATIO_CAP", 3.0)
	minProfit := envF64("MIN_PROFIT_USD", 0.5)
	closeFactor := envF64("CLOSE_FACTOR", 0.2)
	maxBorrowUsd := envF64("MAX_BORROW_USD", 5000.0)
	rescan := time.Duration(envU64("RESCAN_SECS", 30)) * time.Second
	watchRatio := envF64("WATCH_RATIO", 0.9)
	armRatio := envF64("ARM_RATIO", 0.97)
	maxFire := envInt("MAX_FIRE_PER_CYCLE", 4)
	tickPollMs := envU64("TICK_POLL_MS", 1)
	poll := time.Duration(envU64("POLL_MS", 5000)) * time.Millisecond
	simCooldown := time.Duration(envU64("SIM_COOLDOWN_SECS", 60)) * time.Second
	handleCooldown := time.Duration(envU64("HANDLE_COOLDOWN_SECS", 20)) * time.Second
	hbEvery := envU64("HEARTBEAT_SECS", 30)
	tipFractionBps := envU64("TIP_FRACTION_BPS", 3000)
	minTipSol := envF64("MIN_TIP_SOL", 0.0002)
	maxDailyTipSol := envF64("MAX_DAILY_TIP_SOL", 0.05)
	walletMinSol := envF64("WALLET_MIN_SOL", 0.02)
	slippageBps := uint32(envU64("SLIPPAGE_BPS", 100))
	maxSwapAccounts := envInt("MAX_SWAP_ACCOUNTS", 20)
	senderURL := envStr("SENDER_URL", "http://ams-sender.helius-rpc.com/fast")
	tipAccount := solana.MustPublicKeyFromBase58(envStr("SENDER_TIP_ACCOUNT", "2nyhqdwKcJZR2vcqCyrYsaPVdAnFoJjiksCXJ7hfEYgD"))
	var webhook *string
	if v := os.Getenv("ALERT_WEBHOOK"); v != "" {
		webhook = &v
	}

	var kp *solana.PrivateKey
	if p := os.Getenv("KEYPAIR_PATH"); p != "" {
		k, err := solana.PrivateKeyFromSolanaKeygenFile(p)
		if err != nil {
			eprintln(fmt.Sprintf("read keypair: %v", err))
			os.Exit(1)
		}
		kp = &k
	}
	if kp == nil && !dryRun {
		eprintln("LIVE needs KEYPAIR_PATH")
		os.Exit(1)
	}
	var authority solana.PublicKey
	if kp != nil {
		authority = kp.PublicKey()
	} else {
		authority = solana.MustPublicKeyFromBase58(envStr("AUTHORITY", defaultAuthority))
	}

	c := &cfg{
		authority: authority, tipAccount: tipAccount, tipFractionBps: tipFractionBps, minTipSol: minTipSol,
		minProfit: minProfit, closeFactor: closeFactor, maxBorrowUsd: maxBorrowUsd,
		slippageBps: slippageBps, maxSwapAccounts: maxSwapAccounts,
	}

	// Lazer WebSocket: the event-driven trigger. Without a token the loop still
	// runs but only on the slow poll fallback - warn loudly, since that's the
	// exact poll regression this rewrite exists to kill.
	lazerTable := lazer.NewPriceTable()
	mintFeed := lazer.MintFeedMap()
	armFeeds := lazer.ArmFeedIDs()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lazerOn := false
	if token := os.Getenv("PYTH_LAZER_TOKEN"); token != "" {
		lazer.SpawnLazerThread(ctx, token, armFeeds, lazerTable, nil)
		eprintln("[kexec] Pyth Lazer event-driven trigger ENABLED")
		lazerOn = true
	}
	if !lazerOn {
		eprintln("[kexec] WARNING: no PYTH_LAZER_TOKEN - falling back to slow poll (the regression). Set the token for ms detection.")
	}

	eprintln(fmt.Sprintf("[kexec] Kamino liquidation executor %s  authority=%s  min_debt=$%g min_profit=$%g rescan=%s tick_poll=%dms lazer=%v",
		tag(dryRun), authority, minDebt, minProfit, rescan, tickPollMs, lazerOn))
	if !dryRun {
		bal := solBalance(endpoint, authority.String())
		eprintln(fmt.Sprintf("[kexec] wallet balance: %g SOL", bal))
		if bal < walletMinSol {
			panic(fmt.Sprintf("wallet below floor %g", walletMinSol))
		}
	}

	engine := kamino.NewEngine(minDebt, ratioCap)
	scan := fullScanKamino(endpoint, minDebt, mintFeed)
	if scan == nil {
		eprintln("[kexec] initial scan failed")
		os.Exit(1)
	}
	lastScan := time.Now()
	tpCache := map[solana.PublicKey]solana.PublicKey{}

	dailyTip := &dailyTipCounter{}
	tipDay := nowSecs() / 86_400
	var freshBh solana.Hash
	lastBh := time.Now().Add(-9999 * time.Second)
	handled := map[solana.PublicKey]time.Time{}
	// Quote/sim-rejected cooldown: once a candidate is quoted+sim'd and rejected
	// (healthy at the fresh price, unprofitable, or a Jupiter 429), don't
	// re-quote it for sim_cooldown - stops re-hammering the same phantoms every
	// cycle.
	simRejected := map[solana.PublicKey]time.Time{}
	var lastTickUs int64
	lastHb := time.Now().Add(-9999 * time.Second)
	fireDeferred := 0
	// Debt mints seen in the watch-set with no wired flash market - logged once
	// (a one-time summary), never per-cycle.
	loggedUnwired := map[solana.PublicKey]bool{}
	first := true

	lazerSnapshot := func() map[uint32]float64 {
		out := map[uint32]float64{}
		for _, f := range armFeeds {
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
				if s := fullScanKamino(endpoint, minDebt, mintFeed); s != nil {
					scan = s
				}
			}
			lastScan = time.Now()
			snap := lazerSnapshot()
			armed := engine.Rebuild(scan.obls, scan.reserveFeed, watchRatio, snap)
			eprintln(fmt.Sprintf("[kexec] scan: %d v1 obligations (>= $%g) -> engine watch-set %d (ratio >= %g)",
				len(scan.obls), minDebt, armed, watchRatio))
			// One-time summary of watch-set debts with no wired flash market -
			// these can never fire, so they're excluded from fire candidates
			// (never a build attempt). Log the mint once, not per-cycle.
			unwiredNow := 0
			for _, w := range engine.Accounts {
				mint, ok := scan.reserveMint[w.DebtReserve]
				if !ok {
					continue
				}
				if flashloan.HasMarket(mint) {
					continue
				}
				unwiredNow++
				if !loggedUnwired[mint] {
					loggedUnwired[mint] = true
					eprintln(fmt.Sprintf("[kexec] unwired debt mint (no JupLend flash market) - will skip: %s", mint))
				}
			}
			if unwiredNow > 0 {
				eprintln(fmt.Sprintf("[kexec] %d/%d watch-set obligations have an unwired debt mint (excluded from fire candidates)",
					unwiredNow, len(engine.Accounts)))
			}
			first = false
		}

		day := nowSecs() / 86_400
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

		// Trigger cadence: wake on a Lazer tick (in-memory, no RPC) when live,
		// else the slow poll fallback. The tick only paces the loop - it
		// NARROWS which obligations are near threshold (the watch-set), but
		// does NOT decide who fires. The fire set is gated on the ON-CHAIN
		// Scope price below, because Lazer LEADS/diverges from Scope and its
		// projected "liquidatable" set is mostly phantoms that are healthy
		// on-chain.
		var snap map[uint32]float64
		if lazerOn {
			deadline := time.Now().Add(poll)
			for {
				var cur int64
				for _, f := range armFeeds {
					if p, ok := lazerTable.Get(f); ok {
						if v := int64(p.TsUs); v > cur {
							cur = v
						}
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

		// Heartbeat: liveness + detect_lag (the tell this rewrite worked - it
		// must read milliseconds, not the old 5-30s poll interval).
		if lazerOn && hbEvery > 0 && time.Since(lastHb) >= time.Duration(hbEvery)*time.Second {
			totalFeeds := len(armFeeds)
			near := len(engine.Crossed(snap, armRatio))
			// TWO distinct counts: lazer-flagged (the projected set - cheap ARM
			// tier, no Jupiter) vs on-chain liquidatable (the real FIRE
			// candidates at the Scope price). In a calm market on-chain M
			// should be single-digit/zero even while lazer-flagged L is
			// hundreds - that gap IS the phantom set.
			lazerFlagged := len(engine.Crossed(snap, 1.0))
			onChain := engine.OnChainLiquidatableCount()
			var freshest int64
			for _, f := range armFeeds {
				if p, ok := lazerTable.Get(f); ok {
					if v := int64(p.TsUs); v > freshest {
						freshest = v
					}
				}
			}
			lagMs := (nowUs() - freshest) / 1000
			defer_ := ""
			if fireDeferred > 0 {
				defer_ = fmt.Sprintf(" | DEFERRED fire %d/cycle", fireDeferred)
			}
			eprintln(fmt.Sprintf("[hb] lazer feeds %d/%d live | detect_lag %dms | watch %d | %d within arm(%g) | lazer-flagged %d | on-chain liquidatable %d | fire-cap %d%s | %s",
				len(snap), totalFeeds, lagMs, len(engine.Accounts), near, armRatio, lazerFlagged, onChain, maxFire, defer_, lazer.Status(lazerTable)))
			lastHb = time.Now()
		}

		// -- ARM tier (cheap, Lazer-driven): the near-threshold watch-set is
		// maintained by engine.Rebuild - no Jupiter, no sim. It only NARROWS
		// the universe. Nothing to do here per tick; it's reported in the
		// heartbeat.

		// -- FIRE tier (expensive): ONLY obligations liquidatable at the
		// ON-CHAIN Scope price (health RECOMPUTED from fresh reserve prices
		// at the last rescan - NOT the Lazer projection). Ranked by USD
		// deficit, capped to top-K/cycle so the biggest REAL opportunity wins
		// a bounded quote/sim budget. This is the ONLY place Jupiter is
		// called - the whole 429-storm fix is that this set is ~0 in a calm
		// market instead of the hundreds the Lazer projection used to feed
		// here.
		fireRanked := engine.OnChainLiquidatableRanked()
		isWired := func(pk solana.PublicKey) bool {
			_, debt, ok := engine.ReservesOf(pk)
			if !ok {
				return false
			}
			mint, ok := scan.reserveMint[debt]
			if !ok {
				return false
			}
			return flashloan.HasMarket(mint)
		}
		var fireCandidates []solana.PublicKey
		for _, r := range fireRanked {
			pk := r.Obligation
			if !isWired(pk) { // unwired debt -> can never fire; drop cleanly
				continue
			}
			if t, ok := handled[pk]; ok && time.Since(t) < handleCooldown { // not just handled (standing cross)
				continue
			}
			if t, ok := simRejected[pk]; ok && time.Since(t) < simCooldown { // not in quote/sim-reject cooldown
				continue
			}
			fireCandidates = append(fireCandidates, pk)
		}
		if len(fireCandidates) > maxFire {
			fireDeferred = len(fireCandidates) - maxFire
		} else {
			fireDeferred = 0
		}
		if len(fireCandidates) > maxFire {
			fireCandidates = fireCandidates[:maxFire]
		}
		for _, pk := range fireCandidates {
			handled[pk] = time.Now()
			ratio := 1.0
			for _, w := range engine.Accounts {
				if w.Obligation.Equals(pk) {
					ratio = w.OnChainRatio()
					break
				}
			}
			// Build + Jupiter quote (backoff, honors JUP_API_BASE) + sim gate
			// (the authoritative on-chain liquidatability/profit check).
			fireStart := nowUs()
			if c2 := tryArm(endpoint, runDir, c, scan, pk, ratio, tpCache); c2 != nil {
				fireCached(endpoint, runDir, senderURL, c, dryRun, pk, c2, freshBh, kp, dailyTip, maxDailyTipSol, walletMinSol, webhook)
				done := nowUs()
				// Only meaningful with a real Lazer tick (appeared_us = its publish ts).
				if lazerOn {
					logLatency(runDir, map[string]any{
						"t": nowSecs(), "obligation": pk.String(), "protocol": "kamino",
						"clean": c2.clean, "appeared_us": lastTickUs,
						"detected_lag_ms": (fireStart - lastTickUs) / 1000,
						"submit_lag_ms":   (done - lastTickUs) / 1000,
						"fire_submit_ms":  (done - fireStart) / 1000,
						"armed":           false, "dry_run": dryRun,
					})
				}
			} else {
				// Quote/sim rejected (healthy at fresh price, unprofitable, or
				// 429) -> cooldown so we don't re-hammer the same candidate next
				// cycle.
				simRejected[pk] = time.Now()
			}
		}
	}
}

func tag(dryRun bool) string {
	if dryRun {
		return "[DRY RUN]"
	}
	return "[LIVE]"
}
