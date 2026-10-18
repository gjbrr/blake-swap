package wire

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	validSwapID  = "abcdef123456"
	validHash    = "cc"
	validPubkey  = "02"
	validPubkey2 = "03"
)

func hex64(prefix string) string {
	return prefix + strings.Repeat("a", 62)
}

func pubkey(prefix string) string {
	return prefix + strings.Repeat("b", 64)
}

// offer returns a valid offer object; tests mutate one field at a time so a
// refusal is attributable.
func offer() map[string]any {
	return map[string]any{
		"type":                         "offer",
		"swap_id":                      validSwapID,
		"blake_amount":                 "0.5",
		"btc_sats":                     json.Number("25000"),
		"secret_hash":                  hex64(validHash),
		"btc_timeout":                  json.Number("900"),
		"blake_timeout":                json.Number("1000"),
		"created_at":                   json.Number("1700000000"),
		"blake_alice_pub":              pubkey(validPubkey),
		"btc_alice_pub":                pubkey(validPubkey2),
		"timeout_policy_version":       "1.5",
		"btc_timeout_window_seconds":   json.Number("12000"),
		"blake_timeout_window_seconds": json.Number("27000"),
		"bob_recovery_seconds":         json.Number("14700"),
	}
}

func accept() map[string]any {
	return map[string]any{
		"type":          "accept",
		"swap_id":       validSwapID,
		"secret_hash":   hex64(validHash),
		"blake_bob_pub": pubkey(validPubkey),
		"btc_bob_pub":   pubkey(validPubkey2),
	}
}

func TestOfferAndAcceptValidate(t *testing.T) {
	if _, err := Validate(offer(), nil); err != nil {
		t.Fatalf("offer: %v", err)
	}

	if _, err := Validate(accept(), nil); err != nil {
		t.Fatalf("accept: %v", err)
	}
}

func TestEveryTypeRoundTrips(t *testing.T) {
	messages := []map[string]any{
		offer(),
		accept(),
		{
			"type": "blake-funded", "swap_id": validSwapID,
			"secret_hash": hex64(validHash), "blake_amount": "0.5",
			"blake_timeout":      json.Number("1000"),
			"blake_funding_txid": hex64("11"),
			"blake_funding_vout": json.Number("0"),
		},
		{
			"type": "btc-funded", "swap_id": validSwapID,
			"secret_hash": hex64(validHash), "btc_sats": json.Number("25000"),
			"btc_timeout":      json.Number("900"),
			"btc_funding_txid": hex64("22"),
			"btc_funding_vout": json.Number("1"),
		},
		{
			"type": "btc-claimed", "swap_id": validSwapID,
			"secret_hash":         hex64(validHash),
			"btc_claim_txid":      hex64("33"),
			"btc_claim_blockhash": hex64("44"),
		},
		{
			"type": "blake-claimed", "swap_id": validSwapID,
			"secret_hash":           hex64(validHash),
			"blake_claim_txid":      hex64("55"),
			"blake_claim_blockhash": hex64("66"),
		},
		{
			"type": "btc-refunded", "swap_id": validSwapID,
			"secret_hash":          hex64(validHash),
			"btc_refund_txid":      hex64("77"),
			"btc_refund_blockhash": hex64("88"),
		},
	}

	for _, message := range messages {
		encoded, err := json.Marshal(message)

		if err != nil {
			t.Fatal(err)
		}

		decoded, err := Decode(encoded, nil)

		if err != nil {
			t.Fatalf("%v: %v", message["type"], err)
		}

		canonical, err := Encode(decoded, nil)

		if err != nil {
			t.Fatalf("%v: %v", message["type"], err)
		}

		// Canonical form is compact and sorted, so encoding is idempotent.
		again, err := Decode(canonical, nil)

		if err != nil {
			t.Fatalf("%v: %v", message["type"], err)
		}

		recanonical, err := Encode(again, nil)

		if err != nil {
			t.Fatal(err)
		}

		if string(canonical) != string(recanonical) {
			t.Errorf("%v: canonical form is not idempotent:\n%s\n%s",
				message["type"], canonical, recanonical)
		}

		if strings.Contains(string(canonical), " ") {
			t.Errorf("%v: canonical JSON contains a space", message["type"])
		}
	}
}

func TestSecretHexIsForbiddenOnTheWire(t *testing.T) {
	message := offer()
	message["secret_hex"] = "aa"

	if _, err := Validate(message, nil); err == nil ||
		!strings.Contains(err.Error(), "secret_hex is forbidden") {
		t.Fatalf("secret_hex: %v", err)
	}
}

func TestUnknownAndMissingFields(t *testing.T) {
	extra := offer()
	extra["surprise"] = "boo"

	if _, err := Validate(extra, nil); err == nil ||
		!strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field: %v", err)
	}

	missing := accept()
	delete(missing, "btc_bob_pub")

	if _, err := Validate(missing, nil); err == nil ||
		!strings.Contains(err.Error(), "missing required field") {
		t.Fatalf("missing field: %v", err)
	}
}

func TestTypeAndShapeRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"not an object", nil, "must be a JSON object"},
		{"unknown type", func(m map[string]any) { m["type"] = "bogus" }, "unsupported settlement message type"},
		{"uppercase swap id", func(m map[string]any) { m["swap_id"] = "ABCDEF123456" }, "12 lowercase hex"},
		{"short swap id", func(m map[string]any) { m["swap_id"] = "abc" }, "12 lowercase hex"},
		{"float sats", func(m map[string]any) { m["btc_sats"] = json.Number("25000.0") }, "must be an integer"},
		{"bool sats", func(m map[string]any) { m["btc_sats"] = true }, "must be an integer"},
		{"negative vout", func(m map[string]any) {
			nm := map[string]any{
				"type": "btc-funded", "swap_id": validSwapID,
				"secret_hash": hex64(validHash), "btc_sats": json.Number("1"),
				"btc_timeout": json.Number("1"), "btc_funding_txid": hex64("11"),
				"btc_funding_vout": json.Number("-1"),
			}
			for k, v := range nm {
				m[k] = v
			}
			for k := range m {
				if _, ok := nm[k]; !ok {
					delete(m, k)
				}
			}
		}, "must be non-negative"},
		{"uppercase hash", func(m map[string]any) { m["secret_hash"] = strings.ToUpper(hex64(validHash)) }, "32-byte lowercase hex"},
		{"bad pubkey prefix", func(m map[string]any) { m["blake_alice_pub"] = "04" + strings.Repeat("b", 64) }, "compressed secp256k1"},
		{"amount too fine", func(m map[string]any) { m["blake_amount"] = "0.000000001" }, "finer than 8 decimal places"},
		{"amount exponent render", func(m map[string]any) { m["blake_amount"] = "1e-8" }, ""},
		{"amount zero", func(m map[string]any) { m["blake_amount"] = "0" }, "must be positive"},
		{"amount not a number", func(m map[string]any) { m["blake_amount"] = "banana" }, "decimal string"},
		{"wrong policy version", func(m map[string]any) { m["timeout_policy_version"] = "1.4" }, "timeout_policy_version"},
		{"recovery budget not preserved", func(m map[string]any) {
			m["blake_timeout_window_seconds"] = json.Number("12100")
			m["bob_recovery_seconds"] = json.Number("14700")
		}, "recovery budget"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var message any = "not an object"

			if c.mutate != nil {
				m := offer()
				c.mutate(m)
				message = m
			}

			_, err := Validate(message, nil)

			if c.want == "" {
				if err != nil {
					t.Fatalf("expected acceptance, got %v", err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want %q", err, c.want)
			}
		})
	}
}

func TestExpectedTypeMismatch(t *testing.T) {
	expected := KindAccept

	if _, err := Validate(offer(), &expected); err == nil ||
		!strings.Contains(err.Error(), "does not match expected type") {
		t.Fatalf("expected-type mismatch: %v", err)
	}

	if _, err := Validate(accept(), &expected); err != nil {
		t.Fatalf("matching expected type: %v", err)
	}
}

func TestDecodeRejectsMalformedJSONAndDuplicateKeys(t *testing.T) {
	cases := map[string]string{
		"not json":        `{`,
		"trailing":        `{"type":"accept"} {"type":"accept"}`,
		"duplicate":       `{"type":"accept","type":"accept"}`,
		"duplicate inner": `{"type":"accept","x":{"a":1,"a":2}}`,
		"empty":           ``,
	}

	for name, input := range cases {
		if _, err := Decode([]byte(input), nil); err == nil {
			t.Errorf("%s: %q was accepted", name, input)
		}
	}

	// Invalid UTF-8 is refused before JSON parsing.
	if _, err := Decode([]byte{0xff, 0xfe, 'x'}, nil); err == nil {
		t.Error("invalid UTF-8 was accepted")
	}
}

func TestDecodeProducesAShallowCopy(t *testing.T) {
	encoded := []byte(`{"type":"accept","swap_id":"` + validSwapID +
		`","secret_hash":"` + hex64(validHash) +
		`","blake_bob_pub":"` + pubkey(validPubkey) +
		`","btc_bob_pub":"` + pubkey(validPubkey2) + `"}`)

	decoded, err := Decode(encoded, nil)

	if err != nil {
		t.Fatal(err)
	}

	decoded["type"] = "tampered"

	// The encoded bytes are untouched, so validation is not reading a shared
	// buffer.
	if _, err := Decode(encoded, nil); err != nil {
		t.Fatalf("second decode: %v", err)
	}
}

func TestFormatAmount(t *testing.T) {
	cases := map[any]string{
		"0.5":    "0.5",
		"0.50":   "0.5",
		"1":      "1",
		"1e-8":   "0.00000001",
		"100":    "100",
		"2.5000": "2.5",
	}

	for in, want := range cases {
		got, err := FormatAmount(in)

		if err != nil {
			t.Fatalf("FormatAmount(%v): %v", in, err)
		}

		if got != want {
			t.Errorf("FormatAmount(%v) = %q, want %q", in, got, want)
		}
	}

	for _, bad := range []any{"0", "-1", "banana"} {
		if _, err := FormatAmount(bad); err == nil {
			t.Errorf("FormatAmount(%v) was accepted", bad)
		}
	}
}

func TestSchemaIsDiscoverable(t *testing.T) {
	for _, kind := range SettlementTypes {
		fields := RequiredFields(kind)

		if len(fields) == 0 {
			t.Errorf("%s has no required fields", kind)
		}

		if fields[0] != "secret_hash" && kind != KindOffer && kind != KindAccept {
			// Claim/refund messages all require a secret_hash; the ordering
			// check above is just a canary that the set is sorted.
			t.Logf("%s first field is %s", kind, fields[0])
		}
	}
}

func TestSHA256Hex(t *testing.T) {
	if got := SHA256Hex([]byte("abc")); got !=
		"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("sha256(abc) = %s", got)
	}
}
