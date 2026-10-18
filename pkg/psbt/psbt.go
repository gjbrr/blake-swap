// Package psbt is the minimal PSBT handling the settlement pipeline needs.
//
// It is the port of add_sha256_preimage and its two bounds-checked helpers. The
// engine builds a settlement transaction by asking a wallet to sign, then has to
// attach the HTLC preimage as a BIP174 PSBT_IN_SHA256 record before
// finalisation; nothing else about PSBT is interpreted here.
//
// Every read is bounds-checked and CompactSize is minimality-checked, because
// this code runs on bytes that came from a node and a wallet: a truncated or
// non-canonical buffer must be a clean refusal, never a short read that would
// annotate the wrong input.
package psbt

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// Magic is the PSBT header, including the version separator.
var Magic = []byte{'p', 's', 'b', 't', 0xff}

// ReadCompactSize decodes one Bitcoin CompactSize integer.
//
// A truncated or non-minimal encoding is refused: Bitcoin Core's own PSBT
// parser rejects non-minimal encodings too, so accepting one would build a
// transaction the node refuses at the worst possible moment.
func ReadCompactSize(buf []byte, pos int) (value uint64, next int, err error) {
	if pos < 0 || pos >= len(buf) {
		return 0, 0, fmt.Errorf("malformed PSBT: compact-size read past end of buffer")
	}

	first := buf[pos]
	pos++

	if first < 253 {
		return uint64(first), pos, nil
	}

	var width int

	switch first {
	case 253:
		width = 2
	case 254:
		width = 4
	case 255:
		width = 8
	}

	if pos+width > len(buf) {
		return 0, 0, fmt.Errorf("malformed PSBT: truncated compact-size integer")
	}

	var raw uint64

	for i := 0; i < width; i++ {
		raw |= uint64(buf[pos+i]) << (8 * i)
	}

	minimum := uint64(253)

	switch width {
	case 4:
		minimum = 0x10000
	case 8:
		minimum = 0x100000000
	}

	if raw < minimum {
		return 0, 0, fmt.Errorf("malformed PSBT: non-minimal compact-size encoding")
	}

	return raw, pos + width, nil
}

// FindMapEnd returns the offset of the 0x00 terminator of the map at pos.
func FindMapEnd(buf []byte, pos int) (int, error) {
	for {
		if pos < 0 || pos >= len(buf) {
			return 0, fmt.Errorf("malformed PSBT: map is not terminated")
		}

		keyLen, next, err := ReadCompactSize(buf, pos)

		if err != nil {
			return 0, err
		}

		if keyLen == 0 {
			return pos, nil
		}

		pos = next + int(keyLen)

		if pos >= len(buf) {
			return 0, fmt.Errorf("malformed PSBT: key runs past end of buffer")
		}

		valueLen, next, err := ReadCompactSize(buf, pos)

		if err != nil {
			return 0, err
		}

		pos = next + int(valueLen)
	}
}

// AddSHA256Preimage attaches a BIP174 PSBT_IN_SHA256 record to input 0.
//
// Only input 0 is supported, and that is enforced rather than assumed: every
// settlement transaction this engine builds has exactly one input, so annotating
// any other input would be a bug that silently settles nothing.
func AddSHA256Preimage(psbtBase64, hashHex, secretHex string, inputIndex int) (string, error) {
	hash, err := hex.DecodeString(hashHex)

	if err != nil {
		return "", fmt.Errorf("hash must be hex")
	}

	secret, err := hex.DecodeString(secretHex)

	if err != nil {
		return "", fmt.Errorf("secret must be hex")
	}

	if len(hash) != 32 {
		return "", fmt.Errorf("hash must be 32 bytes")
	}

	if len(secret) != 32 {
		return "", fmt.Errorf("secret must be 32 bytes")
	}

	sum := sha256.Sum256(secret)

	if !equalBytes(sum[:], hash) {
		return "", fmt.Errorf("SECRET does not hash to HASH")
	}

	if inputIndex != 0 {
		return "", fmt.Errorf(
			"PSBT hashlock injection only supports input 0; requested input %d: settlement PSBTs are single-input by construction",
			inputIndex,
		)
	}

	raw, err := base64.StdEncoding.DecodeString(psbtBase64)

	if err != nil {
		return "", fmt.Errorf("PSBT is not valid base64: %w", err)
	}

	if len(raw) < len(Magic) || !equalBytes(raw[:len(Magic)], Magic) {
		return "", fmt.Errorf("not a PSBT")
	}

	globalEnd, err := FindMapEnd(raw, len(Magic))

	if err != nil {
		return "", err
	}

	inputStart := globalEnd + 1

	inputEnd, err := FindMapEnd(raw, inputStart)

	if err != nil {
		return "", err
	}

	//
	// BIP174 PSBT_IN_SHA256:
	//   key   = 0x0b || 32-byte hash
	//   value = 32-byte preimage
	//
	key := append([]byte{0x0b}, hash...)

	record := make([]byte, 0, 1+len(key)+1+len(secret))
	record = append(record, byte(len(key)))
	record = append(record, key...)
	record = append(record, byte(len(secret)))
	record = append(record, secret...)

	// Insert before the input map's terminator, so the map keeps its 0x00 and
	// everything after it is untouched.
	out := make([]byte, 0, len(raw)+len(record))
	out = append(out, raw[:inputEnd]...)
	out = append(out, record...)
	out = append(out, raw[inputEnd:]...)

	return base64.StdEncoding.EncodeToString(out), nil
}

// SHA256Records returns every PSBT_IN_SHA256 record on input 0, as hash/preimage
// pairs. It exists so a test (or a forensic tool) can prove what was attached
// without re-implementing the parse.
func SHA256Records(psbtBase64 string) (records []Record, err error) {
	raw, err := base64.StdEncoding.DecodeString(psbtBase64)

	if err != nil {
		return nil, fmt.Errorf("PSBT is not valid base64: %w", err)
	}

	if len(raw) < len(Magic) || !equalBytes(raw[:len(Magic)], Magic) {
		return nil, fmt.Errorf("not a PSBT")
	}

	globalEnd, err := FindMapEnd(raw, len(Magic))

	if err != nil {
		return nil, err
	}

	inputStart := globalEnd + 1

	var out []Record

	pos := inputStart

	for {
		if pos < 0 || pos >= len(raw) {
			return nil, fmt.Errorf("malformed PSBT: map is not terminated")
		}

		keyLen, next, err := ReadCompactSize(raw, pos)

		if err != nil {
			return nil, err
		}

		if keyLen == 0 {
			return out, nil
		}

		key := raw[next : next+int(keyLen)]
		pos = next + int(keyLen)

		valueLen, next, err := ReadCompactSize(raw, pos)

		if err != nil {
			return nil, err
		}

		value := raw[next : next+int(valueLen)]
		pos = next + int(valueLen)

		if len(key) == 33 && key[0] == 0x0b && len(value) == 32 {
			out = append(out, Record{Hash: key[1:], Preimage: value})
		}
	}
}

// Record is one PSBT_IN_SHA256 record.
type Record struct {
	Hash     []byte
	Preimage []byte
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
