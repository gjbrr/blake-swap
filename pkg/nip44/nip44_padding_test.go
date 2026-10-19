package nip44

import (
	"crypto/rand"
	"strings"
	"testing"
)

// This file mirrors the padding half of the reference's `tests/nip44_tests.py`: the payload
// length must reveal only which padding bucket the plaintext fell in, never its exact size.
// Our payloads come from a NIP-44 library rather than this package, which is exactly why the
// property is worth asserting here — an integration that stops padding would still encrypt
// and decrypt correctly, and would still leak.

// paddedPayload encrypts n bytes of a fixed plaintext and returns the payload length.
func paddedPayload(t *testing.T, n int) int {
	t.Helper()

	key := make([]byte, 32)

	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}

	plaintext := strings.Repeat("a", n)

	payload, err := Encrypt(key, []byte(plaintext), nil)

	if err != nil {
		t.Fatalf("encrypting %d bytes: %v", n, err)
	}

	return len(payload)
}

// TestNIP44PaddedLengthsShareBuckets: plaintexts inside one bucket produce payloads of equal
// length, and a plaintext that crosses a boundary produces a longer one.
func TestNIP44PaddedLengthsShareBuckets(t *testing.T) {
	// The reference's bucket edges: 32 bytes up to 32, 64 up to 64, then 96, 128, …
	short := paddedPayload(t, 16)
	full := paddedPayload(t, 32)

	if short != full {
		t.Fatalf("16 and 32 bytes left the same bucket of different sizes: %d vs %d", short, full)
	}

	next := paddedPayload(t, 33)

	if next <= full {
		t.Fatalf("33 bytes did not move to the next bucket: %d vs %d", next, full)
	}

	// And a much longer plaintext is still greater: padding never inverts ordering.
	longer := paddedPayload(t, 200)

	if longer <= next {
		t.Fatalf("200 bytes is not longer than 33: %d vs %d", longer, next)
	}
}

// TestNIP44RefusesAnEmptyPlaintext: an empty message is not a message, and encrypting one
// produces a payload no reader can distinguish from a truncation.
func TestNIP44RefusesAnEmptyPlaintext(t *testing.T) {
	key := make([]byte, 32)

	if _, err := Encrypt(key, []byte{}, nil); err == nil {
		t.Fatal("an empty plaintext was encrypted")
	}
}
