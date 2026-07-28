// Package clmm implements in-memory concentrated-liquidity (Uniswap-v3
// style) math for Orca Whirlpool and Raydium CLMM. Given a pool's current
// sqrtPrice + active liquidity, it applies a swap in closed form (WITHIN the
// current tick — constant liquidity) to get the exact output and post-swap
// price, then computes the optimal cross-venue arb size and its exact
// profit. Pure arithmetic — hot-path safe.
//
// LIMITATION: assumes the swap stays inside the current tick's liquidity
// range (no tick crossing). Valid for small/moderate sizes on deep pools;
// large sizes need tick-array liquidity.
package clmm

import (
	"math"

	"arbengine/internal/solana"
)

// State is normalized pool state for swap math. SqrtP is the raw Q64.64
// sqrt price as a float (sqrt of token1/token0 in raw base units);
// Liquidity is raw L.
type State struct {
	SqrtP     float64
	Liquidity float64
	Mint0     solana.Pubkey
	Mint1     solana.Pubkey
	Dec0      int
	Dec1      int
	FeeBps    float64
}

func u128LEFloat(d []byte, o int) float64 {
	var lo, hi uint64
	for i := 0; i < 8; i++ {
		lo |= uint64(d[o+i]) << (8 * i)
	}
	for i := 0; i < 8; i++ {
		hi |= uint64(d[o+8+i]) << (8 * i)
	}
	return float64(hi)*18446744073709551616.0 + float64(lo)
}

func pkAt(d []byte, o int) solana.Pubkey {
	pk, _ := solana.PubkeyFromBytes(d[o : o+32])
	return pk
}

// FromOrca decodes an Orca Whirlpool account: liquidity u128@49,
// sqrtPrice u128@65, mintA@101, mintB@181. Decimals come from config
// (caller passes base/quote decimals by mint).
func FromOrca(d []byte, decA, decB int, feeBps float64) (State, bool) {
	if len(d) < 213 {
		return State{}, false
	}
	return State{
		Liquidity: u128LEFloat(d, 49),
		SqrtP:     u128LEFloat(d, 65) / math.Pow(2, 64),
		Mint0:     pkAt(d, 101),
		Mint1:     pkAt(d, 181),
		Dec0:      decA,
		Dec1:      decB,
		FeeBps:    feeBps,
	}, true
}

// FromRay decodes a Raydium CLMM account: mint0@73, mint1@105,
// decimals@233/234, liquidity u128@237, sqrtPriceX64 u128@253.
func FromRay(d []byte, feeBps float64) (State, bool) {
	if len(d) < 269 {
		return State{}, false
	}
	return State{
		Liquidity: u128LEFloat(d, 237),
		SqrtP:     u128LEFloat(d, 253) / math.Pow(2, 64),
		Mint0:     pkAt(d, 73),
		Mint1:     pkAt(d, 105),
		Dec0:      int(d[233]),
		Dec1:      int(d[234]),
		FeeBps:    feeBps,
	}, true
}

// ApplySwap applies a swap of amountIn raw units of the input token (token0
// if zeroForOne, else token1). Returns raw amountOut of the other token.
// Within-tick closed form. Fee is taken off the input first.
func (s State) ApplySwap(zeroForOne bool, amountIn float64) float64 {
	if amountIn <= 0.0 || s.Liquidity <= 0.0 {
		return 0.0
	}
	amt := amountIn * (1.0 - s.FeeBps/1e4)
	l, sp := s.Liquidity, s.SqrtP
	if zeroForOne {
		// token0 in -> price decreases. 1/sp_new = 1/sp + amt/L
		spNew := l * sp / (l + amt*sp)
		return l * (sp - spNew) // token1 out
	}
	// token1 in -> price increases. sp_new = sp + amt/L
	spNew := sp + amt/l
	return l * (1.0/sp - 1.0/spNew) // token0 out
}

// AfterSwap returns a post-swap copy (mutates SqrtP as if amountIn were
// swapped). Used to predict a victim's effect before building our arb.
func (s State) AfterSwap(zeroForOne bool, amountIn float64) State {
	out := s
	if amountIn > 0.0 && s.Liquidity > 0.0 {
		amt := amountIn * (1.0 - s.FeeBps/1e4)
		if zeroForOne {
			out.SqrtP = s.Liquidity * s.SqrtP / (s.Liquidity + amt*s.SqrtP)
		} else {
			out.SqrtP = s.SqrtP + amt/s.Liquidity
		}
	}
	return out
}

// AfterBaseSwap applies a decoded victim swap: sellBase = the victim sells
// base (base is input, price of base falls); else the victim buys base
// (quote is input). amountInRaw is the exact-in amount in the input token's
// raw units.
func (s State) AfterBaseSwap(base solana.Pubkey, sellBase bool, amountInRaw float64) State {
	baseIs0 := s.Mint0 == base
	// sellBase -> base is input; zeroForOne iff base is token0.
	// buyBase  -> quote is input; zeroForOne iff quote is token0 (= !baseIs0).
	zeroForOne := baseIs0
	if !sellBase {
		zeroForOne = !baseIs0
	}
	return s.AfterSwap(zeroForOne, amountInRaw)
}

// UIPrice is the UI price of token0 in token1 (e.g. USDC per SOL if token0=SOL).
func (s State) UIPrice() float64 {
	return s.SqrtP * s.SqrtP * math.Pow(10, float64(s.Dec0-s.Dec1))
}

// RoundTripProfit simulates the full round trip for a given USDC borrow
// amount (raw USDC units): buy base on `buy` pool, sell it on `sell` pool.
// Returns net USDC profit (raw, can be negative). base = the base mint
// (e.g. wSOL). Both pools must be the same token pair.
func RoundTripProfit(buy, sell State, base solana.Pubkey, borrowUsdc float64) float64 {
	// Buy base with USDC on `buy`: input is USDC.
	usdcIs0Buy := buy.Mint0 != base
	baseOut := buy.ApplySwap(usdcIs0Buy, borrowUsdc)
	if baseOut <= 0.0 {
		return -borrowUsdc
	}
	// Sell that base for USDC on `sell`: input is base.
	baseIs0Sell := sell.Mint0 == base
	usdcOut := sell.ApplySwap(baseIs0Sell, baseOut)
	return usdcOut - borrowUsdc
}

// OptimalArb finds the best cross-venue arb between two pools. Returns
// (optimal borrow raw USDC, net profit raw USDC, buyASellB). buyASellB=true
// means buy base on a, sell on b; false is the reverse. Ternary search over
// the concave profit curve; maxUsdc caps the search.
func OptimalArb(a, b State, base solana.Pubkey, maxUsdc float64) (size, profit float64, buyASellB bool) {
	opt := func(buy, sell State) (float64, float64) {
		lo, hi := 0.0, maxUsdc
		for i := 0; i < 60; i++ {
			m1 := lo + (hi-lo)/3.0
			m2 := hi - (hi-lo)/3.0
			if RoundTripProfit(buy, sell, base, m1) < RoundTripProfit(buy, sell, base, m2) {
				lo = m1
			} else {
				hi = m2
			}
		}
		size := (lo + hi) / 2.0
		return size, RoundTripProfit(buy, sell, base, size)
	}
	s1, p1 := opt(a, b)
	s2, p2 := opt(b, a)
	if p1 >= p2 {
		return s1, p1, true
	}
	return s2, p2, false
}

// WSOL is the wrapped-SOL mint.
func WSOL() solana.Pubkey {
	return solana.MustPubkeyFromBase58("So11111111111111111111111111111111111111112")
}
