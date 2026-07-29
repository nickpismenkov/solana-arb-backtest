// Command liq_save_executor is the production Save (Solend) liquidation
// executor — EVENT-DRIVEN, DRY_RUN default.
//
// The old build polled stored on-chain health every 30s. That lost every
// race: the census found 45 USDC-debt Solend liquidations in 48h and we
// caught 0, because competitors react to the oracle in milliseconds. This
// rewrite mirrors the marginfi executor's architecture — a Lazer WebSocket
// feeds an in-memory health engine (internal/saveengine) that recomputes
// every obligation's borrowed/unhealthy on each ~ms price tick with ZERO
// RPC, so a cross is noticed in ~ms not ~30s.
//
//	full scan (RESCAN_SECS): v1 (1 collateral / 1 debt, debt in {USDC,USDT,wSOL}) obligations ->
//	  save_engine watch-set (stored on-chain health + per-side Lazer anchors)
//	Lazer tick (TICK_POLL_MS in-memory poll): the trigger to RE-CHECK, not the
//	  liquidatable verdict — Lazer leads/diverges from the on-chain Pyth price
//	FIRE tier (TWO-TIER GATING): Lazer NARROWS the watch-set; the ON-CHAIN
//	  oracle price GATES the expensive sim. Only obligations liquidatable at the
//	  on-chain price Solend settles against (stored health from the last rescan,
//	  ZERO Lazer projection) earn a sim, ranked by USD deficit, capped top-K
//	  (MAX_FIRE_PER_CYCLE). Gating on the Lazer-projected ratio instead flooded
//	  ~390 phantoms/cycle through simulateTransaction/Bundle (healthy on-chain).
//	ARM those FIRE-tier candidates: pre-build+size+sim the fire tx -> hot cache
//	FIRE on tick: stamp fresh blockhash, sign, submit (no build/quote/sim on
//	  the critical path)
//
// Two fire modes, exactly like marginfi:
//
//	Sender — obligation already liquidatable at ON-CHAIN prices -> single tx via
//	  Helius Sender.
//	Crank  — underwater at the true (Lazer) price but Solend hasn't cranked its
//	  Pyth feed yet -> atomic Jito bundle [crank_setup, crank_fire, fire] that
//	  posts the fresh price then liquidates. Save reserves read the SAME shard-0
//	  sponsored feeds we crank, so refresh_reserve inside the fire tx picks up
//	  the cranked price. Sizing + ground truth run through simulateBundle.
//
// Profit-or-revert (payback_all fails unless the swap covered the borrow), so
// a losing fire that lands costs only the base fee; a failing bundle never
// lands.
//
// Usage: HELIUS_RPC=<url> [DRY_RUN=1] [KEYPAIR_PATH=~/arb-keypair.json]
//
//	[PYTH_LAZER_TOKEN=... (required for event-driven + crank)] [CRANK=1]
//	[MIN_DEBT_USD=100] [MIN_PROFIT_USD=0.5] [REPAY_FRACS=0.2,0.1,0.05]
//	[WATCH_RATIO=0.85] [ARM_RATIO=0.97] [RESCAN_SECS=30] [TICK_POLL_MS=1]
//	[MAX_ARM_PER_CYCLE=8] [MAX_FIRE_PER_CYCLE=4] [SLIPPAGE_BPS=100]
//	[MAX_SWAP_ACCOUNTS=18] [MAX_BLOB_AGE_MS=3000] go run ./cmd/liq_save_executor
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"arbengine/internal/config"
	"arbengine/internal/jito"
	"arbengine/internal/lazer"
	"arbengine/internal/liquidation"
	"arbengine/internal/observe"
	"arbengine/internal/pyth"
	"arbengine/internal/pythaccumulator"
	"arbengine/internal/pythcrank"
	"arbengine/internal/rpcclient"
	"arbengine/internal/save"
	"arbengine/internal/saveengine"
	"arbengine/internal/savefire"
	"arbengine/internal/solana"
)

const defaultAuthority = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak"

// classicTokenProgram: every Save main-pool debt mint (USDC/USDT/wSOL).
const classicTokenProgram = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"

// lazerUSDT is the Pyth Lazer USDT/USD numeric feed id (verified against the
// Lazer symbol registry; consistent with the codebase's SOL=6 / USDC=7).
// wSOL debt already maps to the SOL feed (6). Added to the executor's local
// feed set so USDT-debt obligations are subscribed + tracked without editing
// the shared lazer map.
const lazerUSDT uint32 = 8

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

// mintFeedExt is mint -> Lazer feed, the shared map extended with USDT (->
// feed 8) so a USDT debt side is priced by Lazer like USDC is.
func mintFeedExt() map[solana.Pubkey]uint32 {
	m := lazer.MintFeedMap()
	m[solana.MustPubkeyFromBase58(save.USDTMint)] = lazerUSDT
	return m
}

func now() uint64   { return uint64(time.Now().Unix()) }
func nowUs() uint64 { return uint64(time.Now().UnixMicro()) }

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// logLatency is the latency ledger — proves whether SPEED is (still) the
// bottleneck. `appeared_us` is the Lazer PUBLISH timestamp of the tick that
// made the obligation cross; the deltas measure detect + submit lag from
// that instant. -> {run_dir}/latency.jsonl
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

func b64tx(tx *solana.VersionedTransaction) string {
	s, err := tx.Base64()
	if err != nil {
		return ""
	}
	return s
}

func mintOwner(rpc *rpcclient.Client, mint solana.Pubkey) (solana.Pubkey, bool) {
	info, err := rpc.GetAccountInfo(mint)
	if err != nil || info == nil {
		return solana.Pubkey{}, false
	}
	owner, err := solana.PubkeyFromBase58(info.Owner)
	if err != nil {
		return solana.Pubkey{}, false
	}
	return owner, true
}

