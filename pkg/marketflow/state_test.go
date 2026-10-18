package marketflow

import (
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/market"
)

// stateFixture is an offer with a live take available for state derivation.
type stateFixture struct {
	offerEvent event.Event
	takeEvent  event.Event
	take       market.Take
	offer      ListedOffer
	now        int64
}

// buildStateFixture builds a live offer and a take.
func buildStateFixture(t *testing.T) stateFixture {
	t.Helper()

	offerEvent, _, err := BuildOffer(maker(t), BuildOfferOptions{
		BlakeAmount: amount.Blake(1_000_000),
		BTCSats:     25_000,
		Minutes:     30,
		Direction:   market.BlakeForBTC,
		CreatedAt:   1_700_000_000,
	})

	if err != nil {
		t.Fatal(err)
	}

	offer, err := ParseOffer(offerEvent, time.Unix(1_700_000_100, 0))

	if err != nil {
		t.Fatal(err)
	}

	takeEvent, take, err := BuildTake(taker(t), BuildTakeOptions{
		Offer: *offer, CreatedAt: 1_700_000_200,
	})

	if err != nil {
		t.Fatal(err)
	}

	return stateFixture{
		offerEvent: offerEvent,
		takeEvent:  takeEvent,
		take:       take,
		offer:      *offer,
		now:        1_700_000_400,
	}
}

// matchFor builds a match for the fixture's take.
func matchFor(t *testing.T, f stateFixture) event.Event {
	t.Helper()

	built, _, err := BuildMatch(maker(t), BuildMatchOptions{
		Offer:       f.offer,
		OfferEvent:  f.offerEvent,
		TakeEvent:   f.takeEvent,
		Taker:       &f.take,
		SwapID:      "aaaa00000001",
		BlakeAmount: f.offer.BlakeAmount,
		BTCSats:     f.offer.BTCSats,
		CreatedAt:   1_700_000_300,
	})

	if err != nil {
		t.Fatal(err)
	}

	return built
}

func TestDeriveStateOpenAndExpired(t *testing.T) {
	f := buildStateFixture(t)

	result, err := DeriveState(f.offerEvent, nil, f.now)

	if err != nil {
		t.Fatal(err)
	}

	if result.State != StateOpen {
		t.Fatalf("state = %s", result.State)
	}

	// At the expiry instant the offer is already gone.
	result, err = DeriveState(f.offerEvent, nil, f.offer.ExpiresAt)

	if err != nil {
		t.Fatal(err)
	}

	if result.State != StateExpired {
		t.Fatalf("state = %s", result.State)
	}
}

func TestDeriveStateMatched(t *testing.T) {
	f := buildStateFixture(t)
	match := matchFor(t, f)

	result, err := DeriveState(f.offerEvent, []event.Event{f.takeEvent, match}, f.now)

	if err != nil {
		t.Fatal(err)
	}

	if result.State != StateMatched || result.MatchID != match.ID {
		t.Fatalf("result = %+v", result)
	}

	// History is not rewritten by time: a match sealed while the offer was live keeps
	// it MATCHED, however long ago that was.
	result, err = DeriveState(f.offerEvent, []event.Event{f.takeEvent, match}, f.offer.ExpiresAt+86_400)

	if err != nil {
		t.Fatal(err)
	}

	if result.State != StateMatched {
		t.Fatalf("state = %s after expiry", result.State)
	}
}

func TestDeriveStateCancelled(t *testing.T) {
	f := buildStateFixture(t)

	cancel, _, err := BuildCancel(maker(t), BuildCancelOptions{
		OfferID: f.offer.EventID, CreatedAt: 1_700_000_250,
	})

	if err != nil {
		t.Fatal(err)
	}

	result, err := DeriveState(f.offerEvent, []event.Event{cancel}, f.now)

	if err != nil {
		t.Fatal(err)
	}

	if result.State != StateCancelled || result.CancelID != cancel.ID {
		t.Fatalf("result = %+v", result)
	}

	// A cancellation by anyone but the maker is an event about nothing.
	impostor, _, err := BuildCancel(taker(t), BuildCancelOptions{
		OfferID: f.offer.EventID, CreatedAt: 1_700_000_250,
	})

	if err != nil {
		t.Fatal(err)
	}

	result, err = DeriveState(f.offerEvent, []event.Event{impostor}, f.now)

	if err != nil {
		t.Fatal(err)
	}

	if result.State != StateOpen {
		t.Fatalf("a competitor's cancellation changed the state to %s", result.State)
	}
}

