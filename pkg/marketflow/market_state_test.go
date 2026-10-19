package marketflow

import (
	"testing"

	"github.com/gjbrr/blake-swap/pkg/event"
)

// This file mirrors the reference's `tests/market_state_tests.py`: the ordering rules that
// decide an offer's state from competing evidence. They are the rules a marketplace cannot
// get wrong quietly — a state that depends on delivery order, or a cancellation that beats
// expiry when it happened after it, is a different market for every client that reads it.

// TestMarketStateTerminalEvidenceOutlivesExpiry: a match or cancellation that happened while
// the offer was live decides its state forever, however much time has passed.
func TestMarketStateTerminalEvidenceOutlivesExpiry(t *testing.T) {
	f := buildStateFixture(t)

	cancel, _, err := BuildCancel(maker(t), BuildCancelOptions{
		OfferID: f.offer.EventID, CreatedAt: f.now,
	})

	if err != nil {
		t.Fatal(err)
	}

	// Read years later: the cancellation is still what happened.
	result, err := DeriveState(f.offerEvent, []event.Event{cancel}, f.offer.ExpiresAt+10_000_000)

	if err != nil {
		t.Fatal(err)
	}

	if result.State != StateCancelled {
		t.Fatalf("state = %s, want CANCELLED long after expiry", result.State)
	}
}

// TestMarketStateCancellationAtExpiryCannotOverrideExpiry: a cancellation created at the
// expiry instant is not evidence about a live offer.
func TestMarketStateCancellationAtExpiryCannotOverrideExpiry(t *testing.T) {
	f := buildStateFixture(t)

	atExpiry, _, err := BuildCancel(maker(t), BuildCancelOptions{
		OfferID: f.offer.EventID, CreatedAt: f.offer.ExpiresAt,
	})

	if err != nil {
		t.Fatal(err)
	}

	result, err := DeriveState(f.offerEvent, []event.Event{atExpiry}, f.offer.ExpiresAt)

	if err != nil {
		t.Fatal(err)
	}

	if result.State != StateExpired {
		t.Fatalf("state = %s, want EXPIRED: a cancellation at expiry is not evidence", result.State)
	}
}

// TestMarketStateTieBreakIsDeterministic: two terminal events that share a timestamp are
// ordered by event id, so every reader derives the same state.
func TestMarketStateTieBreakIsDeterministic(t *testing.T) {
	f := buildStateFixture(t)

	cancel, _, err := BuildCancel(maker(t), BuildCancelOptions{
		OfferID: f.offer.EventID, CreatedAt: f.now,
	})

	if err != nil {
		t.Fatal(err)
	}

	match, _, err := BuildMatch(maker(t), BuildMatchOptions{
		Offer:       f.offer,
		OfferEvent:  f.offerEvent,
		TakeEvent:   f.takeEvent,
		Taker:       &f.take,
		SwapID:      "aaaa00000001",
		BlakeAmount: f.offer.BlakeAmount,
		BTCSats:     f.offer.BTCSats,
		CreatedAt:   f.now,
	})

	if err != nil {
		t.Fatal(err)
	}

	// One of the two ids is lower; that one wins, whatever order they arrive in.
	want := StateCancelled

	if match.ID < cancel.ID {
		want = StateMatched
	}

	for _, evidence := range [][]event.Event{
		{cancel, match, f.takeEvent},
		{match, cancel, f.takeEvent},
	} {
		result, err := DeriveState(f.offerEvent, evidence, f.now)

		if err != nil {
			t.Fatal(err)
		}

		if result.State != want {
			t.Fatalf("state = %s, want %s (the lower id wins a tie)", result.State, want)
		}
	}
}

// TestMarketStateWithoutEvidenceUsesTheClock: an offer with no terminal evidence is open
// until its expiry, and expired from that instant on.
func TestMarketStateWithoutEvidenceUsesTheClock(t *testing.T) {
	f := buildStateFixture(t)

	result, err := DeriveState(f.offerEvent, nil, f.offer.ExpiresAt-1)

	if err != nil {
		t.Fatal(err)
	}

	if result.State != StateOpen {
		t.Fatalf("state = %s, want OPEN before expiry", result.State)
	}

	result, err = DeriveState(f.offerEvent, nil, f.offer.ExpiresAt)

	if err != nil {
		t.Fatal(err)
	}

	if result.State != StateExpired {
		t.Fatalf("state = %s, want EXPIRED at expiry", result.State)
	}
}
