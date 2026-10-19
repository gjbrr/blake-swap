package event

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/keys"
)

// The id below was computed by the Python implementation's nostr_event_id for
// exactly these values, so it pins the canonical serialization to the other
// side of the port.
const (
	pythonContent = `{"blake_amount":"0.5","swap_id":"abcdef123456","type":"offer"}`
	pythonEventID = "5036fb6d498e835c6f7354ffbeced8e81f4638fac733f0971c1edf9942fb6e29"
	pythonPubkey  = "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
)

func pythonEvent() Event {
	return Event{
		Pubkey:    pythonPubkey,
		CreatedAt: 1700000000,
		Kind:      KindTextNote,
		Tags: [][]string{
			{"t", "blake-swap"},
			{"d", "abcdef123456"},
			{"type", "offer"},
		},
		Content: pythonContent,
	}
}

// Parity: tests/fail_closed_tests.py
func TestEventIDMatchesPython(t *testing.T) {
	id, err := pythonEvent().ComputeID()

	if err != nil {
		t.Fatal(err)
	}

	if id != pythonEventID {
		t.Fatalf("event id = %s, want %s", id, pythonEventID)
	}
}

func TestSerializeShape(t *testing.T) {
	serialized, err := pythonEvent().Serialize()

	if err != nil {
		t.Fatal(err)
	}

	want := `[0,"` + pythonPubkey + `",1700000000,1,[["t","blake-swap"],["d","abcdef123456"],["type","offer"]],` +
		`"` + strings.ReplaceAll(pythonContent, `"`, `\"`) + `"]`

	if string(serialized) != want {
		t.Fatalf("serialized =\n%s\nwant\n%s", serialized, want)
	}
}

func TestSignAndVerify(t *testing.T) {
	key, err := keys.FromSecretHex("1111111111111111111111111111111111111111111111111111111111111111")

	if err != nil {
		t.Fatal(err)
	}

	content := []byte(`{"type":"accept","swap_id":"abcdef123456"}`)
	tags := CoordinationTags("abcdef123456", "accept")

	event, err := Build(key.PublicHex(), WithContent(content), WithTags(tags), WithCreatedAt(1700000000))

	if err != nil {
		t.Fatal(err)
	}

	if err := Sign(&event, key); err != nil {
		t.Fatal(err)
	}

	if len(event.ID) != 64 || len(event.Sig) != 128 {
		t.Fatalf("id/sig lengths %d/%d", len(event.ID), len(event.Sig))
	}

	now := time.Unix(1700000100, 0)

	if err := Verify(event, now); err != nil {
		t.Fatalf("valid event was refused: %v", err)
	}

	// The id is the hash of the serialized event, so recomputing it matches.
	computed, err := event.ComputeID()

	if err != nil {
		t.Fatal(err)
	}

	if computed != event.ID {
		t.Fatal("id is not the hash of the serialization")
	}
}

func TestVerifyRefusals(t *testing.T) {
	key, _ := keys.FromSecretHex(strings.Repeat("2222222222222222222222222222222222222222222222222222222222222222", 1))

	event, _ := Build(key.PublicHex(), WithContent([]byte(`{"a":"b"}`)), WithCreatedAt(1700000000))

	if err := Sign(&event, key); err != nil {
		t.Fatal(err)
	}

	futureNow := time.Unix(1700000100, 0)

	cases := []struct {
		name   string
		mutate func(*Event)
		want   string
	}{
		{"tampered content", func(e *Event) { e.Content = `{"a":"c"}` }, "ID verification failed"},
		{"tampered tags", func(e *Event) { e.Tags = [][]string{{"t", "other"}} }, "ID verification failed"},
		{"tampered timestamp", func(e *Event) { e.CreatedAt = 1700000001 }, "ID verification failed"},
		{"tampered id", func(e *Event) { e.ID = strings.Repeat("00", 32) }, "ID verification failed"},
		{"tampered signature", func(e *Event) {
			raw, _ := hex.DecodeString(e.Sig)
			raw[63] ^= 1
			e.Sig = hex.EncodeToString(raw)
		}, "signature verification failed"},
		{"wrong kind", func(e *Event) { e.Kind = 4 }, "kind is not 1"},
		{"short signature", func(e *Event) { e.Sig = "abcd" }, "malformed"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			broken := event
			c.mutate(&broken)

			err := Verify(broken, futureNow)

			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want %q", err, c.want)
			}
		})
	}

	//
	// A timestamp beyond the skew bound is refused even though the signature
	// is valid: authorship is not timeliness. The event must be signed *after*
	// the timestamp is set, otherwise the id check fires first and the skew
	// bound is never exercised.
	//
	futureTimestamp := futureNow.Unix() + FutureSkewSeconds + 1

	tooNew, err := Build(key.PublicHex(),
		WithContent([]byte(`{"a":"b"}`)),
		WithCreatedAt(futureTimestamp),
	)

	if err != nil {
		t.Fatal(err)
	}

	if err := Sign(&tooNew, key); err != nil {
		t.Fatal(err)
	}

	if err := Verify(tooNew, futureNow); err == nil ||
		!strings.Contains(err.Error(), "too far in the future") {
		t.Fatalf("future-dated event: %v", err)
	}
}

