// The atomic liquidation FIRE path — one flashloan-wrapped v0 tx:
//
//	[cu_limit, cu_price, create ATAs, start_flashloan,
//	 liquidate → withdraw_all seized collateral → Jupiter swap collateral→USDC
//	 → repay_all liability, end_flashloan, tip]
//
// Profit-or-revert with NO external capital: liquidate moves internal
// shares (liquidator gains asset shares + takes on the matching liability),
// so no tokens are needed up front; repay_all fails unless the swap
// produced enough USDC to cover the full liability, and end_flashloan
// re-checks account health — either the whole tx lands net-positive
// (surplus USDC stays in the wallet ATA) or it reverts and costs nothing
// but the fee on a miss.
//
// v1 restriction: the liability bank must be USDC (the dominant marginfi
// debt asset) — the swap leg targets USDC and payback_usdc closes it.
package liquidation

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/arb"
	"solana-arb-backtest-go/internal/clmm"
	"solana-arb-backtest-go/internal/flashloan"
	"solana-arb-backtest-go/internal/jupswap"
	"solana-arb-backtest-go/internal/marginfi"
	"solana-arb-backtest-go/internal/swap"
)

const FireCULimit uint32 = 1_400_000

// LiqALT is the dedicated ALT holding the 18 accounts common to every
// marginfi-USDC liquidation (see liq_alt_print for the set + recreate
// instructions). Override via LIQ_ALT.
const LiqALT = "DEMhLvSJbSZQfCdiH7YicYNopo3EhhapjfoEjt2kJVij"

// FireCandidate is everything the executor knows about one liquidation
// opportunity.
type FireCandidate struct {
	Liquidatee        solana.PublicKey
	AssetBank         solana.PublicKey
	AssetMint         solana.PublicKey
	AssetTokenProgram solana.PublicKey
	// AssetAmount is the collateral native units to seize (sized by the
	// caller via simulation).
	AssetAmount uint64
	// LiabBank is the liability (debt) bank the liquidator absorbs and must
	// repay. Any of USDC/USDT/wSOL in v1.5.
	LiabBank solana.PublicKey
	// DebtMint/DebtTokenProgram: the debt asset's mint + token program — the
	// swap target and payback token (was hardcoded USDC; now the actual
	// absorbed-liability asset).
	DebtMint         solana.PublicKey
	DebtTokenProgram solana.PublicKey
	AssetOracle      solana.PublicKey
	LiabOracle       solana.PublicKey
	// LiquidateeObs is the liquidatee's observation list: [bank(ro),
	// oracle(ro)] per active balance, in balance order.
	LiquidateeObs solana.AccountMetaSlice
}

// FireTx is the built unsigned fire tx.
type FireTx struct {
	// Tx is unsigned (sign before sending; default signature placeholder).
	Tx *solana.Transaction
	// QuotedUSDCOut is Jupiter's quoted DEBT-asset out (native) for the
	// seized collateral — compare against the absorbed liability to decide
	// whether firing is worth it. (Named historically for USDC; now the
	// debt asset, which may be USDC/USDT/wSOL.)
	QuotedUSDCOut uint64
	TxBytes       int
}

// ── Shared, live pool-state cache. The streaming executor gRPC-subscribes
// the DEX pools and pushes their bytes here, so the fire path reads pool
// state from RAM instead of a ~45ms getAccountInfo — critical for the burst
// "tail" (fires that weren't pre-armed). Empty by default → falls back to
// RPC (polling executor).

var (
	poolCacheMu sync.RWMutex
	poolCache   = map[solana.PublicKey][]byte{}
)

// UpdatePoolCache pushes fresh pool bytes (called from the gRPC stream on
// each pool update).
func UpdatePoolCache(pool solana.PublicKey, data []byte) {
	poolCacheMu.Lock()
	defer poolCacheMu.Unlock()
	poolCache[pool] = data
}

func getPoolCache(pool solana.PublicKey) ([]byte, bool) {
	poolCacheMu.RLock()
	defer poolCacheMu.RUnlock()
	b, ok := poolCache[pool]
	return b, ok
}

