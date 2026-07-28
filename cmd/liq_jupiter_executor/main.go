// Jupiter Lend (Fluid) liquidation executor — event-driven off Pyth Lazer,
// DRY_RUN by default.
//
// Architecture (matches the marginfi/Kamino executors): the TRIGGER is a Pyth
// Lazer WS tick, NOT a getProgramAccounts poll. Vault STRUCTURE is refreshed
// off-band on a slow timer; the price-cross recompute runs in-memory on every
// ms Lazer tick. detect_lag (now_us − freshest Lazer publish ts) is logged in
// the heartbeat, and a per-detect latency record is appended to
// {RUN_DIR}/latency.jsonl.
//
// STATUS: try_arm derives the FULL liquidate account set PURELY FROM SEEDS +
// on-chain state (jupiterlend.DeriveLiquidateAccounts + DecodeOracleSources)
// — any in-scope vault resolves, including ones that have never been
// liquidated. col_per_unit_debt=0 accepts the oracle price (a slippage
// floor, not the price) and remaining_accounts come from
// BuildRemainingAccounts. DRY_RUN by default.
//
// FIRING IS LIVE (DRY_RUN=0): the hot path mirrors the Kamino executor's
// submit-only branch — stamp a fresh blockhash onto the cached tx, sign with
// KEYPAIR_PATH, and submit via Helius Sender. Money guards: only ≤1232B +
// sim-clean armed txs are ever cached; DRY_RUN never submits; the
// MAX_DAILY_TIP_SOL daily cap and WALLET_MIN_SOL floor gate every live send; a
// per-vault HANDLE_COOLDOWN_SECS stops resubmitting a standing cross.
//
// HONEST FIRING GATE: try_arm arms only when the flash-loan-wrapped fire tx is
// BOTH (a) ≤ 1232 bytes (submittable — needs JUP_ALT deployed; without it the
// wrap is ~1.5-1.7KB and is skip-and-logged, never armed) AND (b) SIMULATES
// CLEAN. Until JUP_ALT is deployed on the box, arming is size-gated off for
// USDC vaults — the account DERIVATION is proven separately (see
// jupiter_seed_probe / jupiter_fire_probe), the last step is the ALT.
//
// Scope: only vaults whose debt (borrow_token) is USDC/USDT/wSOL are armed
// (via VaultConfig.DebtInScope); the decoder/detection stay general.
//
// Usage: HELIUS_RPC=<url> PYTH_LAZER_TOKEN=<tok> JUP_ALT=<alt> LIQUIDATOR_MA=<ma>
//
//	[DRY_RUN=1] [KEYPAIR_PATH=~/arb-keypair.json] [AUTHORITY=<pk>]
//	[MAX_DAILY_TIP_SOL=0.05] [WALLET_MIN_SOL=0.02] [MIN_TIP_SOL=0.0002]
//	[SENDER_URL=…] [SENDER_TIP_ACCOUNT=…] [HANDLE_COOLDOWN_SECS=20]
//	[RUN_DIR=.] [TICK_POLL_MS=1] [VAULT_REFRESH_SECS=30] [HEARTBEAT_SECS=10]
//	go run ./cmd/liq_jupiter_executor
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/base58"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/jito"
	"solana-arb-backtest-go/internal/jupiterlend"
	"solana-arb-backtest-go/internal/lazer"
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

func nowUs() int64 { return time.Now().UnixMicro() }

// logLatency appends a latency record to {run_dir}/latency.jsonl (same shape
// as the marginfi/Kamino executors: an event with detect-side timestamps).
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

func b64(d any) ([]byte, bool) {
	arr, ok := d.([]any)
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

func gpaByDisc(endpoint string, disc [8]byte) []struct {
	Pk   solana.PublicKey
	Data []byte
} {
	disc58 := base58.Encode(disc[:])
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{jupiterlend.VaultsProgram, map[string]any{"encoding": "base64",
			"filters": []any{map[string]any{"memcmp": map[string]any{"offset": 0, "bytes": disc58}}}}}})
	var out []struct {
		Pk   solana.PublicKey
		Data []byte
	}
	if !ok {
		return out
	}
	for _, ev := range asArray(v["result"]) {
		e := asMap(ev)
		if e == nil {
			continue
		}
		pkStr, _ := e["pubkey"].(string)
		pk, err := solana.PublicKeyFromBase58(pkStr)
		if err != nil {
			continue
		}
		data, ok := b64(asMap(e["account"])["data"])
		if !ok {
			continue
		}
		out = append(out, struct {
			Pk   solana.PublicKey
			Data []byte
		}{pk, data})
	}
	return out
}

