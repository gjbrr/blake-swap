package marketflow

import (
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/nostr"
)

// keyFromSecret is nostr.NewKey, named for how these tests use it.
func keyFromSecret(secret string) (*nostr.Key, error) { return nostr.NewKey(secret) }

// bookNow is the instant the book tests reason at: after the fixture's take and
// match, before its offer expires.
const bookNow = 1_700_000_400

// bookEventAt builds a second offer by a different maker, at a chosen time.
func bookOfferAt(t *testing.T, keySecret string, createdAt int64) event.Event {
	t.Helper()

	key, err := keyFromSecret(keySecret)

	if err != nil {
		t.Fatal(err)
	}

	built, _, err := BuildOffer(key, BuildOfferOptions{
		BlakeAmount: amount.Blake(2_000_000),
		BTCSats:     50_000,
		Minutes:     60,
		Direction:   market.BlakeForBTC,
		CreatedAt:   createdAt,
	})

	if err != nil {
		t.Fatal(err)
	}

	return built
}

// bookAt runs a book over the fixture's evidence.
func bookAt(t *testing.T, events []event.Event) []Entry {
	t.Helper()

	entries, err := Book(events, time.Unix(bookNow, 0))

	if err != nil {
		t.Fatal(err)
	}

	return entries
}

// TestBookOpenOfferIsOpen: one valid offer, nothing else, is OPEN and in the open list.
func TestBookOpenOfferIsOpen(t *testing.T) {
	f := buildStateFixture(t)
	entries := bookAt(t, []event.Event{f.offerEvent})

	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}

	if entries[0].State != StateOpen {
		t.Fatalf("state = %q, want OPEN", entries[0].State)
	}

	if entries[0].EventID() != f.offerEvent.ID {
		t.Fatalf("event id = %q", entries[0].EventID())
	}

	if entries[0].Offer.Maker != f.offerEvent.Pubkey {
		t.Fatalf("maker = %q", entries[0].Offer.Maker)
	}
}

// TestBookDropsDuplicatesAndUnverifiableEvents: the same event from three relays is
// one entry, and an event whose content no longer matches its signature is nothing.
func TestBookDropsDuplicatesAndUnverifiableEvents(t *testing.T) {
	f := buildStateFixture(t)

	forged := f.offerEvent
	forged.Content = strings.Replace(forged.Content, `"btc_sats":25000`, `"btc_sats":1`, 1)

	entries := bookAt(t, []event.Event{
		f.offerEvent,
		f.offerEvent,
		f.offerEvent,
		forged,
	})

	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (duplicates and a forged copy dropped)", len(entries))
	}
}

// TestBookPrefersTheAuthenticCopy: a tampered copy of an event carries the same id as
// the real one, so a book that deduplicated before verifying would keep whichever copy
// arrived first — and a relay that served the forged one first would win. Authenticity
// is checked first, and the forged copy cannot displace the real one in either order.
func TestBookPrefersTheAuthenticCopy(t *testing.T) {
	f := buildStateFixture(t)

	forged := f.offerEvent
	forged.Content = strings.Replace(forged.Content, `"btc_sats":25000`, `"btc_sats":1`, 1)

	for _, order := range [][]event.Event{
		{forged, f.offerEvent},
		{f.offerEvent, forged},
	} {
		entries := bookAt(t, order)

		if len(entries) != 1 || entries[0].State != StateOpen {
			t.Fatalf("entries = %+v, want the authentic offer regardless of arrival order", entries)
		}

		if entries[0].Offer.BTCSats != 25_000 {
			t.Fatalf("kept the forged terms: %d sats", entries[0].Offer.BTCSats)
		}
	}
}

// TestBookMatchedOfferStaysMatched: a match is evidence that outlives the offer's
// expiry, so a matched offer is MATCHED rather than EXPIRED, and it is not on offer.
func TestBookMatchedOfferStaysMatched(t *testing.T) {
	f := buildStateFixture(t)
	match := matchFor(t, f)

	entries := bookAt(t, []event.Event{f.offerEvent, f.takeEvent, match})

	if len(entries) != 1 || entries[0].State != StateMatched {
		t.Fatalf("entries = %+v, want one MATCHED", entries)
	}

	if entries[0].MatchID != match.ID {
		t.Fatalf("match id = %q, want %q", entries[0].MatchID, match.ID)
	}

	if open := OpenOffers(entries); len(open) != 0 {
		t.Fatalf("a matched offer was listed as open: %+v", open)
	}
}

