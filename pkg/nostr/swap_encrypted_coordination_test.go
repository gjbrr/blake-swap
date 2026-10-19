package nostr

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/nip44"
)

// This file mirrors the reference's `tests/swap_encrypted_coordination_tests.py`: the
// confidentiality boundary of the coordination layer.
//
// The rules are four, and each of them is a way to lose a swap rather than a prettiness
// setting: first contact must be readable or the two participants never meet; everything
// after it must be opaque or a relay learns the trade; a peer that cannot read a message must
// refuse it rather than guess; and a peer that requires encryption must refuse a plaintext
// downgrade rather than treat it as ordinary.

// coordinationPair is two identities and the messages one sends the other.
func coordinationPair(t *testing.T) (*Key, *Key) {
	t.Helper()

	alice, err := NewKey(strings.Repeat("11", 32))

	if err != nil {
		t.Fatal(err)
	}

	bob, err := NewKey(strings.Repeat("22", 32))

	if err != nil {
		t.Fatal(err)
	}

	return alice, bob
}

// acceptMessage is a settlement message, which is the class that must be encrypted.
func acceptMessage(swapID string) map[string]any {
	return map[string]any{
		"type":          "accept",
		"swap_id":       swapID,
		"secret_hash":   strings.Repeat("33", 32),
		"blake_bob_pub": "02" + strings.Repeat("44", 32),
		"btc_bob_pub":   "03" + strings.Repeat("55", 32),
	}
}

// TestSwapEncryptedCoordinationFirstContactStaysPlaintext: the offer is published before the
// maker knows the taker, so there is no key to encrypt it to and it must stay readable.
func TestSwapEncryptedCoordinationFirstContactStaysPlaintext(t *testing.T) {
	alice, _ := coordinationPair(t)

	built, err := BuildCoordinationEvent(alice, "0123456789ab", "offer",
		map[string]any{
			"type": "offer", "swap_id": "0123456789ab",
			"blake_amount": "10", "btc_sats": int64(25000),
			"secret_hash": strings.Repeat("33", 32),
		}, BuildOptions{Mode: ModeNIP44})

	if err != nil {
		t.Fatalf("the offer was refused: %v", err)
	}

	if tag, ok := built.TagValue("encryption"); ok {
		t.Fatalf("the offer declared encryption %q", tag)
	}

	var message map[string]any

	if err := json.Unmarshal([]byte(built.Content), &message); err != nil {
		t.Fatalf("the offer content is not JSON: %v", err)
	}

	if message["swap_id"] != "0123456789ab" {
		t.Fatalf("the plaintext offer is not the one published: %v", message)
	}
}

// TestSwapEncryptedCoordinationSettlementIsOpaque: an accept in encrypted mode says so, and
// nothing of the message survives in its content.
func TestSwapEncryptedCoordinationSettlementIsOpaque(t *testing.T) {
	alice, bob := coordinationPair(t)

	swapID := "0123456789ab"

	built, err := BuildCoordinationEvent(bob, swapID, "accept", acceptMessage(swapID),
		BuildOptions{Mode: ModeNIP44, PeerPubkey: alice.Public})

	if err != nil {
		t.Fatalf("the accept was refused: %v", err)
	}

	if tag, ok := built.TagValue("encryption"); !ok || tag != string(ModeNIP44) {
		t.Fatalf("the accept declared %q, want nip44_v2", tag)
	}

	if strings.HasPrefix(strings.TrimSpace(built.Content), "{") {
		t.Fatal("the accept content is plaintext JSON")
	}

	// Nothing from the message may survive in the ciphertext: `swap_id` is 12 hex
	// characters that would appear literally if encryption were a no-op.
	for _, leaked := range []string{swapID, "secret_hash", "blake_bob_pub", "accept"} {
		if strings.Contains(built.Content, leaked) {
			t.Fatalf("the ciphertext contains %q", leaked)
		}
	}

	if !nip44.LooksEncrypted(built.Content) {
		t.Fatal("the accept content is not a NIP-44 payload")
	}

	// And the counterparty can read it back, which is what makes it a message rather than
	// merely opaque bytes.
	decoded, err := DecodeContent(alice, built, swapID, "accept", ModeNIP44)

	if err != nil {
		t.Fatalf("the counterparty could not read the accept: %v", err)
	}

	if decoded["blake_bob_pub"] != "02"+strings.Repeat("44", 32) {
		t.Fatalf("the decoded message is not the one sent: %v", decoded)
	}
}