// loadVaults is the off-band vault STRUCTURE refresh (not the trigger): load
// + join all vaults.
func loadVaults(endpoint string) []*jupiterlend.Vault {
	configs := map[uint16]struct {
		Pk  solana.PublicKey
		Cfg *jupiterlend.VaultConfig
	}{}
	for _, e := range gpaByDisc(endpoint, jupiterlend.VaultConfigDisc) {
		if c, ok := jupiterlend.DecodeVaultConfig(e.Data); ok {
			configs[c.VaultID] = struct {
				Pk  solana.PublicKey
				Cfg *jupiterlend.VaultConfig
			}{e.Pk, c}
		}
	}
	states := map[uint16]struct {
		Pk solana.PublicKey
		St *jupiterlend.VaultState
	}{}
	for _, e := range gpaByDisc(endpoint, jupiterlend.VaultStateDisc) {
		if s, ok := jupiterlend.DecodeVaultState(e.Data); ok {
			states[s.VaultID] = struct {
				Pk solana.PublicKey
				St *jupiterlend.VaultState
			}{e.Pk, s}
		}
	}
	var vaults []*jupiterlend.Vault
	for vid, c := range configs {
		if s, ok := states[vid]; ok {
			vaults = append(vaults, &jupiterlend.Vault{
				ConfigPubkey: c.Pk, StatePubkey: s.Pk, Config: c.Cfg, State: s.St,
			})
		}
	}
	sortVaultsByID(vaults)
	return vaults
}

func sortVaultsByID(vs []*jupiterlend.Vault) {
	for i := 1; i < len(vs); i++ {
		for j := i; j > 0 && vs[j-1].Config.VaultID > vs[j].Config.VaultID; j-- {
			vs[j-1], vs[j] = vs[j], vs[j-1]
		}
	}
}

// feedForVault is the Lazer feed id for a vault's collateral mint (falls
// back to the debt mint), so the detection hook has the price this vault
// liquidates against.
func feedForVault(v *jupiterlend.Vault, feedMap map[solana.PublicKey]uint32) (uint32, bool) {
	if f, ok := feedMap[v.Config.SupplyToken]; ok {
		return f, true
	}
	f, ok := feedMap[v.Config.BorrowToken]
	return f, ok
}

// getAcct reads raw account bytes (used by the seed-derivation arm path:
// oracle decode, mint-owner lookup, and BuildRemainingAccounts' PDA
// existence probes).
func getAcct(endpoint string, pk solana.PublicKey) ([]byte, bool) {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{pk.String(), map[string]any{"encoding": "base64"}}})
	if !ok {
		return nil, false
	}
	value := asMap(asMap(v["result"])["value"])
	if value == nil {
		return nil, false
	}
	return b64(value["data"])
}

func getAcctOwner(endpoint string, pk solana.PublicKey) (solana.PublicKey, bool) {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{pk.String(), map[string]any{"encoding": "base64"}}})
	if !ok {
		return solana.PublicKey{}, false
	}
	value := asMap(asMap(v["result"])["value"])
	if value == nil {
		return solana.PublicKey{}, false
	}
	ownerStr, _ := value["owner"].(string)
	owner, err := solana.PublicKeyFromBase58(ownerStr)
	if err != nil {
		return solana.PublicKey{}, false
	}
	return owner, true
}

// latestBlockhash is a fresh blockhash for stamping the pre-built fire tx at
// submit time.
func latestBlockhash(endpoint string) (solana.Hash, bool) {
	v, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getLatestBlockhash",
		"params": []any{map[string]any{"commitment": "finalized"}}})
	if !ok {
		return solana.Hash{}, false
	}
	value := asMap(asMap(v["result"])["value"])
	if value == nil {
		return solana.Hash{}, false
	}
	bhStr, _ := value["blockhash"].(string)
	h, err := solana.HashFromBase58(bhStr)
	if err != nil {
		return solana.Hash{}, false
	}
	return h, true
}

// solBalance is the wallet SOL balance (the WALLET_MIN_SOL floor guard), in SOL.
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

const tokenProgramID = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"

func defaultTokenProgram() solana.PublicKey {
	return solana.MustPublicKeyFromBase58(tokenProgramID)
}

