package marketflow

import (
	"sort"
	"time"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/nostr"
)

// Entry is one advertisement with the state the evidence supports.
//
// The point of an entry is that a client never derives state itself: OPEN, MATCHED,
// CANCELLED and EXPIRED are decided here, from authenticated events, and a UI renders
// what it is told. Two clients that each re-derived the rules would eventually
// disagree about whether an offer was still takeable.
type Entry struct {
	// Offer is the parsed, authenticated advertisement.
	Offer ListedOffer
	// State is what the evidence makes true.
	State OfferState
	// CancelID and MatchID name the event that settled the offer, when one did.
	CancelID string
	MatchID  string
}

// EventID is the offer event's id.
func (e Entry) EventID() string { return e.Offer.EventID }

// Open reports whether the offer can still be taken.
func (e Entry) Open() bool { return e.State == StateOpen }

// Book derives canonical state for every valid offer in a set of market events.
//
// events may be any mixture of offers, takes, matches and cancellations, from any
// number of relays and from local storage, in any order: duplicates are dropped by
// event id, and an event that does not verify is not evidence of anything. That is
// why the caller can simply concatenate a relay's recent read with the local cache —
// a relay read is a discovery window, not the protocol's history, and the cache is
// what makes a cancellation seen yesterday still count today.
//
// Invalid offers are skipped rather than failing the scan: a marketplace is a stream
// from strangers, and one malformed advertisement must not blind a client to the rest.
func Book(events []event.Event, now time.Time) ([]Entry, error) {
	if now.IsZero() {
		now = time.Now()
	}

	verified := make([]event.Event, 0, len(events))
	seen := make(map[string]bool, len(events))

	for _, e := range events {
		if e.ID == "" || seen[e.ID] {
			continue
		}

		if err := event.Verify(e, now); err != nil {
			continue
		}

		seen[e.ID] = true
		verified = append(verified, e)
	}

	entries := make([]Entry, 0, len(verified))

	for _, e := range verified {
		//
		// Only a canonical, protocol-valid offer becomes a candidate. Everything
		// else in the set is evidence *about* offers, not an offer itself.
		//
		if _, err := ParseOffer(e, now); err != nil {
			continue
		}

		derived, err := DeriveState(e, verified, now.Unix())

		if err != nil {
			continue
		}

		entries = append(entries, Entry{
			Offer:    derived.Offer,
			State:    derived.State,
			CancelID: derived.CancelID,
			MatchID:  derived.MatchID,
		})
	}

	// Newest first, and by id within the same second, so two clients listing the same
	// evidence produce the same order.
	sort.SliceStable(entries, func(i, j int) bool {
		left, right := entries[i].Offer, entries[j].Offer

		if left.CreatedAt != right.CreatedAt {
			return left.CreatedAt > right.CreatedAt
		}

		return left.EventID > right.EventID
	})

	return entries, nil
}

// OpenOffers returns the entries that are open now, in book order.
//
// This is the older, human-facing view of the market: expired offers and valid
// cancellations are hidden, because somebody looking for a trade is not helped by
// either.
func OpenOffers(entries []Entry) []Entry {
	open := make([]Entry, 0, len(entries))

	for _, entry := range entries {
		if entry.Open() {
			open = append(open, entry)
		}
	}

	return open
}

// MarketFilters are the subscriptions one canonical market read issues.
//
// Cancellations and matches are asked for in larger numbers than offers, and that is
// not a tuning choice: they are the evidence that settles offers, so a window sized for
// advertisements would routinely hide the events that make one MATCHED or CANCELLED.
// The reference sizes them the same way.
func MarketFilters(limit int) []nostr.Filter {
	if limit <= 0 {
		limit = 50
	}

	evidence := limit * 4

	if evidence < 100 {
		evidence = 100
	}

	return []nostr.Filter{
		{Kinds: []int{1}, Topics: []string{MarketTag}, Limit: limit},
		{Kinds: []int{1}, Topics: []string{CancelTag}, Limit: evidence},
		{Kinds: []int{1}, Topics: []string{MatchTag}, Limit: evidence},
	}
}

// TakeEntry is one valid take of an offer.
type TakeEntry struct {
	// Event is the signed take.
	Event event.Event
	// Take is its parsed terms.
	Take market.Take
	// EventID and Taker are the event's id and author.
	EventID string
	Taker   string
}

// TakesFor returns the valid takes of one offer, newest first.
//
// A take is valid when it proves itself and when it is *about this offer*: the same
// economics, the same direction, the roles the direction implies, and an author who is
// the taker it names. Every one of those is a way to ask for a different trade than the
// offer describes, so a take that fails any of them is not a worse take, it is not a
// take at all.
func TakesFor(offer *ListedOffer, evidence []event.Event, now time.Time) []TakeEntry {
	if offer == nil {
		return nil
	}

	if now.IsZero() {
		now = time.Now()
	}

	seen := map[string]bool{}
	entries := make([]TakeEntry, 0, len(evidence))

	for _, e := range evidence {
		if e.ID == "" || seen[e.ID] {
			continue
		}

		if referenced, ok := e.TagValue("e"); !ok || referenced != offer.EventID {
			continue
		}

		take, err := ParseTake(e, now)

		if err != nil {
			continue
		}

		if err := take.ValidateAgainstOffer(offer.Offer, offer.EventID, offer.Maker); err != nil {
			continue
		}

		seen[e.ID] = true

		entries = append(entries, TakeEntry{
			Event:   e,
			Take:    *take,
			EventID: e.ID,
			Taker:   e.Pubkey,
		})
	}

	// Newest first, which is the order the reference lists them in: the take a maker
	// is deciding about is usually the most recent one.
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Event.CreatedAt != entries[j].Event.CreatedAt {
			return entries[i].Event.CreatedAt > entries[j].Event.CreatedAt
		}

		return entries[i].EventID > entries[j].EventID
	})

	return entries
}
