// FAST marginfi liquidation executor — the streaming rewrite.
//
// The polling executor reacts in ~150ms because it re-fetches account state
// (getMultipleAccounts ~40ms) and sim-gates (up to 5×45ms) on the hot path.
// This one removes BOTH, and pre-builds the fire so the hot path is
// sign-and-send only:
//
//   - STATE is streamed. A Yellowstone gRPC (Triton Dragon's Mouth)
//     subscription to the watch-set accounts + banks + oracles keeps the loan
//     book in RAM — no hot-path fetch.
//   - PRICES: streamed on-chain oracles (fresh-gated per bank, stale dropped
//     like the chain does) blended with Pyth Lazer (the ms trigger).
//   - PRE-ARM: a background goroutine continuously builds+caches a fire tx
//     for the handful of accounts closest to crossing (the expensive
//     direct-DEX quote + compile happens OFF the hot path).
//   - HOT PATH: on a Lazer tick, recompute health for the full trigger index
//     (in-RAM, binary search) and, on a cross, refresh the cached tx's
//     blockhash, sign, and send with NO sim. Decision → submit ≈ 1ms;
//     profit-or-revert is the safety.
//
// Usage: HELIUS_RPC=<url> GRPC_ENDPOINT=<triton-url> GRPC_X_TOKEN=<tok>
//
//	PYTH_LAZER_TOKEN=<tok> [KEYPAIR_PATH=…] [DRY_RUN=1] [MIN_PROFIT_USD=0.02]
//	[ARM_MAX=10] [RUN_DIR=runs/stream] go run ./cmd/liq_stream_executor
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gagliardetto/solana-go"
	ys "github.com/rpcpool/yellowstone-grpc/examples/golang/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"solana-arb-backtest-go/internal/envfile"
	"solana-arb-backtest-go/internal/jito"
	"solana-arb-backtest-go/internal/lazer"
	"solana-arb-backtest-go/internal/liquidation"
	"solana-arb-backtest-go/internal/pyth"
)

const (
	marginfiProgram   = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA"
	marginfiGroup     = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8"
	solMint           = "So11111111111111111111111111111111111111112"
	usdcMint          = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	usdtMint          = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"
	defaultLiquidator = "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD"
)

func nowSec() uint64 { return uint64(time.Now().Unix()) }
func nowUs() int64   { return time.Now().UnixMicro() }

func isDebtMint(m solana.PublicKey) bool {
	s := m.String()
	return s == usdcMint || s == usdtMint || s == solMint
}

// ── tiny JSON-RPC helper (off the hot path: startup scan + periodic rescan) ──

var httpClient = &http.Client{Timeout: 15 * time.Second}

func rpcCall(endpoint string, body map[string]any) (map[string]any, bool) {
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
		time.Sleep(time.Duration(300<<attempt) * time.Millisecond)
	}
	return nil, false
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func asArray(v any) []any        { a, _ := v.([]any); return a }
func asStr(v any) string         { s, _ := v.(string); return s }

func b64Data(d any) ([]byte, bool) {
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
		v, ok := rpcCall(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getMultipleAccounts",
			"params": []any{strs, map[string]any{"encoding": "base64"}}})
		if !ok {
			continue
		}
		arr := asArray(asMap(v["result"])["value"])
		for j, accv := range arr {
			acc := asMap(accv)
			if acc == nil {
				continue
			}
			if raw, ok := b64Data(acc["data"]); ok {
				out[chunk[j]] = raw
			}
		}
	}
	return out
}

// getOwners batch-resolves each mint's owning token program (SPL Token vs
// Token-2022) in one round-trip per 100. Done ONCE at startup so the
// hot/arm paths never RPC.
func getOwners(endpoint string, keys []solana.PublicKey) map[solana.PublicKey]solana.PublicKey {
	out := map[solana.PublicKey]solana.PublicKey{}
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
		v, ok := rpcCall(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getMultipleAccounts",
			"params": []any{strs, map[string]any{"encoding": "base64"}}})
		if !ok {
			continue
		}
		arr := asArray(asMap(v["result"])["value"])
		for j, accv := range arr {
			acc := asMap(accv)
			if acc == nil {
				continue
			}
			if o, err := solana.PublicKeyFromBase58(asStr(acc["owner"])); err == nil {
				out[chunk[j]] = o
			}
		}
	}
	return out
}

func getSlot(endpoint string) uint64 {
	v, ok := rpcCall(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getSlot",
		"params": []any{map[string]any{"commitment": "processed"}}})
	if !ok {
		return 0
	}
	f, _ := v["result"].(float64)
	return uint64(f)
}

func latestBlockhash(endpoint string) (solana.Hash, bool) {
	v, ok := rpcCall(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getLatestBlockhash",
		"params": []any{map[string]any{"commitment": "finalized"}}})
	if !ok {
		return solana.Hash{}, false
	}
	s := asStr(asMap(asMap(v["result"])["value"])["blockhash"])
	h, err := solana.HashFromBase58(s)
	if err != nil {
		return solana.Hash{}, false
	}
	return h, true
}

// simulateTx simulates a signed tx (verification before real sends). Returns
// (ok, err+log, unitsConsumed).
func simulateTx(endpoint, txB64 string) (bool, string, uint64) {
	v, ok := rpcCall(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "simulateTransaction",
		"params": []any{txB64, map[string]any{"encoding": "base64", "sigVerify": false, "replaceRecentBlockhash": true}}})
	if !ok {
		return false, "rpc call failed", 0
	}
	val := asMap(asMap(v["result"])["value"])
	units, _ := val["unitsConsumed"].(float64)
	errv, hasErr := val["err"]
	if !hasErr || errv == nil {
		return true, "", uint64(units)
	}
	logs := asArray(val["logs"])
	start := 0
	if len(logs) > 3 {
		start = len(logs) - 3
	}
	var lastLines []string
	for _, l := range logs[start:] {
		if s, ok := l.(string); ok {
			lastLines = append(lastLines, s)
		}
	}
	eb, _ := json.Marshal(errv)
	msg := fmt.Sprintf("%s :: %s", string(eb), strings.Join(lastLines, " | "))
	return false, msg, uint64(units)
}

// simCacheable is the pre-arm sim gate: cache a fire only if it simulates
// clean OR fails 6068 (chain says healthy — the obs wiring is correct, it's
// just not liquidatable yet, so it WILL fire cleanly on a cross). Everything
// else (6051 wiring, swap errors, …) is rejected so the hot path never
// blasts a guaranteed-revert tx.
func simCacheable(endpoint string, tx *solana.Transaction) (bool, string) {
	raw, err := tx.MarshalBinary()
	if err != nil {
		return false, err.Error()
	}
	txB64 := base64.StdEncoding.EncodeToString(raw)
	ok, errMsg, _ := simulateTx(endpoint, txB64)
	if ok {
		return true, "ok"
	}
	if strings.Contains(errMsg, "6068") {
		return true, "not-yet(6068)"
	}
	return false, errMsg
}

