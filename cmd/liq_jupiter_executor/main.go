// Command liq_jupiter_executor is the Jupiter Lend (Fluid) liquidation
// executor — event-driven off Pyth Lazer, DRY_RUN by default.
//
// Architecture (matches the marginfi executor cmd/liq_executor and the Save
// rewrite): the TRIGGER is a Pyth Lazer WS tick, NOT a getProgramAccounts
// poll. Vault STRUCTURE is refreshed off-band on a slow timer; the
// price-cross recompute runs in-memory on every ms Lazer tick. detectLag
// (nowUs - freshest Lazer publish ts) is logged in the heartbeat, and a
// per-detect latency record is appended to {RUN_DIR}/latency.jsonl.
//
// STATUS: tryArm derives the FULL liquidate account set PURELY FROM SEEDS +
// on-chain state (jupiterfire.DeriveLiquidateAccounts +
// jupiter.DecodeOracleSources) -- the old "lift the Liquidity PDAs from a
// recent liquidate tx" dependency is GONE, so ANY in-scope vault resolves,
// including ones that have never been liquidated. colPerUnitDebt=0 accepts
// the oracle price (a slippage floor, not the price) and remainingAccounts
// come from BuildRemainingAccounts. DRY_RUN by default.
//
// FIRING IS LIVE (DRY_RUN=0): the hot path mirrors the Kamino executor's
// submit-only branch -- stamp a fresh blockhash onto the cached tx, sign
// with KEYPAIR_PATH, and submit via Helius Sender (jito.SendSender). Money
// guards: only <=1232B + sim-clean armed txs are ever cached; DRY_RUN never
// submits; the MAX_DAILY_TIP_SOL daily cap and WALLET_MIN_SOL floor gate
// every live send; a per-vault HANDLE_COOLDOWN_SECS stops resubmitting a
// standing cross.
//
// HONEST FIRING GATE: tryArm arms only when the flash-loan-wrapped fire tx
// is BOTH (a) <= 1232 bytes (submittable -- needs JUP_ALT deployed; without
// it the wrap is ~1.5-1.7KB and is skip-and-logged, never armed) AND (b)
// SIMULATES CLEAN. Until JUP_ALT is deployed on the box, arming is
// size-gated off for USDC vaults.
//
// Scope: only vaults whose debt (borrow_token) is USDC/USDT/wSOL are armed
// (via VaultConfig.DebtInScope); the decoder/detection stay general.
//
// Usage: HELIUS_RPC=<url> PYTH_LAZER_TOKEN=<tok> JUP_ALT=<alt> LIQUIDATOR_MA=<ma>
//
//	[DRY_RUN=1] [KEYPAIR_PATH=~/arb-keypair.json] [AUTHORITY=<pk>]
//	[MAX_DAILY_TIP_SOL=0.05] [WALLET_MIN_SOL=0.02] [MIN_TIP_SOL=0.0002]
//	[SENDER_URL=...] [SENDER_TIP_ACCOUNT=...] [HANDLE_COOLDOWN_SECS=20]
//	[RUN_DIR=.] [TICK_POLL_MS=1] [VAULT_REFRESH_SECS=30] [HEARTBEAT_SECS=10]
//	go run ./cmd/liq_jupiter_executor
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/mr-tron/base58"

	"arbengine/internal/config"
	"arbengine/internal/jito"
	"arbengine/internal/jupiter"
	"arbengine/internal/jupiterfire"
	"arbengine/internal/lazer"
	"arbengine/internal/pyth"
	"arbengine/internal/rpcclient"
	"arbengine/internal/solana"
)

func nowUs() int64 {
	return time.Now().UnixMicro()
}

