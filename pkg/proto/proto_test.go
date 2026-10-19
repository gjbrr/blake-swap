package proto

import (
	"strings"
	"testing"
)

// Parity: tests/schema_equality_tests.py
func TestEnvelopeRoundTrip(t *testing.T) {
	sent := Envelope{
		Kind: HelloAck,
		Seq:  7,
		Body: "hello world",
		AtMS: 1234,
	}

	text, err := Encode(sent)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	got, err := Decode(text)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got != sent {
		t.Fatalf("round trip changed the envelope: %+v != %+v", got, sent)
	}
}

func TestDecodeRejectsWhatThePeerMustNeverSend(t *testing.T) {
	for _, bad := range []string{
		``,
		`not json`,
		`{}`,
		`{"kind":""}`,
		`{"kind":"hello"`,
	} {
		if _, err := Decode(bad); err == nil {
			t.Errorf("Decode(%q) accepted a message it should refuse", bad)
		}
	}
}

func TestEncodedShapeIsStable(t *testing.T) {
	text, err := Encode(Envelope{Kind: Hello, Seq: 1})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	//
	// The bootstrap JS and any future adapter read this shape, so pin it:
	// a rename here is a protocol change, not a refactor.
	//
	for _, want := range []string{`"kind":"hello"`, `"seq":1`} {
		if !strings.Contains(text, want) {
			t.Errorf("encoded envelope %q is missing %q", text, want)
		}
	}
}
