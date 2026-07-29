package solana

import (
	"errors"
)

// Solana's "compact-u16" (shortvec) length-prefix encoding: 7 bits per byte,
// continuation bit set on all but the last byte. Used to prefix account-key,
// instruction, and signature arrays in the wire format.

func encodeShortVecLen(n int) []byte {
	var out []byte
	v := uint32(n)
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			out = append(out, b|0x80)
		} else {
			out = append(out, b)
			break
		}
	}
	return out
}

func decodeShortVecLen(b []byte) (n int, consumed int, err error) {
	var v uint32
	for i := 0; i < 3; i++ {
		if i >= len(b) {
			return 0, 0, errors.New("solana: truncated shortvec length")
		}
		byteVal := b[i]
		v |= uint32(byteVal&0x7f) << (7 * i)
		if byteVal&0x80 == 0 {
			return int(v), i + 1, nil
		}
	}
	return 0, 0, errors.New("solana: shortvec length too long")
}
