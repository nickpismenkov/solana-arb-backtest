// Arb executor v2 (pragmatic fast reactor, blind-guarded fire). Hot path is
// memory reads + sign + submit ONLY — no RPC, no disk, no network calls in
// the reaction. Slow work on background goroutines:
//   - RPC poll (~1s pool / ~3s blockhash+SOL price) → PoolData cache
//   - config hot-reload (~3s) → RWMutex-guarded Config (pause / size / tip)
//   - log writer goroutine ← channel (decisions/trades JSONL, off hot path)
//   - realized-P&L readback (detached, later)
//
// On a shred trigger (not paused): build guarded arb from cached state +
// blockhash, sign, submit via Helius Sender. The exact-out leg-2 guard is the
// real profitability check — unprofitable txs revert for free, tips only pay
// on wins. No price filtering; every trigger fires unless paused/dry_run.
// DRY_RUN=1 (default) logs and never submits.
//
// Env: RPC_ENDPOINT, ALT_ADDRESS, KEYPAIR_PATH, SHREDSTREAM_PORT, RUN_DIR,
//
//	DRY_RUN, CONFIG_PATH, SENDER_URL, SENDER_TIP_ACCOUNT, PACE_MS,
//	WALLET_MIN_SOL, MAX_DAILY_TIP_SOL, ALERT_WEBHOOK, RPC_FALLBACK,
//	POOL_POLL_MS.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/arb"
	"solana-arb-backtest-go/internal/clmm"
	"solana-arb-backtest-go/internal/decode"
	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/jito"
	"solana-arb-backtest-go/internal/observe"
	"solana-arb-backtest-go/internal/pools"
	"solana-arb-backtest-go/internal/shredstream"
)

// ── config (hot-reloadable) ──

type Config struct {
	Paused                bool    `json:"paused"`
	BorrowUSDC            float64 `json:"borrow_usdc"`
	PriorityMicroLamports uint64  `json:"priority_micro_lamports"`
	// Tip as a fraction of computed profit (bps). Jito/Sender auctions are won
	// by paying a fraction of profit; capped at 80% so we always net positive.
	TipFractionBps uint64 `json:"tip_fraction_bps"`
	// Minimum computed profit (lamports) to fire. Must clear tip + fees.
	MinProfitLamports uint64 `json:"min_profit_lamports"`
}

func defaultConfig() Config {
	return Config{
		Paused:                false,
		BorrowUSDC:            500.0,
		PriorityMicroLamports: 10_000,
		TipFractionBps:        3000,    // 30% of computed profit
		MinProfitLamports:     500_000, // 0.0005 SOL; must clear Sender's 0.0002 tip floor + fees + buffer
	}
}

// SenderMinTip is Helius Sender's required minimum tip (0.0002 SOL).
const senderMinTip = 200_000.0

// solUsdcRef is the SOL/USDC Orca pool (SOL=mintA/dec9, USDC=mintB/dec6) — an
// independent SOL price reference for USDC→SOL tip conversion, regardless of
// the traded pair.
const solUsdcRef = "Czfq3xZZDmsdGdUyrNLtRhGc47cXcZtLG4crryfu44zE"

func loadConfig(path string) Config {
	b, err := os.ReadFile(path)
	if err != nil {
		return defaultConfig()
	}
	cfg := defaultConfig()
	if err := json.Unmarshal(b, &cfg); err != nil {
		return defaultConfig()
	}
	return cfg
}

// ── log records ──

type decisionLog struct {
	T      uint64 `json:"t"`
	Venue  string `json:"venue"`
	Slot   uint64 `json:"slot"`
	Fired  bool   `json:"fired"`
	Reason string `json:"reason"`
}

type tradeLog struct {
	T            uint64   `json:"t"`
	BorrowUSDC   float64  `json:"borrow_usdc"`
	TipLamports  uint64   `json:"tip_lamports"`
	BundleID     *string  `json:"bundle_id"`
	Signature    *string  `json:"signature"`
	BundleStatus *string  `json:"bundle_status"`
	RealizedUSDC *float64 `json:"realized_usdc"`
	Error        *string  `json:"error"`
}

