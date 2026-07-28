// Package execute holds leg-3 execution primitives: read live pool state
// (tick/sqrtPrice/liquidity) and derive the tick-array accounts a swap must
// reference. These are the fiddly, get-them-exactly-right pieces the swap
// instructions are built on.
package execute

import (
	"encoding/binary"

	"arbengine/internal/solana"
)

const (
	OrcaProgram    = "whirLbMiicVdio4qvUfM5KAg6Ct8VwpYzGff3uctyCc"
	RayClmmProgram = "CAMMCzo5YL8w4VFF8KVHrK22GGUsp5VTaW7grrKgrWqK"
)

// PoolState is the subset of on-chain pool state needed to derive tick
// arrays and build swap instructions.
type PoolState struct {
	SqrtPrice   solana128
	Tick        int32
	TickSpacing uint16
	Liquidity   solana128
}

// solana128 is a minimal little-endian u128 stand-in (two u64 halves),
// enough for the fields this package threads through without needing
// arbitrary-precision arithmetic.
type solana128 struct{ Lo, Hi uint64 }

func u16le(d []byte, o int) uint16 { return binary.LittleEndian.Uint16(d[o : o+2]) }
func i32le(d []byte, o int) int32  { return int32(binary.LittleEndian.Uint32(d[o : o+4])) }
func u128le(d []byte, o int) solana128 {
	return solana128{Lo: binary.LittleEndian.Uint64(d[o : o+8]), Hi: binary.LittleEndian.Uint64(d[o+8 : o+16])}
}

// DecodeOrcaState decodes an Orca Whirlpool account: tickSpacing@41,
// liquidity@49, sqrtPrice@65, tickCurrent@81.
func DecodeOrcaState(d []byte) (PoolState, bool) {
	if len(d) < 85 {
		return PoolState{}, false
	}
	return PoolState{
		TickSpacing: u16le(d, 41),
		Liquidity:   u128le(d, 49),
		SqrtPrice:   u128le(d, 65),
		Tick:        i32le(d, 81),
	}, true
}

// DecodeRayState decodes a Raydium CLMM account: tickSpacing@235,
// liquidity@237, sqrtPriceX64@253, tickCurrent@269.
func DecodeRayState(d []byte) (PoolState, bool) {
	if len(d) < 273 {
		return PoolState{}, false
	}
	return PoolState{
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
	if r != 0 && ((r < 0) != (b < 0)) {
		return q - 1
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

// OrcaTickArray derives the Orca tick-array PDA: seeds
// ["tick_array", whirlpool, ASCII(start_index)].
func OrcaTickArray(pool solana.Pubkey, start int32) solana.Pubkey {
	pk, _ := solana.FindProgramAddress(
		[][]byte{[]byte("tick_array"), pool.Bytes(), []byte(itoa(start))},
		solana.MustPubkeyFromBase58(OrcaProgram),
	)
	return pk
}

// RayTickArray derives the Raydium CLMM tick-array PDA: seeds
// ["tick_array", pool, i32 BE(start_index)].
func RayTickArray(pool solana.Pubkey, start int32) solana.Pubkey {
	var be [4]byte
	binary.BigEndian.PutUint32(be[:], uint32(start))
	pk, _ := solana.FindProgramAddress(
		[][]byte{[]byte("tick_array"), pool.Bytes(), be[:]},
		solana.MustPubkeyFromBase58(RayClmmProgram),
	)
	return pk
}

func itoa(n int32) string {
	neg := n < 0
	if neg {
		n = -n
	}
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