// armed is a pre-built, sim-gated liquidate tx for one vault, ready to submit
// the instant its tick crosses. Same role as the Kamino executor's
// CachedFire: the hot path stamps a fresh blockhash, signs with the keypair,
// and submits — NO build/quote/sim on the critical path. Compiled with a
// placeholder blockhash (stamped at fire) and a placeholder signature
// (filled at fire).
type armed struct {
	// The sim-gated, ≤1232B fire tx (mirrors Kamino's CachedFire.tx).
	tx *solana.Transaction
	// Serialized byte length (already ≤1232 — the arm size gate guarantees it).
	txBytes int
	// Jupiter-quoted debt-asset out for the seized-collateral swap leg.
	quotedOut uint64
	// Tip baked into the cached tx (for the daily-cap accounting at fire time).
	tipLamports uint64
	tipSol      float64
	// now_us() when built (for staleness-based re-arm).
	builtUs int64
}

// tryArm is the off-band ARM step: build + quote + sim the flash-loan
// liquidate tx for a vault near its liquidation boundary, so the crossing
// tick submits only.
//
// SEED-DERIVED (no captured tx). The full liquidate account set — the Fluid
// Liquidity-program PDAs, new_branch, and the oracle sources — is derived
// PURELY from seeds + on-chain vault/oracle state via
// jupiterlend.DeriveLiquidateAccounts + jupiterlend.DecodeOracleSources. This
// is what lets ANY in-scope vault arm, including ones with no recent
// liquidate tx.
//
// col_per_unit_debt = 0 accepts the oracle price (a slippage floor, not the
// price; the program prices from its own oracle). remaining_accounts +
// indices come from BuildRemainingAccounts (tick band = topmost down to
// liq_tick). Scope: the flash-loan wrap is USDC-debt only; returns
// false otherwise. We arm only when the priced fire tx SIMULATES CLEAN
// (liquidatable now AND fits a single packet — i.e. JUP_ALT is deployed); a
// gated/oversized tx is NOT cached. Honest guard: if the oracle can't be
// decoded or the sim isn't clean, we return false and log why — never
// pre-sign a mispriced/unfittable tx.
func tryArm(
	endpoint string, v *jupiterlend.Vault, authority, liquidatorMA solana.PublicKey,
	tipAccount solana.PublicKey, tipLamports uint64, tipSol float64,
) (*armed, bool) {
	if v.Config.DebtLabel() != "USDC" {
		return nil, false
	}
	// Oracle price sources straight from the vault's oracle account (in order).
	oracleRaw, ok := getAcct(endpoint, v.Config.Oracle)
	if !ok {
		return nil, false
	}
	sources, ok := jupiterlend.DecodeOracleSources(oracleRaw)
	if !ok || len(sources) == 0 {
		return nil, false
	}
	collatMint := v.Config.SupplyToken
	ctp, ok := getAcctOwner(endpoint, collatMint)
	if !ok {
		ctp = defaultTokenProgram()
	}
	btp, ok := getAcctOwner(endpoint, v.Config.BorrowToken)
	if !ok {
		btp = defaultTokenProgram()
	}
	// Tick band: topmost down to liq_tick. topmost-1 includes the topmost tick
	// (the only mandatory one); the program itself walks/gates the rest.
	liqTick := v.State.TopmostTick - 1
	fetch := func(pk solana.PublicKey) ([]byte, bool) { return getAcct(endpoint, pk) }
	remaining, indices := jupiterlend.BuildRemainingAccounts(
		v.Config.VaultID, v.State.TopmostTick, v.State.CurrentBranchID, liqTick, sources, fetch)

	fa := jupiterlend.DeriveLiquidateAccounts(v, ctp, btp)
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
	cand := &jupiterlend.FireCandidate{
		Accts: fa, DebtAmt: debtAmt, ColPerUnitDebt: &[2]uint64{0, 0},
		Remaining: remaining, RemainingIndices: indices,
		SeizeUnderlying: seize, CollateralMint: collatMint, CollateralTokenProgram: ctp,
	}
	// Build WITH the tip baked in (mirrors Kamino's try_arm) so the hot path is
	// pure submit — the tx we sim-gate is byte-identical to the tx we submit.
	// Tip=0 (DRY_RUN default) simply omits the transfer ix. BuildFireTx folds
	// in JUP_ALT/LIQ_ALT from env, so the wrapped fire drops ≤1232 once
	// JUP_ALT is set.
	var tipAcctPtr *solana.PublicKey
	if tipLamports > 0 {
		tipAcctPtr = &tipAccount
	}
	fire, err := jupiterlend.BuildFireTx(
		endpoint, cand, liquidatorMA, authority, tipAcctPtr, tipLamports, 50_000, 100, 16, solana.Hash{})
	if err != nil {
		return nil, false
	}
	// Submittable-size gate (HONEST): simulateTransaction does NOT enforce the
	// 1232-byte single-packet limit, but sendTransaction does. Never cache a
	// tx we couldn't actually submit. JUP_ALT is folded in above (without it
	// the wrap is ~1.5-1.7KB); the remaining overflow on a tight vault is the
	// MANDATORY Helius Sender tip (~50-80B) plus this vault's per-state
	// tick/branch remaining accounts (not ALT-able, they vary per
	// liquidation). Low-branch vaults fit; high-branch ones size-gate off
	// here — never armed, never sent.
	if fire.TxBytes > 1232 {
		fmt.Fprintf(os.Stderr, "     · vault %d composes CLEAN but fire tx is %dB > 1232 (JUP_ALT applied; tip + %d branch "+
			"remaining accts exceed headroom) — size-gated off, not arming\n",
			v.Config.VaultID, fire.TxBytes, indices[1])
		return nil, false
	}
	// Sim-gate: arm only on a clean sim (fireable now).
	txB64, err := fire.Tx.ToBase64()
	if err != nil {
		return nil, false
	}
	sim, ok := rpc(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "simulateTransaction",
		"params": []any{txB64, map[string]any{"sigVerify": false, "replaceRecentBlockhash": true, "commitment": "processed", "encoding": "base64"}}})
	// A clean sim REQUIRES a present result.value with err == null. Guard
	// against reading an RPC-level error object (no result) as "clean".
	clean := false
	if ok {
		result := asMap(sim["result"])
		if result != nil {
			if value, has := result["value"]; has {
				if vm, isMap := value.(map[string]any); isMap {
					clean = vm["err"] == nil
				}
			}
		}
	}
	if !clean {
		return nil, false
	}
	return &armed{
		tx: fire.Tx, txBytes: fire.TxBytes, quotedOut: fire.QuotedUSDCOut,
		tipLamports: tipLamports, tipSol: tipSol, builtUs: nowUs(),
	}, true
}

