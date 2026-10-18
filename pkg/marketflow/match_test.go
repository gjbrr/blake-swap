package marketflow

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/nostr"
)

// matchFixture is an offer, a take and the parsed forms a match needs.
type matchFixture struct {
	offerEvent event.Event
	takeEvent  event.Event
	offer      ListedOffer
	take       market.Take
}

// buildMatchFixture builds a consistent offer and take.
func buildMatchFixture(t *testing.T) matchFixture {
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

	return matchFixture{
		offerEvent: offerEvent,
		takeEvent:  takeEvent,
		offer:      *offer,
		take:       take,
	}
}

// buildMatch builds a match for the fixture.
func buildMatch(t *testing.T, f matchFixture) event.Event {
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

func TestMatchRoundTrip(t *testing.T) {
	f := buildMatchFixture(t)
	built := buildMatch(t, f)

	parsed, err := ParseMatch(built, f.offerEvent, f.takeEvent, time.Unix(1_700_000_400, 0))

	if err != nil {
		t.Fatal(err)
	}

	if parsed.OfferID != f.offer.EventID || parsed.TakeID != f.takeEvent.ID {
		t.Fatalf("parsed = %+v", parsed)
	}

	if parsed.SwapID != "aaaa00000001" {
		t.Fatalf("swap id = %q", parsed.SwapID)
	}

	if parsed.MakerPubkey != f.offer.Maker || parsed.Taker != f.take.Taker {
		t.Fatalf("parties = %s/%s", parsed.MakerPubkey, parsed.Taker)
	}

	if parsed.MakerRole != "alice" || parsed.TakerRole != "bob" {
		t.Fatalf("roles = %s/%s", parsed.MakerRole, parsed.TakerRole)
	}

	if parsed.BlakeAmount != f.offer.BlakeAmount || parsed.BTCSats != f.offer.BTCSats {
		t.Fatalf("economics changed: %+v", parsed.Match)
	}
}

// TestMatchMustBeSignedByTheMaker: a taker that could publish a match could choose
// which take it is matched with.
func TestMatchMustBeSignedByTheMaker(t *testing.T) {
	f := buildMatchFixture(t)

	_, _, err := BuildMatch(taker(t), BuildMatchOptions{
		Offer:       f.offer,
		OfferEvent:  f.offerEvent,
		TakeEvent:   f.takeEvent,
		Taker:       &f.take,
		SwapID:      "aaaa00000001",
		BlakeAmount: f.offer.BlakeAmount,
		BTCSats:     f.offer.BTCSats,
		CreatedAt:   1_700_000_300,
	})

	if err == nil || !strings.Contains(err.Error(), "signed by the offer maker") {
		t.Fatalf("err = %v", err)
	}
}

// TestMatchCannotChangeTheTerms: the match restates the offer's economics, so a
// different number is a different trade wearing the same take.
func TestMatchCannotChangeTheTerms(t *testing.T) {
	f := buildMatchFixture(t)
	built := buildMatch(t, f)

	mangled := resignWithContentFor(t, built, maker(t), func(message map[string]any) {
		message["btc_sats"] = 30_000
	})

	_, err := ParseMatch(mangled, f.offerEvent, f.takeEvent, time.Unix(1_700_000_400, 0))

	if err == nil || !strings.Contains(err.Error(), "changed BTC terms") {
		t.Fatalf("err = %v", err)
	}
}

// TestMatchCannotReferenceAnotherTake keeps the evidence bound to the take it names.
func TestMatchCannotReferenceAnotherTake(t *testing.T) {
	f := buildMatchFixture(t)
	built := buildMatch(t, f)

	other := f.takeEvent
	other.ID = strings.Repeat("ab", 32)

	_, err := ParseMatch(built, f.offerEvent, other, time.Unix(1_700_000_400, 0))

	if err == nil || !strings.Contains(err.Error(), "different take") {
		t.Fatalf("err = %v", err)
	}
}

// TestMatchRefusesAnExpiredOffer: at expires_at the offer is already gone, so a match
// created then is not a match of a live advertisement.
func TestMatchRefusesAnExpiredOffer(t *testing.T) {
	f := buildMatchFixture(t)

	_, _, err := BuildMatch(maker(t), BuildMatchOptions{
		Offer:       f.offer,
		OfferEvent:  f.offerEvent,
		TakeEvent:   f.takeEvent,
		Taker:       &f.take,
		SwapID:      "aaaa00000002",
		BlakeAmount: f.offer.BlakeAmount,
		BTCSats:     f.offer.BTCSats,
		CreatedAt:   f.offer.ExpiresAt,
	})

	if err == nil || !strings.Contains(err.Error(), "offer expiry") {
		t.Fatalf("err = %v", err)
	}
}

// TestMatchRefusesToPredateTheTake: a maker cannot seal a take before it exists.
func TestMatchRefusesToPredateTheTake(t *testing.T) {
	f := buildMatchFixture(t)

	_, _, err := BuildMatch(maker(t), BuildMatchOptions{
		Offer:       f.offer,
		OfferEvent:  f.offerEvent,
		TakeEvent:   f.takeEvent,
		Taker:       &f.take,
		SwapID:      "aaaa00000003",
		BlakeAmount: f.offer.BlakeAmount,
		BTCSats:     f.offer.BTCSats,
		CreatedAt:   f.takeEvent.CreatedAt - 1,
	})

	if err == nil || !strings.Contains(err.Error(), "predates the take") {
		t.Fatalf("err = %v", err)
	}
}

// TestMatchRefusesATakeFromAnotherOffer keeps the match from binding a take that
// belongs to a different advertisement.
func TestMatchRefusesATakeFromAnotherOffer(t *testing.T) {
	f := buildMatchFixture(t)

	take := f.take
	take.OfferID = strings.Repeat("cd", 32)

	_, _, err := BuildMatch(maker(t), BuildMatchOptions{
		Offer:       f.offer,
		OfferEvent:  f.offerEvent,
		TakeEvent:   f.takeEvent,
		Taker:       &take,
		SwapID:      "aaaa00000004",
		BlakeAmount: f.offer.BlakeAmount,
		BTCSats:     f.offer.BTCSats,
		CreatedAt:   1_700_000_300,
	})

	if err == nil || !strings.Contains(err.Error(), "does not reference the offer") {
		t.Fatalf("err = %v", err)
	}
}

// resignWithContentFor rewrites an event's content and re-signs it with a key.
func resignWithContentFor(
	t *testing.T, built event.Event, key *nostr.Key, mutate func(map[string]any),
) event.Event {
	t.Helper()

	var message map[string]any

	if err := json.Unmarshal([]byte(built.Content), &message); err != nil {
		t.Fatal(err)
	}

	mutate(message)

	content, err := event.CanonicalJSON(message)

	if err != nil {
		t.Fatal(err)
	}

	mangled := built
	mangled.Content = string(content)

	resignEvent(t, &mangled, key)

	return mangled
}

// TestParseMatchRefusesAnotherSigner: a match must be signed by the offer's maker,
// not merely claim to be.
func TestParseMatchRefusesAnotherSigner(t *testing.T) {
	f := buildMatchFixture(t)
	built := buildMatch(t, f)

	// Keep the content, change the signer: the taker now vouches for the match.
	resigned := built
	resignEvent(t, &resigned, taker(t))

	_, err := ParseMatch(resigned, f.offerEvent, f.takeEvent, time.Unix(1_700_000_400, 0))

	if err == nil || !strings.Contains(err.Error(), "original maker") {
		t.Fatalf("err = %v", err)
	}
}

// TestParseMatchRefusesAnExpiredMatch is the receive-side half of the expiry rule: a
// match created at or after the offer's expiry is not a match of a live offer, even
// if it is otherwise well formed.
func TestParseMatchRefusesAnExpiredMatch(t *testing.T) {
	f := buildMatchFixture(t)
	built := buildMatch(t, f)

	expired := built
	expired.CreatedAt = f.offer.ExpiresAt

	// The content carries the same time, so only the boundary check can refuse it.
	mangled := resignWithContentFor(t, expired, maker(t), func(message map[string]any) {
		message["created_at"] = f.offer.ExpiresAt
	})

	_, err := ParseMatch(mangled, f.offerEvent, f.takeEvent, time.Unix(f.offer.ExpiresAt+10, 0))

	if err == nil || !strings.Contains(err.Error(), "offer expiry") {
		t.Fatalf("err = %v", err)
	}
}
