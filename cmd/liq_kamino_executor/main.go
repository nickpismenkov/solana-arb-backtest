// Command liq_kamino_executor is the production Kamino (KLend) liquidation
// executor — EVENT-DRIVEN, DRY_RUN default.
//
// The old build polled stored on-chain health every 30s (RESCAN_SECS) /
// re-read the watch-set every 5s (POLL_MS). That loses the race: Kamino's
// Scope oracle updates on-chain and whoever submits a liquidate in the
// same/next slot wins, while a 5-30s poll shows up long after. This rewrite
// mirrors the marginfi/Save executors -- a Lazer WebSocket feeds an
// in-memory health engine (internal/kaminoengine) that recomputes every
// obligation's bf_debt/threshold on each ~ms price tick with ZERO RPC, so a
// cross is noticed in ms not seconds.
//
// TWO-TIER gating (the overflag fix): Lazer NARROWS the set; the ON-CHAIN
// Scope price GATES the expensive work. KLend liquidations settle at the
// on-chain Scope oracle, and Lazer LEADS/diverges from Scope, so the
// Lazer-projected "liquidatable" set is ~900 phantoms that are healthy
// on-chain. Building a quote+sim fire tx for each hammered Jupiter into a
// 429 storm and starved real opportunities. So:
//
//	full scan (RESCAN_SECS): v1 (1 deposit / 1 wired-debt borrow, non-elevation)
//	  obligations + their reserves -> kaminoengine watch-set (stored on-chain
//	  health + per-side Lazer anchors)
//	ARM tier (cheap, Lazer): the near-threshold watch-set -- recomputed per
//	  tick with ZERO RPC, NO Jupiter, NO sim. Only narrows who's worth watching.
//	FIRE tier (expensive): ONLY obligations liquidatable at the on-chain
//	  Scope price (engine.OnChainLiquidatableRanked -- stored health, not the
//	  Lazer projection), ranked by USD deficit, capped to MAX_FIRE_PER_CYCLE.
//	  These get the Jupiter quote + sim + submit; a quote/sim reject ->
//	  cooldown so the same candidate isn't re-hammered every cycle.
//
// Kamino prices via Scope (its own oracle) which we cannot crank ourselves,
// so unlike Save there is no crank/bundle mode -- a single Sender tx. Safety
// is profit-or-revert: the JupLend fixed-amount payback fails unless the
// seized-collateral swap covered the flash-borrow, so a premature or losing
// fire that lands costs only the base fee; the fire sim is a clean
// full-execution OR a revert only at Kamino's own liquidate/health gate.
//
// v1.5 debt scope (preserved from PR #67): any debt with a wired JupLend
// flash market -- USDC / USDT / wSOL.
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
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"arbengine/internal/config"
	"arbengine/internal/flashloan"
	"arbengine/internal/jito"
	"arbengine/internal/kamino"
	"arbengine/internal/kaminoengine"
	"arbengine/internal/kaminofire"
	"arbengine/internal/kaminoix"
	"arbengine/internal/lazer"
	"arbengine/internal/observe"
	"arbengine/internal/pyth"
	"arbengine/internal/rpcclient"
	"arbengine/internal/solana"
)

const (
	klendProgram     = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"
	mainMarket       = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"
	tokenProgram     = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	obligationSize   = 3344
	defaultAuthority = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak"
	// [cu, cu_price, ata, ata, ata, borrow, refresh, refresh, refresh_ob, LIQUIDATE, ...]
	liquidateIxIndex = 9
)

func nowSecs() uint64 { return uint64(time.Now().Unix()) }
func nowUs() int64    { return time.Now().UnixMicro() }

// logLatency appends the latency ledger -- proves whether SPEED is (still)
// the bottleneck. appearedUs is the Lazer PUBLISH timestamp of the tick that
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

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func getMultiple(rpc *rpcclient.Client, keys []solana.Pubkey) map[solana.Pubkey][]byte {
	out := make(map[solana.Pubkey][]byte)
	for i := 0; i < len(keys); i += 100 {
		end := i + 100
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[i:end]
		accs, err := rpc.GetMultipleAccounts(chunk)
		if err != nil {
			continue
		}
		for j, acc := range accs {
			if acc != nil && acc.Data != nil {
				out[chunk[j]] = acc.Data
			}
		}
	}
	return out
}

func mintOwner(rpc *rpcclient.Client, mint solana.Pubkey, cache map[solana.Pubkey]solana.Pubkey) solana.Pubkey {
	if p, ok := cache[mint]; ok {
		return p
	}
	p := solana.MustPubkeyFromBase58(tokenProgram)
	if info, err := rpc.GetAccountInfo(mint); err == nil && info != nil {
		if owner, err := solana.PubkeyFromBase58(info.Owner); err == nil {
			p = owner
		}
	}
	cache[mint] = p
	return p
}

func solBalance(rpc *rpcclient.Client, owner solana.Pubkey) float64 {
	lamports, err := rpc.GetBalance(owner)
	if err != nil {
		return 0
	}
	return float64(lamports) / 1e9
}

