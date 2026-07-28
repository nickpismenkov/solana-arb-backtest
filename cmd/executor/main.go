// Command executor is the arb executor v2 (pragmatic fast reactor,
// blind-guarded fire). Hot path is memory reads + sign + submit ONLY — no
// RPC, no disk, no network calls in the reaction. Slow work happens on
// background goroutines:
//   - RPC poll (~1s default) -> PoolData cache (pool accounts for building)
//   - RPC poll (~3s) -> recent blockhash + SOL/USDC reference price
//   - config hot-reload (~3s) -> Config (pause / size / tip)
//   - log writer goroutine <- channel (decisions/trades JSONL, off hot path)
//   - realized-P&L readback (detached, later)
//
// On a shred trigger (not paused): build guarded arb from cached state +
// blockhash, sign, submit to Jito. The exact-out leg-2 guard is the real
// profitability check — unprofitable txs revert for free, tips only pay on
// wins. No price filtering; every trigger fires unless paused/dry_run.
// DRY_RUN=1 (default) logs and never submits.
//
// Env: RPC_ENDPOINT, ALT_ADDRESS, KEYPAIR_PATH, SHREDSTREAM_PORT, RUN_DIR,
//
//	DRY_RUN, CONFIG_PATH, JITO_BLOCK_ENGINE, WALLET_MIN_SOL,
//	MAX_DAILY_TIP_SOL, ALERT_WEBHOOK.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"arbengine/internal/arb"
	"arbengine/internal/clmm"
	"arbengine/internal/config"
	"arbengine/internal/decode"
	"arbengine/internal/jito"
	"arbengine/internal/observe"
	"arbengine/internal/pools"
	"arbengine/internal/rpcclient"
	"arbengine/internal/shredstream"
	"arbengine/internal/solana"
)

// execConfig is the hot-reloaded arb.config.json shape.
type execConfig struct {
	Paused                bool    `json:"paused"`
	BorrowUsdc            float64 `json:"borrow_usdc"`
	PriorityMicroLamports uint64  `json:"priority_micro_lamports"`
	// TipFractionBps is the tip as a fraction of computed profit (bps).
	// Jito's auction is won by paying a fraction of profit; capped at 80% so
	// we always net positive.
	TipFractionBps uint64 `json:"tip_fraction_bps"`
	// MinProfitLamports is the minimum computed profit (lamports) to fire.
	// Must clear tip + fees.
	MinProfitLamports uint64 `json:"min_profit_lamports"`
}

func defaultExecConfig() execConfig {
	return execConfig{
		Paused:                false,
		BorrowUsdc:            500.0,
		PriorityMicroLamports: 10_000,
		TipFractionBps:        3000, // 30% of computed profit
		MinProfitLamports:     500_000,
	}
}

// senderMinTip is Helius Sender's required minimum tip (0.0002 SOL).
const senderMinTip = 200_000.0

// solUsdcRef is the SOL/USDC Orca pool (SOL=mintA/dec9, USDC=mintB/dec6) —
// independent SOL price reference for USDC->SOL tip conversion, regardless
// of the traded pair.
const solUsdcRef = "Czfq3xZZDmsdGdUyrNLtRhGc47cXcZtLG4crryfu44zE"

type decisionLog struct {
	T      uint64 `json:"t"`
	Venue  string `json:"venue"`
	Slot   uint64 `json:"slot"`
	Fired  bool   `json:"fired"`
	Reason string `json:"reason"`
}

type tradeLog struct {
	T            uint64   `json:"t"`
	BorrowUsdc   float64  `json:"borrow_usdc"`
	TipLamports  uint64   `json:"tip_lamports"`
	BundleID     *string  `json:"bundle_id"`
	Signature    *string  `json:"signature"`
	BundleStatus *string  `json:"bundle_status"`
	RealizedUsdc *float64 `json:"realized_usdc"`
	Error        *string  `json:"error"`
}

type logMsg struct {
	decision *decisionLog
	trade    *tradeLog
}

func nowSecs() uint64 {
	return uint64(time.Now().Unix())
}

func accountData(endpoint, addr string) ([]byte, bool) {
	pk, err := solana.PubkeyFromBase58(addr)
	if err != nil {
		return nil, false
	}
	data, err := rpcclient.New(endpoint).GetAccountData(pk)
	if err != nil || data == nil {
		return nil, false
	}
	return data, true
}

