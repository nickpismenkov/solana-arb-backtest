// Package ticks implements leg-3 execution primitives: read live pool state
// (tick/sqrtPrice/liquidity) and derive the tick-array accounts a swap must
// reference.
package ticks

import (
	"encoding/binary"
	"math/big"

	"github.com/gagliardetto/solana-go"

	"solana-arb-backtest-go/internal/decode"
)

type PoolState struct {
	SqrtPrice   *big.Int // 128-bit unsigned
	Tick        int32
	TickSpacing uint16
	Liquidity   *big.Int // 128-bit unsigned
}

func u16le(d []byte, o int) uint16 { return binary.LittleEndian.Uint16(d[o : o+2]) }
func i32le(d []byte, o int) int32  { return int32(binary.LittleEndian.Uint32(d[o : o+4])) }
func u128le(d []byte, o int) *big.Int {
	// Reverse the 16 little-endian bytes into big-endian for big.Int.SetBytes.
	be := make([]byte, 16)
	for i := 0; i < 16; i++ {
		be[15-i] = d[o+i]
	}
	return new(big.Int).SetBytes(be)
}

// DecodeOrcaState decodes an Orca Whirlpool: tickSpacing@41, liquidity@49,
// sqrtPrice@65, tickCurrent@81.
func DecodeOrcaState(d []byte) (*PoolState, bool) {
	if len(d) < 85 {
		return nil, false
	}
	return &PoolState{
		TickSpacing: u16le(d, 41),
		Liquidity:   u128le(d, 49),
		SqrtPrice:   u128le(d, 65),
		Tick:        i32le(d, 81),
	}, true
}

// DecodeRayState decodes a Raydium CLMM: tickSpacing@235, liquidity@237,
// sqrtPriceX64@253, tickCurrent@269.
func DecodeRayState(d []byte) (*PoolState, bool) {
	if len(d) < 273 {
		return nil, false
	}
	return &PoolState{
		TickSpacing: u16le(d, 235),
		Liquidity:   u128le(d, 237),
		SqrtPrice:   u128le(d, 253),
		Tick:        i32le(d, 269),
	}, true
}

// floorDiv is floor division (toward negative infinity) — tick indices go negative.
func floorDiv(a, b int32) int32 {
	q := a / b
	r := a % b
	if r != 0 && (r < 0) != (b < 0) {
		q--
	}
	return q
}

// Orca packs 88 ticks per array; Raydium CLMM packs 60.
func OrcaStartIndex(tick int32, spacing uint16) int32 {
	n := 88 * int32(spacing)
	return floorDiv(tick, n) * n
}
func RayStartIndex(tick int32, spacing uint16) int32 {
	n := 60 * int32(spacing)
	return floorDiv(tick, n) * n
}

// OrcaTickArray derives the Orca tick-array PDA: seeds ["tick_array",
// whirlpool, ASCII(start_index)].
func OrcaTickArray(pool solana.PublicKey, start int32) solana.PublicKey {
	s := itoa(start)
	addr, _, err := solana.FindProgramAddress(
		[][]byte{[]byte("tick_array"), pool.Bytes(), []byte(s)},
		solana.MustPublicKeyFromBase58(decode.OrcaProgram),
	)
	if err != nil {
		panic(err)
	}
	return addr
}

// RayTickArray derives the Raydium CLMM tick-array PDA: seeds ["tick_array",
// pool, i32 BE(start_index)].
func RayTickArray(pool solana.PublicKey, start int32) solana.PublicKey {
	be := make([]byte, 4)
	binary.BigEndian.PutUint32(be, uint32(start))
	addr, _, err := solana.FindProgramAddress(
		[][]byte{[]byte("tick_array"), pool.Bytes(), be},
		solana.MustPublicKeyFromBase58(decode.RayClmmProgram),
	)
	if err != nil {
		panic(err)
	}
	return addr
}

func itoa(n int32) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	var buf [16]byte
	i := len(buf)
	u := uint32(n)
	if neg {
		u = uint32(-n)
	}
	for u > 0 {
		i--
		buf[i] = byte('0' + u%10)
		u /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