func TestSignRefusesMismatchedKey(t *testing.T) {
	real, _ := keys.FromSecretHex(strings.Repeat("33", 32))
	other, _ := keys.FromSecretHex(strings.Repeat("44", 32))

	event, _ := Build(other.PublicHex(), WithContent([]byte(`{}`)))

	if err := Sign(&event, real); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched key: %v", err)
	}
}

func TestBuildRefusesEmptyContent(t *testing.T) {
	if _, err := Build(pythonPubkey); err == nil {
		t.Fatal("an event with no content was built")
	}
}

func TestTags(t *testing.T) {
	event := Event{Tags: [][]string{
		{"t", "blake-swap"},
		{"d", "abcdef123456"},
		{"type", "offer"},
		{"e", strings.Repeat("11", 32)},
	}}

	if value, ok := event.TagValue("type"); !ok || value != "offer" {
		t.Fatalf("type tag = %q %v", value, ok)
	}

	if value, ok := event.TagValue("missing"); ok {
		t.Fatalf("missing tag returned %q", value)
	}

	if !event.HasTag("t", "blake-swap") {
		t.Fatal("topic tag not found")
	}

	if event.HasTag("t", "other") {
		t.Fatal("wrong topic matched")
	}

	// Coordination tags have a fixed shape.
	tags := CoordinationTags("abcdef123456", "offer")

	if len(tags) != 3 || tags[0][1] != TopicSwap || tags[1][1] != "abcdef123456" || tags[2][1] != "offer" {
		t.Fatalf("coordination tags = %v", tags)
	}

	// Market tags include the direction only when one is given.
	if tags := MarketTags(TopicMarket, ""); len(tags) != 1 {
		t.Fatalf("market tags without direction = %v", tags)
	}

	if tags := MarketTags(TopicMarket, "btc-for-blake"); len(tags) != 2 ||
		tags[1][1] != "btc-for-blake" {
		t.Fatalf("market tags with direction = %v", tags)
	}
}

func TestDedupeTags(t *testing.T) {
	tags, err := DedupeTags([][]string{
		{"t", "blake-swap", ""},
		{"d", "", "abcdef123456"},
	})

	if err != nil {
		t.Fatal(err)
	}

	if len(tags[0]) != 2 || len(tags[1]) != 2 {
		t.Fatalf("dedupe left empties: %v", tags)
	}

	if _, err := DedupeTags([][]string{{"", ""}}); err == nil {
		t.Fatal("a tag with no name was accepted")
	}
}

func TestCanonicalJSON(t *testing.T) {
	got, err := CanonicalJSON(map[string]any{
		"type":         "offer",
		"swap_id":      "abcdef123456",
		"blake_amount": "0.5",
	})

	if err != nil {
		t.Fatal(err)
	}

	if string(got) != pythonContent {
		t.Fatalf("canonical JSON = %s, want %s", got, pythonContent)
	}

	// A non-ASCII string stays as UTF-8 here, which is the documented
	// canonical form (pkg/wire produces the same). Verification is unaffected
	// either way because the id is computed over the bytes that were sent.
	got, err = CanonicalJSON(map[string]any{"note": "héllo"})

	if err != nil {
		t.Fatal(err)
	}

	if string(got) != `{"note":"héllo"}` {
		t.Fatalf("non-ASCII canonical JSON = %s", got)
	}
}