// TestSwapEncryptedCoordinationPlaintextPeerRefusesEncrypted: a participant that does not
// speak NIP-44 must refuse rather than guess at content it cannot read.
func TestSwapEncryptedCoordinationPlaintextPeerRefusesEncrypted(t *testing.T) {
	alice, bob := coordinationPair(t)

	swapID := "0123456789ab"

	built, err := BuildCoordinationEvent(bob, swapID, "accept", acceptMessage(swapID),
		BuildOptions{Mode: ModeNIP44, PeerPubkey: alice.Public})

	if err != nil {
		t.Fatal(err)
	}

	if _, err := DecodeContent(alice, built, swapID, "accept", ModeOff); err == nil ||
		!strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("a plaintext peer accepted an encrypted message: %v", err)
	}
}

// TestSwapEncryptedCoordinationEncryptedPeerRefusesPlaintext: the direction that matters. Once
// first contact is past, a participant that requires encryption must refuse a plaintext
// message rather than treat it as ordinary.
func TestSwapEncryptedCoordinationEncryptedPeerRefusesPlaintext(t *testing.T) {
	alice, bob := coordinationPair(t)

	swapID := "0123456789ab"

	built, err := BuildCoordinationEvent(bob, swapID, "accept", acceptMessage(swapID),
		BuildOptions{Mode: ModeOff})

	if err != nil {
		t.Fatal(err)
	}

	if tag, ok := built.TagValue("encryption"); ok {
		t.Fatalf("the plaintext accept declared encryption %q", tag)
	}

	if _, err := DecodeContent(alice, built, swapID, "accept", ModeNIP44); err == nil {
		t.Fatal("an encryption-requiring peer accepted a plaintext downgrade")
	}
}

// TestSwapEncryptedCoordinationUntaggedPayloadIsRefused: content that looks encrypted but
// declares nothing is refused while encryption is required — the tag has to be inside the
// signature, so an untagged payload is a payload nobody vouched for.
func TestSwapEncryptedCoordinationUntaggedPayloadIsRefused(t *testing.T) {
	alice, bob := coordinationPair(t)

	swapID := "0123456789ab"

	built, err := BuildCoordinationEvent(bob, swapID, "accept", acceptMessage(swapID),
		BuildOptions{Mode: ModeNIP44, PeerPubkey: alice.Public})

	if err != nil {
		t.Fatal(err)
	}

	stripped := built

	stripped.Tags = nil

	for _, tag := range built.Tags {
		if len(tag) > 0 && tag[0] == "encryption" {
			continue
		}

		stripped.Tags = append(stripped.Tags, tag)
	}

	if _, err := DecodeContent(alice, stripped, swapID, "accept", ModeNIP44); err == nil {
		t.Fatal("an untagged encrypted payload was accepted")
	}
}

// TestSwapEncryptedCoordinationPlaintextParsesWhenOff: with encryption off, an ordinary
// plaintext message is exactly what it says it is.
func TestSwapEncryptedCoordinationPlaintextParsesWhenOff(t *testing.T) {
	alice, bob := coordinationPair(t)

	swapID := "0123456789ab"

	built, err := BuildCoordinationEvent(bob, swapID, "accept", acceptMessage(swapID),
		BuildOptions{Mode: ModeOff})

	if err != nil {
		t.Fatal(err)
	}

	decoded, err := DecodeContent(alice, built, swapID, "accept", ModeOff)

	if err != nil {
		t.Fatalf("a plaintext message was refused with encryption off: %v", err)
	}

	if decoded["type"] != "accept" {
		t.Fatalf("decoded = %v", decoded)
	}
}

// TestSwapEncryptedCoordinationBadModeIsRefused: a mode this engine does not implement is a
// configuration error, not something to guess at.
func TestSwapEncryptedCoordinationBadModeIsRefused(t *testing.T) {
	if _, err := ParseMode("rot13"); err == nil {
		t.Fatal("an unknown encryption mode was accepted")
	}

	for _, known := range []string{"off", "nip44_v2"} {
		if _, err := ParseMode(known); err != nil {
			t.Errorf("%q: %v", known, err)
		}
	}

	// And an unsupported declared encryption on an event is a security refusal rather than a
	// relay artefact.
	alice, bob := coordinationPair(t)

	swapID := "0123456789ab"

	built, err := BuildCoordinationEvent(bob, swapID, "accept", acceptMessage(swapID),
		BuildOptions{Mode: ModeOff, CreatedAt: 1})

	if err != nil {
		t.Fatal(err)
	}

	declared := built

	declared.Tags = nil

	for _, tag := range built.Tags {
		if len(tag) > 0 && tag[0] == "encryption" {
			continue
		}

		declared.Tags = append(declared.Tags, tag)
	}

	declared.Tags = append(declared.Tags, []string{"encryption", "aes-gcm"})

	if _, err := DecodeContent(alice, declared, swapID, "accept", ModeOff); err == nil {
		t.Fatal("an unsupported declared encryption was accepted")
	}
}

// unused import guard: event is used through the helpers above.
var _ = event.KindTextNote