type logMsg struct {
	decision *decisionLog
	trade    *tradeLog
}

func now() uint64 { return uint64(time.Now().Unix()) }

func strp(s string) *string   { return &s }
func f64p(f float64) *float64 { return &f }

// ── minimal RPC client (JSON-RPC over HTTP, with retry) ──

var httpClient = &http.Client{Timeout: 15 * time.Second}

func rpc(endpoint string, body map[string]any) (map[string]any, bool) {
	for attempt := 0; attempt < 3; attempt++ {
		b, _ := json.Marshal(body)
		resp, err := httpClient.Post(endpoint, "application/json", bytes.NewReader(b))
		if err == nil {
			var v map[string]any
			decErr := json.NewDecoder(resp.Body).Decode(&v)
			resp.Body.Close()
			if decErr == nil {
				return v, true
			}
		}
		time.Sleep(time.Duration(300<<attempt) * time.Millisecond)
	}
	return nil, false
}

func accountData(endpoint, addr string) ([]byte, bool) {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{addr, map[string]any{"encoding": "base64"}}})
	if !ok {
		return nil, false
	}
	result, _ := v["result"].(map[string]any)
	value, _ := result["value"].(map[string]any)
	if value == nil {
		return nil, false
	}
	dataArr, _ := value["data"].([]any)
	if len(dataArr) == 0 {
		return nil, false
	}
	s, ok := dataArr[0].(string)
	if !ok {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	return raw, true
}

func latestBlockhash(endpoint string) (solana.Hash, bool) {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getLatestBlockhash",
		"params": []any{map[string]any{"commitment": "confirmed"}}})
	if !ok {
		return solana.Hash{}, false
	}
	result, _ := v["result"].(map[string]any)
	value, _ := result["value"].(map[string]any)
	if value == nil {
		return solana.Hash{}, false
	}
	s, _ := value["blockhash"].(string)
	h, err := solana.HashFromBase58(s)
	if err != nil {
		return solana.Hash{}, false
	}
	return h, true
}

func solBalance(endpoint, pk string) float64 {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getBalance", "params": []any{pk}})
	if !ok {
		return 0
	}
	result, _ := v["result"].(map[string]any)
	value, ok := result["value"].(float64)
	if !ok {
		return 0
	}
	return value / 1e9
}