// TestBookCancelledOfferIsHidden: a valid cancellation settles the offer, names the
// event that did it, and removes it from the open list.
func TestBookCancelledOfferIsHidden(t *testing.T) {
	f := buildStateFixture(t)

	cancel, _, err := BuildCancel(maker(t), BuildCancelOptions{
		OfferID: f.offerEvent.ID, CreatedAt: 1_700_000_250,
	})

	if err != nil {
		t.Fatal(err)
	}

	entries := bookAt(t, []event.Event{f.offerEvent, cancel})

	if len(entries) != 1 || entries[0].State != StateCancelled {
		t.Fatalf("entries = %+v, want one CANCELLED", entries)
	}

	if entries[0].CancelID != cancel.ID {
		t.Fatalf("cancel id = %q, want %q", entries[0].CancelID, cancel.ID)
	}

	if open := OpenOffers(entries); len(open) != 0 {
		t.Fatalf("a cancelled offer was listed as open: %+v", open)
	}
}

// TestBookCancellationFromAStrangerIsNotEvidence: only the maker may withdraw an
// offer, so a cancellation signed by anyone else leaves it open.
func TestBookCancellationFromAStrangerIsNotEvidence(t *testing.T) {
	f := buildStateFixture(t)

	forged, _, err := BuildCancel(taker(t), BuildCancelOptions{
		OfferID: f.offerEvent.ID, CreatedAt: 1_700_000_250,
	})

	if err != nil {
		t.Fatal(err)
	}

	entries := bookAt(t, []event.Event{f.offerEvent, forged})

	if len(entries) != 1 || entries[0].State != StateOpen {
		t.Fatalf("a stranger's cancellation changed the state: %+v", entries)
	}
}

// TestBookExpiredOfferIsExpiredButStillReported: expiry hides an offer from the open
// list without erasing the fact that it was advertised.
func TestBookExpiredOfferIsExpiredButStillReported(t *testing.T) {
	f := buildStateFixture(t)

	entries, err := Book([]event.Event{f.offerEvent}, time.Unix(f.offer.ExpiresAt+1, 0))

	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 || entries[0].State != StateExpired {
		t.Fatalf("entries = %+v, want one EXPIRED", entries)
	}

	if open := OpenOffers(entries); len(open) != 0 {
		t.Fatalf("an expired offer was listed as open: %+v", open)
	}
}

// TestBookOrdersNewestFirst: two offers list newest first, so two clients reading the
// same evidence show the same order.
func TestBookOrdersNewestFirst(t *testing.T) {
	older := bookOfferAt(t, strings.Repeat("33", 32), 1_700_000_000)
	newer := bookOfferAt(t, strings.Repeat("44", 32), 1_700_000_100)

	entries := bookAt(t, []event.Event{older, newer})

	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}

	if entries[0].EventID() != newer.ID || entries[1].EventID() != older.ID {
		t.Fatalf("order = %q, %q; want newest first", entries[0].EventID(), entries[1].EventID())
	}

	// The order must not depend on the order the evidence arrived in.
	shuffled := bookAt(t, []event.Event{newer, older})

	if len(shuffled) != 2 || shuffled[0].EventID() != newer.ID {
		t.Fatalf("shuffled order = %+v", shuffled)
	}
}

// TestBookIgnoresEvidenceAboutNothing: a take or a cancel with no offer in the set
// produces no entries — the book lists offers, not events.
func TestBookIgnoresEvidenceAboutNothing(t *testing.T) {
	f := buildStateFixture(t)

	cancel, _, err := BuildCancel(maker(t), BuildCancelOptions{
		OfferID: f.offerEvent.ID, CreatedAt: 1_700_000_250,
	})

	if err != nil {
		t.Fatal(err)
	}

	if entries := bookAt(t, []event.Event{f.takeEvent, cancel}); len(entries) != 0 {
		t.Fatalf("entries = %+v, want none without the offer", entries)
	}
}
