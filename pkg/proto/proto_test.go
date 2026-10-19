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

// TestResultDataRoundTrips keeps the structured half of a result intact across the
// link. Text and Data travel together: the CLI's wording for a terminal and a bug
// report, the view for the document.
func TestResultDataRoundTrips(t *testing.T) {
	result := Result{Command: "engine.status", OK: true, Text: "BLAKE height: 1"}

	result, err := WithData(result, map[string]int{"blake": 1, "btc": 2})

	if err != nil {
		t.Fatalf("attach data: %v", err)
	}

	if result.Text != "BLAKE height: 1" {
		t.Fatal("attach data changed the text")
	}

	body, err := EncodeResult(result)

	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	decoded, err := DecodeResult(body)

	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	var heights map[string]int

	if err := DecodeData(decoded, &heights); err != nil {
		t.Fatalf("decode data: %v", err)
	}

	if heights["blake"] != 1 || heights["btc"] != 2 {
		t.Fatalf("data changed: %+v", heights)
	}
}

// TestResultWithoutDataIsNotAnError: a command with no view is a command, not a failure.
// The page must be able to ask "is there a view" without treating the answer as one.
func TestResultWithoutDataIsNotAnError(t *testing.T) {
	result := Result{Command: "vault.lock", OK: true, Text: "vault locked"}

	var view map[string]string

	if err := DecodeData(result, &view); err == nil {
		t.Fatal("a result with no data decoded as if it had some")
	}

	// The envelope still round-trips: an absent Data field is omitted, not null.
	body, err := EncodeResult(result)

	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(body, `"data"`) {
		t.Fatalf("an empty data field was encoded: %s", body)
	}
}

// TestWithDataRefusesWhatCannotBeEncoded: a view that will not marshal loses the view and
// keeps the command, rather than failing a command that already succeeded.
func TestWithDataRefusesWhatCannotBeEncoded(t *testing.T) {
	result := Result{Command: "engine.show", OK: true, Text: "swap id: 1"}

	got, err := WithData(result, make(chan int))

	if err == nil {
		t.Fatal("a channel was encoded as a view")
	}

	if got.Text != result.Text || len(got.Data) != 0 {
		t.Fatalf("a failed attach changed the result: %+v", got)
	}
}