func latestBlockhash(endpoint string) (solana.Hash, bool) {
	h, err := rpcclient.New(endpoint).GetLatestBlockhash()
	if err != nil {
		return solana.Hash{}, false
	}
	return h, true
}

func solBalance(endpoint, pkStr string) float64 {
	pk, err := solana.PubkeyFromBase58(pkStr)
	if err != nil {
		return 0
	}
	lamports, err := rpcclient.New(endpoint).GetBalance(pk)
	if err != nil {
		return 0
	}
	return float64(lamports) / 1e9
}

func loadExecConfig(path string) execConfig {
	b, err := os.ReadFile(path)
	if err != nil {
		return defaultExecConfig()
	}
	c := defaultExecConfig()
	if err := json.Unmarshal(b, &c); err != nil {
		return defaultExecConfig()
	}
	return c
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func main() {
	config.LoadDotenv()

	endpoint, ok := config.EnvOptional("RPC_ENDPOINT")
	if !ok {
		fatalf("RPC_ENDPOINT")
	}
	altAddr, ok := config.EnvOptional("ALT_ADDRESS")
	if !ok {
		fatalf("ALT_ADDRESS")
	}
	port := uint16(config.EnvInt("SHREDSTREAM_PORT", 20000))
	runDir := config.EnvOr("RUN_DIR", "runs")
	dryRun := config.EnvOr("DRY_RUN", "1") != "0"
	configPath := config.EnvOr("CONFIG_PATH", "arb.config.json")
	// Helius Sender: fast dual-route landing (validators + Jito), no 1/sec cap.
	senderURL := config.EnvOr("SENDER_URL", "http://ams-sender.helius-rpc.com/fast")
	paceMs := config.EnvInt("PACE_MS", 250)
	walletMinSol := config.EnvFloat("WALLET_MIN_SOL", 0.02)
	maxDailyTipSol := config.EnvFloat("MAX_DAILY_TIP_SOL", 0.05)
	webhook := config.EnvOr("ALERT_WEBHOOK", "")
	cfg := pools.Pair()

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
	signer := solana.MustPubkeyFromBase58("Anu6Awu4kxaEDrg1nkpcikx6tJ2xhfVci5TvDrZBsZEB")
	if kp != nil {
		signer = kp.Public
	}

	// Static, one-time: ALT + tip account.
	altData, ok := accountData(endpoint, altAddr)
	if !ok {
		fatalf("ALT")
	}
	alt := arb.LoadALT(altAddr, altData)
	// Helius Sender requires the tip go to one of ITS wallets (not a Jito tip
	// account). Overridable via SENDER_TIP_ACCOUNT.
	tipAccount := solana.MustPubkeyFromBase58("2nyhqdwKcJZR2vcqCyrYsaPVdAnFoJjiksCXJ7hfEYgD")
	if s, ok := config.EnvOptional("SENDER_TIP_ACCOUNT"); ok {
		if pk, err := solana.PubkeyFromBase58(s); err == nil {
			tipAccount = pk
		}
	}

	// Shared caches.
	var mu sync.RWMutex
	var poolData *arb.PoolData
	var blockhash solana.Hash
	var execCfg = loadExecConfig(configPath)
	// SOL/USDC reference price (USDC per SOL) for converting USDC profit ->
	// SOL tip. When the traded base ISN'T SOL (e.g. SPYx), the trading
	// pool's price is the wrong denominator; we always convert via this
	// independent SOL feed.
	var solUsd float64

	// Seed pool data + blockhash + SOL price before starting.
	if o, ok1 := accountData(endpoint, cfg.OrcaPool); ok1 {
		if r, ok2 := accountData(endpoint, cfg.RayPool); ok2 {
			poolData = &arb.PoolData{Orca: o, Ray: r}
		}
	}
	if bh, ok := latestBlockhash(endpoint); ok {
		blockhash = bh
	}
	if d, ok := accountData(endpoint, solUsdcRef); ok {
		if s, ok2 := clmm.FromOrca(d, 9, 6, 4.0); ok2 {
			solUsd = s.UIPrice()
		}
	}

	dryTag := "[DRY RUN]"
	if !dryRun {
		dryTag = "[LIVE]"
	}
	fmt.Fprintf(os.Stderr, "executor v2 %s pair=%s alt=%s wallet=%s dry_run=%v — hot path: blind-guarded fire\n",
		dryTag, cfg.Label, altAddr[:min(8, len(altAddr))], signer.String(), dryRun)
	if !dryRun {
		bal := solBalance(endpoint, signer.String())
		fmt.Fprintf(os.Stderr, "wallet balance: %v SOL\n", bal)
		if bal < walletMinSol {
			fatalf("wallet below floor %v", walletMinSol)
		}
	}

	// ── background: pool data + blockhash refresh ──
	// Blockhash refreshes frequently because Jito rejects expired
	// blockhashes. Falls back to a secondary RPC if the primary fails (the
	// shredstream feed's ALT fetches share the primary and can rate-limit
	// it).
	go func() {
		fallback := config.EnvOr("RPC_FALLBACK", "https://api.mainnet-beta.solana.com")
		orcaPool, rayPool := cfg.OrcaPool, cfg.RayPool
		// Pool state drives the profit prediction, so keep it as fresh as
		// the RPC allows (POOL_POLL_MS, default 1s). A stale snapshot
		// quantises the predicted profit to the refresh cycle — visible as
		// identical profits across distinct victims. Blockhash only needs
		// ~every few seconds.
		pollMs := config.EnvInt("POOL_POLL_MS", 1000)
		var tick uint64
		var bhFails uint64
		everyN := uint64(3000 / pollMs)
		if everyN == 0 {
			everyN = 1
		}
		for {
			time.Sleep(time.Duration(pollMs) * time.Millisecond)
			// Pool state EVERY tick (freshness matters most).
			o, ok1 := accountData(endpoint, orcaPool)
			if !ok1 {
				o, ok1 = accountData(fallback, orcaPool)
			}
			r, ok2 := accountData(endpoint, rayPool)
			if !ok2 {
				r, ok2 = accountData(fallback, rayPool)
			}
			if ok1 && ok2 {
				mu.Lock()
				poolData = &arb.PoolData{Orca: o, Ray: r}
				mu.Unlock()
			} else {
				fmt.Fprintf(os.Stderr, "[warn] pool data refresh failed on both endpoints\n")
			}
			// Blockhash + SOL/USDC reference price roughly every 3s.
			tick++
			if tick%everyN == 0 {
				bh, ok := latestBlockhash(endpoint)
				if !ok {
					bh, ok = latestBlockhash(fallback)
				}
				if ok {
					bhFails = 0
					mu.Lock()
					blockhash = bh
					mu.Unlock()
				} else {
					bhFails++
					fmt.Fprintf(os.Stderr, "[warn] blockhash refresh failed on BOTH endpoints (%d in a row)\n", bhFails)
				}
				d, ok := accountData(endpoint, solUsdcRef)
				if !ok {
					d, ok = accountData(fallback, solUsdcRef)
				}
				if ok {
					if s, ok2 := clmm.FromOrca(d, 9, 6, 4.0); ok2 {
						mu.Lock()
						solUsd = s.UIPrice()
						mu.Unlock()
					}
				}
			}
		}
	}()

	// ── background: config hot-reload (3s) ──
	go func() {
		for {
			time.Sleep(3 * time.Second)
			c := loadExecConfig(configPath)
			mu.Lock()
			execCfg = c
			mu.Unlock()
		}
	}()

	// ── background: log writer (channel -> JSONL), OFF hot path ──
	logCh := make(chan logMsg, 256)
	go func() {
		for msg := range logCh {
			if msg.decision != nil {
				observe.LogDecision(runDir, msg.decision)
			}
			if msg.trade != nil {
				observe.LogTrade(runDir, msg.trade)
			}
		}
	}()

	// ── shred trigger feed ──
	trigCh := make(chan shredstream.Trigger, 256)
	shredstream.RunShredstreamFeed(port, endpoint, trigCh)

	var dailyTipMu sync.Mutex
	dailyTipSol := 0.0
	var triggers, fired uint64

	base := clmm.WSOL()
	seenSigs := make(map[string]struct{})
	// Jito's unauthenticated lane hard-limits to 1 bundle/sec — firing
	// faster just 429s. Pace to ~1/sec (an auth key would lift this).
	lastSubmit := time.Now().Add(-10 * time.Second)

	// ═══ HOT PATH ═══ decode victim -> predict exact profit -> gate -> co-bundle.
	// All arithmetic on cached state; the only network call is the Jito submit.
	for trigger := range trigCh {
		triggers++
		mu.RLock()
		c := execCfg
		mu.RUnlock()
		if c.Paused {
			continue
		}

		// Only co-bundle DECODABLE direct victims. Routed/CPI swaps decode
		// to empty (we can't predict their pool effect) -> skip silently
		// (logging every such trigger would flood the ledger; they're the
		// majority).
		var victim *decode.SwapInfo
		for i := range trigger.Swaps {
			if trigger.Swaps[i].AmountIsInput && trigger.Swaps[i].Amount > 0 {
				victim = &trigger.Swaps[i]
				break
			}
		}
		if victim == nil {
			continue
		}

		mu.RLock()
		bh := blockhash
		pd := poolData
		solPrice := solUsd
		mu.RUnlock()
		if pd == nil {
			continue
		}
		// Decode both pools. Orca decimals from mintA (offset 101); Ray self-describes.
		orcaMintA, errA := solana.PubkeyFromBytes(pd.Orca[101:133])
		oda, odb := cfg.QuoteDec, cfg.BaseDec
		if errA == nil && orcaMintA == base {
			oda, odb = cfg.BaseDec, cfg.QuoteDec
		}
		orca0, ok1 := clmm.FromOrca(pd.Orca, oda, odb, cfg.OrcaFeeBps)
		ray0, ok2 := clmm.FromRay(pd.Ray, cfg.RayFeeBps)
		if !ok1 || !ok2 {
			continue
		}

		// Apply the victim's swap to the pool it hits -> predicted post-victim state.
		sellBase := victim.Dir == decode.SellBase
		amt := float64(victim.Amount)
		var orcaP, rayP clmm.State
		if trigger.Venue == "Orca" {
			orcaP = orca0.AfterBaseSwap(base, sellBase, amt)
			rayP = ray0
		} else {
			orcaP = orca0
			rayP = ray0.AfterBaseSwap(base, sellBase, amt)
		}
		// Exact optimal arb over the predicted state (borrow capped by config).
		sizeRaw, profitRaw, buyOrca := clmm.OptimalArb(orcaP, rayP, base, c.BorrowUsdc*1e6)
		// Convert USDC profit -> SOL lamports via the independent SOL/USDC
		// price (NOT the trading pool's price — wrong denominator when base
		// != SOL).
		profitLamports := 0.0
		if solPrice > 0.0 {
			profitLamports = profitRaw / 1e6 / solPrice * 1e9
		}

		// GATE: fire only genuinely profitable arbs (clears tip + fees).
		fire := profitLamports > float64(c.MinProfitLamports) && sizeRaw > 1_000_000.0
		reason := "below_threshold"
		if fire {
			reason = "profitable"
		}
		logCh <- logMsg{decision: &decisionLog{
			T: nowSecs(), Venue: trigger.Venue, Slot: trigger.Slot,
			Fired: fire && !dryRun, Reason: reason,
		}}
		if !fire {
			continue
		}

		dir := "ray→orca"
		if buyOrca {
			dir = "orca→ray"
		}
		// Tip <= 80% of profit (leaves margin). The repay_buffer forces
		// leg2 to yield borrow + tip + fees in USDC, so a landed trade is
		// net-positive even if the prediction is optimistic; too-small
		// gaps revert for free.
		tip := clampF(profitLamports*(float64(c.TipFractionBps)/1e4), senderMinTip, profitLamports*0.8)
		tipU64 := uint64(tip)
		const feeLamports = 20_000.0 // tx + priority + cushion
		var repayBuffer uint64
		if solPrice > 0.0 {
			repayBuffer = uint64((tip + feeLamports) / 1e9 * solPrice * 1e6 * 1.05)
		}
		borrowAmount := uint64(sizeRaw)

		// Dedup: the same victim tx can arrive multiple times (retransmits);
		// fire it at most once (a duplicate bundle would fail anyway).
		if _, dup := seenSigs[trigger.Sig]; dup {
			continue
		}
		seenSigs[trigger.Sig] = struct{}{}
		if len(seenSigs) > 5000 {
			seenSigs = make(map[string]struct{})
		}

		if dryRun {
			fmt.Fprintf(os.Stderr, "[dry] would co-bundle %s borrow=%.1fUSDC profit=%.6fSOL tip=%d buffer=%.3fUSDC (victim %s %s %.1f)\n",
				dir, float64(borrowAmount)/1e6, profitLamports/1e9, tipU64, float64(repayBuffer)/1e6,
				trigger.Venue, sellBaseLabel(sellBase), amt)
			continue
		}

		// Daily tip cap: CHECK only (don't pre-charge). Tips are paid only
		// on a landed bundle, so we count actual spend after acceptance,
		// not per attempt — otherwise non-landing fires falsely exhaust the
		// cap.
		dailyTipMu.Lock()
		overCap := dailyTipSol+tip/1e9 > maxDailyTipSol
		dailyTipMu.Unlock()
		if overCap {
			observe.Alert(webhook, "daily_cap", "daily tip cap reached")
			continue
		}
		// Pace submissions (PACE_MS; Sender lifts the Jito 1/sec cap so
		// this can be small). Skip if we submitted too recently.
		if time.Since(lastSubmit) < time.Duration(paceMs)*time.Millisecond {
			continue
		}
		if kp == nil {
			continue
		}
		tx, err := arb.BuildArbTx(*pd, signer, alt, borrowAmount, buyOrca, &tipAccount, tipU64, c.PriorityMicroLamports, bh, repayBuffer)
		if err != nil {
			continue
		}

		if err := tx.Sign([]solana.Keypair{*kp}); err != nil {
			continue
		}
		sig := tx.Signatures[0].String()
		arbB64, err := tx.Base64()
		if err != nil {
			continue
		}

		fired++
		fmt.Fprintf(os.Stderr, "[debug] BACKRUN %s borrow=%.1fUSDC profit=%.6fSOL tip=%d slot=%d sig=%s\n",
			dir, float64(borrowAmount)/1e6, profitLamports/1e9, tipU64, trigger.Slot, sig[:min(16, len(sig))])
		// Submit our arb ALONE (not [victim, arb]): the victim is already
		// propagating to land via its own path (shred = already
		// broadcasting), so bundling it -> "already processed". The
		// victim's landing creates the gap on-chain; our guarded arb
		// bundle races to capture it. Guard reverts free if the gap is
		// already gone.
		lastSubmit = time.Now()
		returnedSig, err := jito.SendSender(senderURL, arbB64)
		if err != nil {
			errStr := err.Error()
			fmt.Fprintf(os.Stderr, "[debug] submit error (%s): %s\n", dir, errStr[:min(400, len(errStr))])
			logCh <- logMsg{trade: &tradeLog{
				T: nowSecs(), BorrowUsdc: float64(borrowAmount) / 1e6, TipLamports: tipU64,
				Error: &errStr,
			}}
		} else {
			sigCopy := sig
			logCh <- logMsg{trade: &tradeLog{
				T: nowSecs(), BorrowUsdc: float64(borrowAmount) / 1e6, TipLamports: tipU64,
				Signature: &sigCopy,
			}}
			fmt.Fprintf(os.Stderr, "⚡ backrun %s sent %s\n", dir, returnedSig[:min(16, len(returnedSig))])
			owner := signer.String()
			borrowUI := float64(borrowAmount) / 1e6
			go func(s string, tipU64 uint64, borrowUI float64) {
				// Landing truth = the tx on-chain (getTransaction via
				// RealizedUSDC); Sender returns a signature, not a Jito
				// bundle id, so poll the chain.
				var pnl *float64
				for _, delay := range []int{4, 8, 20} {
					time.Sleep(time.Duration(delay) * time.Second)
					if v, ok := observe.RealizedUSDC(endpoint, s, owner); ok {
						pnl = &v
						break
					}
				}
				// Count the tip against the daily cap ONLY on a confirmed
				// landing (accepted-but-dropped pays no tip).
				if pnl != nil {
					dailyTipMu.Lock()
					dailyTipSol += float64(tipU64) / 1e9
					dailyTipMu.Unlock()
				}
				fmt.Fprintf(os.Stderr, "[readback] %s… landed=%v realized_usdc=%v\n", s[:min(8, len(s))], pnl != nil, pnlStr(pnl))
				logCh <- logMsg{trade: &tradeLog{
					T: nowSecs(), BorrowUsdc: borrowUI, TipLamports: tipU64,
					Signature: &s, RealizedUsdc: pnl,
				}}
			}(sig, tipU64, borrowUI)
		}

		if triggers%100 == 0 {
			fmt.Fprintf(os.Stderr, "[executor] triggers=%d fired=%d\n", triggers, fired)
		}
	}
}

func sellBaseLabel(sellBase bool) string {
	if sellBase {
		return "sellBase"
	}
	return "buyBase"
}

func pnlStr(pnl *float64) string {
	if pnl == nil {
		return "None"
	}
	return fmt.Sprintf("%v", *pnl)
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
