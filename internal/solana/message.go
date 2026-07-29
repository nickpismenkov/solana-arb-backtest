package solana

import (
	"bytes"
	"errors"
	"fmt"
)

// MessageHeader carries the account-privilege partition sizes shared by both
// legacy and v0 messages.
type MessageHeader struct {
	NumRequiredSignatures uint8
	NumReadonlySigned     uint8
	NumReadonlyUnsigned   uint8
}

// CompiledInstruction is an Instruction with its program id and accounts
// replaced by indexes into the message's resolved account-key list.
type CompiledInstruction struct {
	ProgramIDIndex uint8
	Accounts       []uint8
	Data           []byte
}

// MessageAddressTableLookup references one ALT plus which of its entries
// (by index) this message pulls in as writable / readonly accounts.
type MessageAddressTableLookup struct {
	AccountKey      Pubkey
	WritableIndexes []uint8
	ReadonlyIndexes []uint8
}

// AddressLookupTableAccount is a caller-resolved ALT: its own address plus
// the full list of addresses it stores, in order.
type AddressLookupTableAccount struct {
	Key       Pubkey
	Addresses []Pubkey
}

// MessageLegacy is the original (no ALT) Solana message format.
type MessageLegacy struct {
	Header          MessageHeader
	AccountKeys     []Pubkey
	RecentBlockhash Hash
	Instructions    []CompiledInstruction
}

// MessageV0 is the versioned message format that supports Address Lookup
// Tables: static account keys plus per-table writable/readonly index lists
// resolved at runtime by the cluster.
type MessageV0 struct {
	Header              MessageHeader
	AccountKeys         []Pubkey
	RecentBlockhash     Hash
	Instructions        []CompiledInstruction
	AddressTableLookups []MessageAddressTableLookup
}

// VersionedMessage holds either a Legacy or a V0 message.
type VersionedMessage struct {
	IsV0   bool
	Legacy MessageLegacy
	V0     MessageV0
}

// StaticAccountKeys returns the message's directly-encoded account keys
// (i.e. excluding any ALT-resolved ones) — mirrors
// VersionedMessage::static_account_keys() in the Rust SDK.
func (m VersionedMessage) StaticAccountKeys() []Pubkey {
	if m.IsV0 {
		return m.V0.AccountKeys
	}
	return m.Legacy.AccountKeys
}

func (m VersionedMessage) Instructions() []CompiledInstruction {
	if m.IsV0 {
		return m.V0.Instructions
	}
	return m.Legacy.Instructions
}

func (m VersionedMessage) RecentBlockhash() Hash {
	if m.IsV0 {
		return m.V0.RecentBlockhash
	}
	return m.Legacy.RecentBlockhash
}

// --- Marshaling (write path; v0 only — matches this codebase, which always
// builds v0+ALT transactions) ---

func (m MessageV0) MarshalBinary() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte(0x80) // version prefix: MSB set, version = 0
	buf.WriteByte(m.Header.NumRequiredSignatures)
	buf.WriteByte(m.Header.NumReadonlySigned)
	buf.WriteByte(m.Header.NumReadonlyUnsigned)

	buf.Write(encodeShortVecLen(len(m.AccountKeys)))
	for _, k := range m.AccountKeys {
		buf.Write(k[:])
	}
	buf.Write(m.RecentBlockhash[:])

	buf.Write(encodeShortVecLen(len(m.Instructions)))
	for _, ix := range m.Instructions {
		buf.WriteByte(ix.ProgramIDIndex)
		buf.Write(encodeShortVecLen(len(ix.Accounts)))
		buf.Write(ix.Accounts)
		buf.Write(encodeShortVecLen(len(ix.Data)))
		buf.Write(ix.Data)
	}

	buf.Write(encodeShortVecLen(len(m.AddressTableLookups)))
	for _, l := range m.AddressTableLookups {
		buf.Write(l.AccountKey[:])
		buf.Write(encodeShortVecLen(len(l.WritableIndexes)))
		buf.Write(l.WritableIndexes)
		buf.Write(encodeShortVecLen(len(l.ReadonlyIndexes)))
		buf.Write(l.ReadonlyIndexes)
	}
	return buf.Bytes(), nil
}

func (m MessageLegacy) MarshalBinary() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte(m.Header.NumRequiredSignatures)
	buf.WriteByte(m.Header.NumReadonlySigned)
	buf.WriteByte(m.Header.NumReadonlyUnsigned)

	buf.Write(encodeShortVecLen(len(m.AccountKeys)))
	for _, k := range m.AccountKeys {
		buf.Write(k[:])
	}
	buf.Write(m.RecentBlockhash[:])

	buf.Write(encodeShortVecLen(len(m.Instructions)))
	for _, ix := range m.Instructions {
		buf.WriteByte(ix.ProgramIDIndex)
		buf.Write(encodeShortVecLen(len(ix.Accounts)))
		buf.Write(ix.Accounts)
		buf.Write(encodeShortVecLen(len(ix.Data)))
		buf.Write(ix.Data)
	}
	return buf.Bytes(), nil
}

func (m VersionedMessage) MarshalBinary() ([]byte, error) {
	if m.IsV0 {
		return m.V0.MarshalBinary()
	}
	return m.Legacy.MarshalBinary()
}

