//go:build !js

package marketops

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/coord"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/platform"
	"github.com/gjbrr/blake-swap/pkg/relaystub"
	"github.com/gjbrr/blake-swap/pkg/store"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// refusingEngine is an engine that must not be used.
//
// The reverse direction creates no atomic-swap state: the maker of a btc-for-blake offer pays
// the BTC leg and must never choose the hashlock. A test that let the engine run would pass
// while doing exactly the thing this path exists to prevent, so the double fails loudly.
type refusingEngine struct{ t *testing.T }

func (e refusingEngine) refuse(what string) {
	e.t.Helper()
	e.t.Fatalf("the reverse accept path called Engine.%s", what)
}

func (e refusingEngine) New(context.Context, swap.NewRequest) (*swap.NewResult, error) {
	e.refuse("New")
	return nil, nil
}

func (e refusingEngine) AcceptOffer(context.Context, swap.AcceptRequest) (*swap.AcceptResult, error) {
	e.refuse("AcceptOffer")
	return nil, nil
}

func (e refusingEngine) PublishOffer(context.Context, string) (*swap.MessageResult, error) {
	e.refuse("PublishOffer")
	return nil, nil
}

func (e refusingEngine) Coordination() (coord.Transport, nostr.Mode) {
	e.refuse("Coordination")
	return nil, ""
}

// reverseFixture is one btc-for-blake negotiation on a stub relay: Bob's offer and Alice's
// take, with Bob as the local participant.
type reverseFixture struct {
	env   *Env
	stub  *relaystub.Stub
	bob   *nostr.Key
	alice *nostr.Key
	offer event.Event
	take  event.Event
	// takeID is the canonical id of Alice's take.
	takeID string
}

// newReverseFixture publishes an offer and a take to a loopback relay.
func newReverseFixture(t *testing.T) *reverseFixture {
	t.Helper()

	stub, err := relaystub.New()

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(stub.Close)

	bob, err := nostr.NewKey(strings.Repeat("11", 32))

	if err != nil {
		t.Fatal(err)
	}

	alice, err := nostr.NewKey(strings.Repeat("22", 32))

	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().Unix()

	offerEvent, _, err := marketflow.BuildOffer(bob, marketflow.BuildOfferOptions{
		BlakeAmount: amount.MustParseBlake("10"),
		BTCSats:     50_000,
		Minutes:     30,
		Direction:   market.BTCForBlake,
		CreatedAt:   now,
	})

	if err != nil {
		t.Fatal(err)
	}

	listed, err := marketflow.ParseOffer(offerEvent, time.Now())

	if err != nil {
		t.Fatal(err)
	}

	takeEvent, _, err := marketflow.BuildTake(alice, marketflow.BuildTakeOptions{
		Offer:     *listed,
		CreatedAt: now,
	})

	if err != nil {
		t.Fatal(err)
	}

	stub.Inject(offerEvent)
	stub.Inject(takeEvent)

	env := &Env{
		Client: &nostr.Client{
			Dialer: func(ctx context.Context, url string) (nostr.Conn, error) {
				return platform.DialWebSocket(ctx, url)
			},
			Relays: []string{stub.URL},
			Cache:  nostr.NewMemoryCache(),
		},
		Identity: bob,
		Records:  store.NewMemory(),
		Files:    memoryFiles{},
		Engine:   refusingEngine{t: t},
		Random:   rand.Reader,
		Now:      time.Now,
	}

	return &reverseFixture{
		env: env, stub: stub, bob: bob, alice: alice,
		offer: offerEvent, take: takeEvent, takeID: takeEvent.ID,
	}
}

// TestAcceptReverseTakeReservesTheSwapAndKeepsNoSecret: the whole point of the direction.
func TestAcceptReverseTakeReservesTheSwapAndKeepsNoSecret(t *testing.T) {
	fixture := newReverseFixture(t)
	ctx := context.Background()

	acceptance, err := AcceptTake(ctx, fixture.env, fixture.takeID, &fixture.take, TakeHooks{})

	if err != nil {
		t.Fatalf("accepting the reverse take: %v", err)
	}

	if !acceptance.Reverse {
		t.Fatal("a btc-for-blake take was accepted as the forward direction")
	}

	if acceptance.Direction != market.BTCForBlake {
		t.Fatalf("direction = %q", acceptance.Direction)
	}

	// The maker of a btc-for-blake offer is Bob; the taker is Alice.
	if acceptance.MakerRole != "bob" || acceptance.TakerRole != "alice" {
		t.Fatalf("roles = %q/%q", acceptance.MakerRole, acceptance.TakerRole)
	}

	if acceptance.Bob != fixture.bob.Public || acceptance.Alice != fixture.alice.Public {
		t.Fatalf("identities = %q/%q", acceptance.Bob, acceptance.Alice)
	}

	if acceptance.OfferID != fixture.offer.ID {
		t.Fatalf("offer id = %q", acceptance.OfferID)
	}

	if len(acceptance.SwapID) != 12 || len(acceptance.MatchID) != 64 {
		t.Fatalf("swap = %q, match = %q", acceptance.SwapID, acceptance.MatchID)
	}

	// No swap record: Alice creates it from the match, on her machine.
	live, err := fixture.env.Records.List(ctx)

	if err != nil {
		t.Fatal(err)
	}

	if len(live) != 0 {
		t.Fatalf("the reverse accept created swap state: %v", live)
	}

	// Both selection records exist, name the reserved id and the match, and carry no secret.
	for _, name := range []string{
		selectionPath(fixture.takeID),
		selectionPath("offer-" + fixture.offer.ID),
	} {
		record, ok := fixture.env.Files.(memoryFiles)[name]

		if !ok {
			t.Fatalf("no selection at %s", name)
		}

		var document map[string]any

		if err := json.Unmarshal(record, &document); err != nil {
			t.Fatal(err)
		}

		if document["swap_id"] != acceptance.SwapID {
			t.Fatalf("%s: swap_id = %v, want %s", name, document["swap_id"], acceptance.SwapID)
		}

		if document["market_match_id"] != acceptance.MatchID {
			t.Fatalf("%s: market_match_id = %v", name, document["market_match_id"])
		}

		if document["direction"] != string(market.BTCForBlake) {
			t.Fatalf("%s: direction = %v", name, document["direction"])
		}

		for _, forbidden := range []string{"secret_hex", "secret_hash", "blake_bob_pub"} {
			if _, present := document[forbidden]; present {
				t.Fatalf("%s holds %q", name, forbidden)
			}
		}
	}

	// Exactly one match was published, and it is the one that was reported.
	published := matchesOn(fixture.stub.Events())

	if len(published) != 1 {
		t.Fatalf("published %d matches, want 1", len(published))
	}

	if published[0].ID != acceptance.MatchID {
		t.Fatalf("published match %q, reported %q", published[0].ID, acceptance.MatchID)
	}
}

