package solana

// AccountMeta describes one account referenced by an Instruction.
type AccountMeta struct {
	Pubkey     Pubkey
	IsSigner   bool
	IsWritable bool
}

func NewAccountMeta(pk Pubkey, isSigner, isWritable bool) AccountMeta {
	return AccountMeta{Pubkey: pk, IsSigner: isSigner, IsWritable: isWritable}
}

// Writable is a shorthand for a writable, non-signer account meta.
func Writable(pk Pubkey) AccountMeta { return AccountMeta{Pubkey: pk, IsWritable: true} }

// ReadonlyMeta is a shorthand for a readonly, non-signer account meta.
func ReadonlyMeta(pk Pubkey) AccountMeta { return AccountMeta{Pubkey: pk} }

// SignerMeta is a shorthand for a readonly signer account meta.
func SignerMeta(pk Pubkey) AccountMeta { return AccountMeta{Pubkey: pk, IsSigner: true} }

// WritableSigner is a shorthand for a writable signer account meta (fee payer).
func WritableSigner(pk Pubkey) AccountMeta {
	return AccountMeta{Pubkey: pk, IsSigner: true, IsWritable: true}
}

// Instruction is a single, uncompiled Solana instruction.
type Instruction struct {
	ProgramID Pubkey
	Accounts  []AccountMeta
	Data      []byte
}