// ── env helpers ──

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envU64Or(key string, def uint64) uint64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envF64Or(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envBoolDryRun() bool {
	// DRY_RUN defaults to true; only "0" disables it (mirrors the Rust: `v != "0"`).
	if v, ok := os.LookupEnv("DRY_RUN"); ok {
		return v != "0"
	}
	return true
}

func main() {
	envfile.LoadDotEnv()

	endpoint := os.Getenv("RPC_ENDPOINT")
	if endpoint == "" {
		fmt.Fprintln(os.Stderr, "RPC_ENDPOINT must be set")
		os.Exit(1)
	}
	altAddr := os.Getenv("ALT_ADDRESS")
	if altAddr == "" {
		fmt.Fprintln(os.Stderr, "ALT_ADDRESS must be set")
		os.Exit(1)
	}
	port := uint16(envU64Or("SHREDSTREAM_PORT", 20000))
	runDir := envOr("RUN_DIR", "runs")
	dryRun := envBoolDryRun()
	configPath := envOr("CONFIG_PATH", "arb.config.json")
	// Helius Sender: fast dual-route landing (validators + Jito), no 1/sec cap.
	senderURL := envOr("SENDER_URL", "http://ams-sender.helius-rpc.com/fast")
	paceMs := envU64Or("PACE_MS", 250)
	walletMinSol := envF64Or("WALLET_MIN_SOL", 0.02)
	maxDailyTipSol := envF64Or("MAX_DAILY_TIP_SOL", 0.05)
	var webhook *string
	if v, ok := os.LookupEnv("ALERT_WEBHOOK"); ok {
		webhook = &v
	}
	cfg := pools.Pair()

	var kp *solana.PrivateKey
	if p, ok := os.LookupEnv("KEYPAIR_PATH"); ok {
		k, err := solana.PrivateKeyFromSolanaKeygenFile(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read keypair: %v\n", err)
			os.Exit(1)
		}
		kp = &k
	}
	if kp == nil && !dryRun {
		fmt.Fprintln(os.Stderr, "LIVE needs KEYPAIR_PATH")
		os.Exit(1)
	}
	var signer solana.PublicKey
	if kp != nil {
		signer = kp.PublicKey()
	} else {
		signer = solana.MustPublicKeyFromBase58("Anu6Awu4kxaEDrg1nkpcikx6tJ2xhfVci5TvDrZBsZEB")
	}

	// Static, one-time: ALT + tip account.
	altData, ok := accountData(endpoint, altAddr)
	if !ok {
		fmt.Fprintln(os.Stderr, "ALT: failed to fetch account data")
		os.Exit(1)
	}
	alt := arb.LoadAlt(altAddr, altData)
	// Helius Sender requires the tip go to one of ITS wallets (not a Jito tip
	// account). Overridable via SENDER_TIP_ACCOUNT.
	tipAccount := solana.MustPublicKeyFromBase58("2nyhqdwKcJZR2vcqCyrYsaPVdAnFoJjiksCXJ7hfEYgD")
	if v, ok := os.LookupEnv("SENDER_TIP_ACCOUNT"); ok {
		if pk, err := solana.PublicKeyFromBase58(v); err == nil {
			tipAccount = pk
		}
	}

	// Shared caches.
	var (
		poolMu sync.RWMutex
		poolD  *arb.PoolData

		bhMu sync.RWMutex
		bh   solana.Hash

		cfgMu sync.RWMutex
		conf  = loadConfig(configPath)

		// SOL/USDC reference price (USDC per SOL) for converting USDC profit →
		// SOL tip. When the traded base ISN'T SOL, the trading pool's price is
		// the wrong denominator; we always convert via this independent feed.
		solMu  sync.RWMutex
		solUsd float64
	)

	// Seed pool data + blockhash + SOL price before starting.
	if o, ok1 := accountData(endpoint, cfg.OrcaPool); ok1 {
		if r, ok2 := accountData(endpoint, cfg.RayPool); ok2 {
			poolD = &arb.PoolData{Orca: o, Ray: r}
		}
	}
	if h, ok := latestBlockhash(endpoint); ok {
		bh = h
	}
	if d, ok := accountData(endpoint, solUsdcRef); ok {
		if s, ok := clmm.FromOrca(d, 9, 6, 4.0); ok {
			solUsd = s.UIPrice()
		}
	}

	fmt.Fprintf(os.Stderr, "executor v2 %s pair=%s alt=%s wallet=%s dry_run=%v — hot path: blind-guarded fire\n",
		map[bool]string{true: "[DRY RUN]", false: "[LIVE]"}[dryRun], cfg.Label, altAddr[:min(8, len(altAddr))], signer, dryRun)
	if !dryRun {
		bal := solBalance(endpoint, signer.String())
		fmt.Fprintf(os.Stderr, "wallet balance: %g SOL\n", bal)
		if bal < walletMinSol {
			fmt.Fprintf(os.Stderr, "wallet below floor %g\n", walletMinSol)
			os.Exit(1)
		}
	}

	// ── background: pool data (POOL_POLL_MS, default 1s) + blockhash/SOL-price (~3s) refresh ──
	// Blockhash refreshes frequently because a stale blockhash gets rejected.
	// Falls back to a secondary RPC if the primary fails (the shredstream
	// feed's ALT fetches share the primary and can rate-limit it).
	go func() {
		fb := envOr("RPC_FALLBACK", "https://api.mainnet-beta.solana.com")
		pollMs := envU64Or("POOL_POLL_MS", 1000)
		if pollMs == 0 {
			pollMs = 1000
		}
		tickPeriod := (3000 / pollMs)
		if tickPeriod == 0 {
			tickPeriod = 1
		}
		var tick uint64
		var bhFails uint64
		for {
			time.Sleep(time.Duration(pollMs) * time.Millisecond)
			// Pool state EVERY tick (freshness matters most).
			o, okO := accountData(endpoint, cfg.OrcaPool)
			if !okO {
				o, okO = accountData(fb, cfg.OrcaPool)
			}
			r, okR := accountData(endpoint, cfg.RayPool)
			if !okR {
				r, okR = accountData(fb, cfg.RayPool)
			}
			if okO && okR {
				poolMu.Lock()
				poolD = &arb.PoolData{Orca: o, Ray: r}
				poolMu.Unlock()
			} else {
				fmt.Fprintln(os.Stderr, "[warn] pool data refresh failed on both endpoints")
			}
			// Blockhash + SOL/USDC reference price roughly every 3s.
			tick++
			if tick%tickPeriod == 0 {
				h, okH := latestBlockhash(endpoint)
				if !okH {
					h, okH = latestBlockhash(fb)
				}
				if okH {
					bhFails = 0
					bhMu.Lock()
					bh = h
					bhMu.Unlock()
				} else {
					bhFails++
					fmt.Fprintf(os.Stderr, "[warn] blockhash refresh failed on BOTH endpoints (%d in a row)\n", bhFails)
				}
				d, okD := accountData(endpoint, solUsdcRef)
				if !okD {
					d, okD = accountData(fb, solUsdcRef)
				}
				if okD {
					if s, ok := clmm.FromOrca(d, 9, 6, 4.0); ok {
						solMu.Lock()
						solUsd = s.UIPrice()
						solMu.Unlock()
					}
				}
			}
		}
	}()

	// ── background: config hot-reload (3s) ──
	go func() {
		for {
			time.Sleep(3 * time.Second)
			c := loadConfig(configPath)
			cfgMu.Lock()
			conf = c
			cfgMu.Unlock()
		}
	}()

	// ── background: log writer (channel → JSONL), OFF hot path ──
	logCh := make(chan logMsg, 1024)
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
	go func() {
		if err := shredstream.RunFeed(port, endpoint, nil, trigCh); err != nil {
			fmt.Fprintf(os.Stderr, "shredstream feed error: %v\n", err)
		}
	}()

	var dailyTipMu sync.RWMutex
	var dailyTipSol float64
	var triggers, fired uint64

	base := clmm.WSOL()
	seenSigs := make(map[string]struct{}, 5000)
	// Jito's unauthenticated lane hard-limits to 1 bundle/sec — firing faster
	// just 429s. Pace to ~1/sec (an auth key would lift this); Sender lifts the
	// Jito cap so PACE_MS can be small.
	lastSubmit := time.Now().Add(-10 * time.Second)

	// ═══ HOT PATH ═══ decode victim → predict exact profit → gate → co-bundle.
	// All arithmetic on cached state; the only network call is the Sender submit.
	for trigger := range trigCh {
		triggers++
		cfgMu.RLock()
		c := conf
		cfgMu.RUnlock()
		if c.Paused {
			continue
		}

		// Only co-bundle DECODABLE direct victims. Routed/CPI swaps decode to
		// empty (we can't predict their pool effect) → skip silently (logging
		// every such trigger would flood the ledger; they're the majority).
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

		bhMu.RLock()
		curBh := bh
		bhMu.RUnlock()

		poolMu.RLock()
		pd := poolD
		poolMu.RUnlock()
		if pd == nil {
			continue
		}

		// Decode both pools. Orca decimals from mintA (offset 101); Ray self-describes.
		var oda, odb int32
		if len(pd.Orca) >= 133 {
			orcaMintA := solana.PublicKeyFromBytes(pd.Orca[101:133])
			if orcaMintA.Equals(base) {
				oda, odb = cfg.BaseDec, cfg.QuoteDec
			} else {
				oda, odb = cfg.QuoteDec, cfg.BaseDec
			}
		} else {
			continue
		}
		orca0, ok1 := clmm.FromOrca(pd.Orca, oda, odb, cfg.OrcaFeeBps)
		ray0, ok2 := clmm.FromRay(pd.Ray, cfg.RayFeeBps)
		if !ok1 || !ok2 {
			continue
		}

		// Apply the victim's swap to the pool it hits → predicted post-victim state.
		sellBase := victim.Dir == decode.SellBase
		amt := float64(victim.Amount)
		var orcaP, rayP *clmm.State
		if victim.Venue == "Orca" {
			orcaP = orca0.AfterBaseSwap(base, sellBase, amt)
			rayP = ray0
		} else {
			orcaP = orca0
			rayP = ray0.AfterBaseSwap(base, sellBase, amt)
		}

		// Exact optimal arb over the predicted state (borrow capped by config).
		sizeRaw, profitRaw, buyOrca := clmm.OptimalArb(orcaP, rayP, base, c.BorrowUSDC*1e6)
		// Convert USDC profit → SOL lamports via the independent SOL/USDC price
		// (NOT the trading pool's price — wrong denominator when base ≠ SOL).
		solMu.RLock()
		solPrice := solUsd
		solMu.RUnlock()
		var profitLamports float64
		if solPrice > 0 {
			profitLamports = profitRaw / 1e6 / solPrice * 1e9
		}

		// GATE: fire only genuinely profitable arbs (clears tip + fees).
		fire := profitLamports > float64(c.MinProfitLamports) && sizeRaw > 1_000_000.0
		reason := "below_threshold"
		if fire {
			reason = "profitable"
		}
		logCh <- logMsg{decision: &decisionLog{
			T: now(), Venue: trigger.Venue, Slot: trigger.Slot,
			Fired: fire && !dryRun, Reason: reason,
		}}
		if !fire {
			continue
		}

		dir := "ray→orca"
		if buyOrca {
			dir = "orca→ray"
		}
		// Tip ≤ 80% of profit (leaves margin). The repay buffer forces leg2 to
		// yield borrow + tip + fees in USDC, so a landed trade is net-positive
		// even if the prediction is optimistic; too-small gaps revert for free.
		tip := clampF(profitLamports*(float64(c.TipFractionBps)/1e4), senderMinTip, profitLamports*0.8)
		tipLamports := uint64(tip)
		const feeLamports = 20_000.0 // tx + priority + cushion
		var repayBuffer uint64
		if solPrice > 0 {
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
			seenSigs = make(map[string]struct{}, 5000)
		}

		if dryRun {
			fmt.Fprintf(os.Stderr, "[dry] would co-bundle %s borrow=%.1fUSDC profit=%.6fSOL tip=%d buffer=%.3fUSDC (victim %s %s %.1f)\n",
				dir, float64(borrowAmount)/1e6, profitLamports/1e9, tipLamports, float64(repayBuffer)/1e6,
				victim.Venue, map[bool]string{true: "sellBase", false: "buyBase"}[sellBase], amt)
			continue
		}

		// Daily tip cap: CHECK only (don't pre-charge). Tips are paid only on a
		// landed bundle, so we count actual spend after acceptance, not per
		// attempt — otherwise non-landing fires falsely exhaust the cap.
		dailyTipMu.RLock()
		curDailyTip := dailyTipSol
		dailyTipMu.RUnlock()
		if curDailyTip+float64(tipLamports)/1e9 > maxDailyTipSol {
			observe.Alert(webhook, "daily_cap", "daily tip cap reached")
			continue
		}
		// Pace submissions (PACE_MS; Sender lifts the Jito 1/sec cap so this can
		// be small). Skip if we submitted too recently.
		if time.Since(lastSubmit) < time.Duration(paceMs)*time.Millisecond {
			continue
		}
		if kp == nil {
			continue
		}
		tx, err := arb.BuildArbTx(pd, signer, alt, borrowAmount, buyOrca, &tipAccount, tipLamports, c.PriorityMicroLamports, curBh, repayBuffer)
		if err != nil {
			continue
		}

		if _, err := tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
			if key.Equals(signer) {
				return kp
			}
			return nil
		}); err != nil {
			continue
		}
		sig := tx.Signatures[0].String()
		raw, err := tx.MarshalBinary()
		if err != nil {
			continue
		}
		arbB64 := base64.StdEncoding.EncodeToString(raw)

		fired++
		fmt.Fprintf(os.Stderr, "[debug] BACKRUN %s borrow=%.1fUSDC profit=%.6fSOL tip=%d slot=%d sig=%s\n",
			dir, float64(borrowAmount)/1e6, profitLamports/1e9, tipLamports, trigger.Slot, sig[:min(16, len(sig))])
		// Submit our arb ALONE (not [victim, arb]): the victim is already
		// propagating to land via its own path (shred = already broadcasting),
		// so bundling it → "already processed". The victim's landing creates the
		// gap on-chain; our guarded arb bundle races to capture it. Guard reverts
		// free if the gap is already gone.
		lastSubmit = time.Now()
		returnedSig, err := jito.SendSender(senderURL, arbB64)
		if err != nil {
			errStr := err.Error()
			trimmed := errStr[:min(400, len(errStr))]
			fmt.Fprintf(os.Stderr, "[debug] submit error (%s): %s\n", dir, trimmed)
			logCh <- logMsg{trade: &tradeLog{
				T: now(), BorrowUSDC: float64(borrowAmount) / 1e6, TipLamports: tipLamports,
				Error: strp(errStr),
			}}
		} else {
			logCh <- logMsg{trade: &tradeLog{
				T: now(), BorrowUSDC: float64(borrowAmount) / 1e6, TipLamports: tipLamports,
				Signature: strp(sig),
			}}
			fmt.Fprintf(os.Stderr, "⚡ backrun %s sent %s\n", dir, returnedSig[:min(16, len(returnedSig))])

			owner := signer.String()
			borrowUI := float64(borrowAmount) / 1e6
			go func(sig, owner string, borrowUI float64, tip uint64) {
				// Landing truth = the tx on-chain (getTransaction via
				// observe.RealizedUSDC); Sender returns a signature, not a
				// Jito bundle id, so poll the chain.
				var pnl *float64
				for _, delay := range []int{4, 8, 20} {
					time.Sleep(time.Duration(delay) * time.Second)
					if v, ok := observe.RealizedUSDC(endpoint, sig, owner); ok {
						pnl = f64p(v)
						break
					}
				}
				// Count the tip against the daily cap ONLY on a confirmed
				// landing (accepted-but-dropped pays no tip).
				if pnl != nil {
					dailyTipMu.Lock()
					dailyTipSol += float64(tip) / 1e9
					dailyTipMu.Unlock()
				}
				fmt.Fprintf(os.Stderr, "[readback] %s… landed=%v realized_usdc=%v\n", sig[:min(8, len(sig))], pnl != nil, pnl)
				logCh <- logMsg{trade: &tradeLog{
					T: now(), BorrowUSDC: borrowUI, TipLamports: tip,
					Signature: strp(sig), RealizedUSDC: pnl,
				}}
			}(sig, owner, borrowUI, tipLamports)
		}

		if triggers%100 == 0 {
			fmt.Fprintf(os.Stderr, "[executor] triggers=%d fired=%d\n", triggers, fired)
		}
	}
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
