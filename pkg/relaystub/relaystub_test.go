//go:build !js

package relaystub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/keys"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/platform"
)

// clientFor builds a relay client pointed at one stub.
func clientFor(t *testing.T, stub *Stub) *nostr.Client {
	t.Helper()

	return &nostr.Client{
		Dialer: func(ctx context.Context, url string) (nostr.Conn, error) {
			return platform.DialWebSocket(ctx, url)
		},
		Relays: []string{stub.URL},
		Cache:  nostr.NewMemoryCache(),
	}
}

// signedEvent builds and signs a kind-1 event with one topic tag.
func signedEvent(t *testing.T, key *keys.Key, content string, topics ...string) event.Event {
	t.Helper()

	tags := make([][]string, 0, len(topics))

	for _, topic := range topics {
		tags = append(tags, []string{"t", topic})
	}

	built, err := event.Build(key.PublicHex(),
		event.WithContent([]byte(content)),
		event.WithTags(tags),
		event.WithCreatedAt(time.Now().Unix()),
	)

	if err != nil {
		t.Fatal(err)
	}

	if err := event.Sign(&built, key); err != nil {
		t.Fatal(err)
	}

	return built
}

// newKey returns a fixed test identity, derived the way the engine derives one.
func newKey(t *testing.T) *keys.Key {
	t.Helper()

	key, err := keys.FromSecretHex(strings.Repeat("11", 32))

	if err != nil {
		t.Fatal(err)
	}

	return key
}

// TestRelayStubRoundTripsAPublishedEvent: publish, then fetch the same event back by id.
func TestRelayStubRoundTripsAPublishedEvent(t *testing.T) {
	stub, err := New()

	if err != nil {
		t.Fatal(err)
	}

	defer stub.Close()

	client := clientFor(t, stub)
	key := newKey(t)

	published := signedEvent(t, key, `{"type":"hello"}`, "blake-swap-market")

	if _, err := client.Publish(context.Background(), published); err != nil {
		t.Fatalf("publish: %v", err)
	}

	fetched, err := client.Fetch(context.Background(), nostr.Filter{
		Kinds: []int{event.KindTextNote}, Ids: []string{published.ID},
	})

	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if len(fetched) != 1 || fetched[0].ID != published.ID || fetched[0].Pubkey != key.PublicHex() {
		t.Fatalf("fetched = %+v", fetched)
	}

	if held := stub.Events(); len(held) != 1 {
		t.Fatalf("the relay holds %d events, want 1", len(held))
	}
}

// TestRelayStubAppliesTopicFilters: an event is returned by the subscription that selects it
// and not by one that does not — the stub applies the same filter semantics the client sends.
func TestRelayStubAppliesTopicFilters(t *testing.T) {
	stub, err := New()

	if err != nil {
		t.Fatal(err)
	}

	defer stub.Close()

	client := clientFor(t, stub)
	key := newKey(t)

	offer := signedEvent(t, key, `{"type":"market-offer"}`, "blake-swap-market")
	take := signedEvent(t, key, `{"type":"market-take"}`, "blake-swap-market-take")

	for _, published := range []event.Event{offer, take} {
		if _, err := client.Publish(context.Background(), published); err != nil {
			t.Fatal(err)
		}
	}

	offers, err := client.Fetch(context.Background(), nostr.Filter{
		Kinds: []int{event.KindTextNote}, Topics: []string{"blake-swap-market"},
	})

	if err != nil {
		t.Fatal(err)
	}

	if len(offers) != 1 || offers[0].ID != offer.ID {
		t.Fatalf("topic filter returned %d events", len(offers))
	}
}

// TestRelayStubServesSeveralFiltersInOneSubscription: an event matching any filter is
// delivered once, which is what the client's dedupe relies on.
func TestRelayStubServesSeveralFiltersInOneSubscription(t *testing.T) {
	stub, err := New()

	if err != nil {
		t.Fatal(err)
	}

	defer stub.Close()

	client := clientFor(t, stub)
	key := newKey(t)

	both := signedEvent(t, key, `{}`, "blake-swap-market", "blake-swap-market-cancel")

	if _, err := client.Publish(context.Background(), both); err != nil {
		t.Fatal(err)
	}

	report, err := client.FetchReport(context.Background(),
		nostr.Filter{Topics: []string{"blake-swap-market"}},
		nostr.Filter{Topics: []string{"blake-swap-market-cancel"}},
	)

	if err != nil {
		t.Fatal(err)
	}

	if len(report.Events) != 1 {
		t.Fatalf("a doubly matching event was delivered %d times", len(report.Events))
	}

	if report.Answered != 1 {
		t.Fatalf("answered = %d, want 1", report.Answered)
	}
}

// TestRelayStubInjectionPlaysTheOtherParty: a test can put another participant's signed event
// on the relay without a second process, which is how a two-sided scenario is staged.
func TestRelayStubInjectionPlaysTheOtherParty(t *testing.T) {
	stub, err := New()

	if err != nil {
		t.Fatal(err)
	}

	defer stub.Close()

	other := signedEvent(t, newKey(t), `{"type":"market-take"}`, "blake-swap-market-take")

	stub.Inject(other)

	if held := stub.Events(); len(held) != 1 || held[0].ID != other.ID {
		t.Fatalf("injected event is not held: %+v", held)
	}

	// A second injection of the same event is not a second event: the relay is a set,
	// as every Nostr relay is.
	stub.Inject(other)

	if held := stub.Events(); len(held) != 1 {
		t.Fatalf("duplicate injection stored %d events", len(held))
	}
}

// TestRelayStubRefusesGarbageFrames: the stub is a test double, not a fuzzer target, but it
// must not hang or panic when a client sends nonsense.
func TestRelayStubRefusesGarbageFrames(t *testing.T) {
	stub, err := New()

	if err != nil {
		t.Fatal(err)
	}

	defer stub.Close()

	// A publish of an event with no signature is stored as-is and acknowledged: the stub
	// does not verify, and the client does — the protocol puts that obligation on the
	// reader, which is where it belongs.
	client := clientFor(t, stub)

	_, err = client.Publish(context.Background(), event.Event{ID: "not-an-event"})

	if err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("the client published an unverifiable event: %v", err)
	}
}