func solBalance(rpc *rpcclient.Client, owner solana.Pubkey) float64 {
	lamports, err := rpc.GetBalance(owner)
	if err != nil {
		return 0.0
	}
	return float64(lamports) / 1e9
}

func simulateOk(rpc *rpcclient.Client, txB64 string) bool {
	raw, err := rpc.Call("simulateTransaction", []any{txB64, map[string]any{
		"sigVerify": false, "replaceRecentBlockhash": true, "commitment": "processed", "encoding": "base64",
	}})
	if err != nil {
		return false
	}
	var withValue struct {
		Value struct {
			Err json.RawMessage `json:"err"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &withValue); err != nil {
		return false
	}
	return string(withValue.Value.Err) == "null" || len(withValue.Value.Err) == 0
}

// simulateBundleRanOk reports how many leading txs of a bundle succeed (jito
// stops at the first revert). For [setup, fire, save_fire] ranOk == 3 =
// accepted, < 2 = crank broke. ok=false on an RPC-level error.
func simulateBundleRanOk(rpc *rpcclient.Client, txsB64 []string) (int, bool) {
	nulls := make([]any, len(txsB64))
	raw, err := rpc.Call("simulateBundle", []any{
		map[string]any{"encodedTransactions": txsB64},
		map[string]any{
			"skipSigVerify": true, "replaceRecentBlockhash": true,
			"preExecutionAccountsConfigs": nulls, "postExecutionAccountsConfigs": nulls,
		},
	})
	if err != nil {
		return 0, false
	}
	var withValue struct {
		Value struct {
			TransactionResults []struct {
				Err json.RawMessage `json:"err"`
			} `json:"transactionResults"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &withValue); err != nil {
		return 0, false
	}
	ranOk := 0
	for _, r := range withValue.Value.TransactionResults {
		if string(r.Err) != "null" && len(r.Err) != 0 {
			break
		}
		ranOk++
	}
	return ranOk, true
}

// DecisionLog mirrors the Rust DecisionLog serde shape exactly (field names
// preserved for downstream tooling that reads runs/decisions.jsonl).
type DecisionLog struct {
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

// TradeLog mirrors the Rust TradeLog serde shape exactly.
type TradeLog struct {
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
	// isCrank is false for Sender (obligation already liquidatable at
	// on-chain prices -> single tx via Helius Sender) and true for Crank
	// (underwater at the true Lazer price only; the crank posts the fresh
	// price for refresh_reserve, via a Jito bundle
	// [crank_setup, crank_fire, save_fire]).
	isCrank bool
	feedID  [32]byte
}

func (m fireMode) name() string {
	if m.isCrank {
		return "crank"
	}
	return "sender"
}

// crankCtx holds everything the crank path needs, spun up once at boot
// (shared with marginfi's design).
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
	return c.tips[now()%uint64(len(c.tips))], true
}

// saveScan is a full scan: v1 accepted-debt (USDC/USDT/wSOL) obligations +
// the reserves/oracle metadata they touch.
type saveScan struct {
	obls      []saveengine.ObligationRef
	reserves  map[solana.Pubkey]save.Reserve  // collateral reserves (+ the debt reserves)
	ctpOf     map[solana.Pubkey]solana.Pubkey // collateral liquidity mint -> token program
	feedOf    map[solana.Pubkey][32]byte      // collateral reserve -> 32-byte Pyth feed id
	crankable map[solana.Pubkey]struct{}      // collateral reserves whose pyth_oracle is the shard-0 sponsored PDA
}

// fullScanSave scans obligations (full), keeps v1 / debt in
// {USDC,USDT,wSOL} / >= minDebt, then loads their collateral reserves +
// oracle crank metadata. The debt reserves are passed in pre-decoded
// (stable accounts).
func fullScanSave(rpc *rpcclient.Client, debtReserves map[solana.Pubkey]save.Reserve, minDebt float64, ctpCache map[solana.Pubkey]solana.Pubkey) (*saveScan, bool) {
	// One getProgramAccounts per scanned pool (memcmp matches a single
	// value), merged. The obligation's own lending_market flows through to
	// the fire tx, so multi-pool needs no fire-path change — just these
	// obligations + the pools' debt reserves in debtReserves.
	var entries []rpcclient.ProgramAccount
	for _, pool := range save.SCANPools {
		resp, err := rpc.GetProgramAccounts(solana.MustPubkeyFromBase58(save.SolendProgram), rpcclient.GetProgramAccountsOpts{
			Filters: []any{
				map[string]any{"dataSize": 1300},
				map[string]any{"memcmp": map[string]any{"offset": 10, "bytes": pool}},
			},
		})
		if err != nil {
			continue
		}
		entries = append(entries, resp...)
	}
	if len(entries) == 0 {
		return nil, false
	}

	var obls []saveengine.ObligationRef
	for _, e := range entries {
		if e.Account.Data == nil {
			continue
		}
		o, ok := save.DecodeObligation(e.Account.Data)
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
		obls = append(obls, saveengine.ObligationRef{Pubkey: e.Pubkey, Obligation: o})
	}

	// Load the distinct collateral reserves referenced.
	collSeen := map[solana.Pubkey]struct{}{}
	var collPks []solana.Pubkey
	for _, ob := range obls {
		pk := ob.Obligation.Deposits[0].Reserve
		if _, ok := collSeen[pk]; !ok {
			collSeen[pk] = struct{}{}
			collPks = append(collPks, pk)
		}
	}
	reserves := make(map[solana.Pubkey]save.Reserve, len(debtReserves)+len(collPks))
	for k, v := range debtReserves {
		reserves[k] = v
	}
	if infos, err := rpc.GetMultipleAccounts(collPks); err == nil {
		for i, info := range infos {
			if info == nil || info.Data == nil {
				continue
			}
			if r, ok := save.DecodeReserve(collPks[i], info.Data); ok {
				reserves[collPks[i]] = r
			}
		}
	}

	// Collateral-mint -> token program (for the redeem ATA).
	ctpOf := map[solana.Pubkey]solana.Pubkey{}
	for _, pk := range collPks {
		r, ok := reserves[pk]
		if !ok {
			continue
		}
		tp, cached := ctpCache[r.LiquidityMint]
		if !cached {
			var found bool
			tp, found = mintOwner(rpc, r.LiquidityMint)
			if !found {
				continue
			}
			ctpCache[r.LiquidityMint] = tp
		}
		ctpOf[r.LiquidityMint] = tp
	}

	// Oracle crank metadata: decode each collateral reserve's pyth_oracle ->
	// feed id, and mark crankable when the oracle IS that feed's shard-0
	// sponsored PDA.
	oracleSeen := map[solana.Pubkey]struct{}{}
	var oraclePks []solana.Pubkey
	for _, pk := range collPks {
		r, ok := reserves[pk]
		if !ok {
			continue
		}
		if _, seen := oracleSeen[r.PythOracle]; !seen {
			oracleSeen[r.PythOracle] = struct{}{}
			oraclePks = append(oraclePks, r.PythOracle)
		}
	}
	oracleRaw := map[solana.Pubkey][]byte{}
	if infos, err := rpc.GetMultipleAccounts(oraclePks); err == nil {
		for i, info := range infos {
			if info != nil && info.Data != nil {
				oracleRaw[oraclePks[i]] = info.Data
			}
		}
	}
	feedOf := map[solana.Pubkey][32]byte{}
	crankable := map[solana.Pubkey]struct{}{}
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
		if pythcrank.SponsoredFeed(0, fid) == r.PythOracle {
			crankable[pk] = struct{}{}
		}
	}

	return &saveScan{obls: obls, reserves: reserves, ctpOf: ctpOf, feedOf: feedOf, crankable: crankable}, true
}

