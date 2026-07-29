// Package lazer implements Pyth Lazer pre-positioning for the liquidation
// executors.
//
// Liquidation eligibility is gated by the protocol's ON-CHAIN oracle, so
// Lazer can't make an account liquidatable sooner than the chain sees it.
// Its value is a LEADING signal: Lazer ticks at ms latency, minutes ahead of
// the reserve/bank oracle crank. Blending Lazer prices into the health
// recompute lets the executor ARM (pre-select + keep hot) exactly the
// accounts about to cross the threshold, so when the on-chain oracle catches
// up the fire tx is already built and only needs sign+submit. The FIRE
// decision itself stays gated by full on-chain simulation — Lazer never
// affects safety, only which accounts we spend sim budget on.
//
// Feed ids are Lazer's numeric ids (SOL=6, BTC=1, ETH=2, USDC=7). Only the
// volatile majors matter for arming — a borrower crosses the threshold when
// its volatile collateral drops or volatile debt rises; stables don't move.
package lazer

import (
	"strconv"

	"arbengine/internal/liquidation"
	"arbengine/internal/pyth"
	"arbengine/internal/solana"
)

// Lazer numeric feed ids (VERIFIED live in pyth_probe: SOL=6, USDC=7, BTC=1,
// ETH=2; the rest verified against the Lazer symbol registry
// history.pyth-lazer.dourolabs.app/history/v1/symbols).
const (
	LazerSOL  uint32 = 6
	LazerBTC  uint32 = 1
	LazerETH  uint32 = 2
	LazerUSDC uint32 = 7
	LazerUSDT uint32 = 8
	LazerBONK uint32 = 9
	LazerWIF  uint32 = 10
	LazerPYTH uint32 = 3
)

// JUP/W exist on Lazer but do NOT support the `real_time` channel — including
// them errors the ENTIRE subscription ("Feeds do not support channel
// real_time: 92, 102", verified live 2026-07-14) and the stream goes dark.
// Their banks stay baseline-priced; do not add them to ArmFeedIDs unless the
// channel support changes or a separate fixed-rate subscription is wired.
const (
	LazerJUP uint32 = 92
	LazerW   uint32 = 102
)

// ArmFeedIDs returns every feed the executors subscribe to and arm on. The
// list is CENSUS-DRIVEN: a 7-day scan of landed marginfi liquidations
// (2026-07-14) showed BONK collateral in 91% of them, with PYTH/WIF next
// among Lazer-covered assets — while the old majors-only list
// (SOL/BTC/ETH/USDC) missed all of them between 300s rescans. Stables
// (USDC/USDT) included so stable-debt accounts are fully priced by Lazer.
// Known gaps (baseline-priced): HNT (no Lazer feed), JUP/W (no real_time
// channel — see above).
func ArmFeedIDs() []uint32 {
	return []uint32{LazerSOL, LazerBTC, LazerETH, LazerUSDC, LazerUSDT,
		LazerBONK, LazerWIF, LazerPYTH}
}

// MintFeedMap maps mint → Lazer feed id for assets whose price Lazer leads.
// SOL-correlated LSTs map to SOL (their on-chain valuation moves with SOL,
// scaled by the LST exchange rate — see OneToOneMints for why that scale
// matters).
func MintFeedMap() map[solana.Pubkey]uint32 {
	e := func(s string, id uint32) (solana.Pubkey, uint32) {
		return solana.MustPubkeyFromBase58(s), id
	}
	m := map[solana.Pubkey]uint32{}
	set := func(s string, id uint32) {
		pk, v := e(s, id)
		m[pk] = v
	}
	set("So11111111111111111111111111111111111111112", LazerSOL)   // wSOL
	set("mSoLzYCxHdYgdzU16g5QSh3i5K3z3KZK7ytfqcJm7So", LazerSOL)   // mSOL
	set("J1toso1uCk3RLmjorhTtrVwY9HJ7X8V9yYac6Y7kGCPn", LazerSOL)  // jitoSOL
	set("bSo13r4TkiE4KumL71LsHTPpL2euBYLFx6h9HP3piy1", LazerSOL)   // bSOL
	set("7dHbWXmci3dT8UFYWYZweBLXgycu7Y3iL6trKn1Y7ARj", LazerSOL)  // stSOL
	set("LSTxxxnJzKDFSLr4dUkPcmCf5VyryEqzPLz5j4bpxFp", LazerSOL)   // LST (Marinade)
	set("5oVNBeEEQvYi1cX3ir8Dx5n1P7pdxydbGF2X4TxVusJm", LazerSOL)  // INF
	set("jupSoLaHXQiZZTSfEWMTRRgpnyFm8f6sZdosWBjx93v", LazerSOL)   // jupSOL
	set("he1iusmfkpAdwvxLNGV8Y1iSbj4rUy6yMhEA3fotn9A", LazerSOL)   // hSOL (Helius)
	set("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", LazerUSDC) // USDC
	set("Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", LazerUSDT) // USDT
	set("cbbtcf3aa214zXHbiAZQwf4122FBYbraNdFqgw4iMij", LazerBTC)   // cbBTC
	set("3NZ9JMVBmGAqocybic2c7LQCJScmgsAZ6vQqTDzcqmJh", LazerBTC)  // wBTC (Wormhole)
	set("7vfCXTUXx5WJV5JADk17DUJ4ksgau7utNKj4b963voxs", LazerETH)  // wETH (Wormhole)
	set("DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263", LazerBONK) // BONK
	set("EKpQGSJtjMFqKZ9KQanSqYXRcF8fBopzLHYxdM65zcjm", LazerWIF)  // WIF
	set("HZ1JovNiVvGrGNiiYvEozEVgZ58xaU3RKwX8eACQBCt3", LazerPYTH) // PYTH
	// JUP/W deliberately unmapped: their feeds aren't in ArmFeedIDs (no
	// real_time channel), and a mapped-but-unsubscribed feed would leave
	// accounts permanently !feeds_ready instead of baseline-priced.
	return m
}