// simClass classifies a full-tx sim outcome by where it stopped (mirrors
// kamino_fire_probe).
type simClass int

const (
	// simClean: err null -- whole flashloan-wrapped tx executes (on-chain
	// liquidatable + profitable).
	simClean simClass = iota
	// simLiquidateGate: reverts only at Kamino's own liquidate/health/close-
	// factor gate (ix 9) -- wiring OK, armed AHEAD of the on-chain cross.
	simLiquidateGate
	// simOtherRevert: reverts at some other ix -- a wiring problem; don't arm.
	simOtherRevert
	// simReject: RPC rejected the sim (no value) -- treat as unusable.
	simReject
)

func classifySim(rpc *rpcclient.Client, txB64 string) (simClass, uint64) {
	raw, err := rpc.SimulateTransaction(txB64)
	if err != nil || raw == nil {
		return simReject, 0
	}
	var v struct {
		Err json.RawMessage `json:"err"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return simReject, 0
	}
	if len(v.Err) == 0 || string(v.Err) == "null" {
		return simClean, 0
	}
	var withIxErr struct {
		InstructionError json.RawMessage `json:"InstructionError"`
	}
	if err := json.Unmarshal(v.Err, &withIxErr); err != nil || withIxErr.InstructionError == nil {
		return simReject, 0
	}
	var tuple []json.RawMessage
	if err := json.Unmarshal(withIxErr.InstructionError, &tuple); err != nil || len(tuple) == 0 {
		return simReject, 0
	}
	var ix uint64
	if err := json.Unmarshal(tuple[0], &ix); err != nil {
		return simReject, 0
	}
	if ix == liquidateIxIndex {
		return simLiquidateGate, ix
	}
	return simOtherRevert, ix
}

// decisionLog mirrors the Rust DecisionLog serde field names verbatim.
type decisionLog struct {
	T             uint64  `json:"t"`
	Obligation    string  `json:"obligation"`
	Protocol      string  `json:"protocol"`
	Ratio         float64 `json:"ratio"`
	DebtUsd       float64 `json:"debt_usd"`
	RepayUsd      float64 `json:"repay_usd"`
	QuotedUsdcOut float64 `json:"quoted_usdc_out"`
	EstProfitUsdc float64 `json:"est_profit_usdc"`
	FireSimOk     bool    `json:"fire_sim_ok"`
	Fired         bool    `json:"fired"`
	Reason        string  `json:"reason"`
}

// tradeLog mirrors the Rust TradeLog serde field names verbatim.
type tradeLog struct {
	T             uint64   `json:"t"`
	Obligation    string   `json:"obligation"`
	Protocol      string   `json:"protocol"`
	RepayUsd      float64  `json:"repay_usd"`
	EstProfitUsdc float64  `json:"est_profit_usdc"`
	TipLamports   uint64   `json:"tip_lamports"`
	Signature     *string  `json:"signature"`
	RealizedUsdc  *float64 `json:"realized_usdc"`
	Error         *string  `json:"error"`
}

func skip(runDir string, log *decisionLog, reason string) {
	log.Reason = reason
	observe.LogDecision(runDir, log)
}

// kaminoScan is a full scan: v1 obligations + the reserve -> Lazer-feed map
// they resolve to. (Fresh reserve prices/wiring are re-fetched at arm time;
// only the stable reserve->feed mapping is kept here for the engine's ratio
// anchoring.)
type kaminoScan struct {
	obls        []kaminoengine.ObligationEntry
	obIndex     map[solana.Pubkey]kamino.Obligation
	reserveFeed map[solana.Pubkey]uint32
	// reserveMint maps reserve pk -> liquidity mint (for the wired-flash-
	// market gate).
	reserveMint map[solana.Pubkey]solana.Pubkey
}

func scanObligations(rpc *rpcclient.Client) []kaminoengine.ObligationEntry {
	entries, err := rpc.GetProgramAccounts(solana.MustPubkeyFromBase58(klendProgram), rpcclient.GetProgramAccountsOpts{
		Filters: []any{
			map[string]any{"dataSize": obligationSize},
			map[string]any{"memcmp": map[string]any{"offset": 32, "bytes": mainMarket}},
		},
		DataSlice: &struct {
			Offset int `json:"offset"`
			Length int `json:"length"`
		}{Offset: 0, Length: 2288},
	})
	if err != nil {
		return nil
	}
	out := make([]kaminoengine.ObligationEntry, 0, len(entries))
	for _, e := range entries {
		if e.Account.Data == nil {
			continue
		}
		o, ok := kamino.DecodeObligation(e.Account.Data)
		if !ok {
			continue
		}
		if len(o.Deposits) != 1 || len(o.Borrows) != 1 || o.ElevationGroup != 0 {
			continue
		}
		out = append(out, kaminoengine.ObligationEntry{Pubkey: e.Pubkey, Obligation: *o})
	}
	return out
}

// fullScanKamino scans obligations, keeps v1 shape, loads their reserves
// (price + wiring), and builds the reserve -> Lazer-feed map (via each
// reserve's liquidity mint).
//
// Anchor on CURRENT on-chain (Scope) health, NOT the obligation's stored
// health. The stored bf_adjusted_debt/unhealthy_borrow_value are only as
// fresh as the obligation's last refresh -- a position that WAS underwater
// but has since been priced healthy still reads "liquidatable" from its
// stale stored values, which over-flagged the fire tier (DRY_RUN: ~12
// phantoms, all reverting at the liquidate gate). Recompute reprices every
// position from the reserves' fresh Scope prices, so the engine anchors on
// what KLend will actually settle at. Interest isn't re-accrued (conservative
// -> no false-positive).
func fullScanKamino(rpc *rpcclient.Client, minDebt float64, mintFeed map[solana.Pubkey]uint32) *kaminoScan {
	obls := scanObligations(rpc)
	if len(obls) == 0 {
		return nil
	}
	reservePkSet := map[solana.Pubkey]struct{}{}
	for _, e := range obls {
		for _, d := range e.Obligation.Deposits {
			reservePkSet[d.Reserve] = struct{}{}
		}
		for _, b := range e.Obligation.Borrows {
			reservePkSet[b.Reserve] = struct{}{}
		}
	}
	reservePks := make([]solana.Pubkey, 0, len(reservePkSet))
	for pk := range reservePkSet {
		reservePks = append(reservePks, pk)
	}
	raw := getMultiple(rpc, reservePks)

	reserveFeed := map[solana.Pubkey]uint32{}
	reserveMint := map[solana.Pubkey]solana.Pubkey{}
	reserves := map[solana.Pubkey]*kamino.Reserve{}
	for _, pk := range reservePks {
		d, ok := raw[pk]
		if !ok {
			continue
		}
		if ra, ok := kaminoix.DecodeReserveAccounts(pk, d); ok {
			reserveMint[pk] = ra.LiquidityMint
			if f, ok := mintFeed[ra.LiquidityMint]; ok {
				reserveFeed[pk] = f
			}
		}
		if r, ok := kamino.DecodeReserve(d); ok {
			reserves[pk] = r
		}
	}

	filtered := obls[:0]
	for _, e := range obls {
		o := e.Obligation
		rc := kamino.Recompute(&o, reserves)
		if rc.Trustworthy() {
			o.BfAdjustedDebt = rc.BfAdjustedDebt
			o.UnhealthyBorrowValue = rc.UnhealthyBorrowValue
			o.AllowedBorrowValue = rc.AllowedBorrowValue
			o.DepositedValue = rc.DepositedValue
			var borrowed float64
			for _, b := range o.Borrows {
				r, ok := reserves[b.Reserve]
				if !ok {
					continue
				}
				borrowed += (b.Amount / pow10(int(r.MintDecimals))) * r.MarketPrice
			}
			o.BorrowedValue = borrowed
		}
		if o.BorrowedValue >= minDebt {
			filtered = append(filtered, kaminoengine.ObligationEntry{Pubkey: e.Pubkey, Obligation: o})
		}
	}

	obIndex := make(map[solana.Pubkey]kamino.Obligation, len(filtered))
	for _, e := range filtered {
		obIndex[e.Pubkey] = e.Obligation
	}
	return &kaminoScan{obls: filtered, obIndex: obIndex, reserveFeed: reserveFeed, reserveMint: reserveMint}
}

func pow10(n int) float64 {
	v := 1.0
	for i := 0; i < n; i++ {
		v *= 10
	}
	return v
}

// cfg bundles the executor's static guardrail/config values.
type cfg struct {
	authority       solana.Pubkey
	tipAccount      solana.Pubkey
	tipFractionBps  uint64
	minTipSol       float64
	minProfit       float64
	closeFactor     float64
	maxBorrowUsd    float64
	slippageBps     uint32
	maxSwapAccounts int
}

// cachedFire is a sim-verified fire tx kept hot for an armed obligation.
// Compiled with a placeholder blockhash (sim replaces it); the real hash is
// stamped at fire.
type cachedFire struct {
	tx          solana.VersionedTransaction
	tipLamports uint64
	tipSol      float64
	estProfit   float64
	repayUsd    float64
	debtUsd     float64
	ratio       float64
	// clean = true: sim ran fully CLEAN (already liquidatable on-chain);
	// false = armed ahead of the on-chain cross (sim reverted only at the
	// liquidate gate).
	clean bool
	built time.Time
}

// tryArm builds + sizes + profit-gates + sim-gates one obligation ->
// cachedFire. This is the only place a fire tx is built (build + Jupiter
// quote + sim), all off the fire critical path. Accepts a sim that is CLEAN
// or reverts only at Kamino's own liquidate gate (armed ahead of the Scope
// cross).
func tryArm(
	rpc *rpcclient.Client, endpoint, runDir string, c *cfg, scan *kaminoScan,
	pk solana.Pubkey, engineRatio float64, tpCache map[solana.Pubkey]solana.Pubkey,
) *cachedFire {
	market := solana.MustPubkeyFromBase58(mainMarket)
	o, ok := scan.obIndex[pk]
	if !ok {
		return nil
	}
	if len(o.Deposits) != 1 || len(o.Borrows) != 1 || o.ElevationGroup != 0 {
		return nil
	}
	withdrawPk := o.Deposits[0].Reserve
	repayPk := o.Borrows[0].Reserve

	log := decisionLog{T: nowSecs(), Obligation: pk.String(), Protocol: "kamino", Ratio: engineRatio}

	raw := getMultiple(rpc, []solana.Pubkey{withdrawPk, repayPk})
	wrData, ok1 := raw[withdrawPk]
	rrData, ok2 := raw[repayPk]
	if !ok1 || !ok2 {
		skip(runDir, &log, "reserve fetch failed")
		return nil
	}
	wr, ok1 := kaminoix.DecodeReserveAccounts(withdrawPk, wrData)
	rr, ok2 := kaminoix.DecodeReserveAccounts(repayPk, rrData)
	if !ok1 || !ok2 {
		skip(runDir, &log, "reserve accounts decode failed")
		return nil
	}
	wrRes, ok1 := kamino.DecodeReserve(wrData)
	rrRes, ok2 := kamino.DecodeReserve(rrData)
	if !ok1 || !ok2 {
		skip(runDir, &log, "reserve decode failed")
		return nil
	}
	// v1.5: any debt with a wired JupLend flash market (USDC/USDT/wSOL). Preserved.
	if !flashloan.HasMarket(rr.LiquidityMint) {
		skip(runDir, &log, "debt mint has no wired flash market")
		return nil
	}

	debtDec := int(rrRes.MintDecimals)
	debtPrice := rrRes.MarketPrice
	if debtPrice < 1e-9 {
		debtPrice = 1e-9
	}
	debtUsd := (o.Borrows[0].Amount / pow10(debtDec)) * rrRes.MarketPrice
	repayUsd := debtUsd * c.closeFactor
	if repayUsd > c.maxBorrowUsd {
		repayUsd = c.maxBorrowUsd
	}
	if repayUsd < 1.0 {
		repayUsd = 1.0
	}
	repayAmount := uint64(repayUsd / debtPrice * pow10(debtDec))
	const bonus = 1.05
	wrPrice := wrRes.MarketPrice
	if wrPrice < 1e-9 {
		wrPrice = 1e-9
	}
	seizedNative := repayUsd * bonus / wrPrice * pow10(int(wrRes.MintDecimals))
	swapInAmount := uint64(seizedNative * 0.995)
	log.DebtUsd = debtUsd
	log.RepayUsd = repayUsd

	cand := kaminofire.KaminoFireCandidate{
		Obligation:                     pk,
		LendingMarket:                  market,
		RepayReserve:                   *rr,
		WithdrawReserve:                *wr,
		ObligationReserves:             []solana.Pubkey{withdrawPk, repayPk},
		WithdrawLiquidityMint:          wr.LiquidityMint,
		WithdrawLiquidityTokenProgram:  mintOwner(rpc, wr.LiquidityMint, tpCache),
		WithdrawCollateralTokenProgram: mintOwner(rpc, wr.CollateralMint, tpCache),
		RepayLiquidityTokenProgram:     mintOwner(rpc, rr.LiquidityMint, tpCache),
		RepayAmount:                    repayAmount,
		SwapInAmount:                   swapInAmount,
	}
	// Placeholder blockhash -- sim replaces it; the live fire stamps the fresh hash.
	ph := solana.Hash{}

	// First build (no tip) to get the Jupiter quote for the profit gate.
	fire, err := kaminofire.BuildFireTx(endpoint, &cand, c.authority, nil, 0, 100_000, c.slippageBps, c.maxSwapAccounts, ph)
	if err != nil {
		skip(runDir, &log, "build: "+err.Error())
		return nil
	}
	quotedUsd := float64(fire.QuotedUSDCOut) / pow10(debtDec) * debtPrice
	estProfit := quotedUsd - repayUsd
	log.QuotedUsdcOut = quotedUsd
	log.EstProfitUsdc = estProfit
	const solUsd = 150.0 // conservative; tip is tiny vs profit
	tipSol := estProfit * float64(c.tipFractionBps) / 10_000.0 / solUsd
	if tipSol < c.minTipSol {
		tipSol = c.minTipSol
	}
	tipLamports := uint64(tipSol * 1e9)
	if estProfit < c.minProfit+tipSol*solUsd {
		skip(runDir, &log, fmt.Sprintf("below min profit (est $%.2f, tip $%.2f)", estProfit, tipSol*solUsd))
		return nil
	}

	// Final build WITH the tip, sim-gate.
	tipAccount := c.tipAccount
	fire, err = kaminofire.BuildFireTx(endpoint, &cand, c.authority, &tipAccount, tipLamports, 100_000, c.slippageBps, c.maxSwapAccounts, ph)
	if err != nil {
		skip(runDir, &log, "rebuild: "+err.Error())
		return nil
	}
	txB64, err := fire.Tx.Base64()
	if err != nil {
		skip(runDir, &log, "encode: "+err.Error())
		return nil
	}
	class, ixErr := classifySim(rpc, txB64)
	clean := class == simClean
	log.FireSimOk = class == simClean || class == simLiquidateGate
	switch class {
	case simClean, simLiquidateGate:
	case simOtherRevert:
		skip(runDir, &log, fmt.Sprintf("sim revert at ix %d (wiring) -- not arming", ixErr))
		return nil
	default:
		skip(runDir, &log, "sim rejected by RPC")
		return nil
	}
	if clean {
		log.Reason = "armed (clean -- liquidatable on-chain now)"
	} else {
		log.Reason = "armed (ahead -- reverts at liquidate gate until Scope crosses)"
	}
	observe.LogDecision(runDir, &log)
	return &cachedFire{
		tx: fire.Tx, tipLamports: tipLamports, tipSol: tipSol, estProfit: estProfit,
		repayUsd: repayUsd, debtUsd: debtUsd, ratio: engineRatio, clean: clean, built: time.Now(),
	}
}

// fireCached fires a cached tx: stamps fresh blockhash, signs, submits via
// Helius Sender, logs, spawns P&L readback. No build/quote/sim here -- the
// hot path is submit-only.
func fireCached(
	rpc *rpcclient.Client, endpoint, runDir, senderURL string, c *cfg, dryRun bool,
	pk solana.Pubkey, cached *cachedFire, freshBh solana.Hash, kp *solana.Keypair,
	dailyTip *sync.Mutex, dailyTipSol *float64, maxDailyTip, walletMin float64, webhook string,
) {
	log := decisionLog{
		T: nowSecs(), Obligation: pk.String(), Protocol: "kamino", Ratio: cached.ratio, DebtUsd: cached.debtUsd,
		RepayUsd: cached.repayUsd, EstProfitUsdc: cached.estProfit, FireSimOk: true,
	}
	tag := "ahead"
	if cached.clean {
		tag = "clean"
	}
	pkStr := pk.String()
	shortPk := pkStr
	if len(shortPk) > 8 {
		shortPk = shortPk[:8]
	}
	fmt.Printf("★ KAMINO LIQUIDATABLE %s  debt $%.0f  repay $%.2f  est profit $%.2f  tip %.5f SOL  (%s armed %v ago)\n",
		shortPk, cached.debtUsd, cached.repayUsd, cached.estProfit, cached.tipSol, tag, time.Since(cached.built))
	if dryRun {
		log.Reason = fmt.Sprintf("dry-run: would fire (armed, %s)", tag)
		observe.LogDecision(runDir, &log)
		observe.Alert(webhook, "kliq-dry", fmt.Sprintf("DRY-RUN Kamino liq %s est profit $%.2f", pkStr, cached.estProfit))
		return
	}
	dailyTip.Lock()
	overCap := *dailyTipSol+cached.tipSol > maxDailyTip
	dailyTip.Unlock()
	if overCap {
		log.Reason = "daily tip cap"
		observe.LogDecision(runDir, &log)
		observe.Alert(webhook, "kliq-cap", "daily tip cap reached")
		return
	}
	if solBalance(rpc, c.authority) < walletMin {
		log.Reason = "wallet below floor"
		observe.LogDecision(runDir, &log)
		observe.Alert(webhook, "kliq-floor", "wallet below floor -- not firing")
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
	txB64, err := tx.Base64()
	if err != nil {
		log.Reason = "encode failed: " + err.Error()
		observe.LogDecision(runDir, &log)
		return
	}
	repayUsd, estProfit, tipLamports, tipSol := cached.repayUsd, cached.estProfit, cached.tipLamports, cached.tipSol
	log.Fired = true
	log.Reason = "fired (armed cache)"
	observe.LogDecision(runDir, &log)
	if _, err := jito.SendSender(senderURL, txB64); err != nil {
		fmt.Fprintf(os.Stderr, "[kexec] send failed: %v\n", err)
		errStr := err.Error()
		observe.LogTrade(runDir, &tradeLog{
			T: nowSecs(), Obligation: pkStr, Protocol: "kamino", RepayUsd: repayUsd,
			EstProfitUsdc: estProfit, TipLamports: tipLamports, Error: &errStr,
		})
		return
	}
	fmt.Fprintf(os.Stderr, "[kexec] FIRED %s\n", sig)
	sigCopy := sig
	observe.LogTrade(runDir, &tradeLog{
		T: nowSecs(), Obligation: pkStr, Protocol: "kamino", RepayUsd: repayUsd,
		EstProfitUsdc: estProfit, TipLamports: tipLamports, Signature: &sigCopy,
	})
	owner := c.authority.String()
	go func() {
		for _, wait := range []int{5, 15, 45} {
			time.Sleep(time.Duration(wait) * time.Second)
			if pnl, ok := observe.RealizedUSDC(endpoint, sig, owner); ok {
				dailyTip.Lock()
				*dailyTipSol += tipSol
				dailyTip.Unlock()
				pnlCopy := pnl
				observe.LogTrade(runDir, &tradeLog{
					T: nowSecs(), Protocol: "kamino", Signature: &sigCopy, RealizedUsdc: &pnlCopy,
				})
				observe.Alert(webhook, "kliq-landed", fmt.Sprintf("Kamino liq landed %s: realized $%.2f", sig, pnl))
				return
			}
		}
		observe.Alert(webhook, "kliq-miss", fmt.Sprintf("Kamino liq %s never confirmed", sig))
	}()
}

func lazerSnapshot(table *pyth.PriceTable) map[uint32]float64 {
	out := map[uint32]float64{}
	for _, f := range lazer.ArmFeedIDs() {
		if p, ok := pyth.Get(table, f); ok {
			out[f] = p.Price
		}
	}
	return out
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
	// DRY_RUN=0 is the ONLY way to go live; unset or any other value stays
	// dry-run.
	dryRun := true
	if v, ok := config.EnvOptional("DRY_RUN"); ok {
		dryRun = v != "0"
	}
	runDir := config.EnvOr("RUN_DIR", "runs")
	minDebt := config.EnvFloat("MIN_DEBT_USD", 100.0)
	ratioCap := config.EnvFloat("RATIO_CAP", 3.0)
	minProfit := config.EnvFloat("MIN_PROFIT_USD", 0.5)
	closeFactor := config.EnvFloat("CLOSE_FACTOR", 0.2)
	maxBorrowUsd := config.EnvFloat("MAX_BORROW_USD", 5000.0)
	rescan := time.Duration(config.EnvInt("RESCAN_SECS", 30)) * time.Second
	watchRatio := config.EnvFloat("WATCH_RATIO", 0.9)
	armRatio := config.EnvFloat("ARM_RATIO", 0.97)
	maxFire := config.EnvInt("MAX_FIRE_PER_CYCLE", 4)
	tickPollMs := config.EnvInt("TICK_POLL_MS", 1)
	poll := time.Duration(config.EnvInt("POLL_MS", 5000)) * time.Millisecond
	simCooldown := time.Duration(config.EnvInt("SIM_COOLDOWN_SECS", 60)) * time.Second
	handleCooldown := time.Duration(config.EnvInt("HANDLE_COOLDOWN_SECS", 20)) * time.Second
	hbEvery := config.EnvInt("HEARTBEAT_SECS", 30)
	tipFractionBps := config.EnvUint64("TIP_FRACTION_BPS", 3000)
	minTipSol := config.EnvFloat("MIN_TIP_SOL", 0.0002)
	maxDailyTipSol := config.EnvFloat("MAX_DAILY_TIP_SOL", 0.05)
	walletMinSol := config.EnvFloat("WALLET_MIN_SOL", 0.02)
	slippageBps := uint32(config.EnvInt("SLIPPAGE_BPS", 100))
	maxSwapAccounts := config.EnvInt("MAX_SWAP_ACCOUNTS", 20)
	senderURL := config.EnvOr("SENDER_URL", "http://ams-sender.helius-rpc.com/fast")
	tipAccount := solana.MustPubkeyFromBase58(config.EnvOr("SENDER_TIP_ACCOUNT", "2nyhqdwKcJZR2vcqCyrYsaPVdAnFoJjiksCXJ7hfEYgD"))
	webhook := config.EnvOr("ALERT_WEBHOOK", "")

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
	authority := solana.MustPubkeyFromBase58(config.EnvOr("AUTHORITY", defaultAuthority))
	if kp != nil {
		authority = kp.Public
	}

	c := cfg{
		authority: authority, tipAccount: tipAccount, tipFractionBps: tipFractionBps, minTipSol: minTipSol,
		minProfit: minProfit, closeFactor: closeFactor, maxBorrowUsd: maxBorrowUsd,
		slippageBps: slippageBps, maxSwapAccounts: maxSwapAccounts,
	}

	rpc := rpcclient.New(endpoint)

	// Lazer WebSocket: the event-driven trigger. Without a token the loop
	// still runs but only on the slow poll fallback -- warn loudly, since
	// that's the exact poll regression this rewrite exists to kill.
	lazerTable := pyth.NewTable()
	mintFeed := lazer.MintFeedMap()
	lazerOn := false
	if token, ok := config.EnvOptional("PYTH_LAZER_TOKEN"); ok {
		lazer.SpawnLazerThread(token, lazer.ArmFeedIDs(), lazerTable)
		fmt.Fprintln(os.Stderr, "[kexec] Pyth Lazer event-driven trigger ENABLED")
		lazerOn = true
	}
	if !lazerOn {
		fmt.Fprintln(os.Stderr, "[kexec] WARNING: no PYTH_LAZER_TOKEN -- falling back to slow poll (the regression). Set the token for ms detection.")
	}

	dryTag := "[DRY RUN]"
	if !dryRun {
		dryTag = "[LIVE]"
	}
	fmt.Fprintf(os.Stderr, "[kexec] Kamino liquidation executor %s  authority=%s  min_debt=$%v min_profit=$%v rescan=%v tick_poll=%dms lazer=%v\n",
		dryTag, authority, minDebt, minProfit, rescan, tickPollMs, lazerOn)
	if !dryRun {
		bal := solBalance(rpc, authority)
		fmt.Fprintf(os.Stderr, "[kexec] wallet balance: %v SOL\n", bal)
		if bal < walletMinSol {
			fatalf("wallet below floor %v", walletMinSol)
		}
	}

	engine := kaminoengine.NewEngine(minDebt, ratioCap)
	scan := fullScanKamino(rpc, minDebt, mintFeed)
	if scan == nil {
		fatalf("initial scan")
	}
	lastScan := time.Now()
	tpCache := map[solana.Pubkey]solana.Pubkey{}

	var dailyTipMu sync.Mutex
	dailyTipSol := 0.0
	tipDay := int64(nowSecs() / 86_400)
	var freshBh solana.Hash
	lastBh := time.Now().Add(-9999 * time.Second)
	handled := map[solana.Pubkey]time.Time{}
	// Quote/sim-rejected cooldown: once a candidate is quoted+sim'd and
	// rejected (healthy at the fresh price, unprofitable, or a Jupiter 429),
	// don't re-quote it for simCooldown -- stops re-hammering the same
	// phantoms every cycle.
	simRejected := map[solana.Pubkey]time.Time{}
	var lastTickUs int64
	lastHb := time.Now().Add(-9999 * time.Second)
	fireDeferred := 0
	// Debt mints seen in the watch-set with no wired flash market -- logged
	// once (a one-time summary), never per-cycle.
	loggedUnwired := map[solana.Pubkey]struct{}{}
	first := true

	for {
		// Rebuild the watch-set + engine from a full scan.
		if first || time.Since(lastScan) >= rescan {
			if !first {
				if s := fullScanKamino(rpc, minDebt, mintFeed); s != nil {
					scan = s
				}
			}
			lastScan = time.Now()
			snap := lazerSnapshot(lazerTable)
			armed := engine.Rebuild(scan.obls, scan.reserveFeed, watchRatio, snap)
			fmt.Fprintf(os.Stderr, "[kexec] scan: %d v1 obligations (>= $%v) -> engine watch-set %d (ratio >= %v)\n",
				len(scan.obls), minDebt, armed, watchRatio)
			// One-time summary of watch-set debts with no wired flash market
			// -- these can never fire, so they're excluded from fire
			// candidates (never a build attempt). Log the mint once, not
			// per-cycle.
			unwiredNow := 0
			for i := range engine.Accounts {
				_, debtReserve, ok := engine.ReservesOf(engine.Accounts[i].Obligation)
				if !ok {
					continue
				}
				mint, ok := scan.reserveMint[debtReserve]
				if !ok || flashloan.HasMarket(mint) {
					continue
				}
				unwiredNow++
				if _, seen := loggedUnwired[mint]; !seen {
					loggedUnwired[mint] = struct{}{}
					fmt.Fprintf(os.Stderr, "[kexec] unwired debt mint (no JupLend flash market) -- will skip: %s\n", mint)
				}
			}
			if unwiredNow > 0 {
				fmt.Fprintf(os.Stderr, "[kexec] %d/%d watch-set obligations have an unwired debt mint (excluded from fire candidates)\n",
					unwiredNow, len(engine.Accounts))
			}
			first = false
		}

		day := int64(nowSecs() / 86_400)
		if day != tipDay {
			tipDay = day
			dailyTipMu.Lock()
			dailyTipSol = 0.0
			dailyTipMu.Unlock()
		}
		if time.Since(lastBh) >= 2*time.Second {
			if bh, err := rpc.GetLatestBlockhash(); err == nil {
				freshBh = bh
				lastBh = time.Now()
			}
		}

		// Trigger cadence: wake on a Lazer tick (in-memory, no RPC) when
		// live, else the slow poll fallback. The tick only paces the loop --
		// it NARROWS which obligations are near threshold (the watch-set),
		// but does NOT decide who fires. The fire set is gated on the
		// ON-CHAIN Scope price below, because Lazer LEADS/diverges from
		// Scope and its projected "liquidatable" set is ~900 phantoms that
		// are healthy on-chain.
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
				if int64(cur) > lastTickUs {
					lastTickUs = int64(cur)
					break
				}
				if time.Now().After(deadline) {
					break
				}
				time.Sleep(time.Duration(tickPollMs) * time.Millisecond)
			}
			snap = lazerSnapshot(lazerTable)
		} else {
			time.Sleep(poll)
			snap = lazerSnapshot(lazerTable)
		}

		// Heartbeat: liveness + detect_lag (the tell this rewrite worked --
		// it must read milliseconds, not the old 5-30s poll interval).
		if lazerOn && hbEvery > 0 && time.Since(lastHb) >= time.Duration(hbEvery)*time.Second {
			totalFeeds := len(lazer.ArmFeedIDs())
			near := len(engine.Crossed(snap, armRatio))
			// TWO distinct counts: lazer-flagged (the projected set -- cheap
			// ARM tier, no Jupiter) vs on-chain liquidatable (the real FIRE
			// candidates at the Scope price). In a calm market on-chain M
			// should be single-digit/zero even while lazer-flagged L is
			// hundreds -- that gap IS the phantom set.
			lazerFlagged := len(engine.Crossed(snap, 1.0))
			onChain := engine.OnChainLiquidatableCount()
			var freshest uint64
			for _, f := range lazer.ArmFeedIDs() {
				if p, ok := pyth.Get(lazerTable, f); ok && p.TsUs > freshest {
					freshest = p.TsUs
				}
			}
			lagMs := (nowUs() - int64(freshest)) / 1000
			if lagMs < 0 {
				lagMs = 0
			}
			deferredMsg := ""
			if fireDeferred > 0 {
				deferredMsg = fmt.Sprintf(" | DEFERRED fire %d/cycle", fireDeferred)
			}
			fmt.Fprintf(os.Stderr, "[hb] lazer feeds %d/%d live | detect_lag %dms | watch %d | %d within arm(%v) | lazer-flagged %d | on-chain liquidatable %d | fire-cap %d%s | %s\n",
				len(snap), totalFeeds, lagMs, len(engine.Accounts), near, armRatio, lazerFlagged, onChain, maxFire, deferredMsg, lazer.Status(lazerTable))
			lastHb = time.Now()
		}

		// -- ARM tier (cheap, Lazer-driven): the near-threshold watch-set is
		// maintained by engine.Rebuild -- no Jupiter, no sim. It only
		// NARROWS the universe. Nothing to do here per tick; it's reported
		// in the heartbeat.

		// -- FIRE tier (expensive): ONLY obligations liquidatable at the
		// ON-CHAIN Scope price (health RECOMPUTED from fresh reserve prices
		// at the last rescan -- NOT the Lazer projection). Ranked by USD
		// deficit, capped to top-K/cycle so the biggest REAL opportunity
		// wins a bounded quote/sim budget. This is the ONLY place Jupiter is
		// called -- the whole 429-storm fix is that this set is ~0 in a calm
		// market instead of the ~900 the Lazer projection used to feed here.
		fireRanked := engine.OnChainLiquidatableRanked()
		isWired := func(pk solana.Pubkey) bool {
			_, debtReserve, ok := engine.ReservesOf(pk)
			if !ok {
				return false
			}
			mint, ok := scan.reserveMint[debtReserve]
			if !ok {
				return false
			}
			return flashloan.HasMarket(mint)
		}
		var fireCandidates []solana.Pubkey
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
		fireDeferred = len(fireCandidates) - maxFire
		if fireDeferred < 0 {
			fireDeferred = 0
		}
		if len(fireCandidates) > maxFire {
			fireCandidates = fireCandidates[:maxFire]
		}
		for _, pk := range fireCandidates {
			handled[pk] = time.Now()
			ratio := 1.0
			for i := range engine.Accounts {
				if engine.Accounts[i].Obligation == pk {
					ratio = engine.Accounts[i].OnChainRatio()
					break
				}
			}
			// Build + Jupiter quote (jup backoff, honors JUP_API_BASE) + sim
			// gate (the authoritative on-chain liquidatability/profit check).
			fireStart := nowUs()
			cached := tryArm(rpc, endpoint, runDir, &c, scan, pk, ratio, tpCache)
			if cached != nil {
				fireCached(rpc, endpoint, runDir, senderURL, &c, dryRun, pk, cached, freshBh, kp,
					&dailyTipMu, &dailyTipSol, maxDailyTipSol, walletMinSol, webhook)
				done := nowUs()
				// Only meaningful with a real Lazer tick (appearedUs = its publish ts).
				if lazerOn {
					detectLag := (fireStart - lastTickUs) / 1000
					submitLag := (done - lastTickUs) / 1000
					fireSubmit := (done - fireStart) / 1000
					logLatency(runDir, map[string]any{
						"t": nowSecs(), "obligation": pk.String(), "protocol": "kamino",
						"clean": cached.clean, "appeared_us": lastTickUs,
						"detected_lag_ms": detectLag, "submit_lag_ms": submitLag,
						"fire_submit_ms": fireSubmit, "armed": false, "dry_run": dryRun,
					})
				}
			} else {
				// Quote/sim rejected (healthy at fresh price, unprofitable,
				// or 429) -> cooldown so we don't re-hammer the same
				// candidate next cycle.
				simRejected[pk] = time.Now()
			}
		}
	}
}