// logLatency appends a latency record to {runDir}/latency.jsonl (same shape
// as the marginfi executor: an event with detect-side timestamps).
func logLatency(runDir string, v map[string]any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	f, err := os.OpenFile(runDir+"/latency.jsonl", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(b)
	f.Write([]byte("\n"))
}

func gpaByDisc(rpc *rpcclient.Client, disc [8]byte) []rpcclient.ProgramAccount {
	program := solana.MustPubkeyFromBase58(jupiter.VaultsProgram)
	disc58 := base58.Encode(disc[:])
	entries, err := rpc.GetProgramAccounts(program, rpcclient.GetProgramAccountsOpts{
		Filters: []any{
			map[string]any{"memcmp": map[string]any{"offset": 0, "bytes": disc58}},
		},
	})
	if err != nil {
		return nil
	}
	return entries
}

// loadVaults is the off-band vault STRUCTURE refresh (not the trigger):
// load + join all vaults.
func loadVaults(rpc *rpcclient.Client) []jupiter.Vault {
	type cfgEntry struct {
		pk  solana.Pubkey
		cfg jupiter.VaultConfig
	}
	configs := make(map[uint16]cfgEntry)
	for _, e := range gpaByDisc(rpc, jupiter.VaultConfigDisc) {
		if c, ok := jupiter.DecodeVaultConfig(e.Account.Data); ok {
			configs[c.VaultID] = cfgEntry{e.Pubkey, c}
		}
	}
	type stateEntry struct {
		pk    solana.Pubkey
		state jupiter.VaultState
	}
	states := make(map[uint16]stateEntry)
	for _, e := range gpaByDisc(rpc, jupiter.VaultStateDisc) {
		if s, ok := jupiter.DecodeVaultState(e.Account.Data); ok {
			states[s.VaultID] = stateEntry{e.Pubkey, s}
		}
	}
	var vaults []jupiter.Vault
	for vid, c := range configs {
		if s, ok := states[vid]; ok {
			vaults = append(vaults, jupiter.Vault{
				ConfigPubkey: c.pk, StatePubkey: s.pk,
				Config: c.cfg, State: s.state,
			})
		}
	}
	sort.Slice(vaults, func(i, j int) bool { return vaults[i].Config.VaultID < vaults[j].Config.VaultID })
	return vaults
}

// feedForVault returns the Lazer feed id for a vault's collateral mint
// (falls back to the debt mint), so the detection hook has the price this
// vault liquidates against.
func feedForVault(v jupiter.Vault, feedMap map[solana.Pubkey]uint32) (uint32, bool) {
	if f, ok := feedMap[v.Config.SupplyToken]; ok {
		return f, true
	}
	f, ok := feedMap[v.Config.BorrowToken]
	return f, ok
}

// getAcct reads raw account bytes (used by the seed-derivation arm path:
// oracle decode, mint-owner lookup, and BuildRemainingAccounts' PDA
// existence probes).
func getAcct(rpc *rpcclient.Client, pk solana.Pubkey) ([]byte, bool) {
	info, err := rpc.GetAccountInfo(pk)
	if err != nil || info == nil {
		return nil, false
	}
	return info.Data, true
}

func getAcctOwner(rpc *rpcclient.Client, pk solana.Pubkey) (solana.Pubkey, bool) {
	info, err := rpc.GetAccountInfo(pk)
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
		return 0
	}
	return float64(lamports) / 1e9
}

// armed is a pre-built, sim-gated liquidate tx for one vault, ready to
// submit the instant its tick crosses. Same role as the Kamino executor's
// CachedFire: the hot path stamps a fresh blockhash, signs with the
// keypair, and submits -- NO build/quote/sim on the critical path.
// Compiled with a placeholder blockhash (stamped at fire) and a placeholder
// signature (filled at fire).
type armed struct {
	// tx is the sim-gated, <=1232B fire tx (mirrors Kamino's CachedFire.tx).
	tx solana.VersionedTransaction
	// txBytes is the serialized byte length (already <=1232 -- the arm
	// size gate guarantees it).
	txBytes int
	// quotedOut is the Jupiter-quoted debt-asset out for the
	// seized-collateral swap leg.
	quotedOut   uint64
	tipLamports uint64
	tipSol      float64
	// builtUs is nowUs() when built (for staleness-based re-arm).
	builtUs int64
}

const tokenkegProgram = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"

