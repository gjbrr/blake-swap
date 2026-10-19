package coord

import (
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/wire"
)

// This file mirrors the reference's `tests/schema_equality_tests.py`: the wire schema and the
// transport validator must agree about every message type, or a message one accepts the other
// silently rejects. In this port the two are separate packages on purpose — one describes the
// fields, the other enforces the transport's identity rules — so the agreement is a property
// worth testing rather than an implementation detail.

// schemaSample is one valid message per settlement type, as the reference builds them.
func schemaSample(t *testing.T, kind wire.Kind) map[string]any {
	t.Helper()

	hash := strings.Repeat("11", 32)
	swapID := "0123456789ab"

	switch kind {
	case wire.KindOffer:
		return map[string]any{
			"type": "offer", "swap_id": swapID, "blake_amount": "10.0",
			"btc_sats": int64(25000), "secret_hash": hash,
			"btc_timeout": int64(1000), "blake_timeout": int64(1200),
			"created_at":             int64(1700000000),
			"blake_alice_pub":        "02" + strings.Repeat("22", 32),
			"btc_alice_pub":          "03" + strings.Repeat("33", 32),
			"timeout_policy_version": "1.5", "btc_timeout_window_seconds": int64(12600),
			"blake_timeout_window_seconds": int64(27600), "bob_recovery_seconds": int64(14700),
		}
	case wire.KindAccept:
		return map[string]any{
			"type": "accept", "swap_id": swapID, "secret_hash": hash,
			"blake_bob_pub": "02" + strings.Repeat("44", 32),
			"btc_bob_pub":   "03" + strings.Repeat("55", 32),
		}
	case wire.KindBlakeFunded:
		return map[string]any{
			"type": "blake-funded", "swap_id": swapID, "secret_hash": hash,
			"blake_amount": "10.0", "blake_timeout": int64(1200),
			"blake_funding_txid": strings.Repeat("66", 32), "blake_funding_vout": int64(0),
		}
	case wire.KindBTCFunded:
		return map[string]any{
			"type": "btc-funded", "swap_id": swapID, "secret_hash": hash,
			"btc_sats": int64(25000), "btc_timeout": int64(1000),
			"btc_funding_txid": strings.Repeat("66", 32), "btc_funding_vout": int64(0),
		}
	case wire.KindBTCCLaimed:
		return map[string]any{
			"type": "btc-claimed", "swap_id": swapID, "secret_hash": hash,
			"btc_claim_txid":      strings.Repeat("66", 32),
			"btc_claim_blockhash": strings.Repeat("99", 32),
		}
	case wire.KindBlakeClaimed:
		return map[string]any{
			"type": "blake-claimed", "swap_id": swapID, "secret_hash": hash,
			"blake_claim_txid":      strings.Repeat("66", 32),
			"blake_claim_blockhash": strings.Repeat("99", 32),
		}
	case wire.KindBTCRefunded:
		return map[string]any{
			"type": "btc-refunded", "swap_id": swapID, "secret_hash": hash,
			"btc_refund_txid":      strings.Repeat("66", 32),
			"btc_refund_blockhash": strings.Repeat("99", 32),
		}
	}

	// A settlement type with no sample here is a schema the test has not covered,
	// which is itself a failure: a new type must arrive with its sample.
	t.Fatalf("no sample message for settlement type %q", kind)

	return nil
}

// TestSchemaEqualityEveryTypeIsAcceptedByBoth: a schema-valid message must pass the transport
// validator, or the two disagree about what a message is.
func TestSchemaEqualityEveryTypeIsAcceptedByBoth(t *testing.T) {
	for _, kind := range wire.SettlementTypes {
		sample := schemaSample(t, kind)
		expected := kind

		if _, err := wire.Validate(sample, &expected); err != nil {
			t.Errorf("%s: the schema rejected its own sample: %v", kind, err)

			continue
		}

		swapID, _ := sample["swap_id"].(string)

		if err := Validate(swapID, string(kind), sample, false); err != nil {
			t.Errorf("%s: the transport rejected a schema-valid message: %v", kind, err)
		}
	}
}

