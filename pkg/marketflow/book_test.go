package marketflow

import (
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/keys"
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

// TestMarketFiltersSizesEvidenceGenerously: offers are asked for at the caller's limit,
// but cancellations and matches at least at 100, because a window sized for offers would
// hide the events that settle them.
func TestMarketFiltersSizesEvidenceGenerously(t *testing.T) {
	small := MarketFilters(5)

	if len(small) != 3 {
		t.Fatalf("filters = %d, want 3", len(small))
	}

	if small[0].Limit != 5 {
		t.Fatalf("offer limit = %d, want 5", small[0].Limit)
	}

	if small[1].Limit != 100 || small[2].Limit != 100 {
		t.Fatalf("evidence limits = %d, %d, want 100", small[1].Limit, small[2].Limit)
	}

	if small[0].Topics[0] != MarketTag ||
		small[1].Topics[0] != CancelTag ||
		small[2].Topics[0] != MatchTag {
		t.Fatalf("topics = %v %v %v", small[0].Topics, small[1].Topics, small[2].Topics)
	}

	large := MarketFilters(100)

	if large[1].Limit != 400 {
		t.Fatalf("evidence limit = %d, want 400", large[1].Limit)
	}

	if defaulted := MarketFilters(0); defaulted[0].Limit != 50 {
		t.Fatalf("zero limit = %d, want the default 50", defaulted[0].Limit)
	}
}

// TestTakesForAcceptsAValidTakeAndRejectsTheRest is the whole binding rule in one
// fixture: one good take, and one of each way to ask for a different trade.
func TestTakesForAcceptsAValidTakeAndRejectsTheRest(t *testing.T) {
	f := buildStateFixture(t)

	// The good take, and a second from another taker.
	second, _, err := BuildTake(maker(t), BuildTakeOptions{Offer: f.offer, CreatedAt: 1_700_000_260})

	if err != nil {
		t.Fatal(err)
	}

	// A take whose content was edited after signing: the terms no longer match.
	tampered := f.takeEvent
	tampered.Content = strings.Replace(tampered.Content, `"btc_sats":25000`, `"btc_sats":1`, 1)

	// A take for a different offer.
	other, _, err := BuildOffer(maker(t), BuildOfferOptions{
		BlakeAmount: amount.Blake(3_000_000), BTCSats: 30_000, Minutes: 30,
		Direction: market.BlakeForBTC, CreatedAt: 1_700_000_010,
	})

	if err != nil {
		t.Fatal(err)
	}

	otherListed, err := ParseOffer(other, time.Unix(1_700_000_100, 0))

	if err != nil {
		t.Fatal(err)
	}

	elsewhere, _, err := BuildTake(taker(t), BuildTakeOptions{Offer: *otherListed, CreatedAt: 1_700_000_270})

	if err != nil {
		t.Fatal(err)
	}

	takes := TakesFor(&f.offer, []event.Event{
		f.takeEvent, second, tampered, elsewhere, f.takeEvent,
	}, time.Unix(bookNow, 0))

	if len(takes) != 2 {
		t.Fatalf("takes = %d, want 2 valid: %+v", len(takes), takes)
	}

	// Newest first, and the duplicate counted once.
	if takes[0].EventID != second.ID || takes[1].EventID != f.takeEvent.ID {
		t.Fatalf("order = %q, %q", takes[0].EventID, takes[1].EventID)
	}

	for _, take := range takes {
		if take.Taker != take.Event.Pubkey {
			t.Fatalf("taker is not the author: %+v", take)
		}

		if take.Take.BTCSats != f.offer.BTCSats {
			t.Fatalf("a take with different terms was accepted: %+v", take)
		}
	}
}

// TestTakesForAnOfferWithNone: an untaken offer has no takes, not an error.
func TestTakesForAnOfferWithNone(t *testing.T) {
	f := buildStateFixture(t)

	if takes := TakesFor(&f.offer, []event.Event{f.offerEvent}, time.Unix(bookNow, 0)); len(takes) != 0 {
		t.Fatalf("takes = %+v, want none", takes)
	}

	if takes := TakesFor(nil, []event.Event{f.takeEvent}, time.Unix(bookNow, 0)); takes != nil {
		t.Fatalf("a nil offer produced takes: %+v", takes)
	}
}

// resignTake rewrites an event's content and signs it again, so a test can produce a
// well-formed event that says something its author would never have signed.
func resignTake(t *testing.T, original event.Event, secret string, rewrite func(string) string) event.Event {
	t.Helper()

	forged := original
	forged.Content = rewrite(forged.Content)

	// A forger signs as themselves, so the event names their key and hashes to a
	// different id. Keeping the original pubkey would fail at signing time, which would
	// test the signer rather than the resolver.
	identity, err := nostr.NewKey(secret)

	if err != nil {
		t.Fatal(err)
	}

	forged.Pubkey = identity.PublicKey()

	id, err := forged.ComputeID()

	if err != nil {
		t.Fatal(err)
	}

	forged.ID = id

	signer, err := keys.FromSecretHex(secret)

	if err != nil {
		t.Fatal(err)
	}

	if err := event.Sign(&forged, signer); err != nil {
		t.Fatal(err)
	}

	return forged
}

// TestTakesForRejectsATakeThatChangesTheTerms: a take can convince every structural
// check and still ask for a different trade. It references this offer, its direction and
// roles are right, its author is its taker — and its amounts say something else. Only the
// binding check catches that, which is why that check exists.
func TestTakesForRejectsATakeThatChangesTheTerms(t *testing.T) {
	f := buildStateFixture(t)

	cheaper := resignTake(t, f.takeEvent, strings.Repeat("22", 32), func(content string) string {
		return strings.Replace(content, `"btc_sats":25000`, `"btc_sats":24000`, 1)
	})

	// It really is a valid take in isolation: signature, tags and roles all check out.
	if _, err := ParseTake(cheaper, time.Unix(bookNow, 0)); err != nil {
		t.Fatalf("the fixture is not a parseable take: %v", err)
	}

	takes := TakesFor(&f.offer, []event.Event{f.takeEvent, cheaper}, time.Unix(bookNow, 0))

	if len(takes) != 1 || takes[0].EventID != f.takeEvent.ID {
		t.Fatalf("a take that changed the terms was accepted: %+v", takes)
	}
}

// TestMatchesForResolvesBothReferencedEvents: a match is evidence only with the offer
// and take it names, so it is resolved from the evidence set and cross-checked.
func TestMatchesForResolvesBothReferencedEvents(t *testing.T) {
	f := buildStateFixture(t)
	match := matchFor(t, f)
	now := time.Unix(bookNow, 0)

	// A second, later match, so the newest-first order is a property the test checks
	// rather than one it happens to agree with.
	later, _, err := BuildMatch(maker(t), BuildMatchOptions{
		Offer:       f.offer,
		OfferEvent:  f.offerEvent,
		TakeEvent:   f.takeEvent,
		Taker:       &f.take,
		SwapID:      "aaaa00000002",
		BlakeAmount: f.offer.BlakeAmount,
		BTCSats:     f.offer.BTCSats,
		CreatedAt:   1_700_000_350,
	})

	if err != nil {
		t.Fatal(err)
	}

	for _, participant := range []string{f.offerEvent.Pubkey, f.takeEvent.Pubkey} {
		matches := MatchesFor(participant, []event.Event{f.offerEvent, f.takeEvent, match, later}, now)

		if len(matches) != 2 {
			t.Fatalf("matches for %s = %d, want 2", participant[:8], len(matches))
		}

		if matches[0].Event.ID != later.ID || matches[1].Event.ID != match.ID {
			t.Fatalf("order = %q, %q; want newest first", matches[0].Event.ID, matches[1].Event.ID)
		}

		if matches[1].Match.SwapID != "aaaa00000001" {
			t.Fatalf("swap id = %q", matches[1].Match.SwapID)
		}
	}
}

// TestMatchesForDropsAMatchItCannotResolve: without both referenced events a match is a
// claim about a match, not evidence of one.
func TestMatchesForDropsAMatchItCannotResolve(t *testing.T) {
	f := buildStateFixture(t)
	match := matchFor(t, f)
	now := time.Unix(bookNow, 0)

	participants := []string{f.offerEvent.Pubkey, f.takeEvent.Pubkey}

	for _, evidence := range [][]event.Event{
		{match},               // neither referenced event present
		{f.offerEvent, match}, // the take is missing
		{f.takeEvent, match},  // the offer is missing
	} {
		for _, participant := range participants {
			if matches := MatchesFor(participant, evidence, now); len(matches) != 0 {
				t.Fatalf("an unresolvable match was reported: %+v", matches)
			}
		}
	}
}

// TestMatchesForIgnoresOtherParticipantsAndForgedSigners: the inbox is addressed, and a
// match that is not signed by the offer's maker is not a match.
func TestMatchesForIgnoresOtherParticipantsAndForgedSigners(t *testing.T) {
	f := buildStateFixture(t)
	match := matchFor(t, f)
	now := time.Unix(bookNow, 0)
	evidence := []event.Event{f.offerEvent, f.takeEvent, match}

	if matches := MatchesFor(strings.Repeat("99", 32), evidence, now); len(matches) != 0 {
		t.Fatalf("a stranger's inbox listed a match: %+v", matches)
	}

	// A match with the right shape, signed by the taker instead of the maker.
	forged := resignTake(t, match, strings.Repeat("22", 32), func(content string) string { return content })

	if matches := MatchesFor(f.takeEvent.Pubkey, []event.Event{f.offerEvent, f.takeEvent, forged}, now); len(matches) != 0 {
		t.Fatalf("a match signed by the taker was accepted: %+v", matches)
	}
}