// DEXPools is a direct-DEX route for the collateral→debt swap (bypasses
// Jupiter/lite-api, which is rate-limited to death). Orca Whirlpool only
// for now. v1 targets BONK → USDC — the dominant marginfi liquidation (BONK
// = 91% of collateral, USDC = 100% of debt in the census). Override the
// pool via DEX_POOL_BONK_USDC. (crankable collateral mint → deepest
// Orca/USDC Whirlpool), discovered on-chain. Direct-DEX, no Jupiter. The
// pre-arm sim-gate rejects any that don't build/sim cleanly, so a
// wrong/thin entry is harmless — it just never fires.
var dexPools = [][2]string{
	{"DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263", "5P6n5omLbLbP4kaPGL8etqQAHEx2UCkaUyvjLDnwV4EY"}, // BONK
	{"7vfCXTUXx5WJV5JADk17DUJ4ksgau7utNKj4b963voxs", "AU971DrPyhhrpRnmEBp5pDTWL2ny7nofb5vYBjDJkR2E"},
	{"85VBFQZC9TZkfaptBWjvUw7YbZjy52A6mjtPGjstQAmQ", "91E61RiGhH9b9Ns8wrb4E3oBNdtkQx2k4xb33pSqt5am"},
	{"HZ1JovNiVvGrGNiiYvEozEVgZ58xaU3RKwX8eACQBCt3", "Fra9rBL1F5eAgtoqjXsBzZocD1UKbxXoERKVs6e23ixn"},
	{"2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo", "9tXiuRRw7kbejLhZXtxDxYs2REe43uH2e7k1kocgdM9B"},
	{"EKpQGSJtjMFqKZ9KQanSqYXRcF8fBopzLHYxdM65zcjm", "CN8M75cH57DuZNzW5wSUpTXtMrSfXBFScJoQxVCgAXes"},
	{"pumpCmXqMfrsAkQ5r49WcJnRayYRqmXz6ae8H7H9Dfn", "4AFAkCSkSNmra64irggEFd8ZtF4WCtFe51qVaFFNBL2D"}, // PUMP
	{"3NZ9JMVBmGAqocybic2c7LQCJScmgsAZ6vQqTDzcqmJh", "55BrDTCLWayM16GwrMEQU57o4PTm6ceF9wavSdNZcEiy"},
	{"USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA", "AxqAWNZqozhTn2pkDPgpf5kc5DeBuhLKKNWnt3dLrxdi"}, // USDS
	{"JUPyiwrYJFskUPiHa7hkeR8VUtAeFoSYbKedZNsDvCN", "4Ui9QdDNuUaAGqCPcDSp191QrixLzQiLxJ1Gnqvz3szP"}, // JUP
	{"2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH", "9RqDTfwCx2SgxsvKpspQHc38HUo3B6hRd3oR9JR966Ps"},
	{"27G8MtK7VtTcCHkpASjSDdkWWYfoqT6ggEuKidVJidD4", "HD8i7qr1hd9ida6sN71RbkLxbWcbvZS4NA5CY6vfcDpj"},
	{"cbbtcf3aa214zXHbiAZQwf4122FBYbraNdFqgw4iMij", "HxA6SKW5qA4o12fjVgTpXdq2YnZ5Zv1s7SB4FFomsyLM"},  // cbBTC
	{"CASHx9KJUStyftLFWGvEVf59SGeG9sh5FfcnZMVPCASH", "3wijQvPKm6jHQrAkfPpok5o8WjCWPm1DGG17NmeW8q1w"}, // CASH
	{"hntyVP6YFm1Hg25TN9WGLqM12b8TQmcknKrdu1oxWux", "5LnAsMfjG32kdUauAzEuzANT6YmM3TSRpL1rWsCUDKus"},  // HNT
}

// DEXPoolAddresses returns the DEX pool addresses to subscribe/stream (so
// the executor knows what to watch).
func DEXPoolAddresses() []solana.PublicKey {
	out := make([]solana.PublicKey, 0, len(dexPools))
	for _, p := range dexPools {
		if pk, err := solana.PublicKeyFromBase58(p[1]); err == nil {
			out = append(out, pk)
		}
	}
	return out
}

const usdcMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

// DirectDEXPool returns the direct-DEX pool for collateral -> debt, if
// wired.
func DirectDEXPool(collateral, debt solana.PublicKey) (solana.PublicKey, bool) {
	if debt.String() != usdcMint {
		return solana.PublicKey{}, false // every pool here routes to USDC
	}
	c := collateral.String()
	for _, p := range dexPools {
		if p[0] == c {
			pk, err := solana.PublicKeyFromBase58(p[1])
			if err != nil {
				return solana.PublicKey{}, false
			}
			return pk, true
		}
	}
	return solana.PublicKey{}, false
}

