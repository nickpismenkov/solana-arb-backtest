package solana

import (
	"fmt"
)

type acctFlags struct {
	pk         Pubkey
	isSigner   bool
	isWritable bool
}

// CompileV0 builds a MessageV0 from a payer, an ordered instruction list, the
// ALT accounts the caller has already resolved (so their contents are known
// without an RPC round-trip), and a recent blockhash. It mirrors Solana's
// v0::Message::try_compile: collect every referenced account (deduplicated,
// flags OR'd), partition into signers / non-signer-static / non-signer-ALT,
// and compile instructions against the resulting index space.
func CompileV0(payer Pubkey, instructions []Instruction, alts []AddressLookupTableAccount, recentBlockhash Hash) (MessageV0, error) {
	order := []Pubkey{payer}
	flags := map[Pubkey]*acctFlags{payer: {pk: payer, isSigner: true, isWritable: true}}

	touch := func(pk Pubkey, isSigner, isWritable bool) {
		f, ok := flags[pk]
		if !ok {
			f = &acctFlags{pk: pk}
			flags[pk] = f
			order = append(order, pk)
		}
		f.isSigner = f.isSigner || isSigner
		f.isWritable = f.isWritable || isWritable
	}

	for _, ix := range instructions {
		touch(ix.ProgramID, false, false)
		for _, a := range ix.Accounts {
			touch(a.Pubkey, a.IsSigner, a.IsWritable)
		}
	}

	// Index ALT contents once: pubkey -> (table index, position in table).
	type altPos struct {
		table int
		pos   int
	}
	altIndex := make(map[Pubkey]altPos)
	for ti, t := range alts {
		for pi, a := range t.Addresses {
			if _, exists := altIndex[a]; !exists {
				altIndex[a] = altPos{table: ti, pos: pi}
			}
		}
	}

	var signersWritable, signersReadonly []Pubkey
	var staticWritable, staticReadonly []Pubkey
	type lookupEntry struct {
		pk  Pubkey
		pos int
	}
	writableByTable := make([][]lookupEntry, len(alts))
	readonlyByTable := make([][]lookupEntry, len(alts))

	for _, pk := range order {
		f := flags[pk]
		switch {
		case f.isSigner && f.isWritable:
			signersWritable = append(signersWritable, pk)
		case f.isSigner && !f.isWritable:
			signersReadonly = append(signersReadonly, pk)
		default:
			if ap, ok := altIndex[pk]; ok {
				if f.isWritable {
					writableByTable[ap.table] = append(writableByTable[ap.table], lookupEntry{pk, ap.pos})
				} else {
					readonlyByTable[ap.table] = append(readonlyByTable[ap.table], lookupEntry{pk, ap.pos})
				}
			} else if f.isWritable {
				staticWritable = append(staticWritable, pk)
			} else {
				staticReadonly = append(staticReadonly, pk)
			}
		}
	}

	staticKeys := append(append(append([]Pubkey{}, signersWritable...), signersReadonly...), append(staticWritable, staticReadonly...)...)

	var lookups []MessageAddressTableLookup
	var loadedWritable, loadedReadonly []Pubkey
	for ti, t := range alts {
		if len(writableByTable[ti]) == 0 && len(readonlyByTable[ti]) == 0 {
			continue
		}
		l := MessageAddressTableLookup{AccountKey: t.Key}
		for _, e := range writableByTable[ti] {
			l.WritableIndexes = append(l.WritableIndexes, uint8(e.pos))
			loadedWritable = append(loadedWritable, e.pk)
		}
		for _, e := range readonlyByTable[ti] {
			l.ReadonlyIndexes = append(l.ReadonlyIndexes, uint8(e.pos))
			loadedReadonly = append(loadedReadonly, e.pk)
		}
		lookups = append(lookups, l)
	}

	fullKeys := append(append(append([]Pubkey{}, staticKeys...), loadedWritable...), loadedReadonly...)
	if len(fullKeys) > 256 {
		return MessageV0{}, fmt.Errorf("solana: too many accounts (%d) for a single message", len(fullKeys))
	}
	keyIndex := make(map[Pubkey]uint8, len(fullKeys))
	for i, k := range fullKeys {
		keyIndex[k] = uint8(i)
	}

	compiled := make([]CompiledInstruction, len(instructions))
	for i, ix := range instructions {
		accs := make([]uint8, len(ix.Accounts))
		for j, a := range ix.Accounts {
			accs[j] = keyIndex[a.Pubkey]
		}
		compiled[i] = CompiledInstruction{
			ProgramIDIndex: keyIndex[ix.ProgramID],
			Accounts:       accs,
			Data:           ix.Data,
		}
	}

	return MessageV0{
		Header: MessageHeader{
			NumRequiredSignatures: uint8(len(signersWritable) + len(signersReadonly)),
			NumReadonlySigned:     uint8(len(signersReadonly)),
			NumReadonlyUnsigned:   uint8(len(staticReadonly)),
		},
		AccountKeys:         staticKeys,
		RecentBlockhash:     recentBlockhash,
		Instructions:        compiled,
		AddressTableLookups: lookups,
	}, nil
}
