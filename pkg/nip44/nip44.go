// Package nip44 is NIP-44 v2 payload handling.
//
// It wraps orly's encryption package, which already implements the construction
// (HKDF over the ECDH secret, ChaCha20, HMAC-SHA256, base64 with a version
// byte), and adds the payload checks the Python engine performs before it trusts
// a string: size bounds, the version byte, the "future encoding" prefix, and a
// non-authenticating "does this look encrypted" test so a plaintext reader
// cannot mistake one form for the other.
//
// The same construction is reused for the local vault, which is why encryption
// takes an explicit conversation key rather than a key pair: at rest there is no
// peer, only a key derived from the user's passphrase.
package nip44

import (
	"encoding/base64"
	"fmt"

	"git.smesh.lol/orly/pkg/nostr/crypto/encryption"
)

const (
	// Version is the only payload version this implementation speaks.
	Version = 2

	// MinBase64Chars is the shortest plausible v2 payload (a 1-byte message).
	MinBase64Chars = 132

	// MaxBase64Chars is the largest payload the engine will consider.
	MaxBase64Chars = 1 << 20

	// FutureEncodingPrefix marks a payload that declares an encoding this
	// implementation does not support.
	FutureEncodingPrefix = "#"

	// NonceSize is the payload nonce size.
	NonceSize = 32
	// MACSize is the payload MAC size.
	MACSize = 32
	// MinPayloadBytes is version byte + nonce + at least one ciphertext
	// block + MAC.
	MinPayloadBytes = 1 + NonceSize + 32 + MACSize
)

// Error is a payload or key refusal.
type Error struct{ msg string }

func (e *Error) Error() string { return e.msg }

func refuse(format string, args ...any) error {
	return &Error{msg: fmt.Sprintf(format, args...)}
}

// ConversationKey derives the NIP-44 conversation key for a secret key and a
// peer's x-only public key.
func ConversationKey(secret []byte, peerPublic []byte) ([]byte, error) {
	if len(secret) != 32 {
		return nil, refuse("secret key must be 32 bytes")
	}

	if len(peerPublic) != 32 {
		return nil, refuse("peer public key must be 32 bytes")
	}

	key, err := encryption.GenerateConversationKey(secret, peerPublic)

	if err != nil {
		return nil, fmt.Errorf("conversation key: %w", err)
	}

	return key, nil
}

// Encrypt encrypts a plaintext with a conversation key.
//
// `salt`, when non-nil, is used as the payload nonce; when nil the wrapper
// generates a random one. The vault needs to supply its own so the nonce can be
// bound to the record it encrypts.
func Encrypt(conversationKey, plaintext, salt []byte) (string, error) {
	if len(conversationKey) != 32 {
		return "", refuse("conversation key must be 32 bytes")
	}

	if salt != nil && len(salt) != NonceSize {
		return "", refuse("nonce must be %d bytes", NonceSize)
	}

	var options *encryption.EncryptOptions

	if salt != nil {
		options = &encryption.EncryptOptions{Salt: salt, Version: Version}
	}

	payload, err := encryption.Encrypt(conversationKey, plaintext, options)

	if err != nil {
		return "", fmt.Errorf("encrypt: %w", err)
	}

	return payload, nil
}

// Decrypt decrypts a payload with a conversation key.
//
// The payload is checked structurally first, so a truncated or future-version
// payload is refused by name rather than surfacing as a MAC failure.
func Decrypt(conversationKey []byte, payload string) (string, error) {
	if len(conversationKey) != 32 {
		return "", refuse("conversation key must be 32 bytes")
	}

	if _, _, _, err := decodePayload(payload); err != nil {
		return "", err
	}

	plaintext, err := encryption.Decrypt(conversationKey, payload)

	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}

	return plaintext, nil
}

// decodePayload splits a payload into nonce, ciphertext and MAC.
func decodePayload(payload string) (nonce, ciphertext, mac []byte, err error) {
	if payload == "" {
		return nil, nil, nil, refuse("payload is empty")
	}

	if payload[0] == FutureEncodingPrefix[0] {
		return nil, nil, nil, refuse(
			"payload declares a future encoding, which this implementation does not support")
	}

	if len(payload) < MinBase64Chars {
		return nil, nil, nil, refuse("payload is too short to be a NIP-44 v2 payload")
	}

	if len(payload) > MaxBase64Chars {
		return nil, nil, nil, refuse("payload is implausibly large")
	}

	raw, err := base64.StdEncoding.DecodeString(payload)

	if err != nil {
		return nil, nil, nil, refuse("payload is not valid base64")
	}

	if len(raw) < MinPayloadBytes {
		return nil, nil, nil, refuse("decoded payload is too short")
	}

	if raw[0] != Version {
		return nil, nil, nil, refuse(
			"unsupported NIP-44 version %d; expected %d", raw[0], Version)
	}

	return raw[1:33], raw[33 : len(raw)-32], raw[len(raw)-32:], nil
}

// LooksEncrypted reports whether a string is plausibly a NIP-44 v2 payload.
//
// This is a candidate test, not authentication: the MAC decides whether a
// payload is genuine. It exists so a plaintext reader cannot mistake one form
// for the other, because a JSON object begins with `{`, which is not in the
// base64 alphabet, and a v2 payload always begins with `A` (the base64 of the
// 0x02 version byte).
func LooksEncrypted(content string) bool {
	if content == "" {
		return false
	}

	if content[0] == '{' || content[0] == ' ' || content[0] == '\t' ||
		content[0] == '\n' || content[0] == '\r' {
		return false
	}

	if content[0] == FutureEncodingPrefix[0] {
		return false
	}

	if len(content) < MinBase64Chars || len(content) > MaxBase64Chars {
		return false
	}

	if _, err := base64.StdEncoding.DecodeString(content); err != nil {
		return false
	}

	return true
}

// DescribePayload is a short, safe description for diagnostics. It never
// includes plaintext or key material, and never panics.
func DescribePayload(payload string) string {
	nonce, ciphertext, _, err := decodePayload(payload)

	if err != nil {
		return "invalid NIP-44 payload: " + err.Error()
	}

	return fmt.Sprintf(
		"NIP-44 v%d payload, %d bytes of ciphertext, nonce %x...",
		Version, len(ciphertext), nonce[:4],
	)
}