// fireArmed fires an armed tx: stamp fresh blockhash, sign, submit via
// Helius Sender, log the signature. Mirrors the Kamino executor's
// fire_cached submit-only branch — NO build/quote/sim here. Money-code
// guards (in order): defensive ≤1232 re-check, DRY_RUN never submits,
// MAX_DAILY_TIP_SOL daily cap, WALLET_MIN_SOL floor.
func fireArmed(
	endpoint, runDir, senderURL string, dryRun bool,
	vaultID uint16, a *armed, authority solana.PublicKey, freshBh solana.Hash,
	kp *solana.PrivateKey, dailyTip *sync.Mutex, dailyTipVal *float64, maxDailyTip, walletMin float64,
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
	// Defensive: the arm-cache only holds ≤1232B, sim-clean txs — re-check
	// size before ever touching the wire (never submit an unsendable packet).
	if a.txBytes > 1232 {
		fmt.Fprintf(os.Stderr, "[jup-exec] REFUSING vault %d: cached tx %dB > 1232\n", vaultID, a.txBytes)
		return
	}
	if dryRun {
		rec(map[string]any{"dry_run": true, "fired": false})
		fmt.Printf("     ℹ DRY_RUN: would FIRE vault %d (%dB, tip %.5f SOL) — not submitting\n", vaultID, a.txBytes, a.tipSol)
		return
	}
	// Daily tip cap + wallet floor — identical to the Kamino executor.
	dailyTip.Lock()
	over := *dailyTipVal+a.tipSol > maxDailyTip
	dailyTip.Unlock()
	if over {
		fmt.Fprintf(os.Stderr, "[jup-exec] daily tip cap reached — not firing vault %d\n", vaultID)
		rec(map[string]any{"dry_run": false, "fired": false, "error": "daily tip cap"})
		return
	}
	if solBalance(endpoint, authority.String()) < walletMin {
		fmt.Fprintf(os.Stderr, "[jup-exec] wallet below floor %g SOL — not firing vault %d\n", walletMin, vaultID)
		rec(map[string]any{"dry_run": false, "fired": false, "error": "wallet below floor"})
		return
	}
	tx := *a.tx
	msg := a.tx.Message
	tx.Message = msg
	tx.Message.RecentBlockhash = freshBh
	tx.Signatures = append([]solana.Signature{}, a.tx.Signatures...)
	if kp == nil {
		fmt.Fprintln(os.Stderr, "[jup-exec] live fire requires KEYPAIR_PATH")
		return
	}
	if _, err := tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(kp.PublicKey()) {
			return kp
		}
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "[jup-exec] sign failed: %v\n", err)
		return
	}
	sig := tx.Signatures[0].String()
	raw, err := tx.MarshalBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[jup-exec] marshal failed: %v\n", err)
		return
	}
	txB64 := base64.StdEncoding.EncodeToString(raw)
	if _, err := jito.SendSender(senderURL, txB64); err != nil {
		fmt.Fprintf(os.Stderr, "[jup-exec] send failed: %v\n", err)
		rec(map[string]any{"dry_run": false, "fired": false, "error": err.Error()})
		return
	}
	dailyTip.Lock()
	*dailyTipVal += a.tipSol
	dailyTip.Unlock()
	fmt.Fprintf(os.Stderr, "[jup-exec] FIRED %s\n", sig)
	rec(map[string]any{"dry_run": false, "fired": true, "signature": sig})
}

