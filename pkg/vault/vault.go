// Package vault is the encrypted local state store's cryptography.
//
// The browser has no OS key store and no filesystem, so swap records — which
// include the swap secret, the thing that can move funds — are encrypted with a
// key derived from a user passphrase and stored as NIP-44 payloads. Reusing the
// NIP-44 construction means the same audited primitives (HKDF, ChaCha20,
// HMAC-SHA256) already exercised by the coordination layer protect state at
// rest, and there is no second cipher to get wrong.
//
// Two properties are deliberate:
//
//   - the record key is bound into the plaintext (`recordKey || 0x00 || data`),
//     because NIP-44's MAC authenticates the nonce and ciphertext but not the
//     storage key: without the binding, a ciphertext could be moved between
//     records and would still decrypt; and
//   - the nonce is derived per record from the same random source as the
//     ciphertext, and is never reused, so two seals of the same record differ.
package vault

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"

	"github.com/gjbrr/blake-swap/pkg/nip44"
)

// DefaultIterations is the PBKDF2 work factor.
//
// It is a stored parameter, not a constant of the format: a vault written with a
// higher factor must stay openable, and raising the default later must not lock
// anyone out.
const DefaultIterations = 210_000

// MinIterations is the weakest work factor this implementation will accept when
// opening a vault. A file asking for less is treated as hostile, because the
// cost of guessing the passphrase is the only thing protecting the secret.
const MinIterations = 100_000

// MaxIterations bounds the work an attacker-supplied header can make us do.
const MaxIterations = 4_000_000

// SaltSize is the KDF salt size.
const SaltSize = 32

// KeySize is the derived conversation key size.
const KeySize = 32

// Header is the stored KDF parameter block. It is not secret.
type Header struct {
	Version    int    `json:"version"`
	KDF        string `json:"kdf"`
	Iterations int    `json:"iterations"`
	Salt       string `json:"salt"`
}

// Params are the KDF inputs for one vault.
type Params struct {
	Iterations int
	Salt       []byte
}

// NewParams creates fresh parameters from a random source.
func NewParams(r io.Reader) (Params, error) {
	salt := make([]byte, SaltSize)

	if _, err := io.ReadFull(r, salt); err != nil {
		return Params{}, fmt.Errorf("vault salt: %w", err)
	}

	return Params{Iterations: DefaultIterations, Salt: salt}, nil
}

// Header renders the parameters for storage.
func (p Params) Header() Header {
	return Header{
		Version:    1,
		KDF:        "pbkdf2-hmac-sha256",
		Iterations: p.Iterations,
		Salt:       hexString(p.Salt),
	}
}

// ParamsFromHeader validates a stored header and returns its parameters.
func ParamsFromHeader(header Header) (Params, error) {
	if header.Version != 1 {
		return Params{}, fmt.Errorf("unsupported vault version %d", header.Version)
	}

	if header.KDF != "pbkdf2-hmac-sha256" {
		return Params{}, fmt.Errorf("unsupported vault KDF %q", header.KDF)
	}

	if header.Iterations < MinIterations {
		return Params{}, fmt.Errorf(
			"vault iteration count %d is below the minimum %d",
			header.Iterations, MinIterations,
		)
	}

	if header.Iterations > MaxIterations {
		return Params{}, fmt.Errorf(
			"vault iteration count %d is implausibly large",
			header.Iterations,
		)
	}

	salt, err := parseHex(header.Salt)

	if err != nil {
		return Params{}, fmt.Errorf("vault salt is not hex")
	}

	if len(salt) != SaltSize {
		return Params{}, fmt.Errorf("vault salt must be %d bytes", SaltSize)
	}

	return Params{Iterations: header.Iterations, Salt: salt}, nil
}

// DeriveKey turns a passphrase and parameters into the 32-byte conversation key
// used for every record in this vault.
func DeriveKey(passphrase string, p Params) ([]byte, error) {
	if passphrase == "" {
		return nil, fmt.Errorf("passphrase is empty")
	}

	if len(p.Salt) != SaltSize {
		return nil, fmt.Errorf("vault salt must be %d bytes", SaltSize)
	}

	if p.Iterations < MinIterations || p.Iterations > MaxIterations {
		return nil, fmt.Errorf("vault iteration count %d is out of range", p.Iterations)
	}

	master := pbkdf2SHA256([]byte(passphrase), p.Salt, p.Iterations, KeySize)

	// Domain-separate the record-encryption key from any other use of the same
	// master key.
	key := hkdfExpand(master, []byte("blake-swap vault v1"), KeySize)

	return key, nil
}