// --- Unmarshaling (read path; both legacy and v0, needed to decode
// shred/gRPC-carried transactions) ---

// UnmarshalVersionedMessage parses a wire-format message. The high bit of
// the first byte distinguishes v0 (set, low 7 bits = version number) from
// legacy (unset — that byte is instead NumRequiredSignatures, which is a
// small tx-defined count and so never has the high bit set in practice).
func UnmarshalVersionedMessage(b []byte) (VersionedMessage, int, error) {
	if len(b) == 0 {
		return VersionedMessage{}, 0, errors.New("solana: empty message")
	}
	if b[0]&0x80 != 0 {
		version := b[0] & 0x7f
		if version != 0 {
			return VersionedMessage{}, 0, fmt.Errorf("solana: unsupported message version %d", version)
		}
		v0, n, err := unmarshalMessageV0(b[1:])
		if err != nil {
			return VersionedMessage{}, 0, err
		}
		return VersionedMessage{IsV0: true, V0: v0}, n + 1, nil
	}
	legacy, n, err := unmarshalMessageLegacy(b)
	if err != nil {
		return VersionedMessage{}, 0, err
	}
	return VersionedMessage{Legacy: legacy}, n, nil
}

func unmarshalMessageLegacy(b []byte) (MessageLegacy, int, error) {
	var m MessageLegacy
	if len(b) < 3+32 {
		return m, 0, errors.New("solana: truncated legacy message header")
	}
	off := 0
	m.Header = MessageHeader{b[0], b[1], b[2]}
	off += 3

	nKeys, n, err := decodeShortVecLen(b[off:])
	if err != nil {
		return m, 0, err
	}
	off += n
	m.AccountKeys = make([]Pubkey, nKeys)
	for i := 0; i < nKeys; i++ {
		pk, err := PubkeyFromBytes(b[off : off+32])
		if err != nil {
			return m, 0, err
		}
		m.AccountKeys[i] = pk
		off += 32
	}

	copy(m.RecentBlockhash[:], b[off:off+32])
	off += 32

	instrs, n, err := unmarshalCompiledInstructions(b[off:])
	if err != nil {
		return m, 0, err
	}
	m.Instructions = instrs.list
	off += n
	return m, off, nil
}

func unmarshalMessageV0(b []byte) (MessageV0, int, error) {
	var m MessageV0
	if len(b) < 3+32 {
		return m, 0, errors.New("solana: truncated v0 message header")
	}
	off := 0
	m.Header = MessageHeader{b[0], b[1], b[2]}
	off += 3

	nKeys, n, err := decodeShortVecLen(b[off:])
	if err != nil {
		return m, 0, err
	}
	off += n
	m.AccountKeys = make([]Pubkey, nKeys)
	for i := 0; i < nKeys; i++ {
		pk, err := PubkeyFromBytes(b[off : off+32])
		if err != nil {
			return m, 0, err
		}
		m.AccountKeys[i] = pk
		off += 32
	}

	copy(m.RecentBlockhash[:], b[off:off+32])
	off += 32

	instrs, n, err := unmarshalCompiledInstructions(b[off:])
	if err != nil {
		return m, 0, err
	}
	m.Instructions = instrs.list
	off += n

	nLookups, n, err := decodeShortVecLen(b[off:])
	if err != nil {
		return m, 0, err
	}
	off += n
	m.AddressTableLookups = make([]MessageAddressTableLookup, nLookups)
	for i := 0; i < nLookups; i++ {
		var l MessageAddressTableLookup
		pk, err := PubkeyFromBytes(b[off : off+32])
		if err != nil {
			return m, 0, err
		}
		l.AccountKey = pk
		off += 32

		nw, n, err := decodeShortVecLen(b[off:])
		if err != nil {
			return m, 0, err
		}
		off += n
		l.WritableIndexes = append([]uint8{}, b[off:off+nw]...)
		off += nw

		nr, n, err := decodeShortVecLen(b[off:])
		if err != nil {
			return m, 0, err
		}
		off += n
		l.ReadonlyIndexes = append([]uint8{}, b[off:off+nr]...)
		off += nr

		m.AddressTableLookups[i] = l
	}
	return m, off, nil
}

type compiledInstructionList struct{ list []CompiledInstruction }

func unmarshalCompiledInstructions(b []byte) (compiledInstructionList, int, error) {
	off := 0
	nIx, n, err := decodeShortVecLen(b[off:])
	if err != nil {
		return compiledInstructionList{}, 0, err
	}
	off += n
	out := make([]CompiledInstruction, nIx)
	for i := 0; i < nIx; i++ {
		if off >= len(b) {
			return compiledInstructionList{}, 0, errors.New("solana: truncated instruction")
		}
		programIdx := b[off]
		off++
		nAcc, n, err := decodeShortVecLen(b[off:])
		if err != nil {
			return compiledInstructionList{}, 0, err
		}
		off += n
		accs := append([]uint8{}, b[off:off+nAcc]...)
		off += nAcc

		nData, n, err := decodeShortVecLen(b[off:])
		if err != nil {
			return compiledInstructionList{}, 0, err
		}
		off += n
		data := append([]byte{}, b[off:off+nData]...)
		off += nData

		out[i] = CompiledInstruction{ProgramIDIndex: programIdx, Accounts: accs, Data: data}
	}
	return compiledInstructionList{list: out}, off, nil
}