// cfg holds config resolved once at boot.
type cfg struct {
	authority       solana.Pubkey
	tipAccount      solana.Pubkey
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
	tx          solana.VersionedTransaction
	mode        fireMode
	tipLamports uint64
	tipSol      float64
	estProfit   float64
	repay       uint64
	debtUSD     float64
	ratio       float64
	built       time.Time
}

func logSkip(runDir string, pk solana.Pubkey, mode string, debt, ratio float64, reason string) {
	observe.LogDecision(runDir, &DecisionLog{
		T: now(), Obligation: pk.String(), Protocol: "save", Mode: mode,
		DebtUSD: debt, Ratio: ratio, RepayNative: 0, QuotedUSDCOut: 0.0, EstProfitUSDC: 0.0,
		Fired: false, Reason: reason,
	})
}

// tryArm builds + sizes + profit-gates + full-sim-gates one obligation into
// a cachedFire. Mirrors the marginfi try_arm: mode from on-chain vs Lazer
// health, size by a sim ladder (bundle sim in crank mode so the chain judges
// at the cranked price), profit gate, ground-truth sim. This is the only
// place a fire tx is built; the sim lives here (arm time), off the fire
// critical path.
func tryArm(
	rpc *rpcclient.Client, runDir string, c *cfg, crank *crankCtx, scan *saveScan,
	pk solana.Pubkey, repayFracs []float64, engineRatio float64,
) (*cachedFire, bool) {
	// Fresh obligation (health may have moved since scan) + its collateral reserve.
	data, err := rpc.GetAccountData(pk)
	if err != nil || data == nil {
		return nil, false
	}
	o, ok := save.DecodeObligation(data)
	if !ok {
		return nil, false
	}
	if len(o.Deposits) != 1 || len(o.Borrows) != 1 {
		return nil, false
	}
	collPk := o.Deposits[0].Reserve
	coll, ok := scan.reserves[collPk]
	if !ok {
		return nil, false
	}
	ctp, ok := scan.ctpOf[coll.LiquidityMint]
	if !ok {
		return nil, false
	}
	// The obligation's actual debt reserve (USDC/USDT/wSOL) — prices the
	// repay and is the flash-borrow/swap-target asset.
	debtReserve, ok := scan.reserves[o.Borrows[0].Reserve]
	if !ok {
		return nil, false
	}
	debtDec := pow10(int(debtReserve.MintDecimals))
	debtTP := solana.MustPubkeyFromBase58(classicTokenProgram)
	debtUSD := o.BorrowedValue

	// Mode: liquidatable at FRESH on-chain prices (the value Solend's
	// `liquidate` recomputes at settle time — same cToken-exchange-rate math
	// the fire tier gates on, so routing stays consistent and never drops a
	// genuine fire) -> Sender. Else the Lazer engine flagged it but Solend
	// hasn't cranked yet -> crank + liquidate bundle (needs a crankable
	// oracle + a fresh Hermes blob).
	var mode fireMode
	if o.FreshLiquidatable(scan.reserves) {
		mode = fireMode{isCrank: false}
	} else {
		if !crank.on {
			return nil, false
		}
		if _, ok := scan.crankable[collPk]; !ok {
			logSkip(runDir, pk, "crank", debtUSD, engineRatio, "flagged at Lazer price but healthy on-chain and collateral oracle not crankable — cannot act")
			return nil, false
		}
		feedID, ok := scan.feedOf[collPk]
		if !ok {
			logSkip(runDir, pk, "crank", debtUSD, engineRatio, "crankable but feed id missing")
			return nil, false
		}
		if _, _, _, ok := crank.hermes.UpdateFor(feedID); !ok {
			logSkip(runDir, pk, "crank", debtUSD, engineRatio, "crankable but no fresh Hermes blob for feed yet")
			return nil, false
		}
		mode = fireMode{isCrank: true, feedID: feedID}
	}

	// Crank txs for the sizing/ground-truth bundle (placeholder blockhash —
	// sims replace it; the LIVE fire rebuilds from the freshest blob).
	var crankSetupB64, crankFireB64 string
	haveCrankB64 := false
	if mode.isCrank {
		mu, vaa, _, ok := crank.hermes.UpdateFor(mode.feedID)
		if !ok {
			return nil, false
		}
		txs, err := pythcrank.BuildCrankTxs(c.authority, vaa, []pythaccumulator.MerkleUpdate{mu}, 0, 0, solana.Hash{})
		if err != nil {
			return nil, false
		}
		s, f, err := txs.ToB64()
		if err != nil {
			return nil, false
		}
		crankSetupB64, crankFireB64, haveCrankB64 = s, f, true
	}

	// The in-tx tip destination differs by mode: a Sender fire tips a Helius
	// Sender wallet; a crank fire rides a Jito bundle and must tip a Jito
	// account.
	var tipTo solana.Pubkey
	if mode.isCrank {
		t, ok := crank.pickTip()
		if !ok {
			logSkip(runDir, pk, "crank", debtUSD, engineRatio, "no Jito tip accounts")
			return nil, false
		}
		tipTo = t
	} else {
		tipTo = c.tipAccount
	}

	mk := func(repay, seize, tip uint64, bh solana.Hash) (*savefire.SaveFireTx, bool) {
		cand := savefire.SaveFireCandidate{
			Obligation: pk, RepayReserve: debtReserve, WithdrawReserve: coll,
			CollateralTokenProgram: ctp, DebtTokenProgram: debtTP, RepayAmount: repay, SeizeUnderlying: seize,
			DepositReserves: []solana.Pubkey{coll.Reserve}, BorrowReserves: []solana.Pubkey{debtReserve.Reserve},
		}
		tx, err := savefire.BuildSaveFireTx(rpc.Endpoint, &cand, c.authority,
			&tipTo, tip, 100_000, c.slippageBps, c.maxSwapAccounts, bh)
		if err != nil {
			return nil, false
		}
		return &tx, true
	}
	// gate: standalone sim (Sender) or bundle sim (crank) so the chain
	// judges at the actionable price.
	gate := func(fire *solana.VersionedTransaction) bool {
		if !haveCrankB64 {
			return simulateOk(rpc, b64tx(fire))
		}
		ranOk, ok := simulateBundleRanOk(rpc, []string{crankSetupB64, crankFireB64, b64tx(fire)})
		return ok && ranOk == 3
	}

	// Size by simulation ladder — largest repay fraction Solend accepts.
	ph := solana.Hash{}
	var chosenRepay uint64
	var chosenFire *savefire.SaveFireTx
	for _, frac := range repayFracs {
		repayUSD := debtUSD * frac
		repay := uint64(max(repayUSD/max(debtReserve.MarketPrice, 1e-9)*debtDec, 1.0))
		seizedUSD := repayUSD * (1.0 + float64(coll.LiquidationBonusPct)/100.0)
		seize := uint64(seizedUSD / max(coll.MarketPrice, 1e-9) * pow10(int(coll.MintDecimals)))
		fire, ok := mk(repay, seize, 0, ph)
		if !ok {
			continue
		}
		if gate(&fire.Tx) {
			chosenRepay, chosenFire = repay, fire
			break
		}
	}
	if chosenFire == nil {
		logSkip(runDir, pk, mode.name(), debtUSD, engineRatio, "no repay fraction passed sim (healthy at actionable price / too small)")
		return nil, false
	}

	// Profit gate — price both legs in the debt asset's decimals + market price.
	repayUSD := float64(chosenRepay) / debtDec * debtReserve.MarketPrice
	usdcOut := float64(chosenFire.QuotedDebtOut) / debtDec * debtReserve.MarketPrice
	estProfit := usdcOut - repayUSD
	const solUSD = 150.0 // conservative; tip is tiny vs profit
	tipSol := max(estProfit*float64(c.tipFractionBps)/10_000.0/solUSD, c.minTipSol)
	tipLamports := uint64(tipSol * 1e9)
	log := DecisionLog{
		T: now(), Obligation: pk.String(), Protocol: "save", Mode: mode.name(),
		DebtUSD: debtUSD, Ratio: engineRatio, RepayNative: chosenRepay, QuotedUSDCOut: usdcOut,
		EstProfitUSDC: estProfit, Fired: false, Reason: "",
	}
	if estProfit < c.minProfit+tipSol*solUSD {
		log.Reason = fmt.Sprintf("below min profit (est $%.2f)", estProfit)
		observe.LogDecision(runDir, &log)
		return nil, false
	}

	// Final build WITH the tip, ground-truth sim gate.
	seizedUSD := repayUSD * (1.0 + float64(coll.LiquidationBonusPct)/100.0)
	seize := uint64(seizedUSD / max(coll.MarketPrice, 1e-9) * pow10(int(coll.MintDecimals)))
	finalFire, ok := mk(chosenRepay, seize, tipLamports, ph)
	if !ok {
		log.Reason = "final build failed"
		observe.LogDecision(runDir, &log)
		return nil, false
	}
	if !gate(&finalFire.Tx) {
		log.Reason = "final fire sim revert (swap/repay would not cover the borrow)"
		observe.LogDecision(runDir, &log)
		return nil, false
	}
	return &cachedFire{
		tx: finalFire.Tx, mode: mode, tipLamports: tipLamports, tipSol: tipSol,
		estProfit: estProfit, repay: chosenRepay, debtUSD: debtUSD, ratio: engineRatio, built: time.Now(),
	}, true
}