// ── book scan ──

type acctEntry struct {
	pk solana.PublicKey
	a  *liquidation.MarginfiAccount
}

// scanBook does one getProgramAccounts scan of the marginfi group → every
// borrower (accounts with a liability) + each one's active-bank obs list.
func scanBook(endpoint string) ([]acctEntry, map[solana.PublicKey][]solana.PublicKey) {
	var accts []acctEntry
	obs := map[solana.PublicKey][]solana.PublicKey{}
	resp, ok := rpcCall(endpoint, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "getProgramAccounts",
		"params": []any{marginfiProgram, map[string]any{"encoding": "base64", "dataSlice": map[string]any{"offset": 0, "length": 1736},
			"filters": []any{
				map[string]any{"dataSize": liquidation.MASize},
				map[string]any{"memcmp": map[string]any{"offset": 8, "bytes": marginfiGroup}},
			}}}})
	if !ok {
		return accts, obs
	}
	for _, ev := range asArray(resp["result"]) {
		e := asMap(ev)
		pk, err := solana.PublicKeyFromBase58(asStr(e["pubkey"]))
		if err != nil {
			continue
		}
		raw, ok := b64Data(asMap(e["account"])["data"])
		if !ok {
			continue
		}
		a, ok := liquidation.DecodeMarginfiAccount(raw)
		if !ok {
			continue
		}
		hasLiab := false
		for _, b := range a.Balances {
			if b.LiabilityShares > 0.0 {
				hasLiab = true
				break
			}
		}
		if !hasLiab {
			continue
		}
		obs[pk] = liquidation.ActiveBankPks(raw)
		accts = append(accts, acctEntry{pk, a})
	}
	return accts, obs
}

// ── live loan book — written by the gRPC task, read by the arm/fire loops ──

type liveState struct {
	mu        sync.RWMutex
	accounts  map[solana.PublicKey]*liquidation.MarginfiAccount
	banks     liquidation.BankMap
	oracleOf  map[solana.PublicKey]solana.PublicKey // bank_pk -> oracle_pk
	oracleRaw map[solana.PublicKey][]byte           // oracle_pk -> latest raw bytes
	obsBanks  map[solana.PublicKey][]solana.PublicKey
}

func newLiveState() *liveState {
	return &liveState{
		accounts:  map[solana.PublicKey]*liquidation.MarginfiAccount{},
		banks:     liquidation.BankMap{},
		oracleOf:  map[solana.PublicKey]solana.PublicKey{},
		oracleRaw: map[solana.PublicKey][]byte{},
		obsBanks:  map[solana.PublicKey][]solana.PublicKey{},
	}
}

// freshBase computes the on-chain baseline PriceMap (bank -> USD) — stale
// oracles DROPPED per bank (DecodeOraclePriceFresh), exactly matching the
// chain's staleness gate. Caller must hold at least a read lock... actually
// takes its own RLock for simplicity (called off very-hot paths only).
func (s *liveState) freshBase(slot, defaultStale uint64) liquidation.PriceMap {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := liquidation.PriceMap{}
	for bk, oc := range s.oracleOf {
		maxAge := uint16(0)
		if b, ok := s.banks[bk]; ok {
			maxAge = b.OracleMaxAge
		}
		maxStale := liquidation.MaxStaleSlotsFor(maxAge, defaultStale)
		raw, ok := s.oracleRaw[oc]
		if !ok {
			continue
		}
		usd, ok := liquidation.DecodeOraclePriceFresh(raw, slot, maxStale)
		if !ok {
			continue
		}
		out[bk] = usd
	}
	return out
}

// cachedFire is a pre-built fire kept hot for an armed account. Blockhash is
// refreshed at fire time, so only the swap quote ages — hence rebuild.
type cachedFire struct {
	tx        *solana.Transaction
	seize     uint64
	quotedOut uint64
	built     time.Time
	assetBank solana.PublicKey
	crank     bool
}

// ── env helpers ──

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

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		fmt.Fprintf(os.Stderr, "%s must be set\n", name)
		os.Exit(1)
	}
	return v
}