// OneToOneMints returns mints whose on-chain price IS the feed price (1
// token = 1 feed unit). LSTs are deliberately absent: an LST is worth
// (exchange rate)× SOL — pricing it at the RAW SOL feed undervalues the
// collateral by 15–35% and makes healthy LST-collateral accounts look deep
// underwater (the phantom-candidate bug found 2026-07-14). Consumers that
// substitute the feed price directly (Blend) must restrict themselves to
// this set; coefficient-based consumers (liq_engine) anchor-scale mapped
// banks to the on-chain baseline instead.
func OneToOneMints() map[solana.Pubkey]struct{} {
	mints := []string{
		"So11111111111111111111111111111111111111112",  // wSOL
		"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", // USDC
		"Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", // USDT
		"cbbtcf3aa214zXHbiAZQwf4122FBYbraNdFqgw4iMij",  // cbBTC
		"3NZ9JMVBmGAqocybic2c7LQCJScmgsAZ6vQqTDzcqmJh", // wBTC
		"7vfCXTUXx5WJV5JADk17DUJ4ksgau7utNKj4b963voxs", // wETH
		"DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263", // BONK
		"EKpQGSJtjMFqKZ9KQanSqYXRcF8fBopzLHYxdM65zcjm", // WIF
		"HZ1JovNiVvGrGNiiYvEozEVgZ58xaU3RKwX8eACQBCt3", // PYTH
	}
	out := make(map[solana.Pubkey]struct{}, len(mints))
	for _, s := range mints {
		out[solana.MustPubkeyFromBase58(s)] = struct{}{}
	}
	return out
}

// Blend blends Lazer prices over an on-chain baseline: start from the
// on-chain PriceMap (authoritative for anything Lazer doesn't cover) and
// override a bank's price with the fresher Lazer tick when we have both a
// mint mapping and a recent tick. Only 1:1 mints are overridden — an LST
// priced at the raw SOL feed would be undervalued by its exchange rate, so
// LST banks keep the on-chain baseline here (the tick-driven engine
// anchor-scales them instead). Returns the blended map + how many banks
// Lazer led.
func Blend(banks liquidation.BankMap, onChain liquidation.PriceMap, table *pyth.PriceTable, feedMap map[solana.Pubkey]uint32) (liquidation.PriceMap, int) {
	direct := OneToOneMints()
	out := make(liquidation.PriceMap, len(onChain))
	for k, v := range onChain {
		out[k] = v
	}
	led := 0
	for bankPk, bank := range banks {
		if _, ok := direct[bank.Mint]; !ok {
			continue
		}
		feed, ok := feedMap[bank.Mint]
		if !ok {
			continue
		}
		if p, ok := pyth.Get(table, feed); ok {
			out[bankPk] = p.Price
			led++
		}
	}
	return out, led
}

// SpawnLazerThread runs the Lazer WS feed on its own background goroutine,
// writing into `table`. Lets a synchronous executor loop read fresh Lazer
// prices without adopting an async runtime. Returns immediately.
//
// The Rust original spins up a dedicated tokio runtime on a background OS
// thread (so a sync executor doesn't need to adopt async); Go's goroutines
// don't need that ceremony; a single background goroutine achieves the same
// "returns immediately, keeps running" effect.
func SpawnLazerThread(token string, feedIDs []uint32, table *pyth.PriceTable) {
	pyth.SpawnLazer(token, feedIDs, table)
}

// Status returns a compact log line describing which Lazer majors are live
// (for boot output).
func Status(table *pyth.PriceTable) string {
	f := func(id uint32, name string) (string, bool) {
		p, ok := pyth.Get(table, id)
		if !ok {
			return "", false
		}
		return name + "=$" + strconv.FormatFloat(p.Price, 'f', 2, 64), true
	}
	g := func(id uint32, name string) (string, bool) {
		p, ok := pyth.Get(table, id)
		if !ok {
			return "", false
		}
		return name + "=$" + strconv.FormatFloat(p.Price, 'f', 6, 64), true
	}

	var parts []string
	if s, ok := f(LazerSOL, "SOL"); ok {
		parts = append(parts, s)
	}
	if s, ok := f(LazerBTC, "BTC"); ok {
		parts = append(parts, s)
	}
	if s, ok := f(LazerETH, "ETH"); ok {
		parts = append(parts, s)
	}
	if s, ok := f(LazerUSDC, "USDC"); ok {
		parts = append(parts, s)
	}
	if s, ok := g(LazerBONK, "BONK"); ok {
		parts = append(parts, s)
	}
	if s, ok := f(LazerJUP, "JUP"); ok {
		parts = append(parts, s)
	}

	out := ""
	for i, s := range parts {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}
