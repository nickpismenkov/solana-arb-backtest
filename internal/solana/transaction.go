package solana

import (
	"bytes"
	"encoding/base64"
	"errors"
)

// VersionedTransaction is a signed (or not-yet-signed) message.
type VersionedTransaction struct {
	Signatures []Signature
	Message    VersionedMessage
}

// NewUnsignedVersionedTransaction builds a v0 transaction with placeholder
// (zero) signatures — one per required signer — ready for Sign().
func NewUnsignedVersionedTransaction(msg MessageV0) VersionedTransaction {
	return VersionedTransaction{
		Signatures: make([]Signature, msg.Header.NumRequiredSignatures),
		Message:    VersionedMessage{IsV0: true, V0: msg},
	}
}

// Sign signs the transaction's message with the given keypairs, placing each
// signature at the index matching its signer's position in the account-key
// list (payer/fee-payer is always index 0).
func (tx *VersionedTransaction) Sign(signers []Keypair) error {
	msgBytes, err := tx.Message.MarshalBinary()
	if err != nil {
		return err
	}
	keys := tx.Message.StaticAccountKeys()
	for _, kp := range signers {
		idx := -1
		for i, k := range keys {
			if k == kp.Public {
				idx = i
				break
			}
		}
		if idx < 0 || idx >= len(tx.Signatures) {
			return errors.New("solana: signer is not a required signer of this message")
		}
		tx.Signatures[idx] = kp.Sign(msgBytes)
	}
	return nil
}

// MarshalBinary serializes the full transaction wire format: shortvec of
// signatures followed by the serialized message.
func (tx VersionedTransaction) MarshalBinary() ([]byte, error) {
	msgBytes, err := tx.Message.MarshalBinary()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.Write(encodeShortVecLen(len(tx.Signatures)))
	for _, s := range tx.Signatures {
		buf.Write(s[:])
	}
	buf.Write(msgBytes)
	return buf.Bytes(), nil
}

// Base64 encodes the transaction for JSON-RPC / Jito bundle submission.
func (tx VersionedTransaction) Base64() (string, error) {
	b, err := tx.MarshalBinary()
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// UnmarshalVersionedTransaction parses the wire format produced above (and
// emitted by shreds / gRPC account & transaction feeds).
func UnmarshalVersionedTransaction(b []byte) (VersionedTransaction, error) {
	nSigs, n, err := decodeShortVecLen(b)
	if err != nil {
		return VersionedTransaction{}, err
	}
	off := n
	sigs := make([]Signature, nSigs)
	for i := 0; i < nSigs; i++ {
		if off+64 > len(b) {
			return VersionedTransaction{}, errors.New("solana: truncated signatures")
		}
		copy(sigs[i][:], b[off:off+64])
		off += 64
	}
	msg, _, err := UnmarshalVersionedMessage(b[off:])
	if err != nil {
		return VersionedTransaction{}, err
	}
	return VersionedTransaction{Signatures: sigs, Message: msg}, nil
}