// fetchAccount fetches one account's raw bytes via RPC (off the hot path) —
// live pool state for the direct-DEX swap (tick arrays + price).
func fetchAccount(endpoint string, key solana.PublicKey) ([]byte, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{key.String(), map[string]string{"encoding": "base64"}},
	})
	if err != nil {
		return nil, err
	}
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var v struct {
		Result struct {
			Value struct {
				Data []string `json:"data"`
			} `json:"value"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, err
	}
	if len(v.Result.Value.Data) == 0 {
		return nil, fmt.Errorf("no account data for %s", key)
	}
	return base64.StdEncoding.DecodeString(v.Result.Value.Data[0])
}

// orcaDirectSwap builds the Orca Whirlpool swap ix for the seized
// collateral → debt asset, entirely from live pool bytes (no network
// aggregator). Returns (ixs, quoted_out).
func orcaDirectSwap(rpcEndpoint string, poolPk solana.PublicKey, c *FireCandidate, authority solana.PublicKey,
	swapIn uint64, slippageBps uint32) ([]solana.Instruction, uint64, error) {
	// Streamed pool state from RAM (µs) if present; else RPC fetch (~45ms fallback).
	pb, ok := getPoolCache(poolPk)
	if !ok {
		var err error
		pb, err = fetchAccount(rpcEndpoint, poolPk)
		if err != nil {
			return nil, 0, err
		}
	}
	if len(pb) < 213 {
		return nil, 0, fmt.Errorf("orca pool too small")
	}
	// Direction: input is token0 (a_to_b / zero_for_one) iff asset_mint == mint0@101.
	mint0 := arb.PkAt(pb, 101)
	aToB := c.AssetMint.Equals(mint0)
	feeRate := float64(binary.LittleEndian.Uint16(pb[45:47])) // Orca feeRate (1e-6) → bps = /100
	cl, ok := clmm.FromOrca(pb, 0, 0, feeRate/100.0)
	if !ok {
		return nil, 0, fmt.Errorf("orca state")
	}
	quoted := cl.ApplySwap(aToB, float64(swapIn))
	if quoted < 0 {
		quoted = 0
	}
	quotedU64 := uint64(quoted)
	minOut := uint64(quoted * (1.0 - float64(slippageBps)/1e4))
	accts, err := arb.OrcaAccounts(pb, poolPk, authority, aToB, c.AssetMint, c.AssetTokenProgram, c.DebtTokenProgram)
	if err != nil {
		return nil, 0, err
	}
	// Token-2022 mints (e.g. cbBTC) need swap_v2 (passes token programs + mints).
	const t22 = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"
	needsV2 := c.AssetTokenProgram.String() == t22 || c.DebtTokenProgram.String() == t22
	var ix solana.Instruction
	if needsV2 {
		mintA := arb.PkAt(pb, 101)
		mintB := arb.PkAt(pb, 181)
		tpA, tpB := c.AssetTokenProgram, c.DebtTokenProgram
		if !aToB {
			tpA, tpB = c.DebtTokenProgram, c.AssetTokenProgram
		}
		ix = swap.OrcaSwapV2Ix(accts, mintA, mintB, tpA, tpB, swapIn, minOut, swap.SqrtLimit(aToB), true, aToB)
	} else {
		ix = swap.OrcaSwapIx(accts, swapIn, minOut, swap.SqrtLimit(aToB), true, aToB)
	}
	return []solana.Instruction{ix}, quotedU64, nil
}

// saturatingSub is Go's stand-in for Rust's u64::saturating_sub.
func saturatingSub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

// BuildFireTx builds the unsigned fire tx. Quotes the collateral→USDC swap
// live (direct-DEX), so call this only for a sim-confirmed candidate.
// blockhash = real recent hash for live submission, or default for
// replace-blockhash simulation.
func BuildFireTx(
	rpcEndpoint string,
	c *FireCandidate,
	liquidatorMA solana.PublicKey,
	authority solana.PublicKey,
	tipAccount *solana.PublicKey,
	tipLamports uint64,
	priorityMicroLamports uint64,
	slippageBps uint32,
	maxSwapAccounts int,
	blockhash solana.Hash,
) (*FireTx, error) {
	tokenProgram := solana.MustPublicKeyFromBase58("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA")

	// Swap leg: ExactIn the seized collateral → DEBT asset (USDC/USDT/wSOL).
	// Haircut 0.05%: the seize→withdraw round-trip goes through marginfi
	// share math and can round down a few native units, and an ExactIn of
	// the full amount would fail on insufficient funds — the dust stays in
	// the asset ATA.
	// wrap_sol=false — a SOL collateral withdraw lands wSOL in the wSOL ATA,
	// and a wSOL-debt swap output also lands as wSOL, which payback spends
	// directly.
	//
	// Same-mint case (collateral mint == debt mint, e.g. SOL collateral /
	// SOL debt): no swap — the withdrawn collateral IS the debt asset (same
	// ATA), so repay spends it directly. Jupiter rejects equal in/out
	// mints, so we must skip the quote entirely. quoted_out ≈ the withdrawn
	// amount.
	swapIn := saturatingSub(c.AssetAmount, c.AssetAmount/2000+1)
	sameMint := c.AssetMint.Equals(c.DebtMint)
	var swapIxs []solana.Instruction
	var quotedOut uint64
	var swapAlts []solana.PublicKey
	switch {
	case sameMint:
		quotedOut = swapIn
	default:
		if pool, ok := DirectDEXPool(c.AssetMint, c.DebtMint); ok {
			// Direct-DEX (Orca Whirlpool) — no Jupiter, no HTTP quote, no rate limit.
			ixs, quoted, err := orcaDirectSwap(rpcEndpoint, pool, c, authority, swapIn, slippageBps)
			if err != nil {
				return nil, err
			}
			swapIxs, quotedOut = ixs, quoted
		} else {
			// NO Jupiter on the fire path. A Jupiter HTTP quote is 100s of ms
			// (+429 backoff) — it can never be a 1ms fire, and its
			// multi-second hangs starve the MAX_INFLIGHT slots and block the
			// fast direct-DEX fires. A pair with no direct-DEX pool
			// fast-fails here (µs); recapture it by ADDING a pool.
			_ = maxSwapAccounts
			return nil, fmt.Errorf("no direct-DEX pool for %s→%s — add a pool", c.AssetMint, c.DebtMint)
		}
	}
	// Jupiter's route ALTs + our liquidation ALT (the fixed marginfi accounts).
	liqAltStr := os.Getenv("LIQ_ALT")
	if liqAltStr == "" {
		liqAltStr = LiqALT
	}
	liqAlt, err := solana.PublicKeyFromBase58(liqAltStr)
	if err != nil {
		return nil, err
	}
	altAddrs := append(append([]solana.PublicKey{}, swapAlts...), liqAlt)
	alts, err := jupswap.FetchAlts(rpcEndpoint, altAddrs)
	if err != nil {
		return nil, err
	}

	assetAta := flashloan.AtaFor(authority, c.AssetMint, c.AssetTokenProgram)
	debtAta := flashloan.AtaFor(authority, c.DebtMint, c.DebtTokenProgram)

	ixs := []solana.Instruction{
		arb.CuLimitIx(FireCULimit),
		arb.CuPriceIx(priorityMicroLamports),
		flashloan.CreateAtaIdempotentFor(authority, c.AssetMint, c.AssetTokenProgram),
		flashloan.CreateAtaIdempotentFor(authority, c.DebtMint, c.DebtTokenProgram),
	}
	_ = tokenProgram
	startIdx := len(ixs)
	ixs = append(ixs, marginfi.StartFlashloan(liquidatorMA, authority, 0)) // end_index patched below
	ixs = append(ixs, marginfi.LendingAccountLiquidate(
		c.AssetBank, c.LiabBank, liquidatorMA, authority, c.Liquidatee,
		tokenProgram, c.AssetAmount, c.AssetOracle, c.LiabOracle, c.LiquidateeObs,
	))
	ixs = append(ixs, marginfi.LendingAccountWithdraw(
		liquidatorMA, authority, c.AssetBank, assetAta, c.AssetTokenProgram, c.AssetAmount, true,
	))
	ixs = append(ixs, swapIxs...)
	// repay_all clears the entire liability regardless of amount (verified
	// in marginfi_probe); pass the quoted swap output as a plausible
	// amount. Uses the generic payback for the actual debt bank
	// (USDC/USDT/wSOL).
	ixs = append(ixs, marginfi.PaybackAsset(liquidatorMA, authority, c.LiabBank, debtAta, quotedOut, true))
	// withdraw_all + repay_all close both balances → end_flashloan health
	// check runs over zero active balances (empty observation list).
	endIndex := uint64(len(ixs))
	ixs[startIdx] = marginfi.StartFlashloan(liquidatorMA, authority, endIndex)
	ixs = append(ixs, marginfi.EndFlashloan(liquidatorMA, authority, nil))
	// Tip last, in-tx → only paid when the liquidation lands.
	if tipAccount != nil && tipLamports > 0 {
		ixs = append(ixs, arb.TransferIx(authority, *tipAccount, tipLamports))
	}

	tx, err := arb.CompileV0(authority, ixs, alts, blockhash)
	if err != nil {
		return nil, fmt.Errorf("compile v0: %w", err)
	}
	txBytes, err := tx.MarshalBinary()
	if err != nil {
		return nil, err
	}
	return &FireTx{Tx: tx, QuotedUSDCOut: quotedOut, TxBytes: len(txBytes)}, nil
}