// TestAcceptReverseTakeRepeatsWithoutASecondSwap: a retry is a success, not a second object.
func TestAcceptReverseTakeRepeatsWithoutASecondSwap(t *testing.T) {
	fixture := newReverseFixture(t)
	ctx := context.Background()

	first, err := AcceptTake(ctx, fixture.env, fixture.takeID, &fixture.take, TakeHooks{})

	if err != nil {
		t.Fatal(err)
	}

	second, err := AcceptTake(ctx, fixture.env, fixture.takeID, &fixture.take, TakeHooks{})

	if err != nil {
		t.Fatalf("the retry was refused: %v", err)
	}

	if !second.Repeated {
		t.Fatal("the retry did not report itself as a repeat")
	}

	if second.SwapID != first.SwapID || second.MatchID != first.MatchID {
		t.Fatalf("the retry changed the negotiation: %+v vs %+v", second, first)
	}

	if got := len(matchesOn(fixture.stub.Events())); got != 1 {
		t.Fatalf("the retry published %d matches, want 1", got)
	}
}

// TestAcceptReverseTakeRefusesAForeignIdentity: only the offer's maker may accept its take.
func TestAcceptReverseTakeRefusesAForeignIdentity(t *testing.T) {
	fixture := newReverseFixture(t)
	fixture.env.Identity = fixture.alice

	_, err := AcceptTake(context.Background(), fixture.env, fixture.takeID, &fixture.take, TakeHooks{})

	if err == nil || !strings.Contains(err.Error(), "did not create this market offer") {
		t.Fatalf("a foreign identity was accepted: %v", err)
	}

	if got := len(matchesOn(fixture.stub.Events())); got != 0 {
		t.Fatalf("%d matches were published by a refused acceptance", got)
	}
}

// TestAcceptReverseTakeRefusesTamperedEvidence: the canonical validator is what decides, so a
// take that does not verify cannot reserve anything.
func TestAcceptReverseTakeRefusesTamperedEvidence(t *testing.T) {
	fixture := newReverseFixture(t)

	tampered := fixture.take
	tampered.Content = strings.Replace(tampered.Content, "btc_sats", "btc_sats ", 1)

	if _, err := AcceptTake(context.Background(), fixture.env, fixture.takeID, &tampered, TakeHooks{}); err == nil {
		t.Fatal("a tampered take was accepted")
	}

	if got := len(matchesOn(fixture.stub.Events())); got != 0 {
		t.Fatalf("%d matches were published from tampered evidence", got)
	}
}

// TestAcceptReverseTakeRefusesDisagreeingSelections: two durable records that disagree are a
// fact about this machine, and repairing them would invent a negotiation.
func TestAcceptReverseTakeRefusesDisagreeingSelections(t *testing.T) {
	fixture := newReverseFixture(t)

	files := fixture.env.Files.(memoryFiles)
	files[selectionPath(fixture.takeID)] = []byte(`{"offer_id":"` + fixture.offer.ID +
		`","take_id":"` + fixture.takeID + `","swap_id":"aaaa00000001"}`)
	files[selectionPath("offer-"+fixture.offer.ID)] = []byte(`{"offer_id":"` + fixture.offer.ID +
		`","take_id":"` + fixture.takeID + `","swap_id":"bbbb00000002"}`)

	_, err := AcceptTake(context.Background(), fixture.env, fixture.takeID, &fixture.take, TakeHooks{})

	if err == nil || !strings.Contains(err.Error(), "selection records disagree") {
		t.Fatalf("disagreeing records were accepted: %v", err)
	}
}

// matchesOn returns the market matches among some events.
func matchesOn(events []event.Event) []event.Event {
	var found []event.Event

	for _, candidate := range events {
		if candidate.HasTag("t", marketflow.MatchTag) {
			found = append(found, candidate)
		}
	}

	return found
}