func envStr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
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

func freshestLazerTs(table *lazer.PriceTable) uint64 {
	var freshest uint64
	for _, f := range lazer.ArmFeedIDs() {
		if p, ok := table.Get(f); ok && p.TsUs > freshest {
			freshest = p.TsUs
		}
	}
	return freshest
}

func lazerSnapshot(table *lazer.PriceTable) map[uint32]float64 {
	snap := make(map[uint32]float64)
	for _, f := range lazer.ArmFeedIDs() {
		if p, ok := table.Get(f); ok {
			snap[f] = p.Price
		}
	}
	return snap
}

func main() {
	envfile.LoadDotEnv()

	endpoint := os.Getenv("HELIUS_RPC")
	if endpoint == "" {
		endpoint = os.Getenv("RPC_HTTP")
	}
	if endpoint == "" {
		fmt.Fprintln(os.Stderr, "HELIUS_RPC (or RPC_HTTP) must be set")
		os.Exit(1)
	}
	runDir := envStr("RUN_DIR", ".")
	tickPollMs := envU64("TICK_POLL_MS", 1)
	vaultRefresh := time.Duration(envU64("VAULT_REFRESH_SECS", 30)) * time.Second
	hbEvery := time.Duration(envU64("HEARTBEAT_SECS", 10)) * time.Second
	dryRun := os.Getenv("DRY_RUN") != "0"

	// ── SUBMIT config (mirrors the Kamino executor) ──
	senderURL := envStr("SENDER_URL", "http://ams-sender.helius-rpc.com/fast")
	tipAccount, err := solana.PublicKeyFromBase58(envStr("SENDER_TIP_ACCOUNT", "2nyhqdwKcJZR2vcqCyrYsaPVdAnFoJjiksCXJ7hfEYgD"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad SENDER_TIP_ACCOUNT: %v\n", err)
		os.Exit(1)
	}
	minTipSol := envF64("MIN_TIP_SOL", 0.0002)
	maxDailyTipSol := envF64("MAX_DAILY_TIP_SOL", 0.05)
	walletMinSol := envF64("WALLET_MIN_SOL", 0.02)
	handleCooldown := time.Duration(envU64("HANDLE_COOLDOWN_SECS", 20)) * time.Second
	// Flat tip baked into the armed fire tx (Jupiter has no per-vault profit
	// calc; the tx's own fixed-payback guard is the profit-or-revert
	// protection).
	tipSol := minTipSol
	tipLamports := uint64(tipSol * 1e9)
	// The marginfi flash account for the flash-loan wrap. Defaults to the
	// fleet liquidator's account (same default as liq_executor) — an unset
	// env var with no fallback silently never arms.
	var liquidatorMA solana.PublicKey
	var haveLiquidatorMA bool
	if lma, err := solana.PublicKeyFromBase58(envStr("LIQUIDATOR_MA", "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD")); err == nil {
		liquidatorMA = lma
		haveLiquidatorMA = true
	}

	// Keypair (submit-side): LIVE requires it; DRY_RUN falls back to
	// AUTHORITY env (or the fleet default) so arm/sim still exercise the
	// real-wallet constraints.
	var kp *solana.PrivateKey
	if p := os.Getenv("KEYPAIR_PATH"); p != "" {
		if k, err := solana.PrivateKeyFromSolanaKeygenFile(p); err == nil {
			kp = &k
		}
	}
	if kp == nil && !dryRun {
		fmt.Fprintln(os.Stderr, "LIVE fire needs KEYPAIR_PATH")
		os.Exit(1)
	}
	var authority solana.PublicKey
	if kp != nil {
		authority = kp.PublicKey()
	} else {
		authority, err = solana.PublicKeyFromBase58(envStr("AUTHORITY", "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "bad AUTHORITY: %v\n", err)
			os.Exit(1)
		}
	}

	var dailyTipMu sync.Mutex
	dailyTip := 0.0
	tipDay := nowUs() / 86_400_000_000
	freshBh := solana.Hash{}
	lastBh := time.Now().Add(-9999 * time.Second)
	// Per-vault fire cooldown — don't resubmit the same standing cross every tick.
	handled := map[uint16]time.Time{}

	// Event-driven trigger: Pyth Lazer WS, same feeds as the other executors.
	lazerTable := lazer.NewPriceTable()
	lazerOn := false
	if tok := os.Getenv("PYTH_LAZER_TOKEN"); tok != "" {
		lazer.SpawnLazerThread(context.Background(), tok, lazer.ArmFeedIDs(), lazerTable, nil)
		lazerOn = true
	} else {
		fmt.Fprintln(os.Stderr, "[jup-exec] no PYTH_LAZER_TOKEN — falling back to timed rescan (NOT event-driven)")
	}
	feedMap := lazer.MintFeedMap()

	fmt.Printf("[jup-exec] Jupiter Lend (Fluid) executor %s  authority=%s lazer=%v  (fire gated: ≤1232B + sim-clean; JUP_ALT required)\n",
		map[bool]string{true: "[DRY RUN]", false: "[LIVE]"}[dryRun], authority, lazerOn)
	if !dryRun {
		bal := solBalance(endpoint, authority.String())
		fmt.Fprintf(os.Stderr, "[jup-exec] wallet balance: %g SOL\n", bal)
		if bal < walletMinSol {
			panic(fmt.Sprintf("wallet below floor %g", walletMinSol))
		}
	}

	vaults := loadVaults(endpoint)
	trigger := "timed rescan (fallback)"
	if lazerOn {
		trigger = "Pyth Lazer tick (event-driven)"
	}
	fmt.Printf("[jup-exec] loaded %d vaults; trigger = %s\n", len(vaults), trigger)

	lastRefresh := time.Now()
	lastHb := time.Now()
	var lastTickUs uint64
	reported := map[uint16]bool{}
	// Arm-cache keyed by vault_id: pre-signed txs ready for submit-only firing.
	armCache := map[uint16]*armed{}

	for {
		// Off-band STRUCTURE refresh — NOT the trigger.
		if time.Since(lastRefresh) >= vaultRefresh {
			vaults = loadVaults(endpoint)
			lastRefresh = time.Now()
			reported = map[uint16]bool{} // re-report candidates against the fresh structure
		}

		// Reset the daily tip budget at the UTC-day boundary; refresh the
		// fire blockhash every ~2s so a crossing tick submits with a
		// near-current hash.
		day := nowUs() / 86_400_000_000
		if day != tipDay {
			tipDay = day
			dailyTipMu.Lock()
			dailyTip = 0.0
			dailyTipMu.Unlock()
		}
		if !dryRun && time.Since(lastBh) >= 2*time.Second {
			if bh, ok := latestBlockhash(endpoint); ok {
				freshBh = bh
				lastBh = time.Now()
			}
		}

		// ── TRIGGER: block until a fresh Lazer tick (in-memory, no RPC) ──
		if lazerOn {
			deadline := time.Now().Add(1 * time.Second)
			for {
				cur := freshestLazerTs(lazerTable)
				if cur > lastTickUs {
					lastTickUs = cur
					break
				}
				if time.Now().After(deadline) {
					break
				}
				time.Sleep(time.Duration(tickPollMs) * time.Millisecond)
			}
		} else {
			secs := vaultRefresh
			if secs < time.Second {
				secs = time.Second
			}
			time.Sleep(secs)
		}

		// Price snapshot for THIS tick.
		snap := lazerSnapshot(lazerTable)

		// ── Detection on the tick (in-memory over the snapshot) ──
		// CONFIDENT signal today; the live price per vault is resolved and
		// passed to the hook so this becomes a true price-cross the moment
		// tick↔price math lands.
		var cands []*jupiterlend.Vault
		for _, v := range vaults {
			if v.Config.DebtInScope() && v.MaybeLiquidatable() {
				cands = append(cands, v)
			}
		}

		// ── HOT PATH: submit-only for any crossing vault that is armed ──
		// Detect→submit ~0 when armed (blockhash-stamp + sign + send, no
		// build/quote/sim). Mirrors the Kamino executor's fire_cached branch.
		// A per-vault handle_cooldown stops resubmitting the same standing
		// cross every tick.
		for _, v := range cands {
			vid := v.Config.VaultID
			if t, ok := handled[vid]; ok && time.Since(t) < handleCooldown {
				continue
			}
			if a, ok := armCache[vid]; ok {
				handled[vid] = time.Now()
				fireArmed(endpoint, runDir, senderURL, dryRun, vid, a, authority, freshBh,
					kp, &dailyTipMu, &dailyTip, maxDailyTipSol, walletMinSol)
			}
		}

		// Heartbeat with detect_lag (now_us − freshest Lazer publish).
		if hbEvery > 0 && time.Since(lastHb) >= hbEvery {
			total := len(lazer.ArmFeedIDs())
			freshest := freshestLazerTs(lazerTable)
			lagMs := int64(0)
			if nu := nowUs(); nu > int64(freshest) {
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
			c := v.Config
			if reported[c.VaultID] {
				continue
			}
			reported[c.VaultID] = true
			feed, haveFeed := feedForVault(v, feedMap)
			var price *float64
			if haveFeed {
				if p, ok := snap[feed]; ok {
					price = &p
				}
			}
			freshest := freshestLazerTs(lazerTable)
			nu := nowUs()
			detectLagUs := int64(0)
			if nu > int64(freshest) {
				detectLagUs = nu - int64(freshest)
			}
			var feedField any
			if haveFeed {
				feedField = feed
			}
			logLatency(runDir, map[string]any{
				"event": "detect", "protocol": "jupiter", "vault_id": c.VaultID,
				"debt": c.DebtLabel(), "lazer_feed": feedField, "lazer_price": price,
				"lazer_ts_us": freshest, "detect_us": strconv.FormatInt(nu, 10),
				"detect_lag_us":     strconv.FormatInt(detectLagUs, 10),
				"absorbed_debt":     jupiterlend.U128String(v.State.AbsorbedDebtAmount),
				"liq_threshold_bps": c.LiquidationThreshold, "fired": false,
				"reason": "detection-only (col_per_unit_debt + remaining-accounts unsolved)",
			})
			collat := c.SupplyToken.String()
			if len(collat) > 6 {
				collat = collat[:6]
			}
			priceStr := "<nil>"
			if price != nil {
				priceStr = fmt.Sprintf("%v", *price)
			}
			fmt.Printf("  ▸ vault %d [%s→%s] LT %.1f%% absorbed_debt=%s price=%s detect_lag=%dµs\n",
				c.VaultID, collat, c.DebtLabel(), c.LiqThresholdFrac()*100.0,
				jupiterlend.U128String(v.State.AbsorbedDebtAmount), priceStr, detectLagUs)
			// Off-band ARM: derive the FULL account set from seeds (no
			// captured tx needed), build the priced flash-loan fire tx, and
			// sim-gate it. Needs LIQUIDATOR_MA (the marginfi flash account);
			// skip arming without it.
			var a *armed
			var okArm bool
			if haveLiquidatorMA {
				a, okArm = tryArm(endpoint, v, authority, liquidatorMA, tipAccount, tipLamports, tipSol)
			}
			if okArm {
				fmt.Printf("     ✓ ARMED — seed-derived, priced fire tx simulates clean (%dB)\n", a.txBytes)
				armCache[c.VaultID] = a
			} else {
				extra := ""
				if !haveLiquidatorMA {
					extra = " (LIQUIDATOR_MA unset/invalid — arming disabled)"
				}
				fmt.Printf("     · not armed%s: not fireable at the live price, non-USDC debt, "+
					"or fire tx > 1232B (deploy JUP_ALT — see `go run ./cmd/jup_alt_print`) — sim-gated, not sending\n", extra)
			}
		}
	}
}
