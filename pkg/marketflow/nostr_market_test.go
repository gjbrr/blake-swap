//go:build !js

package marketflow

import (
	"context"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/platform"
	"github.com/gjbrr/blake-swap/pkg/relaystub"
)

// This file mirrors the live half of the reference's `tests/nostr_market_tests.py`: a market
// advertisement, a take and a cancellation, over a real relay connection.
//
// The relay is a stub on loopback rather than a public server, because a test that needs the
// internet fails for reasons that have nothing to do with this code. What is exercised is the
// same: the client builds, signs, publishes, subscribes and validates, and the state derivation
// reads the evidence it gets back.

// marketClientFor builds a relay client pointed at one stub.
func marketClientFor(t *testing.T, stub *relaystub.Stub) *nostr.Client {
	t.Helper()

	return &nostr.Client{
		Dialer: func(ctx context.Context, url string) (nostr.Conn, error) {
			return platform.DialWebSocket(ctx, url)
		},
		Relays: []string{stub.URL},
		Cache:  nostr.NewMemoryCache(),
	}
}

// readMarketEvents reads the market the way the discovery commands do.
func readMarketEvents(t *testing.T, client *nostr.Client) []event.Event {
	t.Helper()

	report, err := client.FetchReport(context.Background(), MarketFilters(200)...)

	if err != nil {
		t.Fatalf("market read: %v", err)
	}

	return report.Events
}

// readTakes reads the takes of one offer, as the take commands do: a separate subscription,
// because a market read and a take read are different questions.
func readTakes(t *testing.T, client *nostr.Client, offerID string) []event.Event {
	t.Helper()

	report, err := client.FetchReport(context.Background(), nostr.Filter{
		Kinds:  []int{event.KindTextNote},
		Topics: []string{TakeTag},
		ETags:  []string{offerID},
		Limit:  200,
	})

	if err != nil {
		t.Fatalf("take read: %v", err)
	}

	return report.Events
}

// TestNostrMarketOfferLifecycle: an offer is open, a take is a valid take of it, and a
// cancellation ends it — decided from evidence the relay actually returned.
func TestNostrMarketOfferLifecycle(t *testing.T) {
	stub, err := relaystub.New()

	if err != nil {
		t.Fatal(err)
	}

	defer stub.Close()

	client := marketClientFor(t, stub)
	now := time.Now()

	offerEvent, _, err := BuildOffer(maker(t), BuildOfferOptions{
		BlakeAmount: amount.Blake(1_000_000_000),
		BTCSats:     25_000,
		Minutes:     30,
		Direction:   market.BlakeForBTC,
		CreatedAt:   now.Unix(),
	})

	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.Publish(context.Background(), offerEvent); err != nil {
		t.Fatalf("publishing the offer: %v", err)
	}

	entries, err := Book(readMarketEvents(t, client), now)

	if err != nil {
		t.Fatal(err)
	}

	open := OpenOffers(entries)

	if len(open) != 1 || open[0].EventID() != offerEvent.ID {
		t.Fatalf("open offers = %d, want the one just published", len(open))
	}

	listed, err := ParseOffer(offerEvent, now)

	if err != nil {
		t.Fatal(err)
	}

	takeEvent, _, err := BuildTake(taker(t), BuildTakeOptions{
		Offer: *listed, CreatedAt: now.Unix() + 1,
	})

	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.Publish(context.Background(), takeEvent); err != nil {
		t.Fatalf("publishing the take: %v", err)
	}

	takes := TakesFor(listed, readTakes(t, client, offerEvent.ID), now.Add(2*time.Second))

	if len(takes) != 1 || takes[0].EventID != takeEvent.ID {
		t.Fatalf("takes = %d, want the one just published", len(takes))
	}

	if takes[0].Taker != taker(t).Public {
		t.Fatalf("taker = %q", takes[0].Taker)
	}

	// A cancellation by the maker ends the offer, and the state is derived from the
	// evidence rather than from the clock.
	cancelEvent, _, err := BuildCancel(maker(t), BuildCancelOptions{
		OfferID: offerEvent.ID, CreatedAt: now.Unix() + 2,
	})

	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.Publish(context.Background(), cancelEvent); err != nil {
		t.Fatalf("publishing the cancellation: %v", err)
	}

	derived, err := DeriveState(offerEvent, readMarketEvents(t, client), now.Unix()+3)

	if err != nil {
		t.Fatal(err)
	}

	if derived.State != StateCancelled {
		t.Fatalf("state = %s, want CANCELLED from the published cancellation", derived.State)
	}

	if derived.CancelID != cancelEvent.ID {
		t.Fatalf("cancel id = %q, want %q", derived.CancelID, cancelEvent.ID)
	}

	if len(OpenOffers(mustBook(t, readMarketEvents(t, client), now))) != 0 {
		t.Fatal("a cancelled offer is still advertised as open")
	}
}

// TestNostrMarketIgnoresAtakeForAnotherOffer: the relay returns what it holds, and validation
// is what decides which of those events is a take of *this* offer.
func TestNostrMarketIgnoresAtakeForAnotherOffer(t *testing.T) {
	stub, err := relaystub.New()

	if err != nil {
		t.Fatal(err)
	}

	defer stub.Close()

	client := marketClientFor(t, stub)
	now := time.Now()

	first, _, err := BuildOffer(maker(t), BuildOfferOptions{
		BlakeAmount: amount.Blake(1_000_000_000), BTCSats: 25_000, Minutes: 30,
		Direction: market.BlakeForBTC, CreatedAt: now.Unix(),
	})

	if err != nil {
		t.Fatal(err)
	}

	second, _, err := BuildOffer(maker(t), BuildOfferOptions{
		BlakeAmount: amount.Blake(2_000_000_000), BTCSats: 50_000, Minutes: 30,
		Direction: market.BlakeForBTC, CreatedAt: now.Unix(),
	})

	if err != nil {
		t.Fatal(err)
	}

	for _, published := range []event.Event{first, second} {
		if _, err := client.Publish(context.Background(), published); err != nil {
			t.Fatal(err)
		}
	}

	listedFirst, err := ParseOffer(first, now)

	if err != nil {
		t.Fatal(err)
	}

	// A take of the *second* offer, which is valid — for that offer.
	listedSecond, err := ParseOffer(second, now)

	if err != nil {
		t.Fatal(err)
	}

	otherTake, _, err := BuildTake(taker(t), BuildTakeOptions{
		Offer: *listedSecond, CreatedAt: now.Unix() + 1,
	})

	if err != nil {
		t.Fatalf("a take of the other offer was refused: %v", err)
	}

	// Publish it, so the relay really does hold a take that is not this offer's.
	if _, err := client.Publish(context.Background(), otherTake); err != nil {
		t.Fatal(err)
	}

	// The relay holds a take for the other offer, and this offer's take list is still
	// empty: an event being on the relay is not the same as being evidence about this
	// trade.
	if takes := TakesFor(listedFirst, append(readTakes(t, client, first.ID), readTakes(t, client, second.ID)...),
		now.Add(time.Second)); len(takes) != 0 {
		t.Fatalf("a take for another offer was accepted: %d takes", len(takes))
	}
}

// mustBook is the book, failing the test on an error.
func mustBook(t *testing.T, events []event.Event, now time.Time) []Entry {
	t.Helper()

	entries, err := Book(events, now)

	if err != nil {
		t.Fatal(err)
	}

	return entries
}