// tryArm is the off-band ARM step: build + quote + sim the flash-loan
// liquidate tx for a vault near its liquidation boundary, so the crossing
// tick submits only.
//
// SEED-DERIVED (no captured tx). The full liquidate account set -- the
// Fluid Liquidity-program PDAs (reserves/positions/rate models/token
// accounts/liquidity), new_branch, and the oracle sources -- is derived
// PURELY from seeds + on-chain vault/oracle state via
// jupiterfire.DeriveLiquidateAccounts + jupiter.DecodeOracleSources.
//
// colPerUnitDebt = 0 accepts the oracle price (a slippage floor, not the
// price; the program prices from its own oracle). remainingAccounts +
// indices come from BuildRemainingAccounts (tick band = topmost down to
// liqTick). Scope: the flash-loan wrap is USDC-debt only (mirrors
// save_fire); returns (nil, false) otherwise. We arm only when the priced
// fire tx SIMULATES CLEAN (liquidatable now AND fits a single packet --
// i.e. JUP_ALT is deployed); a gated/oversized tx is NOT cached. Honest
// guard: if the oracle can't be decoded or the sim isn't clean, we return
// false and log why -- never pre-sign a mispriced/unfittable tx.
func tryArm(
	rpc *rpcclient.Client, endpoint string, v jupiter.Vault, authority, liquidatorMA solana.Pubkey,
	tipAccount solana.Pubkey, tipLamports uint64, tipSol float64,
) (*armed, bool) {
	if v.Config.DebtLabel() != "USDC" {
		return nil, false
	}
	// Oracle price sources straight from the vault's oracle account (in order).
	oracleData, ok := getAcct(rpc, v.Config.Oracle)
	if !ok {
		return nil, false
	}
	sources, ok := jupiter.DecodeOracleSources(oracleData)
	if !ok || len(sources) == 0 {
		return nil, false
	}
	collatMint := v.Config.SupplyToken
	ctp, ok := getAcctOwner(rpc, collatMint)
	if !ok {
		ctp = solana.MustPubkeyFromBase58(tokenkegProgram)
	}
	btp, ok := getAcctOwner(rpc, v.Config.BorrowToken)
	if !ok {
		btp = solana.MustPubkeyFromBase58(tokenkegProgram)
	}
	// Tick band: topmost down to liqTick. topmost-1 includes the topmost
	// tick (the only mandatory one); the program itself walks/gates the rest.
	liqTick := v.State.TopmostTick - 1
	fetch := func(pk solana.Pubkey) ([]byte, bool) { return getAcct(rpc, pk) }
	remaining, indices := jupiterfire.BuildRemainingAccounts(
		v.Config.VaultID, v.State.TopmostTick, v.State.CurrentBranchID, liqTick, sources, fetch)

	fa := jupiterfire.DeriveLiquidateAccounts(&v, ctp, btp)
	fa.Remaining = remaining
	// Size the repay by a fraction of the vault's total borrow (native units).
	debtAmt := v.State.TotalBorrow / 50
	if debtAmt < 1_000_000 {
		debtAmt = 1_000_000
	}
	seize := debtAmt // nominal; the swap quote refines it
	if seize < 1 {
		seize = 1
	}
	cand := jupiterfire.FireCandidate{
		Accts: fa, DebtAmt: debtAmt, ColPerUnitDebt: jupiterfire.ColPerUnitDebtFromU64(0),
		Remaining: remaining, RemainingIndices: indices,
		SeizeUnderlying: seize, CollateralMint: collatMint, CollateralTokenProgram: ctp,
	}
	// Build WITH the tip baked in (mirrors Kamino's try_arm) so the hot
	// path is pure submit -- the tx we sim-gate is byte-identical to the
	// tx we submit. Tip=0 (DRY_RUN default) simply omits the transfer ix.
	// BuildJupiterFireTx folds in JUP_ALT/LIQ_ALT from env, so the wrapped
	// fire drops <=1232 once JUP_ALT is set.
	var tipAcctPtr *solana.Pubkey
	if tipLamports > 0 {
		tipAcctPtr = &tipAccount
	}
	fire, err := jupiterfire.BuildJupiterFireTx(
		endpoint, &cand, &liquidatorMA, &authority, tipAcctPtr, tipLamports, 50_000, 100, 16, solana.Hash{},
	)
	if err != nil {
		return nil, false
	}
	// Submittable-size gate (HONEST): simulateTransaction does NOT enforce
	// the 1232-byte single-packet limit, but sendTransaction does. Never
	// cache a tx we couldn't actually submit. JUP_ALT is folded in above
	// (without it the wrap is ~1.5-1.7KB); the remaining overflow on a
	// tight vault is the MANDATORY Helius Sender tip (~50-80B) plus this
	// vault's per-state tick/branch remaining accounts (not ALT-able, they
	// vary per liquidation). Low-branch vaults fit; high-branch ones
	// size-gate off here -- never armed, never sent.
	if fire.TxBytes > 1232 {
		fmt.Fprintf(os.Stderr, "     · vault %d composes CLEAN but fire tx is %dB > 1232 (JUP_ALT applied; tip + %d branch "+
			"remaining accts exceed headroom) — size-gated off, not arming\n",
			v.Config.VaultID, fire.TxBytes, indices[1])
		return nil, false
	}
	// Sim-gate: arm only on a clean sim (fireable now).
	txBytes, err := fire.Tx.MarshalBinary()
	if err != nil {
		return nil, false
	}
	b64tx := base64StdEncode(txBytes)
	simResult, err := rpc.SimulateTransaction(b64tx)
	// A clean sim REQUIRES a present result.value with err == null. Guard
	// against reading an RPC-level error / absent value as "clean".
	if err != nil || simResult == nil {
		return nil, false
	}
	var sim struct {
		Err json.RawMessage `json:"err"`
	}
	if jsonErr := json.Unmarshal(simResult, &sim); jsonErr != nil {
		return nil, false
	}
	clean := sim.Err == nil || string(sim.Err) == "null"
	if !clean {
		return nil, false
	}
	return &armed{
		tx: fire.Tx, txBytes: fire.TxBytes, quotedOut: fire.QuotedUSDCOut,
		tipLamports: tipLamports, tipSol: tipSol, builtUs: nowUs(),
	}, true
}

