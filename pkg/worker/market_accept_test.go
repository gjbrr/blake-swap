//go:build !js

// The market accept surface: what the worker refuses before it touches a relay.
package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/platform"
	"github.com/gjbrr/blake-swap/pkg/proto"
	"github.com/gjbrr/blake-swap/pkg/relaystub"
)

// TestMarketAcceptRefusesBadIDs: a malformed id is refused as what it is, not looked up.
func TestMarketAcceptRefusesBadIDs(t *testing.T) {
	proxy := newFakeProxy(t)
	_, vault := engineDomainFor(t, proxy.server.URL)
	domain := newMarketDomain(vault, nil)

	cases := []struct {
		command string
		args    map[string]string
		want    string
	}{
		{"market.takes", map[string]string{}, "64-character"},
		{"market.takes", map[string]string{"offer_id": "short"}, "64-character"},
		{"market.accept-take", map[string]string{"take_id": "abc"}, "64-character"},
		{"market.accept-match", map[string]string{"match_id": ""}, "64-character"},
		{"market.accept-reverse-offer", map[string]string{"match_id": "short"}, "64-character"},
		{"market.cancel", map[string]string{"offer_id": "nope"}, "64-character"},
	}

	for _, test := range cases {
		result := domain.Handle(context.Background(), proto.Request{
			Command: test.command, Args: test.args,
		})

		if result.OK {
			t.Fatalf("%s %v was accepted", test.command, test.args)
		}

		if !strings.Contains(result.Error, test.want) {
			t.Fatalf("%s %v: error = %q, want %q", test.command, test.args, result.Error, test.want)
		}
	}
}

// TestMarketAcceptNeedsAnEngine: the accept path creates a swap, so a worker without an
// engine must say so rather than half-accept a take.
func TestMarketAcceptNeedsAnEngine(t *testing.T) {
	proxy := newFakeProxy(t)
	_, vault := engineDomainFor(t, proxy.server.URL)
	domain := newMarketDomain(vault, nil)

	id := strings.Repeat("11", 32)

	for _, command := range []string{
		"market.accept-take", "market.accept-match", "market.accept-reverse-offer",
	} {
		argument := map[string]string{"take_id": id}

		if command != "market.accept-take" {
			argument = map[string]string{"match_id": id}
		}

		result := domain.Handle(context.Background(), proto.Request{
			Command: command, Args: argument,
		})

		if result.OK || !strings.Contains(result.Error, "no engine") {
			t.Fatalf("%s = %+v", command, result)
		}
	}
}

// TestMarketAcceptTakeReverseDirection: the browser can be the reverse maker.
//
// Bob publishes a btc-for-blake offer through the page's own command, Alice takes it, and the
// accept reserves a swap id and publishes the match. No swap record is created: in this
// direction the maker pays the BTC leg and must never choose the hashlock, so the taker's
// client is the one that creates the swap.
func TestMarketAcceptTakeReverseDirection(t *testing.T) {
	stub, err := relaystub.New()

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(stub.Close)

	proxy := newFakeProxy(t)
	engine, vault := engineDomainFor(t, proxy.server.URL)

	if err := vault.writeSecret(context.Background(), SecretRelays, stub.URL); err != nil {
		t.Fatal(err)
	}

	domain := newMarketDomain(vault, func(ctx context.Context, url string) (nostr.Conn, error) {
		return platform.DialWebSocket(ctx, url)
	}).withEngine(engine.build)

	published := ask(t, domain, "market.publish", map[string]string{
		"blake": "10", "sats": "50000", "minutes": "30", "direction": "btc-for-blake",
	})

	if !published.OK {
		t.Fatalf("publishing the reverse offer: %+v", published)
	}

	offerID := fieldValue(t, published.Text, "offer id:")

	// Alice is another participant, so her take arrives through the relay.
	alice, err := nostr.NewKey(strings.Repeat("22", 32))

	if err != nil {
		t.Fatal(err)
	}

	var offerEvent event.Event

	for _, candidate := range stub.Events() {
		if candidate.ID == offerID {
			offerEvent = candidate
		}
	}

	if offerEvent.ID == "" {
		t.Fatalf("the published offer %s is not on the relay", offerID)
	}

	listed, err := marketflow.ParseOffer(offerEvent, time.Now())

	if err != nil {
		t.Fatal(err)
	}

	take, _, err := marketflow.BuildTake(alice, marketflow.BuildTakeOptions{
		Offer: *listed, CreatedAt: time.Now().Unix(),
	})

	if err != nil {
		t.Fatal(err)
	}

	stub.Inject(take)

	accepted := ask(t, domain, "market.accept-take", map[string]string{"take_id": take.ID})

	if !accepted.OK {
		t.Fatalf("accepting the reverse take: %+v", accepted)
	}

	var action proto.ActionView

	if err := proto.DecodeData(accepted, &action); err != nil {
		t.Fatal(err)
	}

	if action.Kind != "accept-market-take" {
		t.Fatalf("action kind = %q", action.Kind)
	}

	details := map[string]string{}

	for _, fact := range action.Details {
		details[fact.Label] = fact.Value
	}

	if details["direction"] != string(market.BTCForBlake) {
		t.Fatalf("direction = %q", details["direction"])
	}

	if details["maker is protocol"] != "bob" || details["taker is protocol"] != "alice" {
		t.Fatalf("roles = %q/%q", details["maker is protocol"], details["taker is protocol"])
	}

	if details["match id"] == "" || details["take id"] != take.ID {
		t.Fatalf("action names the wrong negotiation: %v", details)
	}

	if !strings.Contains(details["swap id"], "reserved") {
		t.Fatalf("the reserved swap id was not described as reserved: %q", details["swap id"])
	}

	// The record store is untouched: this client created no swap and holds no secret.
	live, err := vault.store.List(context.Background())

	if err != nil {
		t.Fatal(err)
	}

	if len(live) != 0 {
		t.Fatalf("the reverse accept created swap state: %v", live)
	}
}
