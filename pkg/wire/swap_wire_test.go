package wire

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// This file mirrors the reference's `tests/swap_wire_tests.py`: the same message, the same
// refusals, one assertion per contract rule. It exists because the wire schema is shared with
// the Python engine, and a rule that only one implementation enforces is not a rule.

// referenceOffer is the suite's v1.5 offer, with the metadata that makes its window coherent.
func referenceOffer() map[string]any {
	return map[string]any{
		"type":                         "offer",
		"swap_id":                      "0123456789ab",
		"blake_amount":                 "10.0",
		"btc_sats":                     int64(25000),
		"secret_hash":                  strings.Repeat("11", 32),
		"btc_timeout":                  int64(1000),
		"blake_timeout":                int64(1200),
		"created_at":                   int64(1700000000),
		"blake_alice_pub":              "02" + strings.Repeat("22", 32),
		"btc_alice_pub":                "03" + strings.Repeat("33", 32),
		"timeout_policy_version":       "1.5",
		"btc_timeout_window_seconds":   int64(12600),
		"blake_timeout_window_seconds": int64(27600),
		"bob_recovery_seconds":         int64(14700),
	}
}

// expectWireReject asserts that a message is refused.
func expectWireReject(t *testing.T, name string, message any) {
	t.Helper()

	kind := Kind("offer")

	if _, err := Validate(message, &kind); err == nil {
		t.Errorf("%s: was accepted", name)
	}
}

// TestSwapWireAcceptsTheReferenceOffer: the healthy message every other check is a mutation of.
func TestSwapWireAcceptsTheReferenceOffer(t *testing.T) {
	offer := referenceOffer()
	kind := Kind("offer")

	if _, err := Validate(offer, &kind); err != nil {
		t.Fatalf("a valid v1.5 offer was refused: %v", err)
	}
}

// TestSwapWireCanonicalEncodingIsSortedAndCompact: the encoded form is what the event id is
// computed over, so it must be canonical: sorted keys, no spaces between fields.
func TestSwapWireCanonicalEncodingIsSortedAndCompact(t *testing.T) {
	offer := referenceOffer()

	encoded, err := Encode(offer, nil)

	if err != nil {
		t.Fatal(err)
	}

	var keys []string

	if err := json.Unmarshal(encoded, &map[string]any{}); err != nil {
		t.Fatal(err)
	}

	// The byte-level property: keys in sorted order, and no whitespace after separators.
	text := string(encoded)

	if strings.Contains(text, ": ") || strings.Contains(text, ", ") {
		t.Fatalf("encoded form is not compact: %s", text)
	}

	keys = jsonKeys(t, encoded)

	if !sort.StringsAreSorted(keys) {
		t.Fatalf("keys are not sorted: %v", keys)
	}
}

// TestSwapWireRoundTrips: decode(encode(message)) is the message.
func TestSwapWireRoundTrips(t *testing.T) {
	offer := referenceOffer()

	encoded, err := Encode(offer, nil)

	if err != nil {
		t.Fatal(err)
	}

	decoded, err := Decode(encoded, nil)

	if err != nil {
		t.Fatal(err)
	}

	if len(decoded) != len(offer) {
		t.Fatalf("round trip lost fields: %d vs %d", len(decoded), len(offer))
	}

	// Decoding produces JSON numbers, so values are compared by their rendering rather
	// than by Go type: 25000 and 25000.0 are the same wire value.
	for key, want := range offer {
		if fmt.Sprint(decoded[key]) != fmt.Sprint(want) {
			t.Errorf("%s = %v, want %v", key, decoded[key], want)
		}
	}
}

// TestSwapWireRefusesEveryBrokenOfferRule: one refusal per rule the reference names.
func TestSwapWireRefusesEveryBrokenOfferRule(t *testing.T) {
	broken := map[string]func(map[string]any){
		"secret_hex is never on the wire": func(m map[string]any) {
			m["secret_hex"] = strings.Repeat("aa", 32)
		},
		"malformed swap id":     func(m map[string]any) { m["swap_id"] = "not-a-swap" },
		"malformed secret hash": func(m map[string]any) { m["secret_hash"] = "xyz" },
		"boolean integer":       func(m map[string]any) { m["btc_sats"] = true },
		"zero BTC amount":       func(m map[string]any) { m["btc_sats"] = int64(0) },
		"unknown timeout policy": func(m map[string]any) {
			m["timeout_policy_version"] = "9.9"
		},
		"incomplete v1.5 metadata": func(m map[string]any) {
			delete(m, "bob_recovery_seconds")
		},
		"incoherent recovery budget": func(m map[string]any) {
			m["bob_recovery_seconds"] = int64(16000)
		},
		"unknown field": func(m map[string]any) { m["surprise"] = "field" },
	}

	for name, mutate := range broken {
		offer := referenceOffer()
		mutate(offer)
		expectWireReject(t, name, offer)
	}
}