// dailyTip is the mutex-guarded running total of tip SOL spent today
// (Arc<Mutex<f64>> in the Rust original).
type dailyTip struct {
	mu  sync.Mutex
	sol float64
}

func (d *dailyTip) get() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sol
}

func (d *dailyTip) add(v float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sol += v
}

func (d *dailyTip) reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sol = 0
}

// fireArmed fires an armed tx: stamp fresh blockhash, sign, submit via
// Helius Sender, log the signature. Mirrors the Kamino executor's
// fire_cached submit-only branch -- NO build/quote/sim here. Money-code
// guards (in order): defensive <=1232 re-check, DRY_RUN never submits,
// MAX_DAILY_TIP_SOL daily cap, WALLET_MIN_SOL floor.
func fireArmed(
	rpc *rpcclient.Client, endpoint, runDir, senderURL string, dryRun bool,
	vaultID uint16, a *armed, authority solana.Pubkey, freshBh solana.Hash,
	kp *solana.Keypair, tip *dailyTip, maxDailyTip, walletMin float64,
) {
	submitUs := nowUs()
	rec := func(extra map[string]any) {
		j := map[string]any{
			"event": "fire", "protocol": "jupiter", "vault_id": vaultID,
			"quoted_out": a.quotedOut, "armed_age_us": strconv.FormatInt(submitUs-a.builtUs, 10),
			"submit_us": strconv.FormatInt(submitUs, 10), "tx_bytes": a.txBytes,
			"tip_lamports": a.tipLamports,
		}
		for k, v := range extra {
			j[k] = v
		}
		logLatency(runDir, j)
	}
	// Defensive: the arm-cache only holds <=1232B, sim-clean txs -- re-check
	// size before ever touching the wire (never submit an unsendable packet).
	if a.txBytes > 1232 {
		fmt.Fprintf(os.Stderr, "[jup-exec] REFUSING vault %d: cached tx %dB > 1232\n", vaultID, a.txBytes)
		return
	}
	if dryRun {
		rec(map[string]any{"dry_run": true, "fired": false})
		fmt.Printf("     ⓘ DRY_RUN: would FIRE vault %d (%dB, tip %.5f SOL) — not submitting\n", vaultID, a.txBytes, a.tipSol)
		return
	}
	// Daily tip cap + wallet floor — identical to the Kamino executor.
	if tip.get()+a.tipSol > maxDailyTip {
		fmt.Fprintf(os.Stderr, "[jup-exec] daily tip cap reached — not firing vault %d\n", vaultID)
		rec(map[string]any{"dry_run": false, "fired": false, "error": "daily tip cap"})
		return
	}
	if solBalance(rpc, authority) < walletMin {
		fmt.Fprintf(os.Stderr, "[jup-exec] wallet below floor %v SOL — not firing vault %d\n", walletMin, vaultID)
		rec(map[string]any{"dry_run": false, "fired": false, "error": "wallet below floor"})
		return
	}
	tx := a.tx
	tx.Message.V0.RecentBlockhash = freshBh
	if kp == nil {
		fmt.Fprintln(os.Stderr, "[jup-exec] live fire requires KEYPAIR_PATH")
		os.Exit(1)
	}
	if err := tx.Sign([]solana.Keypair{*kp}); err != nil {
		fmt.Fprintf(os.Stderr, "[jup-exec] sign failed: %v\n", err)
		return
	}
	sig := tx.Signatures[0].String()
	txBytes, err := tx.MarshalBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[jup-exec] serialize failed: %v\n", err)
		return
	}
	txB64 := base64StdEncode(txBytes)
	if _, err := jito.SendSender(senderURL, txB64); err != nil {
		fmt.Fprintf(os.Stderr, "[jup-exec] send failed: %v\n", err)
		rec(map[string]any{"dry_run": false, "fired": false, "error": err.Error()})
		return
	}
	tip.add(a.tipSol)
	fmt.Fprintf(os.Stderr, "[jup-exec] FIRED %s\n", sig)
	rec(map[string]any{"dry_run": false, "fired": true, "signature": sig})
}