func logLine(runDir, s string) {
	f, err := os.OpenFile(runDir+"/stream.jsonl", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err == nil {
		fmt.Fprintln(f, s)
		f.Close()
	}
	fmt.Fprintf(os.Stderr, "[fire] %s\n", s)
}

func main() {
	envfile.LoadDotEnv()

	endpoint := mustEnv("HELIUS_RPC")
	grpcEp := mustEnv("GRPC_ENDPOINT")
	grpcTok := mustEnv("GRPC_X_TOKEN")
	dryRun := envBool("DRY_RUN", true)
	minCollateral := envF64("MIN_COLLATERAL_USD", 5.0)
	// Fire only when CLEARLY underwater (liab/assets >= 1 + margin), not
	// borderline — aligns our Lazer flag with what Pyth/marginfi actually
	// judge, so fired bundles land instead of reverting on healthy-at-Pyth
	// phantoms.
	underwaterMargin := envF64("MIN_UNDERWATER_MARGIN", 0.01)
	verifyArm := envBool("VERIFY_ARM", true)
	armMax := envInt("ARM_MAX", 10)
	armRebuild := time.Duration(envU64("ARM_REBUILD_SECS", 20)) * time.Second
	quoteGapMs := envU64("QUOTE_GAP_MS", 1200)
	synth := os.Getenv("ARM_SYNTH") != "" // measurement-only: skip the swap quote, cache placeholders
	defaultStale := envU64("MAX_SB_STALE_SLOTS", liquidation.DefaultMaxSBStaleSlots)
	runDir := envStr("RUN_DIR", "runs/stream")
	liquidatorMA, err := solana.PublicKeyFromBase58(envStr("LIQUIDATOR_MA", defaultLiquidator))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad LIQUIDATOR_MA: %v\n", err)
		os.Exit(1)
	}
	slippageBps := uint32(envU64("SLIPPAGE_BPS", 100))
	tipSol := envF64("MIN_TIP_SOL", 0.0002)
	_ = os.MkdirAll(runDir, 0755)

	var kp *solana.PrivateKey
	if p := os.Getenv("KEYPAIR_PATH"); p != "" {
		k, err := solana.PrivateKeyFromSolanaKeygenFile(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "KEYPAIR_PATH: %v\n", err)
			os.Exit(1)
		}
		kp = &k
	}
	if kp == nil && !dryRun {
		fmt.Fprintln(os.Stderr, "LIVE needs KEYPAIR_PATH")
		os.Exit(1)
	}
	authority := solana.MustPublicKeyFromBase58("DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak")
	if kp != nil {
		authority = kp.PublicKey()
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// ── ONE-TIME heavy scan: the FULL book (every borrower) + banks + oracles ──
	// We keep ALL borrowers, not a near-threshold slice — a price crash
	// liquidates accounts that were HEALTHY at startup, so a static watch-set
	// is blind to them.
	fmt.Fprintln(os.Stderr, "[stream-exec] initial scan (one getProgramAccounts) …")
	slot0 := getSlot(endpoint)
	accts, obsBanksMap := scanBook(endpoint)

	bankSet := map[solana.PublicKey]bool{}
	for _, ae := range accts {
		for _, b := range ae.a.Balances {
			bankSet[b.BankPk] = true
		}
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
	oracleSet := map[solana.PublicKey]bool{}
	for _, oc := range oracleOf {
		oracleSet[oc] = true
	}
	oraclePks := make([]solana.PublicKey, 0, len(oracleSet))
	for pk := range oracleSet {
		oraclePks = append(oraclePks, pk)
	}
	oracleRaw := getMultiple(endpoint, oraclePks)

	// Pre-resolve every bank mint's token program ONCE (SPL vs Token-2022) so
	// the arm/hot paths build candidates with zero RPC.
	mintSet := map[solana.PublicKey]bool{}
	for _, bk := range banks {
		mintSet[bk.Mint] = true
	}
	allMints := make([]solana.PublicKey, 0, len(mintSet))
	for m := range mintSet {
		allMints = append(allMints, m)
	}
	mintTP := getOwners(endpoint, allMints)
	fmt.Fprintf(os.Stderr, "[stream-exec] resolved %d mint token-programs\n", len(mintTP))

	// Crank metadata: banks whose oracle is a crankable Pyth shard-0
	// sponsored feed, + each bank's feed id. Lets us POST a fresh price then
	// liquidate atomically — the edge for accounts underwater at the true
	// price but healthy at the stale on-chain price (they'd otherwise 6068).
	feedOf := map[solana.PublicKey][32]byte{}
	crankable := map[solana.PublicKey]bool{}
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
			crankable[bank] = true
		}
	}
	fmt.Fprintf(os.Stderr, "[stream-exec] %d crankable banks (Pyth sponsored feeds)\n", len(crankable))

	state := newLiveState()
	state.mu.Lock()
	for _, ae := range accts {
		state.accounts[ae.pk] = ae.a
	}
	state.banks = banks
	state.oracleOf = oracleOf
	state.oracleRaw = oracleRaw
	state.obsBanks = obsBanksMap
	state.mu.Unlock()

	nBook := len(accts)
	fmt.Fprintf(os.Stderr, "[stream-exec] FULL BOOK: %d borrowers, %d banks, %d oracles @ slot %d\n",
		nBook, len(banks), len(oraclePks), slot0)

	// ── gRPC subscription: banks + oracles ONLY (fresh prices). Account
	// STATE also flows here (bank/MA discriminator dispatch), and DEX pool
	// accounts stream too — pool state in RAM, no ~45ms RPC on the fire path.
	// The periodic re-scan below refills new borrowers / balance drift. ──
	poolAddrs := liquidation.DEXPoolAddresses()
	poolSet := map[solana.PublicKey]bool{}
	for _, p := range poolAddrs {
		poolSet[p] = true
	}
	sub := make([]string, 0, len(bankPks)+len(oraclePks)+len(poolAddrs))
	for _, p := range bankPks {
		sub = append(sub, p.String())
	}
	for _, p := range oraclePks {
		sub = append(sub, p.String())
	}
	for _, p := range poolAddrs {
		sub = append(sub, p.String())
	}
	fmt.Fprintf(os.Stderr, "[stream-exec] streaming %d DEX pools (fire-path RPC eliminated)\n", len(poolSet))

	go func() {
		for {
			if ctx.Err() != nil {
				return
			}
			if err := runStream(ctx, grpcEp, grpcTok, sub, oracleSet, poolSet, state); err != nil {
				fmt.Fprintf(os.Stderr, "[stream-exec] gRPC dropped (%v); reconnecting in 2s\n", err)
				select {
				case <-time.After(2 * time.Second):
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	// ── Re-scan goroutine: refresh the FULL book's account state
	// periodically so we stay current with new borrowers / balance changes
	// (a crash moves prices, not balances, so this cadence is fine). ──
	rescanSecs := envU64("RESCAN_SECS", 90)
	armDebug := os.Getenv("ARM_DEBUG") != ""
	go func() {
		for {
			select {
			case <-time.After(time.Duration(rescanSecs) * time.Second):
			case <-ctx.Done():
				return
			}
			accts, obs := scanBook(endpoint)
			if len(accts) == 0 {
				continue
			}
			state.mu.Lock()
			newAccounts := make(map[solana.PublicKey]*liquidation.MarginfiAccount, len(accts))
			for _, ae := range accts {
				newAccounts[ae.pk] = ae.a
			}
			state.accounts = newAccounts
			state.obsBanks = obs
			state.mu.Unlock()
			if armDebug {
				fmt.Fprintf(os.Stderr, "[rescan] refreshed full book: %d borrowers\n", len(accts))
			}
		}
	}()

	// ── Pyth Lazer prices (fast trigger) ──
	lazerTable := lazer.NewPriceTable()
	lazerMap := lazer.MintFeedMap()
	armFeeds := lazer.ArmFeedIDs()
	if tok := os.Getenv("PYTH_LAZER_TOKEN"); tok != "" {
		lazer.SpawnLazerThread(ctx, tok, armFeeds, lazerTable, nil)
		fmt.Fprintln(os.Stderr, "[stream-exec] Pyth Lazer trigger ENABLED")
	}

	// ── current slot + blockhash hot (light RPC, off hot path) ──
	var curSlot atomic.Uint64
	curSlot.Store(slot0)
	var bhMu sync.RWMutex
	bh, _ := latestBlockhash(endpoint)
	go func() {
		for {
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return
			}
			if s := getSlot(endpoint); s > 0 {
				curSlot.Store(s)
			}
			if h, ok := latestBlockhash(endpoint); ok {
				bhMu.Lock()
				bh = h
				bhMu.Unlock()
			}
		}
	}()
	getBH := func() solana.Hash { bhMu.RLock(); defer bhMu.RUnlock(); return bh }

	// Two tip destinations: crank fires go out as a Jito BUNDLE (tip -> Jito
	// tip account); plain Sender fires tip a Helius wallet (the /fast
	// endpoint requires it).
	blockEngine := jito.DefaultBlockEngine()
	var jitoTip *solana.PublicKey
	if tips, err := jito.GetTipAccounts(blockEngine); err == nil && len(tips) > 0 {
		jitoTip = &tips[0]
	}
	heliusTipPk := solana.MustPublicKeyFromBase58(envStr("SENDER_TIP_ACCOUNT", "2nyhqdwKcJZR2vcqCyrYsaPVdAnFoJjiksCXJ7hfEYgD"))
	heliusTip := &heliusTipPk
	crankOn := envBool("CRANK", true)
	maxBlobAge := time.Duration(envU64("MAX_BLOB_MS", 2000)) * time.Millisecond

	// Hermes cache: keep fresh signed price blobs hot for every crankable feed.
	hermesURL := envStr("HERMES", "https://hermes.pyth.network")
	feedHexSet := map[string]bool{}
	for b := range crankable {
		if fid, ok := feedOf[b]; ok {
			feedHexSet[hex.EncodeToString(fid[:])] = true
		}
	}
	feedHex := make([]string, 0, len(feedHexSet))
	for h := range feedHexSet {
		feedHex = append(feedHex, h)
	}
	poll := os.Getenv("HERMES_POLL") != ""
	var hermes *pyth.HermesCache
	if poll {
		hermes = pyth.SpawnHermesCache(hermesURL, nil, 400*time.Millisecond)
		hermes.SetFeeds(feedHex)
	} else {
		hermes = pyth.SpawnHermesStream(hermesURL, feedHex)
	}
	fmt.Fprintf(os.Stderr, "[stream-exec] Hermes %s tracking %d crankable feeds%s\n",
		map[bool]string{true: "poll(400ms)", false: "STREAM(SSE)"}[poll], len(feedHex),
		map[bool]string{true: "", false: " (CRANK disabled)"}[crankOn])

	simOnly := os.Getenv("SIM_ONLY") != "" // verify the fire simulates before real sends
	senderURL := envStr("SENDER_URL", "http://ams-sender.helius-rpc.com/fast")

	var cacheMu sync.RWMutex
	cache := map[solana.PublicKey]*cachedFire{}

	// Trigger index: per dominant-collateral BANK, accounts sorted DESC by
	// the collateral price at which they become liquidatable. The hot path
	// binary-searches this against the live price — O(log n) full-book
	// detection, no per-tick recompute.
	var trigMu sync.RWMutex
	triggers := map[solana.PublicKey][]triggerEntry{}

	armBand := envF64("ARM_BAND", 0.03)
	armBandMax := envF64("ARM_BAND_MAX", 0.15)
	volGain := envF64("ARM_BAND_VOL_GAIN", 3.0)

	// ── TRIGGER-INDEX + PRE-ARM goroutine: over the FULL book, compute each
	// account's liquidation trigger price (2-eval perturbation), build the
	// sorted index, and pre-build fire txs for the arm-band (accounts within
	// ARM_BAND of crossing). OFF the hot path. ──
	go func() {
		prevPrices := map[solana.PublicKey]float64{}
		usdc := solana.MustPublicKeyFromBase58(usdcMint)
		triggerSecs := envU64("TRIGGER_SECS", 2)
		for {
			if ctx.Err() != nil {
				return
			}
			slot := curSlot.Load()

			idx, ranked, nBookNow, nNow, snap, dynBand, vol := computeTriggersAndArm(
				state, slot, lazerTable, lazerMap, defaultStale, minCollateral, underwaterMargin,
				prevPrices, armBand, armBandMax, volGain)
			prevPrices = snap
			nTrig := 0
			for _, v := range idx {
				nTrig += len(v)
			}
			trigMu.Lock()
			triggers = idx
			trigMu.Unlock()

			// Pre-arm the candidates: dedupe direct-DEX by asset, cap, build+sim-gate.
			sort.Slice(ranked, func(i, j int) bool { return ranked[i].ratio > ranked[j].ratio })
			seenAsset := map[solana.PublicKey]bool{}
			filtered := ranked[:0]
			for _, r := range ranked {
				if _, ok := liquidation.DirectDEXPool(r.asset, usdc); ok {
					filtered = append(filtered, r)
					continue
				}
				if !seenAsset[r.asset] {
					seenAsset[r.asset] = true
					filtered = append(filtered, r)
				}
			}
			ranked = filtered
			if len(ranked) > armMax {
				ranked = ranked[:armMax]
			}
			armed := map[solana.PublicKey]bool{}
			for _, r := range ranked {
				armed[r.pk] = true
			}
			cacheMu.Lock()
			for pk := range cache {
				if !armed[pk] {
					delete(cache, pk)
				}
			}
			cacheMu.Unlock()

			curBH := getBH()
			var noCand, buildErr, builtOK uint32
			lastErr := ""
			for _, r := range ranked {
				cacheMu.RLock()
				cf, has := cache[r.pk]
				cacheMu.RUnlock()
				stale := !has || time.Since(cf.built) > armRebuild
				if !stale {
					continue
				}
				state.mu.RLock()
				a := state.accounts[r.pk]
				ob := append([]solana.PublicKey{}, state.obsBanks[r.pk]...)
				banksSnapshot := state.banks
				oracleOfSnapshot := state.oracleOf
				state.mu.RUnlock()
				if a == nil {
					continue
				}
				cand := buildCandidate(a, r.pk, banksSnapshot, oracleOfSnapshot, mintTP, ob)
				if cand == nil {
					noCand++
					continue
				}
				isCrank := crankOn && crankable[cand.AssetBank]
				tip := heliusTip
				if isCrank {
					tip = jitoTip
				}
				if synth {
					builtOK++
					cacheMu.Lock()
					cache[r.pk] = &cachedFire{tx: &solana.Transaction{}, seize: cand.AssetAmount, quotedOut: 0,
						built: time.Now(), assetBank: cand.AssetBank, crank: isCrank}
					cacheMu.Unlock()
					continue
				}
				f, err := liquidation.BuildFireTx(endpoint, cand, liquidatorMA, authority, tip,
					uint64(tipSol*1e9), 100_000, slippageBps, 20, curBH)
				if err != nil {
					buildErr++
					lastErr = err.Error()
					time.Sleep(time.Duration(quoteGapMs) * time.Millisecond)
					continue
				}
				cacheable, why := true, "unverified"
				if verifyArm {
					cacheable, why = simCacheable(endpoint, f.Tx)
				}
				if cacheable {
					builtOK++
					cacheMu.Lock()
					cache[r.pk] = &cachedFire{tx: f.Tx, seize: cand.AssetAmount, quotedOut: f.QuotedUSDCOut,
						built: time.Now(), assetBank: cand.AssetBank, crank: isCrank}
					cacheMu.Unlock()
				} else {
					buildErr++
					pks := r.pk.String()
					if len(pks) > 8 {
						pks = pks[:8]
					}
					lastErr = fmt.Sprintf("sim reject %s: %s", pks, why)
				}
				// Space out swap quotes to stay under any rate limit.
				time.Sleep(time.Duration(quoteGapMs) * time.Millisecond)
			}
			if armDebug {
				cacheMu.RLock()
				cn := len(cache)
				cacheMu.RUnlock()
				extra := ""
				if lastErr != "" {
					s := lastErr
					if len(s) > 90 {
						s = s[:90]
					}
					extra = " | last_err: " + s
				}
				fmt.Fprintf(os.Stderr, "[arm] book %d now-liq %d triggers %d vol %.3f%% band %.1f%% -> armed %d cache %d | no_cand %d build_err %d built_ok %d%s\n",
					nBookNow, nNow, nTrig, vol*100.0, dynBand*100.0, len(ranked), cn, noCand, buildErr, builtOK, extra)
			}
			select {
			case <-time.After(time.Duration(triggerSecs) * time.Second):
			case <-ctx.Done():
				return
			}
		}
	}()

	dryTag := "[LIVE]"
	if dryRun {
		dryTag = "[DRY RUN]"
	}
	fmt.Fprintf(os.Stderr, "[stream-exec] marginfi FAST executor %s authority=%s full-book=%d arm_max=%d arm_band=%g\n",
		dryTag, authority, nBook, armMax, armBand)

	// ── HOT LOOP: Lazer tick -> health over trigger index (µs) -> fire
	// cached (~1ms) ──
	var lastTickUs uint64
	handled := map[solana.PublicKey]time.Time{}
	handleCD := time.Duration(envU64("HANDLE_COOLDOWN_SECS", 15)) * time.Second
	var decideSamples []float64
	lastHB := time.Now()

	// Fire ASYNC in bounded worker goroutines so a slow submit (Jito
	// rate-limit, up to 5s) NEVER blocks detection, and load is capped.
	// Excess crossings this tick are dropped (re-detected next tick).
	var inFlight atomic.Int64
	maxInflight := int64(envU64("MAX_INFLIGHT", 6))

	for ctx.Err() == nil {
		// Block until a fresh Lazer tick (in-memory poll).
		deadline := time.Now().Add(500 * time.Millisecond)
		for {
			var cur uint64
			for _, fid := range armFeeds {
				if p, ok := lazerTable.Get(fid); ok && p.TsUs > cur {
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
			time.Sleep(100 * time.Microsecond)
		}
		tTick := nowUs()

		// FULL-BOOK detection: binary-search the per-bank trigger index
		// against the live blended price. O(log n) per moved asset — covers
		// EVERY account, not just a pre-armed set.
		slot := curSlot.Load()
		base := state.freshBase(slot, defaultStale)
		state.mu.RLock()
		prices, _ := lazer.Blend(state.banks, base, lazerTable, lazerMap)
		banksSnap := state.banks
		state.mu.RUnlock()

		trigMu.RLock()
		trigSnap := triggers
		trigMu.RUnlock()
		nTrig := 0
		for _, v := range trigSnap {
			nTrig += len(v)
		}

		var crossed []solana.PublicKey
		for bank, list := range trigSnap {
			p, ok := prices[bank]
			if !ok {
				continue
			}
			// list sorted DESC by trigger price: all entries with trigger >= p have crossed.
			k := sort.Search(len(list), func(i int) bool { return list[i].trigger < p })
			for _, e := range list[:k] {
				if t, ok := handled[e.pk]; ok && time.Since(t) < handleCD {
					continue
				}
				state.mu.RLock()
				a := state.accounts[e.pk]
				state.mu.RUnlock()
				if a == nil {
					continue
				}
				h := liquidation.MaintenanceHealth(a, banksSnap, prices)
				if h.Missing == 0 && h.Health.Ratio() >= 1.0+underwaterMargin && h.Health.WeightedAssets >= minCollateral {
					crossed = append(crossed, e.pk)
				}
			}
		}
		// ALSO fire any pre-armed account that is liquidatable RIGHT NOW —
		// these were already-underwater at trigger-compute time so they
		// aren't in the trigger index (which only holds not-yet-crossed
		// accounts). Small set.
		cacheMu.RLock()
		cachedPks := make([]solana.PublicKey, 0, len(cache))
		for pk := range cache {
			cachedPks = append(cachedPks, pk)
		}
		cacheMu.RUnlock()
		for _, pk := range cachedPks {
			if t, ok := handled[pk]; ok && time.Since(t) < handleCD {
				continue
			}
			state.mu.RLock()
			a := state.accounts[pk]
			state.mu.RUnlock()
			if a == nil {
				continue
			}
			h := liquidation.MaintenanceHealth(a, banksSnap, prices)
			if h.Missing == 0 && h.Health.Ratio() >= 1.0+underwaterMargin && h.Health.WeightedAssets >= minCollateral {
				crossed = append(crossed, pk)
			}
		}
		crossed = dedupePks(crossed)

		// Hot-path decision latency: tick -> full-book crossing verdict.
		decideUs := float64(nowUs() - tTick)
		decideSamples = append(decideSamples, decideUs/1000.0)
		if time.Since(lastHB) > 5*time.Second && len(decideSamples) > 0 {
			sort.Float64s(decideSamples)
			med := decideSamples[len(decideSamples)/2]
			p90i := len(decideSamples) * 9 / 10
			if p90i >= len(decideSamples) {
				p90i = len(decideSamples) - 1
			}
			p90 := decideSamples[p90i]
			cacheMu.RLock()
			cn := len(cache)
			cacheMu.RUnlock()
			fmt.Fprintf(os.Stderr, "[hb] triggers %d cache %d | hot-path decide: median %.3fms p90 %.3fms (n=%d) | crossed %d\n",
				nTrig, cn, med, p90, len(decideSamples), len(crossed))
			decideSamples = decideSamples[:0]
			lastHB = time.Now()
		}

		for _, pk := range crossed {
			if inFlight.Load() >= maxInflight {
				break // cap load; rest re-detected next tick
			}
			handled[pk] = time.Now()
			freshBH := getBH()
			inFlight.Add(1)
			pk := pk
			go func() {
				defer inFlight.Add(-1)
				fireOne(fireCtx{
					pk: pk, tTick: tTick, state: state, cache: &cache, cacheMu: &cacheMu,
					mintTP: mintTP, endpoint: endpoint, runDir: runDir, blockEngine: blockEngine,
					senderURL: senderURL, crankable: crankable, feedOf: feedOf, hermes: hermes,
					liquidatorMA: liquidatorMA, authority: authority, tipSol: tipSol, slippageBps: slippageBps,
					crankOn: crankOn, jitoTip: jitoTip, heliusTip: heliusTip, freshBH: freshBH,
					kp: kp, dryRun: dryRun, simOnly: simOnly, maxBlobAge: maxBlobAge,
					underwaterMargin: underwaterMargin, defaultStale: defaultStale, slot: slot,
				})
			}()
		}
	}
}

// triggerEntry is one account's liquidation trigger price under a
// dominant-collateral bank.
type triggerEntry struct {
	trigger float64
	pk      solana.PublicKey
}

// rankedEntry pairs a to-be-armed account with its current health ratio
// and dominant collateral mint.
type rankedEntry struct {
	pk    solana.PublicKey
	ratio float64
	asset solana.PublicKey
}

// computeTriggersAndArm mirrors the Rust trigger-index + arm-candidate pass:
// for every book account, find the dominant collateral bank, compute a
// 2-eval linear trigger price via price perturbation, and collect accounts
// within the (volatility-widened) arm band.
func computeTriggersAndArm(
	state *liveState, slot uint64, lazerTable *lazer.PriceTable, lazerMap map[solana.PublicKey]uint32, defaultStale uint64,
	minCollateral, underwaterMargin float64, prevPrices map[solana.PublicKey]float64,
	armBand, armBandMax, volGain float64,
) (idx map[solana.PublicKey][]triggerEntry, ranked []rankedEntry, nBook, nNow int, snap map[solana.PublicKey]float64, dynBand, vol float64) {
	state.mu.RLock()
	defer state.mu.RUnlock()

	// freshBase inlined here (avoid recursive RLock): duplicate the logic
	// against the already-held read lock.
	base := liquidation.PriceMap{}
	for bk, oc := range state.oracleOf {
		maxAge := uint16(0)
		if b, ok := state.banks[bk]; ok {
			maxAge = b.OracleMaxAge
		}
		maxStale := liquidation.MaxStaleSlotsFor(maxAge, defaultStale)
		raw, ok := state.oracleRaw[oc]
		if !ok {
			continue
		}
		usd, ok := liquidation.DecodeOraclePriceFresh(raw, slot, maxStale)
		if !ok {
			continue
		}
		base[bk] = usd
	}

	m, _ := lazer.Blend(state.banks, base, lazerTable, lazerMap)

	// Recent volatility = max |Δprice|/price since last pass -> widen the arm band.
	for b, pp := range prevPrices {
		if pp <= 0 {
			continue
		}
		if cp, ok := m[b]; ok {
			d := (cp - pp) / pp
			if d < 0 {
				d = -d
			}
			if d > vol {
				vol = d
			}
		}
	}
	dynBand = armBand + volGain*vol
	if dynBand > armBandMax {
		dynBand = armBandMax
	}

	idx = map[solana.PublicKey][]triggerEntry{}
	for pk, a := range state.accounts {
		nBook++
		// Dominant collateral bank = the balance with the largest USD value.
		var domBank solana.PublicKey
		domVal := 0.0
		haveDom := false
		for _, bal := range a.Balances {
			if bal.AssetShares <= 0 {
				continue
			}
			bk, ok := state.banks[bal.BankPk]
			if !ok {
				continue
			}
			p, ok := m[bal.BankPk]
			if !ok {
				continue
			}
			v := bal.AssetShares * bk.AssetShareValue * p
			if !haveDom || v > domVal {
				domBank, domVal, haveDom = bal.BankPk, v, true
			}
		}
		if !haveDom {
			continue
		}
		domBk, ok := state.banks[domBank]
		if !ok {
			continue
		}
		domMint := domBk.Mint

		h0 := liquidation.MaintenanceHealth(a, state.banks, m)
		if h0.Missing != 0 || h0.Health.WeightedAssets < minCollateral {
			continue
		}
		if h0.Health.Ratio() >= 1.0+underwaterMargin {
			nNow++
			ranked = append(ranked, rankedEntry{pk, h0.Health.Ratio(), domMint})
			continue
		}
		p0, ok := m[domBank]
		if !ok {
			continue
		}
		m[domBank] = p0 * 0.9 // perturb dominant collateral price
		h1 := liquidation.MaintenanceHealth(a, state.banks, m)
		m[domBank] = p0 // restore
		slope := (h1.Health.WeightedAssets - h0.Health.WeightedAssets) / (p0*0.9 - p0)
		if slope <= 0 {
			continue
		}
		trigger := p0 + (h0.Health.WeightedLiabilities-h0.Health.WeightedAssets)/slope
		if !isFinite(trigger) || trigger <= 0 || trigger >= p0 {
			continue
		}
		idx[domBank] = append(idx[domBank], triggerEntry{trigger, pk})
		if trigger >= p0*(1.0-dynBand) {
			ranked = append(ranked, rankedEntry{pk, h0.Health.Ratio(), domMint})
		}
	}
	for b, list := range idx {
		l := list
		sort.Slice(l, func(i, j int) bool { return l[i].trigger > l[j].trigger })
		idx[b] = l
	}
	snap = m
	return
}

func isFinite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func dedupePks(pks []solana.PublicKey) []solana.PublicKey {
	if len(pks) == 0 {
		return pks
	}
	sort.Slice(pks, func(i, j int) bool { return bytes.Compare(pks[i][:], pks[j][:]) < 0 })
	out := pks[:1]
	for _, p := range pks[1:] {
		if !p.Equals(out[len(out)-1]) {
			out = append(out, p)
		}
	}
	return out
}

// buildCandidate builds a FireCandidate from LIVE state (no fetch): largest
// collateral × a wired-debt leg, full observation list.
func buildCandidate(a *liquidation.MarginfiAccount, pk solana.PublicKey, banks liquidation.BankMap,
	oracleOf map[solana.PublicKey]solana.PublicKey, mintTP map[solana.PublicKey]solana.PublicKey,
	obsBanks []solana.PublicKey) *liquidation.FireCandidate {

	var asset *liquidation.Balance
	for i := range a.Balances {
		b := &a.Balances[i]
		if b.AssetShares <= 0 {
			continue
		}
		if asset == nil || b.AssetShares > asset.AssetShares {
			asset = b
		}
	}
	if asset == nil {
		return nil
	}
	var debt *liquidation.Balance
	for i := range a.Balances {
		b := &a.Balances[i]
		if b.LiabilityShares <= 0 {
			continue
		}
		bk, ok := banks[b.BankPk]
		if !ok || !isDebtMint(bk.Mint) {
			continue
		}
		debt = b
		break
	}
	if debt == nil {
		return nil
	}
	abk, ok := banks[asset.BankPk]
	if !ok {
		return nil
	}
	lbk, ok := banks[debt.BankPk]
	if !ok {
		return nil
	}
	native := asset.AssetShares * abk.AssetShareValue
	seize := uint64(native * 0.5)
	if seize == 0 {
		return nil
	}
	assetTP, ok := mintTP[abk.Mint]
	if !ok {
		return nil
	}
	debtTP, ok := mintTP[lbk.Mint]
	if !ok {
		return nil
	}
	// Observation list covers ALL active-flag banks (incl. zero-share) —
	// marginfi requires an oracle for each or it fails 6051. Falls back to
	// the funded balances if the active-bank list is somehow empty.
	bankList := obsBanks
	if len(bankList) == 0 {
		bankList = make([]solana.PublicKey, len(a.Balances))
		for i, b := range a.Balances {
			bankList[i] = b.BankPk
		}
	}
	var obs solana.AccountMetaSlice
	for _, bankPk := range bankList {
		oc, ok := oracleOf[bankPk]
		if !ok {
			return nil
		}
		obs = append(obs, solana.Meta(bankPk), solana.Meta(oc))
	}
	assetOracle, ok := oracleOf[asset.BankPk]
	if !ok {
		return nil
	}
	liabOracle, ok := oracleOf[debt.BankPk]
	if !ok {
		return nil
	}
	return &liquidation.FireCandidate{
		Liquidatee: pk, AssetBank: asset.BankPk, AssetMint: abk.Mint, AssetTokenProgram: assetTP,
		AssetAmount: seize, LiabBank: debt.BankPk, DebtMint: lbk.Mint, DebtTokenProgram: debtTP,
		AssetOracle: assetOracle, LiabOracle: liabOracle, LiquidateeObs: obs,
	}
}

// fireCtx bundles everything one hot-path fire goroutine needs.
type fireCtx struct {
	pk               solana.PublicKey
	tTick            int64
	state            *liveState
	cache            *map[solana.PublicKey]*cachedFire
	cacheMu          *sync.RWMutex
	mintTP           map[solana.PublicKey]solana.PublicKey
	endpoint         string
	runDir           string
	blockEngine      string
	senderURL        string
	crankable        map[solana.PublicKey]bool
	feedOf           map[solana.PublicKey][32]byte
	hermes           *pyth.HermesCache
	liquidatorMA     solana.PublicKey
	authority        solana.PublicKey
	tipSol           float64
	slippageBps      uint32
	crankOn          bool
	jitoTip          *solana.PublicKey
	heliusTip        *solana.PublicKey
	freshBH          solana.Hash
	kp               *solana.PrivateKey
	dryRun           bool
	simOnly          bool
	maxBlobAge       time.Duration
	underwaterMargin float64
	defaultStale     uint64
	slot             uint64
}

// fireOne is the per-crossing worker: reuse the pre-armed cached tx if
// present, else build fresh; then sign+submit (or dry-run log).
func fireOne(c fireCtx) {
	c.cacheMu.RLock()
	cf, ok := (*c.cache)[c.pk]
	c.cacheMu.RUnlock()

	var tx *solana.Transaction
	var seize, quotedOut uint64
	var isCrank bool
	var assetBank solana.PublicKey

	if ok {
		tx, seize, quotedOut, isCrank, assetBank = cf.tx, cf.seize, cf.quotedOut, cf.crank, cf.assetBank
	} else {
		c.state.mu.RLock()
		a := c.state.accounts[c.pk]
		ob := append([]solana.PublicKey{}, c.state.obsBanks[c.pk]...)
		banksSnap := c.state.banks
		oracleOfSnap := c.state.oracleOf
		c.state.mu.RUnlock()
		if a == nil {
			return
		}
		cand := buildCandidate(a, c.pk, banksSnap, oracleOfSnap, c.mintTP, ob)
		if cand == nil {
			return
		}
		ic := c.crankOn && c.crankable[cand.AssetBank]
		tip := c.heliusTip
		if ic {
			tip = c.jitoTip
		}
		f, err := liquidation.BuildFireTx(c.endpoint, cand, c.liquidatorMA, c.authority, tip,
			uint64(c.tipSol*1e9), 100_000, c.slippageBps, 20, c.freshBH)
		if err != nil {
			return
		}
		tx, seize, quotedOut, isCrank, assetBank = f.Tx, cand.AssetAmount, f.QuotedUSDCOut, ic, cand.AssetBank
	}

	decideMs := float64(nowUs()-c.tTick) / 1000.0
	if c.dryRun {
		logLine(c.runDir, mustJSON(map[string]any{
			"t": nowSec(), "liquidatee": c.pk.String(), "seize": seize,
			"mode": modeStr(isCrank), "quoted_out": quotedOut, "decide_ms": decideMs, "dry_run": true,
		}))
		return
	}
	if c.kp == nil {
		return
	}
	tx.Message.RecentBlockhash = c.freshBH
	if _, err := tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(c.authority) {
			return c.kp
		}
		return nil
	}); err != nil {
		return
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		return
	}
	liqB64 := base64.StdEncoding.EncodeToString(raw)
	sig := ""
	if len(tx.Signatures) > 0 {
		sig = tx.Signatures[0].String()
	}

	if c.simOnly {
		ok, errMsg, units := simulateTx(c.endpoint, liqB64)
		logLine(c.runDir, mustJSON(map[string]any{
			"t": nowSec(), "liquidatee": c.pk.String(), "seize": seize, "mode": modeStr(isCrank),
			"quoted_out": quotedOut, "SIM_ONLY": true, "sim_ok": ok, "units": units, "sim_err": errMsg,
		}))
		return
	}

	if isCrank {
		fireCrank(c, sig, liqB64, seize, quotedOut, assetBank, decideMs)
		return
	}

	res, err := jito.SendSender(c.senderURL, liqB64)
	submitMs := float64(nowUs()-c.tTick) / 1000.0
	logLine(c.runDir, mustJSON(map[string]any{
		"t": nowSec(), "liquidatee": c.pk.String(), "seize": seize, "mode": "sender",
		"decide_ms": decideMs, "submit_ms": submitMs, "signature": sig, "sent": err == nil,
		"send_err": errString(err), "bundle_or_sig": res, "fired": true,
	}))
}

func fireCrank(c fireCtx, sig, liqB64 string, seize, quotedOut uint64, assetBank solana.PublicKey, decideMs float64) {
	pks := c.pk.String()
	shortPk := pks
	if len(shortPk) > 8 {
		shortPk = shortPk[:8]
	}
	feedID, ok := c.feedOf[assetBank]
	if !ok {
		logLine(c.runDir, fmt.Sprintf("crank skip %s: no feed", shortPk))
		return
	}
	upd, vaa, age, ok := c.hermes.UpdateFor(feedID)
	if !ok {
		logLine(c.runDir, fmt.Sprintf("crank skip %s: no Hermes blob", shortPk))
		return
	}
	if age > c.maxBlobAge {
		logLine(c.runDir, fmt.Sprintf("crank skip %s: blob stale %s", shortPk, age))
		return
	}
	// Judge at the PRICE WE POST. The crank writes this exact Hermes price
	// on-chain, so health at it IS the liquidate outcome at the leader.
	// Firing on the Lazer blend alone sprays phantoms (healthy at Pyth) that
	// get silently dropped.
	if px, ok := upd.PriceUSD(); ok {
		c.state.mu.RLock()
		base := liquidation.PriceMap{}
		for bk, oc := range c.state.oracleOf {
			maxAge := uint16(0)
			if b, ok := c.state.banks[bk]; ok {
				maxAge = b.OracleMaxAge
			}
			maxStale := liquidation.MaxStaleSlotsFor(maxAge, c.defaultStale)
			if raw, ok := c.state.oracleRaw[oc]; ok {
				if usd, ok := liquidation.DecodeOraclePriceFresh(raw, c.slot, maxStale); ok {
					base[bk] = usd
				}
			}
		}
		base[assetBank] = px
		a := c.state.accounts[c.pk]
		banksSnap := c.state.banks
		c.state.mu.RUnlock()
		healthy := false
		if a != nil {
			h := liquidation.MaintenanceHealth(a, banksSnap, base)
			healthy = h.Missing == 0 && h.Health.Ratio() >= 1.0+c.underwaterMargin
		}
		if !healthy {
			logLine(c.runDir, mustJSON(map[string]any{
				"t": nowSec(), "liquidatee": c.pk.String(), "mode": "crank",
				"judge": "healthy_at_hermes", "px": px, "fired": false,
			}))
			return
		}
	}
	ctxs, err := pyth.BuildCrankTxs(c.authority, vaa, []pyth.MerkleUpdate{upd}, 0, 0, c.freshBH)
	if err != nil {
		logLine(c.runDir, fmt.Sprintf("crank build fail %s: %v", shortPk, err))
		return
	}
	if err := ctxs.StampAndSign(*c.kp, c.freshBH); err != nil {
		logLine(c.runDir, fmt.Sprintf("crank sign fail %s: %v", shortPk, err))
		return
	}
	setupB64, crankB64, err := ctxs.ToB64()
	if err != nil {
		logLine(c.runDir, fmt.Sprintf("crank b64 fail: %v", err))
		return
	}
	res, err := jito.SendBundle(c.blockEngine, []string{setupB64, crankB64, liqB64})
	submitMs := float64(nowUs()-c.tTick) / 1000.0
	logLine(c.runDir, mustJSON(map[string]any{
		"t": nowSec(), "liquidatee": c.pk.String(), "seize": seize, "mode": "crank",
		"decide_ms": decideMs, "submit_ms": submitMs, "signature": sig, "bundle": res,
		"sent": err == nil, "send_err": errString(err), "blob_age_ms": age.Milliseconds(), "fired": true,
	}))
	_ = quotedOut
}

func modeStr(isCrank bool) string {
	if isCrank {
		return "crank"
	}
	return "sender"
}
func errString(err error) any {
	if err == nil {
		return nil
	}
	return err.Error()
}
func mustJSON(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// ── Yellowstone gRPC stream: decode each account update into the live maps ──

type xTokenCreds struct{ token string }

func (x xTokenCreds) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{"x-token": x.token}, nil
}
func (x xTokenCreds) RequireTransportSecurity() bool { return true }

// runStream is one gRPC subscription lifecycle. Oracle accounts are stored
// RAW (fresh-decoded at use); MA/Bank are decoded; DEX pool bytes go to the
// shared liquidation pool cache.
func runStream(ctx context.Context, endpoint, xToken string, sub []string,
	oracleSet, poolSet map[solana.PublicKey]bool, state *liveState) error {

	creds := credentials.NewTLS(&tls.Config{})
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds), grpc.WithPerRPCCredentials(xTokenCreds{token: xToken})}
	conn, err := grpc.NewClient(endpoint, opts...)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	client := ys.NewGeyserClient(conn)
	stream, err := client.Subscribe(ctx)
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	commitment := ys.CommitmentLevel_PROCESSED
	req := &ys.SubscribeRequest{
		Accounts: map[string]*ys.SubscribeRequestFilterAccounts{
			"liq": {Account: sub, Owner: []string{}, Filters: []*ys.SubscribeRequestFilterAccountsFilter{}},
		},
		Commitment: &commitment,
	}
	if err := stream.Send(req); err != nil {
		return fmt.Errorf("send subscribe request: %w", err)
	}

	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		acc := msg.GetAccount()
		if acc == nil {
			continue
		}
		info := acc.GetAccount()
		if info == nil {
			continue
		}
		pk := solana.PublicKeyFromBytes(info.GetPubkey())
		data := info.GetData()
		switch {
		case poolSet[pk]:
			liquidation.UpdatePoolCache(pk, data)
		case oracleSet[pk]:
			state.mu.Lock()
			state.oracleRaw[pk] = data
			state.mu.Unlock()
		case len(data) == liquidation.MASize:
			if a, ok := liquidation.DecodeMarginfiAccount(data); ok {
				obs := liquidation.ActiveBankPks(data)
				state.mu.Lock()
				state.accounts[pk] = a
				state.obsBanks[pk] = obs
				state.mu.Unlock()
			}
		default:
			if b, ok := liquidation.DecodeBank(data); ok {
				state.mu.Lock()
				state.banks[pk] = b
				state.mu.Unlock()
			}
		}
	}
}