// fireCached fires a cached tx: stamp fresh blockhash, sign, submit (Sender
// or a Jito bundle with freshly-built crank txs), log, spawn P&L readback.
func fireCached(
	rpc *rpcclient.Client, runDir, senderURL string, c *cfg, crank *crankCtx, dryRun bool,
	pk solana.Pubkey, cached *cachedFire, freshBh solana.Hash, kp *solana.Keypair,
	dailyTip *sync.Mutex, dailyTipVal *float64, maxDailyTip, walletMin float64, webhook string,
) {
	mode := cached.mode.name()
	log := DecisionLog{
		T: now(), Obligation: pk.String(), Protocol: "save", Mode: mode, DebtUSD: cached.debtUSD,
		Ratio: cached.ratio, RepayNative: cached.repay, QuotedUSDCOut: 0.0, EstProfitUSDC: cached.estProfit,
		Fired: false, Reason: "",
	}
	pkStr := pk.String()
	short := pkStr
	if len(short) > 8 {
		short = short[:8]
	}
	fmt.Printf("★ SAVE LIQUIDATABLE [%s]  %s  debt $%.0f  repay %d  est profit $%.2f  tip %.5f SOL  (armed %s ago)\n",
		mode, short, cached.debtUSD, cached.repay, cached.estProfit, cached.tipSol, time.Since(cached.built))
	if dryRun {
		log.Reason = fmt.Sprintf("dry-run: would fire (%s, armed)", mode)
		observe.LogDecision(runDir, &log)
		observe.Alert(webhook, "save-dry", fmt.Sprintf("DRY-RUN Save %s liquidation %s est profit $%.2f", mode, pkStr, cached.estProfit))
		return
	}
	dailyTip.Lock()
	overCap := *dailyTipVal+cached.tipSol > maxDailyTip
	dailyTip.Unlock()
	if overCap {
		log.Reason = "daily tip cap"
		observe.LogDecision(runDir, &log)
		observe.Alert(webhook, "save-cap", "daily tip cap reached")
		return
	}
	if solBalance(rpc, c.authority) < walletMin {
		log.Reason = "wallet below floor"
		observe.LogDecision(runDir, &log)
		observe.Alert(webhook, "save-floor", "wallet below floor — not firing")
		return
	}
	tx := cached.tx
	tx.Message.V0.RecentBlockhash = freshBh
	if err := tx.Sign([]solana.Keypair{*kp}); err != nil {
		log.Reason = "sign failed: " + err.Error()
		observe.LogDecision(runDir, &log)
		return
	}
	sig := tx.Signatures[0].String()
	txB64 := b64tx(&tx)
	repay, estProfit, tipLamports, tipSol := cached.repay, cached.estProfit, cached.tipLamports, cached.tipSol

	var bundleID *string
	var submitErr error
	if mode == "sender" {
		_, submitErr = jito.SendSender(senderURL, txB64)
	} else {
		mu, vaa, age, ok := crank.hermes.UpdateFor(cached.mode.feedID)
		if !ok {
			submitErr = fmt.Errorf("no Hermes blob for feed")
		} else if age > crank.maxBlobAge {
			submitErr = fmt.Errorf("Hermes blob stale (%s) — not bundling", age)
		} else {
			ctxs, err := pythcrank.BuildCrankTxs(c.authority, vaa, []pythaccumulator.MerkleUpdate{mu}, 0, 0, freshBh)
			if err != nil {
				submitErr = err
			} else if err := ctxs.StampAndSign(*kp, freshBh); err != nil {
				submitErr = err
			} else {
				setupB64, fireB64, err := ctxs.ToB64()
				if err != nil {
					submitErr = err
				} else {
					var lastErr error
					for attempt := 0; attempt < 3; attempt++ {
						id, err := jito.SendBundle(crank.blockEngine, []string{setupB64, fireB64, txB64})
						if err == nil {
							bundleID = &id
							submitErr = nil
							lastErr = nil
							break
						}
						lastErr = err
						if strings.Contains(err.Error(), "429") && attempt < 2 {
							time.Sleep(250 * time.Millisecond)
							continue
						}
						break
					}
					if bundleID == nil {
						submitErr = lastErr
					}
				}
			}
		}
	}

	fired := submitErr == nil
	log.Fired = fired
	log.Reason = fmt.Sprintf("fired (%s, armed cache)", mode)
	observe.LogDecision(runDir, &log)
	if fired {
		bundleSuffix := ""
		if bundleID != nil {
			bundleSuffix = " bundle " + *bundleID
		}
		fmt.Fprintf(os.Stderr, "[save] FIRED [%s] %s%s\n", mode, sig, bundleSuffix)
		sigCopy := sig
		observe.LogTrade(runDir, &TradeLog{T: now(), Obligation: pkStr, Protocol: "save", RepayNative: repay,
			EstProfitUSDC: estProfit, TipLamports: tipLamports, Signature: &sigCopy, Bundle: bundleID, RealizedUSDC: nil, Error: nil})
		ownerStr := c.authority.String()
		be := crank.blockEngine
		bid := bundleID
		go func() {
			for _, wait := range []time.Duration{5 * time.Second, 15 * time.Second, 45 * time.Second} {
				time.Sleep(wait)
				if pnl, ok := observe.RealizedUSDC(rpc.Endpoint, sigCopy, ownerStr); ok {
					dailyTip.Lock()
					*dailyTipVal += tipSol
					dailyTip.Unlock()
					pnlCopy := pnl
					observe.LogTrade(runDir, &TradeLog{T: now(), Obligation: "", Protocol: "save", RepayNative: 0,
						EstProfitUSDC: 0.0, TipLamports: 0, Signature: &sigCopy, Bundle: nil, RealizedUSDC: &pnlCopy, Error: nil})
					observe.Alert(webhook, "save-landed", fmt.Sprintf("Save liquidation landed %s: realized $%.2f", sigCopy, pnl))
					return
				}
			}
			status := ""
			if bid != nil {
				status = jito.BundleStatus(be, *bid)
			}
			observe.Alert(webhook, "save-miss", fmt.Sprintf("Save liquidation %s never confirmed (bundle status: %s)", sigCopy, status))
		}()
	} else {
		errStr := submitErr.Error()
		fmt.Fprintf(os.Stderr, "[save] send failed: %s\n", errStr)
		observe.LogTrade(runDir, &TradeLog{T: now(), Obligation: pkStr, Protocol: "save", RepayNative: repay,
			EstProfitUSDC: estProfit, TipLamports: tipLamports, Signature: nil, Bundle: nil, RealizedUSDC: nil, Error: &errStr})
	}
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
	// DRY_RUN unset -> dry_run=true (safe default); ONLY "DRY_RUN=0" goes live.
	dryRun := true
	if v, isSet := os.LookupEnv("DRY_RUN"); isSet {
		dryRun = v != "0"
	}
	runDir := config.EnvOr("RUN_DIR", "runs")
	minDebt := config.EnvFloat("MIN_DEBT_USD", 100.0)
	ratioCap := config.EnvFloat("RATIO_CAP", 3.0)
	minProfit := config.EnvFloat("MIN_PROFIT_USD", 0.5)
	rescan := time.Duration(config.EnvUint64("RESCAN_SECS", 30)) * time.Second
	watchRatio := config.EnvFloat("WATCH_RATIO", 0.85)
	armRatio := config.EnvFloat("ARM_RATIO", 0.97)
	armTTL := time.Duration(config.EnvUint64("ARM_TTL_SECS", 20)) * time.Second
	maxArm := config.EnvInt("MAX_ARM_PER_CYCLE", 8)
	maxFire := config.EnvInt("MAX_FIRE_PER_CYCLE", 4)
	tickPollMs := config.EnvUint64("TICK_POLL_MS", 1)
	poll := time.Duration(config.EnvUint64("POLL_MS", 5000)) * time.Millisecond
	simCooldown := time.Duration(config.EnvUint64("SIM_COOLDOWN_SECS", 60)) * time.Second
	handleCooldown := time.Duration(config.EnvUint64("HANDLE_COOLDOWN_SECS", 20)) * time.Second
	hbEvery := config.EnvUint64("HEARTBEAT_SECS", 30)
	senderURL := config.EnvOr("SENDER_URL", "http://ams-sender.helius-rpc.com/fast")
	webhook := config.EnvOr("ALERT_WEBHOOK", "")
	repayFracsStr, hasRepayFracs := config.EnvOptional("REPAY_FRACS")
	var repayFracs []float64
	if hasRepayFracs {
		for _, x := range strings.Split(repayFracsStr, ",") {
			if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
				repayFracs = append(repayFracs, f)
			}
		}
	}
	if len(repayFracs) == 0 {
		repayFracs = []float64{0.2, 0.1, 0.05}
	}
	maxDailyTipSol := config.EnvFloat("MAX_DAILY_TIP_SOL", 0.05)
	walletMinSol := config.EnvFloat("WALLET_MIN_SOL", 0.02)

	c := cfg{
		authority:       solana.Pubkey{}, // set after keypair
		tipAccount:      solana.MustPubkeyFromBase58(config.EnvOr("SENDER_TIP_ACCOUNT", "2nyhqdwKcJZR2vcqCyrYsaPVdAnFoJjiksCXJ7hfEYgD")),
		tipFractionBps:  config.EnvUint64("TIP_FRACTION_BPS", 3000),
		minTipSol:       config.EnvFloat("MIN_TIP_SOL", 0.0002),
		minProfit:       minProfit,
		slippageBps:     uint32(config.EnvUint64("SLIPPAGE_BPS", 100)),
		maxSwapAccounts: config.EnvInt("MAX_SWAP_ACCOUNTS", 18),
	}

	var kp *solana.Keypair
	if keypairPath, isSet := config.EnvOptional("KEYPAIR_PATH"); isSet {
		raw, err := os.ReadFile(keypairPath)
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
	c.authority = authority

	rpc := rpcclient.New(endpoint)

	// Lazer WebSocket: the event-driven trigger. Without a token the loop
	// still runs but only on the slow poll fallback — warn loudly, since
	// that's the exact 30s-poll regression this rewrite exists to kill.
	lazerTable := pyth.NewTable()
	mintFeed := mintFeedExt()
	lazerOn := false
	if token, isSet := config.EnvOptional("PYTH_LAZER_TOKEN"); isSet {
		lazer.SpawnLazerThread(token, armFeeds(), lazerTable)
		fmt.Fprintln(os.Stderr, "[save] Pyth Lazer event-driven trigger ENABLED")
		lazerOn = true
	}
	if !lazerOn {
		fmt.Fprintln(os.Stderr, "[save] WARNING: no PYTH_LAZER_TOKEN — falling back to slow poll (the 30s regression). Set the token for ms detection.")
	}

	// Crank context (front-run Solend's own cranker on stale feeds).
	crankOn := true
	if v, isSet := os.LookupEnv("CRANK"); isSet {
		crankOn = v != "0"
	}
	crankOn = crankOn && lazerOn
	blockEngine := jito.DefaultBlockEngine()
	var tips []solana.Pubkey
	if crankOn {
		tips, _ = jito.GetTipAccounts(blockEngine)
	}
	if crankOn && len(tips) == 0 {
		fmt.Fprintln(os.Stderr, "[save] getTipAccounts failed — using fallback Jito tip list")
		tips = []solana.Pubkey{solana.MustPubkeyFromBase58("DttWaMuVvTiduZRnguLF7jNxTgiMBZ1hyAumKUiL2KRL")}
	}
	hermesURL := config.EnvOr("HERMES", "https://hermes.pyth.network")
	maxBlobMs := config.EnvUint64("MAX_BLOB_AGE_MS", 3000)
	crank := crankCtx{
		on:          crankOn,
		hermes:      pythaccumulator.SpawnHermesCache(hermesURL, nil, 400*time.Millisecond),
		tips:        tips,
		blockEngine: blockEngine,
		maxBlobAge:  time.Duration(maxBlobMs) * time.Millisecond,
	}

	// Debt reserves decoded once (stable accounts). Each has a wired JupLend
	// flash market and is what the fire path repays: USDC/USDT/wSOL.
	debtReserves := map[solana.Pubkey]save.Reserve{}
	for _, res := range save.DebtReserves {
		pk := solana.MustPubkeyFromBase58(res)
		data, err := rpc.GetAccountData(pk)
		if err != nil || data == nil {
			fatalf("fetch debt reserve %s", res)
		}
		r, ok := save.DecodeReserve(pk, data)
		if !ok {
			fatalf("decode debt reserve %s", res)
		}
		debtReserves[pk] = r
	}

	mode := "[DRY RUN]"
	if !dryRun {
		mode = "[LIVE]"
	}
	fmt.Fprintf(os.Stderr, "[save] Solend liquidation executor %s  authority=%s  min_debt=$%v rescan=%s tick_poll=%dms lazer=%v crank=%v\n",
		mode, authority, minDebt, rescan, tickPollMs, lazerOn, crank.on)
	if !dryRun {
		bal := solBalance(rpc, authority)
		fmt.Fprintf(os.Stderr, "[save] wallet balance: %v SOL\n", bal)
		if bal < walletMinSol {
			fatalf("wallet below floor")
		}
	}

	engine := saveengine.NewEngine(minDebt, ratioCap)
	ctpCache := map[solana.Pubkey]solana.Pubkey{}
	scan, ok := fullScanSave(rpc, debtReserves, minDebt, ctpCache)
	if !ok {
		fatalf("initial scan")
	}
	lastScan := time.Now()

	var dailyTipMu sync.Mutex
	dailyTip := 0.0
	tipDay := now() / 86_400
	freshBh := solana.Hash{}
	lastBh := time.Now().Add(-9999 * time.Second)
	handled := map[solana.Pubkey]time.Time{}
	simRejected := map[solana.Pubkey]time.Time{}
	cache := map[solana.Pubkey]cachedFire{}
	var lastTickUs uint64
	lastHb := time.Now().Add(-9999 * time.Second)
	armDeferred := 0
	first := true

	lazerSnapshot := func() map[uint32]float64 {
		out := map[uint32]float64{}
		for _, f := range armFeeds() {
			if p, ok := pyth.Get(lazerTable, f); ok {
				out[f] = p.Price
			}
		}
		return out
	}

	for {
		// Rebuild the watch-set + engine from a full scan.
		if first || time.Since(lastScan) >= rescan {
			if !first {
				if s, ok := fullScanSave(rpc, debtReserves, minDebt, ctpCache); ok {
					scan = s
				}
			}
			lastScan = time.Now()
			snap := lazerSnapshot()
			armed := engine.Rebuild(scan.obls, scan.reserves, mintFeed, watchRatio, snap)
			fmt.Fprintf(os.Stderr, "[save] scan: %d v1 USDC/USDT/wSOL-debt obligations (>= $%v) -> engine watch-set %d (ratio >= %v)\n",
				len(scan.obls), minDebt, armed, watchRatio)
			if crank.on {
				feedSeen := map[[32]byte]struct{}{}
				var hex []string
				for i := range engine.Accounts {
					w := &engine.Accounts[i]
					if _, ok := scan.crankable[w.CollReserve]; !ok {
						continue
					}
					fid, ok := scan.feedOf[w.CollReserve]
					if !ok {
						continue
					}
					if _, seen := feedSeen[fid]; seen {
						continue
					}
					feedSeen[fid] = struct{}{}
					hex = append(hex, fmt.Sprintf("%x", fid[:]))
				}
				fmt.Fprintf(os.Stderr, "[save] crank: %d crankable collateral reserves, %d feeds in Hermes cache\n", len(scan.crankable), len(hex))
				crank.hermes.SetFeeds(hex)
			}
			first = false
		}

		day := now() / 86_400
		if day != tipDay {
			tipDay = day
			dailyTipMu.Lock()
			dailyTip = 0.0
			dailyTipMu.Unlock()
		}
		if time.Since(lastBh) >= 2*time.Second {
			if bh, err := rpc.GetLatestBlockhash(); err == nil {
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
				time.Sleep(time.Duration(tickPollMs) * time.Millisecond)
			}
			snap = lazerSnapshot()
		} else {
			time.Sleep(poll)
			snap = lazerSnapshot()
		}

		// FIRE tier: TWO-TIER GATING. Lazer NARROWS the watch-set (below);
		// the ON-CHAIN oracle price GATES the expensive sim/submit work.
		// Only obligations liquidatable at the on-chain price Solend settles
		// against (Solend's authoritative stored health captured fresh at
		// the last rescan — ZERO Lazer projection) earn a sim, ranked by USD
		// deficit and capped top-K so the biggest real opportunity wins.
		// Gating on the Lazer-projected ratio instead flooded ~390
		// phantoms/cycle through simulateTransaction/Bundle (healthy
		// on-chain), starving a genuine opportunity's sim budget.
		//
		// Solend refreshes obligation health lazily, so some read
		// stored-liquidatable while a fresh sim shows them healthy ("healthy
		// at fresh price"). Once one sim-rejects we SUPPRESS it for the
		// cooldown, so a learned phantom can't keep occupying the capped
		// top-K and crowd out a real opportunity — the fire set converges
		// onto genuine/untested obligations.
		var liveFire []saveengine.ObligationDeficit
		for _, od := range engine.OnchainLiquidatableRanked() {
			if t, ok := simRejected[od.Obligation]; !ok || time.Since(t) >= simCooldown {
				liveFire = append(liveFire, od)
			}
		}
		fireDeferred := 0
		if len(liveFire) > maxFire {
			fireDeferred = len(liveFire) - maxFire
		}
		if len(liveFire) > maxFire {
			liveFire = liveFire[:maxFire]
		}
		crossed := make([]solana.Pubkey, 0, len(liveFire))
		for _, od := range liveFire {
			crossed = append(crossed, od.Obligation)
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
			onchainLiq := engine.OnchainLiquidatableCount()
			liveFireCt := 0
			for _, od := range engine.OnchainLiquidatableRanked() {
				if t, ok := simRejected[od.Obligation]; !ok || time.Since(t) >= simCooldown {
					liveFireCt++
				}
			}
			var freshest uint64
			for _, f := range armFeeds() {
				if p, ok := pyth.Get(lazerTable, f); ok && p.TsUs > freshest {
					freshest = p.TsUs
				}
			}
			lagMs := (nowUs() - freshest) / 1000
			deferStr := ""
			if fireDeferred+armDeferred > 0 {
				deferStr = fmt.Sprintf(" | DEFERRED fire %d/arm %d", fireDeferred, armDeferred)
			}
			fmt.Fprintf(os.Stderr, "[hb] lazer feeds %d/%d live | detect_lag %dms | watch %d | lazer-flagged %d (>=arm(%v) %d) | on-chain liquidatable %d | LIVE fire %d (cap %d) | cache %d%s | %s\n",
				len(snap), totalFeeds, lagMs, len(engine.Accounts), lazerFlagged, armRatio, lazerNear,
				onchainLiq, liveFireCt, maxFire, len(cache), deferStr, lazer.Status(lazerTable))
			lastHb = time.Now()
		}

		// ARM phase: keep a hot, sim-verified fire tx for the FIRE tier (the
		// on-chain-liquidatable set, top-K) so a tick -> sign+send is
		// instant. This is the ONLY place a sim runs, and it is bounded by
		// that small set — the broad Lazer near-threshold set is WATCHED
		// but NEVER simulated (that was the phantom flood). simRejected
		// suppresses re-simming an obligation that just sim-rejected
		// ("healthy at fresh price / too small") for a cooldown.
		if lazerOn {
			fireKeys := map[solana.Pubkey]struct{}{}
			for _, pk := range crossed {
				fireKeys[pk] = struct{}{}
			}
			for pk, c2 := range cache {
				if _, ok := fireKeys[pk]; !ok || time.Since(c2.built) >= armTTL {
					delete(cache, pk)
				}
			}
			var candidates []solana.Pubkey
			for _, pk := range crossed {
				if _, ok := cache[pk]; ok {
					continue
				}
				if t, ok := simRejected[pk]; ok && time.Since(t) < simCooldown {
					continue
				}
				candidates = append(candidates, pk)
			}
			if len(candidates) > maxArm {
				armDeferred = len(candidates) - maxArm
				candidates = candidates[:maxArm]
			} else {
				armDeferred = 0
			}
			for _, pk := range candidates {
				ratio, _ := engine.OnchainRatioOf(pk)
				if cf, ok := tryArm(rpc, runDir, &c, &crank, scan, pk, repayFracs, ratio); ok {
					cache[pk] = *cf
				} else {
					simRejected[pk] = time.Now()
				}
			}
		}

		// Drop recently-handled obligations (avoid per-tick spin on a standing cross).
		var toFire []solana.Pubkey
		for _, pk := range crossed {
			if t, ok := handled[pk]; !ok || time.Since(t) >= handleCooldown {
				toFire = append(toFire, pk)
			}
		}
		if len(toFire) == 0 {
			continue
		}

		// FIRE phase: prefer the armed cache (instant); else arm inline now.
		for _, pk := range toFire {
			handled[pk] = time.Now()
			ratio, ok := engine.OnchainRatioOf(pk)
			if !ok {
				ratio = 1.0
			}
			var cached *cachedFire
			if cf, ok := cache[pk]; ok && time.Since(cf.built) < armTTL {
				cfCopy := cf
				cached = &cfCopy
				delete(cache, pk)
			} else {
				delete(cache, pk)
				// Respect the sim cooldown here too: a just-rejected
				// obligation stays on-chain-liquidatable (stored health is
				// fixed until the next rescan), so without this guard the
				// fire path would re-sim the same phantom every cycle — the
				// exact flood we're killing.
				if t, isRejected := simRejected[pk]; isRejected && time.Since(t) < simCooldown {
					cached = nil
				} else if cf, ok := tryArm(rpc, runDir, &c, &crank, scan, pk, repayFracs, ratio); ok {
					cached = cf
				} else {
					simRejected[pk] = time.Now()
				}
			}
			if cached != nil {
				armedFromCache := time.Since(cached.built).Milliseconds() > 0
				fireStart := nowUs()
				fireCached(rpc, runDir, senderURL, &c, &crank, dryRun, pk, cached, freshBh,
					kp, &dailyTipMu, &dailyTip, maxDailyTipSol, walletMinSol, webhook)
				done := nowUs()
				logLatency(runDir, map[string]any{
					"t": now(), "obligation": pk.String(), "protocol": "save", "mode": cached.mode.name(),
					"appeared_us":     lastTickUs,
					"detected_lag_ms": saturatingSubDiv(fireStart, lastTickUs, 1000),
					"submit_lag_ms":   saturatingSubDiv(done, lastTickUs, 1000),
					"fire_submit_ms":  (done - fireStart) / 1000,
					"armed":           armedFromCache, "dry_run": dryRun,
				})
			}
		}
	}
}

func pow10(exp int) float64 {
	r := 1.0
	for i := 0; i < exp; i++ {
		r *= 10
	}
	return r
}

func saturatingSubDiv(a, b, div uint64) uint64 {
	if a < b {
		return 0
	}
	return (a - b) / div
}