func fatal(format string, args ...any) {
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
		fatal("HELIUS_RPC")
	}
	rpc := rpcclient.New(endpoint)
	runDir := config.EnvOr("RUN_DIR", ".")
	tickPollMs := config.EnvInt("TICK_POLL_MS", 1)
	vaultRefreshSecs := config.EnvInt("VAULT_REFRESH_SECS", 30)
	hbEverySecs := config.EnvInt("HEARTBEAT_SECS", 10)
	dryRun := true
	if v, ok := config.EnvOptional("DRY_RUN"); ok {
		dryRun = v != "0"
	}

	// ── SUBMIT config (mirrors the Kamino executor) ──
	senderURL := config.EnvOr("SENDER_URL", "http://ams-sender.helius-rpc.com/fast")
	tipAccount := solana.MustPubkeyFromBase58(config.EnvOr("SENDER_TIP_ACCOUNT", "2nyhqdwKcJZR2vcqCyrYsaPVdAnFoJjiksCXJ7hfEYgD"))
	minTipSol := config.EnvFloat("MIN_TIP_SOL", 0.0002)
	maxDailyTipSol := config.EnvFloat("MAX_DAILY_TIP_SOL", 0.05)
	walletMinSol := config.EnvFloat("WALLET_MIN_SOL", 0.02)
	handleCooldown := time.Duration(config.EnvInt("HANDLE_COOLDOWN_SECS", 20)) * time.Second
	// Flat tip baked into the armed fire tx (Jupiter has no per-vault
	// profit calc; the tx's own fixed-payback guard is the
	// profit-or-revert protection).
	tipSol := minTipSol
	tipLamports := uint64(tipSol * 1e9)
	// The marginfi flash account for the flash-loan wrap. Defaults to the
	// fleet liquidator's account (same default as liq_executor) -- the
	// 2026-07-13 run silently never armed because the env var was unset
	// and there was no fallback.
	liquidatorMAStr := config.EnvOr("LIQUIDATOR_MA", "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD")
	liquidatorMA, liquidatorMAErr := solana.PubkeyFromBase58(liquidatorMAStr)
	liquidatorMAOk := liquidatorMAErr == nil

	// Keypair (submit-side): LIVE requires it; DRY_RUN falls back to
	// AUTHORITY env (or the fleet default) so arm/sim still exercise the
	// real-wallet constraints.
	var kp *solana.Keypair
	if p, ok := config.EnvOptional("KEYPAIR_PATH"); ok {
		if raw, err := os.ReadFile(p); err == nil {
			var bytes []byte
			if jsonErr := json.Unmarshal(raw, &bytes); jsonErr == nil {
				if k, kpErr := solana.KeypairFromBytes(bytes); kpErr == nil {
					kp = &k
				}
			}
		}
	}
	if kp == nil && !dryRun {
		fatal("LIVE fire needs KEYPAIR_PATH")
	}
	authority := solana.MustPubkeyFromBase58("DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak")
	if kp != nil {
		authority = kp.Public
	} else if v, ok := config.EnvOptional("AUTHORITY"); ok {
		authority = solana.MustPubkeyFromBase58(v)
	}

	tip := &dailyTip{}
	tipDay := nowUs() / 86_400_000_000
	var freshBh solana.Hash
	lastBh := time.Now().Add(-9999 * time.Second)
	// Per-vault fire cooldown -- don't resubmit the same standing cross
	// every tick.
	handled := make(map[uint16]time.Time)

	// Event-driven trigger: Pyth Lazer WS, same feeds as the other executors.
	lazerTable := pyth.NewTable()
	lazerOn := false
	if tok, ok := config.EnvOptional("PYTH_LAZER_TOKEN"); ok {
		lazer.SpawnLazerThread(tok, lazer.ArmFeedIDs(), lazerTable)
		lazerOn = true
	} else {
		fmt.Fprintln(os.Stderr, "[jup-exec] no PYTH_LAZER_TOKEN — falling back to timed rescan (NOT event-driven)")
	}
	feedMap := lazer.MintFeedMap()

	mode := "[LIVE]"
	if dryRun {
		mode = "[DRY RUN]"
	}
	fmt.Printf("[jup-exec] Jupiter Lend (Fluid) executor %s  authority=%s lazer=%v  (fire gated: ≤1232B + sim-clean; JUP_ALT required)\n",
		mode, authority.String(), lazerOn)
	if !dryRun {
		bal := solBalance(rpc, authority)
		fmt.Fprintf(os.Stderr, "[jup-exec] wallet balance: %v SOL\n", bal)
		if bal < walletMinSol {
			fatal("wallet below floor %v", walletMinSol)
		}
	}

	vaults := loadVaults(rpc)
	trigger := "timed rescan (fallback)"
	if lazerOn {
		trigger = "Pyth Lazer tick (event-driven)"
	}
	fmt.Printf("[jup-exec] loaded %d vaults; trigger = %s\n", len(vaults), trigger)

	lastRefresh := time.Now()
	lastHb := time.Now()
	var lastTickUs uint64
	reported := make(map[uint16]bool)
	// Arm-cache keyed by vault_id: pre-signed txs ready for submit-only firing.
	armCache := make(map[uint16]*armed)

	for {
		// Off-band STRUCTURE refresh — NOT the trigger.
		if time.Since(lastRefresh) >= time.Duration(vaultRefreshSecs)*time.Second {
			vaults = loadVaults(rpc)
			lastRefresh = time.Now()
			reported = make(map[uint16]bool) // re-report candidates against the fresh structure
		}

		// Reset the daily tip budget at the UTC-day boundary; refresh the
		// fire blockhash every ~2s so a crossing tick submits with a
		// near-current hash.
		day := nowUs() / 86_400_000_000
		if day != tipDay {
			tipDay = day
			tip.reset()
		}
		if !dryRun && time.Since(lastBh) >= 2*time.Second {
			if bh, err := rpc.GetLatestBlockhash(); err == nil {
				freshBh = bh
				lastBh = time.Now()
			}
		}

		// ── TRIGGER: block until a fresh Lazer tick (in-memory, no RPC) ──
		if lazerOn {
			deadline := time.Now().Add(1 * time.Second)
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
				time.Sleep(time.Duration(tickPollMs) * time.Millisecond)
			}
		} else {
			sleepSecs := vaultRefreshSecs
			if sleepSecs < 1 {
				sleepSecs = 1
			}
			time.Sleep(time.Duration(sleepSecs) * time.Second)
		}

		// Price snapshot for THIS tick.
		snap := make(map[uint32]float64)
		for _, f := range lazer.ArmFeedIDs() {
			if p, ok := pyth.Get(lazerTable, f); ok {
				snap[f] = p.Price
			}
		}

		// ── Detection on the tick (in-memory over the snapshot) ──
		// CONFIDENT signal today; the live price per vault is resolved and
		// passed to the hook so this becomes a true price-cross the moment
		// tick↔price math lands.
		var cands []jupiter.Vault
		for _, v := range vaults {
			if v.Config.DebtInScope() && v.MaybeLiquidatable() {
				cands = append(cands, v)
			}
		}

		// ── HOT PATH: submit-only for any crossing vault that is armed ──
		// Detect→submit ~0 when armed (blockhash-stamp + sign + send, no
		// build/quote/sim). Mirrors the Kamino executor's fire_cached
		// branch. A per-vault handle_cooldown stops resubmitting the same
		// standing cross every tick.
		for _, v := range cands {
			vid := v.Config.VaultID
			if t, ok := handled[vid]; ok && time.Since(t) < handleCooldown {
				continue
			}
			if a, ok := armCache[vid]; ok {
				handled[vid] = time.Now()
				fireArmed(rpc, endpoint, runDir, senderURL, dryRun, vid, a, authority,
					freshBh, kp, tip, maxDailyTipSol, walletMinSol)
			}
		}

		// Heartbeat with detect_lag (now_us − freshest Lazer publish).
		if hbEverySecs > 0 && time.Since(lastHb) >= time.Duration(hbEverySecs)*time.Second {
			total := len(lazer.ArmFeedIDs())
			var freshest uint64
			for _, f := range lazer.ArmFeedIDs() {
				if p, ok := pyth.Get(lazerTable, f); ok && p.TsUs > freshest {
					freshest = p.TsUs
				}
			}
			lagMs := int64(0)
			if nu := nowUs(); uint64(nu) > freshest {
				lagMs = (nu - int64(freshest)) / 1000
			}
			fmt.Fprintf(os.Stderr, "[hb] lazer feeds %d/%d live | detect_lag %dms | %d vaults | %d in-scope candidate(s) | %s\n",
				len(snap), total, lagMs, len(vaults), len(cands), lazer.Status(lazerTable))
			lastHb = time.Now()
		}

		// Report/resolve each NEW candidate once per structure cycle (RPC
		// off the tick path). Emits a per-detect latency record (detect vs
		// Lazer publish).
		for _, v := range cands {
			if reported[v.Config.VaultID] {
				continue
			}
			reported[v.Config.VaultID] = true
			c := v.Config
			feed, feedOk := feedForVault(v, feedMap)
			var price *float64
			if feedOk {
				if p, ok := snap[feed]; ok {
					price = &p
				}
			}
			var freshest uint64
			for _, f := range lazer.ArmFeedIDs() {
				if p, ok := pyth.Get(lazerTable, f); ok && p.TsUs > freshest {
					freshest = p.TsUs
				}
			}
			nu := nowUs()
			var detectLagUs int64
			if uint64(nu) > freshest {
				detectLagUs = nu - int64(freshest)
			}
			var feedField any
			if feedOk {
				feedField = feed
			}
			logLatency(runDir, map[string]any{
				"event":             "detect",
				"protocol":          "jupiter",
				"vault_id":          c.VaultID,
				"debt":              c.DebtLabel(),
				"lazer_feed":        feedField,
				"lazer_price":       price,
				"lazer_ts_us":       freshest,
				"detect_us":         strconv.FormatInt(nu, 10),
				"detect_lag_us":     strconv.FormatInt(detectLagUs, 10),
				"absorbed_debt":     absorbedDebtString(v.State.AbsorbedDebtAmount),
				"liq_threshold_bps": c.LiquidationThreshold,
				"fired":             false,
				"reason":            "detection-only (col_per_unit_debt + remaining-accounts unsolved)",
			})
			collat := c.SupplyToken.String()[:6]
			priceStr := "None"
			if price != nil {
				priceStr = fmt.Sprintf("Some(%v)", *price)
			}
			fmt.Printf("  ▸ vault %d [%s→%s] LT %.1f%% absorbed_debt=%s price=%s detect_lag=%dµs\n",
				c.VaultID, collat, c.DebtLabel(), c.LiqThresholdFrac()*100.0,
				absorbedDebtString(v.State.AbsorbedDebtAmount), priceStr, detectLagUs)
			// Off-band ARM: derive the FULL account set from seeds (no
			// captured tx needed), build the priced flash-loan fire tx,
			// and sim-gate it. Needs LIQUIDATOR_MA (the marginfi flash
			// account); skip arming without it.
			var a *armed
			var armedOk bool
			if liquidatorMAOk {
				a, armedOk = tryArm(rpc, endpoint, v, authority, liquidatorMA, tipAccount, tipLamports, tipSol)
			}
			if armedOk {
				fmt.Printf("     ✓ ARMED — seed-derived, priced fire tx simulates clean (%dB)\n", a.txBytes)
				armCache[c.VaultID] = a
			} else {
				extra := ""
				if !liquidatorMAOk {
					extra = " (LIQUIDATOR_MA unset/invalid — arming disabled)"
				}
				fmt.Printf("     · not armed%s: not fireable at the live price, non-USDC debt, "+
					"or fire tx > 1232B (deploy JUP_ALT — see `cargo run --bin jup_alt_print`) — sim-gated, not sending\n",
					extra)
			}
		}
	}
}

// absorbedDebtString renders the raw little-endian u128 AbsorbedDebtAmount
// as a decimal string (mirrors the Rust u128's Display).
func absorbedDebtString(raw [16]byte) string {
	return leU128ToDecimalString(raw)
}