// TestDeriveStateIgnoresAnInconsistentMatch: a maker-signed but economically
// inconsistent match must not hide a live offer.
func TestDeriveStateIgnoresAnInconsistentMatch(t *testing.T) {
	f := buildStateFixture(t)
	match := matchFor(t, f)

	mangled := resignWithContentFor(t, match, maker(t), func(message map[string]any) {
		message["btc_sats"] = 30_000
	})

	result, err := DeriveState(f.offerEvent, []event.Event{f.takeEvent, mangled}, f.now)

	if err != nil {
		t.Fatal(err)
	}

	if result.State != StateOpen {
		t.Fatalf("an inconsistent match produced %s", result.State)
	}
}

// TestDeriveStateFirstTerminalWins: when two terminal events exist, the earliest one
// decides, and the id breaks a tie deterministically.
func TestDeriveStateFirstTerminalWins(t *testing.T) {
	f := buildStateFixture(t)
	match := matchFor(t, f)

	cancel, _, err := BuildCancel(maker(t), BuildCancelOptions{
		OfferID: f.offer.EventID, CreatedAt: 1_700_000_350,
	})

	if err != nil {
		t.Fatal(err)
	}

	// The take must be among the events: a match is only evidence when the take it
	// names can be checked, which is how the reference derives it too.
	take := f.takeEvent

	// The match is earlier (300) than the cancellation (350), so the offer is MATCHED.
	result, err := DeriveState(f.offerEvent, []event.Event{cancel, match, take}, f.now)

	if err != nil {
		t.Fatal(err)
	}

	if result.State != StateMatched {
		t.Fatalf("state = %s, want the earlier match to win", result.State)
	}

	// Fed in the other order, the answer must be the same.
	result, err = DeriveState(f.offerEvent, []event.Event{match, cancel, take}, f.now)

	if err != nil {
		t.Fatal(err)
	}

	if result.State != StateMatched {
		t.Fatalf("state = %s, delivery order changed the answer", result.State)
	}
}

// TestDeriveStateIgnoresEventsAfterExpiry: a terminal event created at or after the
// offer's expiry is not evidence about a live offer.
//
// The clock is moved past the expiry as well, so the refusal cannot be an artefact of
// the event being dated in the future: the only reason left is the expiry boundary.
func TestDeriveStateIgnoresEventsAfterExpiry(t *testing.T) {
	f := buildStateFixture(t)

	lateCancel, _, err := BuildCancel(maker(t), BuildCancelOptions{
		OfferID: f.offer.EventID, CreatedAt: f.offer.ExpiresAt + 100,
	})

	if err != nil {
		t.Fatal(err)
	}

	result, err := DeriveState(f.offerEvent, []event.Event{lateCancel}, f.offer.ExpiresAt+200)

	if err != nil {
		t.Fatal(err)
	}

	if result.State != StateExpired {
		t.Fatalf("a cancellation after expiry produced %s, want EXPIRED", result.State)
	}
}

// TestDeriveStateRefusesAnUnverifiableOffer: the offer is the subject of the report,
// so it has to be a real event.
func TestDeriveStateRefusesAnUnverifiableOffer(t *testing.T) {
	f := buildStateFixture(t)

	forged := f.offerEvent
	forged.Sig = strings.Repeat("00", 64)

	if _, err := DeriveState(forged, nil, f.now); err == nil {
		t.Fatal("a forged offer produced a state")
	}
}
