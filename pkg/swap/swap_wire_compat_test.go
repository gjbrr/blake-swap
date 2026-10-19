package swap

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/coord"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/store"
	"github.com/gjbrr/blake-swap/pkg/wire"
)

// This file mirrors the reference's `tests/swap_wire_compat_tests.py`: the messages a real
// swap writes must satisfy the shared wire schema, and must survive the canonical round trip
// unchanged.
//
// The point is not that the engine "uses" the schema — it is that a message is only
// interchangeable if the *other* implementation's validator accepts it. A field name, an
// amount rendering or an integer that is a float on one side and a string on the other passes
// every test that only round-trips through its own writer, and fails here.

// wireCheckedTransport records what the engine sends and can pre-load a counterparty message.
type wireCheckedTransport struct {
	sent     []recordedMessage
	incoming map[string]*coord.Message
}

// recordedMessage is one coordination message as the engine wrote it.
type recordedMessage struct {
	swapID string
	kind   string
	body   map[string]any
}

func newWireCheckedTransport() *wireCheckedTransport {
	return &wireCheckedTransport{incoming: map[string]*coord.Message{}}
}

func (w *wireCheckedTransport) Send(
	_ context.Context, swapID, kind string, body map[string]any, _ coord.SendOptions,
) (string, error) {
	w.sent = append(w.sent, recordedMessage{swapID: swapID, kind: kind, body: body})

	return "memory://" + kind, nil
}

func (w *wireCheckedTransport) Receive(
	_ context.Context, _, kind string, _ coord.ReceiveOptions,
) (*coord.Message, error) {
	message, ok := w.incoming[kind]

	if !ok {
		return nil, fmt.Errorf("no %s message", kind)
	}

	return message, nil
}

// withTimeouts gives a fixture the v1.5 metadata a swap created by the engine always carries.
//
// A record without it is a legacy record, and an offer built from one is refused by the wire
// schema on the other side — which is why the engine writes the metadata in `New` rather than
// leaving it to whoever publishes.
func withTimeouts(t *testing.T, st store.Store, record *model.Swap) {
	t.Helper()

	version := "1.5"
	btcWindow, blakeWindow, recovery := int64(12_600), int64(27_600), int64(14_700)

	record.TimeoutPolicyVersion = &version
	record.BTCTimeoutWindowSeconds = &btcWindow
	record.BlakeTimeoutWindowSeconds = &blakeWindow
	record.BobRecoverySeconds = &recovery

	if err := st.Save(context.Background(), store.Alice, record); err != nil {
		t.Fatal(err)
	}
}

// TestSwapWireCompatEverySentMessageSatisfiesTheSchema runs one swap as far as the engine can
// get without a second participant and validates every message it wrote.
func TestSwapWireCompatEverySentMessageSatisfiesTheSchema(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	engine, _, _ := fundingEngine(t, st)
	transport := newWireCheckedTransport()
	engine.Transport = transport

	record := pinnedSwap(t, st, "aaaa00000201")
	withTimeouts(t, st, record)

	// The offer is what Alice publishes first; the funding announcement follows the
	// funding. Both are settlement types and both must be schema-valid.
	if _, err := engine.PublishOffer(ctx, record.SwapID); err != nil {
		t.Fatalf("publishing the offer: %v", err)
	}

	if _, err := engine.FundBlake(ctx, record.SwapID); err != nil {
		t.Fatalf("funding BLAKE: %v", err)
	}

	if _, err := engine.AnnounceBlakeFunding(ctx, record.SwapID); err != nil {
		t.Fatalf("announcing BLAKE funding: %v", err)
	}

	if len(transport.sent) < 2 {
		t.Fatalf("the engine sent %d messages; the test expected an offer and a funding announcement", len(transport.sent))
	}

	for _, message := range transport.sent {
		kind := wire.Kind(message.kind)

		if _, err := wire.Validate(message.body, &kind); err != nil {
			t.Errorf("%s does not satisfy the canonical wire schema: %v\n%v", message.kind, err, message.body)

			continue
		}

		// A value the schema accepts but the canonical encoder cannot round-trip is a
		// message the other implementation would receive as a different message.
		encoded, err := wire.Encode(message.body, &kind)

		if err != nil {
			t.Errorf("%s could not be encoded canonically: %v", message.kind, err)

			continue
		}

		decoded, err := wire.Decode(encoded, &kind)

		if err != nil {
			t.Errorf("%s could not be decoded back: %v", message.kind, err)

			continue
		}

		if len(decoded) != len(message.body) {
			t.Errorf("%s lost fields in the canonical round trip", message.kind)

			continue
		}

		for field, want := range message.body {
			if fmt.Sprint(decoded[field]) != fmt.Sprint(want) {
				t.Errorf("%s: %s became %v, want %v", message.kind, field, decoded[field], want)
			}
		}

		// And Alice's secret never travels, on any message, by any spelling.
		for _, forbidden := range []string{"secret_hex"} {
			if _, present := message.body[forbidden]; present {
				t.Errorf("%s carries %s", message.kind, forbidden)
			}
		}
	}
}

// TestSwapWireCompatTheOfferIsTheShapeTheOtherSideParses: the first message a swap sends is
// the one another implementation must accept before anything else happens, so its fields are
// checked by name rather than only by validity.
func TestSwapWireCompatTheOfferIsTheShapeTheOtherSideParses(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	engine, _, _ := fundingEngine(t, st)
	transport := newWireCheckedTransport()
	engine.Transport = transport

	record := pinnedSwap(t, st, "aaaa00000202")
	withTimeouts(t, st, record)

	if _, err := engine.PublishOffer(ctx, record.SwapID); err != nil {
		t.Fatal(err)
	}

	if len(transport.sent) != 1 {
		t.Fatalf("publishing an offer sent %d messages", len(transport.sent))
	}

	offer := transport.sent[0].body

	for _, field := range []string{
		"type", "swap_id", "blake_amount", "btc_sats", "secret_hash",
		"btc_timeout", "blake_timeout", "created_at",
		"blake_alice_pub", "btc_alice_pub",
		"timeout_policy_version", "btc_timeout_window_seconds",
		"blake_timeout_window_seconds", "bob_recovery_seconds",
	} {
		if _, present := offer[field]; !present {
			t.Errorf("the offer has no %s", field)
		}
	}

	if offer["swap_id"] != "aaaa00000202" {
		t.Errorf("swap_id = %v", offer["swap_id"])
	}

	// The amounts are what the record says, rendered the way the wire carries them.
	if fmt.Sprint(offer["blake_amount"]) != record.BlakeAmount().Format() {
		t.Errorf("blake_amount = %v, want %s", offer["blake_amount"], record.BlakeAmount().Format())
	}

	if fmt.Sprint(offer["secret_hash"]) != strings.ToLower(record.SecretHash) {
		t.Errorf("secret_hash = %v", offer["secret_hash"])
	}
}