// TestSwapWireRefusesVersionlessTimeoutMetadata: a message with no policy version but some
// window fields is not a legacy message, it is an incoherent one.
func TestSwapWireRefusesVersionlessTimeoutMetadata(t *testing.T) {
	legacy := referenceOffer()

	for _, field := range []string{
		"timeout_policy_version", "btc_timeout_window_seconds",
		"blake_timeout_window_seconds", "bob_recovery_seconds",
	} {
		delete(legacy, field)
	}

	expectWireReject(t, "versionless timeout offer", legacy)

	orphan := map[string]any{}
	for key, value := range legacy {
		orphan[key] = value
	}

	orphan["btc_timeout_window_seconds"] = int64(100)

	expectWireReject(t, "orphan timeout metadata", orphan)
}

// TestSwapWireAcceptsTheOtherSettlementMessages: accept and the funding announcement are the
// two other small shapes, and both are legal as the reference sends them.
func TestSwapWireAcceptsTheOtherSettlementMessages(t *testing.T) {
	accept := map[string]any{
		"type":          "accept",
		"swap_id":       "0123456789ab",
		"secret_hash":   strings.Repeat("11", 32),
		"blake_bob_pub": "02" + strings.Repeat("44", 32),
		"btc_bob_pub":   "03" + strings.Repeat("55", 32),
	}

	if _, err := Validate(accept, nil); err != nil {
		t.Fatalf("a valid accept was refused: %v", err)
	}

	funding := map[string]any{
		"type":             "btc-funded",
		"swap_id":          "0123456789ab",
		"secret_hash":      strings.Repeat("11", 32),
		"btc_sats":         int64(25000),
		"btc_timeout":      int64(1000),
		"btc_funding_txid": strings.Repeat("66", 32),
		"btc_funding_vout": int64(0),
	}

	if _, err := Validate(funding, nil); err != nil {
		t.Fatalf("a valid BTC funding announcement was refused: %v", err)
	}
}

// TestSwapWireRefusesDuplicateJSONKeys: a document that names the same field twice has no single
// reading, so it is refused rather than resolved by whichever parser reads it.
func TestSwapWireRefusesDuplicateJSONKeys(t *testing.T) {
	duplicate := `{"type":"accept","type":"offer","swap_id":"0123456789ab"}`

	if _, err := Decode([]byte(duplicate), nil); err == nil {
		t.Fatal("a duplicate key was accepted")
	}
}

// TestSwapWireRefusesStructuralGarbage: the input is not trusted to be an object at all.
func TestSwapWireRefusesStructuralGarbage(t *testing.T) {
	cases := map[string]any{
		"a non-object message": "not a dict",
		"a list message":       []any{},
		"a null message":       nil,
	}

	for name, message := range cases {
		expectWireReject(t, name, message)
	}

	for name, raw := range map[string][]byte{
		"invalid UTF-8":  {0xff, 0xfe},
		"malformed JSON": []byte("{not json"),
	} {
		if _, err := Decode(raw, nil); err == nil {
			t.Errorf("%s: was accepted", name)
		}
	}
}

// TestFormatAmountKeepsEightDecimals: an amount is a decimal on the wire, and rounding it is
// losing money in a place nobody looks.
func TestFormatAmountKeepsEightDecimals(t *testing.T) {
	cases := map[any]string{
		"10":         "10",
		"10.0":       "10",
		"0.00000001": "0.00000001",
		int64(25):    "25",
	}

	for input, want := range cases {
		got, err := FormatAmount(input)

		if err != nil {
			t.Errorf("%v: %v", input, err)

			continue
		}

		if got != want {
			t.Errorf("%v = %q, want %q", input, got, want)
		}
	}
}

// TestSHA256HexIsTheReferenceDigest: the wire's digest helper is the same function the
// protocol uses for the hashlock, so it is checked against a known answer.
func TestSHA256HexIsTheReferenceDigest(t *testing.T) {
	got := SHA256Hex([]byte("blake-swap"))

	// The digest is computed rather than remembered: a hard-coded expectation would only
	// duplicate the implementation. What is worth checking is that it is the standard
	// SHA-256, which the reference's own hashlock depends on.
	want := sha256.Sum256([]byte("blake-swap"))

	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("digest = %q", got)
	}

	if len(got) != 64 {
		t.Fatalf("not a hex digest: %q", got)
	}
}

// jsonKeys reads a JSON document's top-level keys in the order they appear.
func jsonKeys(t *testing.T, raw []byte) []string {
	t.Helper()

	decoder := json.NewDecoder(bytes.NewReader(raw))

	token, err := decoder.Token()

	if err != nil {
		t.Fatal(err)
	}

	if token != json.Delim('{') {
		t.Fatalf("not an object: %s", raw)
	}

	var keys []string

	for decoder.More() {
		token, err := decoder.Token()

		if err != nil {
			t.Fatal(err)
		}

		key, ok := token.(string)

		if !ok {
			t.Fatalf("not a key: %v", token)
		}

		keys = append(keys, key)

		var skip any

		if err := decoder.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}

	return keys
}