// TestSchemaEqualityEveryRequiredFieldIsRequiredByBoth: removing a required field must be
// refused by both validators.
func TestSchemaEqualityEveryRequiredFieldIsRequiredByBoth(t *testing.T) {
	for _, kind := range wire.SettlementTypes {
		for _, field := range wire.RequiredFields(kind) {
			sample := schemaSample(t, kind)
			delete(sample, field)

			expected := kind

			if _, err := wire.Validate(sample, &expected); err == nil {
				t.Errorf("%s: the schema accepted a message with no %q", kind, field)
			}

			swapID, _ := sample["swap_id"].(string)

			if err := Validate(swapID, string(kind), sample, false); err == nil {
				t.Errorf("%s: the transport accepted a message with no %q", kind, field)
			}
		}
	}
}

// TestSchemaEqualityUnknownFieldsAreRefusedByBoth: a field neither side knows is a field one
// side will ignore, so both refuse it.
func TestSchemaEqualityUnknownFieldsAreRefusedByBoth(t *testing.T) {
	for _, kind := range wire.SettlementTypes {
		sample := schemaSample(t, kind)
		sample["unexpected_field"] = "x"

		expected := kind

		if _, err := wire.Validate(sample, &expected); err == nil {
			t.Errorf("%s: the schema accepted an unknown field", kind)
		}

		swapID, _ := sample["swap_id"].(string)

		if err := Validate(swapID, string(kind), sample, false); err == nil {
			t.Errorf("%s: the transport accepted an unknown field", kind)
		}
	}
}

// TestSchemaEqualitySecretHexIsASecurityRefusal: the secret is not a schema mistake, and the
// transport says so in the words that stop a person rather than a parser.
func TestSchemaEqualitySecretHexIsASecurityRefusal(t *testing.T) {
	sample := schemaSample(t, wire.KindAccept)
	sample["secret_hex"] = strings.Repeat("88", 32)

	if _, err := wire.Validate(sample, nil); err == nil {
		t.Fatal("the schema accepted a top-level secret_hex")
	}

	err := Validate("0123456789ab", "accept", sample, false)

	if err == nil {
		t.Fatal("the transport accepted a top-level secret_hex")
	}

	if !strings.Contains(err.Error(), "SECURITY FAILURE") || !strings.Contains(err.Error(), "secret_hex") {
		t.Fatalf("unhelpful refusal: %v", err)
	}
}

// TestSchemaEqualityInternalAnnotationsNeverTravel: the receive path attaches the verified
// sender as `_nostr_pubkey`; receipt tolerates it and sending refuses it.
func TestSchemaEqualityInternalAnnotationsNeverTravel(t *testing.T) {
	sample := schemaSample(t, wire.KindAccept)
	sample["_nostr_pubkey"] = strings.Repeat("aa", 32)

	// Receipt tolerates the annotation: the sender is authenticated metadata, not content.
	if err := Validate("0123456789ab", "accept", sample, true); err != nil {
		t.Fatalf("receipt refused the sender annotation: %v", err)
	}

	// Transmission does not: signing it would leak an engine detail onto the wire.
	err := Validate("0123456789ab", "accept", sample, false)

	if err == nil {
		t.Fatal("an engine-internal annotation was accepted for transmission")
	}

	if !strings.Contains(err.Error(), "engine-internal") {
		t.Fatalf("unhelpful refusal: %v", err)
	}
}

// TestSchemaEqualityWireViewIsTheWireFields: the canonical view of a message is the message
// without its internal keys.
func TestSchemaEqualityWireViewIsTheWireFields(t *testing.T) {
	sample := schemaSample(t, wire.KindBTCCLaimed)

	annotated := map[string]any{}

	for key, value := range sample {
		annotated[key] = value
	}

	annotated["_nostr_pubkey"] = strings.Repeat("aa", 32)
	annotated["_other"] = 1

	view := WireView(annotated)

	if len(view) != len(sample) {
		t.Fatalf("wire view has %d fields, want %d", len(view), len(sample))
	}

	for key, want := range sample {
		if view[key] != want {
			t.Errorf("%s = %v, want %v", key, view[key], want)
		}
	}
}