// Seal encrypts one record under its key, binding the record key into the
// plaintext so the ciphertext cannot be moved to another record.
func Seal(conversationKey []byte, recordKey string, plaintext []byte, r io.Reader) (string, error) {
	if recordKey == "" {
		return "", fmt.Errorf("record key is empty")
	}

	nonce := make([]byte, nip44.NonceSize)

	if _, err := io.ReadFull(r, nonce); err != nil {
		return "", fmt.Errorf("vault nonce: %w", err)
	}

	bound := make([]byte, 0, len(recordKey)+1+len(plaintext))
	bound = append(bound, []byte(recordKey)...)
	bound = append(bound, 0x00)
	bound = append(bound, plaintext...)

	return nip44.Encrypt(conversationKey, bound, nonce)
}

// Open decrypts one record and refuses a payload that was sealed under a
// different record key.
func Open(conversationKey []byte, recordKey, payload string) ([]byte, error) {
	if recordKey == "" {
		return nil, fmt.Errorf("record key is empty")
	}

	bound, err := nip44.Decrypt(conversationKey, payload)

	if err != nil {
		return nil, err
	}

	raw := []byte(bound)

	prefix := append([]byte(recordKey), 0x00)

	if !bytes.HasPrefix(raw, prefix) {
		return nil, fmt.Errorf(
			"vault record does not belong to key %q: it was sealed under a different record",
			recordKey,
		)
	}

	return raw[len(prefix):], nil
}

// MarshalHeader / UnmarshalHeader are helpers for storing the header as JSON.
func (p Params) MarshalHeader() ([]byte, error) {
	return json.Marshal(p.Header())
}

// UnmarshalHeader parses a stored header.
func UnmarshalHeader(data []byte) (Params, error) {
	var header Header

	if err := json.Unmarshal(data, &header); err != nil {
		return Params{}, fmt.Errorf("vault header is not valid JSON")
	}

	return ParamsFromHeader(header)
}

// pbkdf2SHA256 is PBKDF2-HMAC-SHA256, implemented here because the algorithm is
// small, well specified, and one dependency fewer in a wasm build.
func pbkdf2SHA256(password, salt []byte, iterations, keyLen int) []byte {
	hashLen := sha256.Size
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)

	var counter [4]byte

	for block := 1; block <= blocks; block++ {
		binary.BigEndian.PutUint32(counter[:], uint32(block))

		//
		// One HMAC, reset per iteration. hmac.New precomputes the two keyed blocks of
		// its own state, so constructing a fresh one 210,000 times per key re-hashes
		// the password 210,000 times. Reset keeps that precomputation and is the
		// difference between a derivation that finishes in a browser and one that does
		// not.
		//
		mac := hmac.New(sha256.New, password)
		mac.Write(salt)
		mac.Write(counter[:])
		u := mac.Sum(nil)

		t := make([]byte, len(u))
		copy(t, u)

		for i := 1; i < iterations; i++ {
			mac.Reset()
			mac.Write(u)
			u = mac.Sum(u[:0])

			for j := range t {
				t[j] ^= u[j]
			}
		}

		out = append(out, t...)
	}

	return out[:keyLen]
}

// hkdfExpand is the HKDF-Expand step (RFC 5869) with SHA-256.
func hkdfExpand(prk, info []byte, length int) []byte {
	var out []byte

	var previous []byte

	mac := hmac.New(sha256.New, prk)

	for counter := byte(1); len(out) < length; counter++ {
		mac.Reset()
		mac.Write(previous)
		mac.Write(info)
		mac.Write([]byte{counter})
		previous = mac.Sum(nil)
		out = append(out, previous...)
	}

	return out[:length]
}

func hexString(data []byte) string {
	const digits = "0123456789abcdef"

	out := make([]byte, 0, len(data)*2)

	for _, b := range data {
		out = append(out, digits[b>>4], digits[b&0x0f])
	}

	return string(out)
}

func parseHex(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("odd length")
	}

	out := make([]byte, len(s)/2)

	for i := 0; i < len(out); i++ {
		high, ok := hexDigit(s[2*i])

		if !ok {
			return nil, fmt.Errorf("not hex")
		}

		low, ok := hexDigit(s[2*i+1])

		if !ok {
			return nil, fmt.Errorf("not hex")
		}

		out[i] = high<<4 | low
	}

	return out, nil
}

func hexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}
