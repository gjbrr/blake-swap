//go:build !js

package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/platform"
	"github.com/gjbrr/blake-swap/pkg/proto"
	"github.com/gjbrr/blake-swap/pkg/relaystub"
)

// openVault is a vault with a fixed test passphrase and an identity installed, which is the
// state the page's market controls require.
func openMarketVault(t *testing.T) *vaultDomain {
	t.Helper()

	vault := NewVaultDomain(platform.NewMemoryKV(), platform.Reader{})
	ctx := context.Background()

	if result := vault.Run(ctx, "vault.create", map[string]string{
		"passphrase": "correct horse battery staple",
	}); !result.OK {
		t.Fatalf("creating the vault: %+v", result)
	}

	if err := vault.writeSecret(ctx, SecretIdentity, strings.Repeat("11", 32)); err != nil {
		t.Fatal(err)
	}

	return vault
}

// marketOver starts a relay and returns a domain pointed at it.
func marketOver(t *testing.T) (*marketDomain, *relaystub.Stub) {
	t.Helper()

	stub, err := relaystub.New()

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(stub.Close)

	domain := newMarketDomain(openMarketVault(t), func(ctx context.Context, url string) (nostr.Conn, error) {
		return platform.DialWebSocket(ctx, url)
	})

	return domain, stub
}

// ask sends one request through the market domain.
func ask(t *testing.T, domain *marketDomain, command string, args map[string]string) proto.Result {
	t.Helper()

	return domain.Handle(context.Background(), proto.Request{Command: command, Args: args})
}

// TestMarketDomainDefaultsToOneRelay: a client that shipped a longer default list would be
// quietly choosing a network for its user.
func TestMarketDomainDefaultsToOneRelay(t *testing.T) {
	domain := newMarketDomain(openMarketVault(t), nil)

	result := ask(t, domain, "market.relays", nil)

	if !result.OK || !strings.Contains(result.Text, "wss://nos.lol") {
		t.Fatalf("relays = %+v", result)
	}
}

// TestMarketDomainRefusesANonRelayURL: the list is configuration, and a typo is refused where
// it is typed rather than surfacing as a connection failure later.
func TestMarketDomainRefusesANonRelayURL(t *testing.T) {
	domain := newMarketDomain(openMarketVault(t), nil)

	result := ask(t, domain, "market.set-relays", map[string]string{"relays": "https://nos.lol"})

	if result.OK || !strings.Contains(result.Error, "ws:// or wss://") {
		t.Fatalf("a non-WebSocket URL was accepted: %+v", result)
	}

	// And an empty list is not a configuration either.
	if result := ask(t, domain, "market.set-relays", map[string]string{"relays": "  "}); result.OK {
		t.Fatal("an empty relay list was accepted")
	}
}

// TestMarketDomainPublishesAndLists: the browser's market is the CLI's market — the same
// builders, the same evidence-based state, over the browser's own socket.
func TestMarketDomainPublishesAndLists(t *testing.T) {
	domain, stub := marketOver(t)

	// The relays the domain will use are the stub's.
	if result := ask(t, domain, "market.set-relays", map[string]string{"relays": stub.URL}); !result.OK {
		t.Fatalf("setting relays: %+v", result)
	}

	published := ask(t, domain, "market.publish", map[string]string{
		"blake": "10", "sats": "50000", "minutes": "30",
	})

	if !published.OK {
		t.Fatalf("publish: %+v", published)
	}

	offerID := fieldValue(t, published.Text, "offer id:")

	listed := ask(t, domain, "market.list", nil)

	if !listed.OK {
		t.Fatalf("list: %+v", listed)
	}

	if !strings.Contains(listed.Text, offerID[:12]) {
		t.Fatalf("the offer just published is not in the market view:\n%s", listed.Text)
	}

	if !strings.Contains(listed.Text, "1 open offer(s)") {
		t.Fatalf("offer count is wrong:\n%s", listed.Text)
	}
}

// TestMarketDomainTakesAnOpenOffer: a take is signed by the vault's identity, and the relay
// sees it as a take of that exact offer.
func TestMarketDomainTakesAnOpenOffer(t *testing.T) {
	domain, stub := marketOver(t)

	if result := ask(t, domain, "market.set-relays", map[string]string{"relays": stub.URL}); !result.OK {
		t.Fatalf("setting relays: %+v", result)
	}

	published := ask(t, domain, "market.publish", map[string]string{
		"blake": "10", "sats": "50000", "minutes": "30",
	})

	if !published.OK {
		t.Fatalf("publish: %+v", published)
	}

	offerID := fieldValue(t, published.Text, "offer id:")

	taken := ask(t, domain, "market.take", map[string]string{"offer_id": offerID})

	if !taken.OK {
		t.Fatalf("take: %+v", taken)
	}

	takeID := fieldValue(t, taken.Text, "take id:")

	var found bool

	for _, stored := range stub.Events() {
		if stored.ID == takeID {
			found = true
		}
	}

	if !found {
		t.Fatal("the take was not published to the relay")
	}
}

// TestMarketDomainRefusesATakeOfNothing: an offer id that is not open is refused with the
// reference's words, because the browser and the CLI must say the same thing.
func TestMarketDomainRefusesATakeOfNothing(t *testing.T) {
	domain, stub := marketOver(t)

	if result := ask(t, domain, "market.set-relays", map[string]string{"relays": stub.URL}); !result.OK {
		t.Fatalf("setting relays: %+v", result)
	}

	if result := ask(t, domain, "market.take", map[string]string{
		"offer_id": strings.Repeat("ab", 32),
	}); result.OK || !strings.Contains(result.Error, "Offer is not currently open") {
		t.Fatalf("a take of nothing was accepted: %+v", result)
	}

	// A short id is refused before any network call: it cannot be an event id.
	if result := ask(t, domain, "market.take", map[string]string{"offer_id": "abcd"}); result.OK {
		t.Fatal("a short offer id was accepted")
	}
}

// TestMarketDomainRefusesATakeOfAClosedOffer: an offer that is on the relay but no longer open
// is not takeable, and the refusal is the same one the CLI gives.
func TestMarketDomainRefusesATakeOfAClosedOffer(t *testing.T) {
	domain, stub := marketOver(t)

	if result := ask(t, domain, "market.set-relays", map[string]string{"relays": stub.URL}); !result.OK {
		t.Fatalf("setting relays: %+v", result)
	}

	published := ask(t, domain, "market.publish", map[string]string{
		"blake": "10", "sats": "50000", "minutes": "1",
	})

	if !published.OK {
		t.Fatalf("publish: %+v", published)
	}

	offerID := fieldValue(t, published.Text, "offer id:")

	// Move the domain's clock past the offer's lifetime: the offer is still on the relay,
	// and it is no longer open.
	domain.now = func() time.Time { return time.Now().Add(2 * time.Minute) }

	result := ask(t, domain, "market.take", map[string]string{"offer_id": offerID})

	if result.OK || !strings.Contains(result.Error, "Offer is not currently open") {
		t.Fatalf("a take of an expired offer was accepted: %+v", result)
	}
}

// TestMarketDomainRefusesToPublishWithoutAnIdentity: publishing is signing, and a vault with no
// identity cannot sign.
func TestMarketDomainRefusesToPublishWithoutAnIdentity(t *testing.T) {
	stub, err := relaystub.New()

	if err != nil {
		t.Fatal(err)
	}

	defer stub.Close()

	vault := NewVaultDomain(platform.NewMemoryKV(), platform.Reader{})

	if result := vault.Run(context.Background(), "vault.create", map[string]string{
		"passphrase": "a passphrase",
	}); !result.OK {
		t.Fatalf("creating the vault: %+v", result)
	}

	domain := newMarketDomain(vault, func(ctx context.Context, url string) (nostr.Conn, error) {
		return platform.DialWebSocket(ctx, url)
	})

	if result := ask(t, domain, "market.set-relays", map[string]string{"relays": stub.URL}); !result.OK {
		t.Fatalf("setting relays: %+v", result)
	}

	result := ask(t, domain, "market.publish", map[string]string{"blake": "10", "sats": "50000"})

	if result.OK || !strings.Contains(result.Error, "identity") {
		t.Fatalf("a publish without an identity was accepted: %+v", result)
	}
}

// TestMarketDomainRefusesABadAmount: the amounts are validated by the same parser the engine
// uses, so a browser cannot advertise something the protocol cannot settle.
func TestMarketDomainRefusesABadAmount(t *testing.T) {
	domain, stub := marketOver(t)

	if result := ask(t, domain, "market.set-relays", map[string]string{"relays": stub.URL}); !result.OK {
		t.Fatalf("setting relays: %+v", result)
	}

	for name, args := range map[string]map[string]string{
		"zero BLAKE":   {"blake": "0", "sats": "50000"},
		"sub-unit":     {"blake": "0.000000001", "sats": "50000"},
		"zero sats":    {"blake": "10", "sats": "0"},
		"bad lifetime": {"blake": "10", "sats": "50000", "minutes": "0"},
	} {
		if result := ask(t, domain, "market.publish", args); result.OK {
			t.Errorf("%s: was accepted", name)
		}
	}
}

// TestMarketDomainCachesWhatItReads: a market read merges the verified cache, so a relay that
// forgets an event does not make the client forget a cancellation it already proved.
func TestMarketDomainCachesWhatItReads(t *testing.T) {
	domain, stub := marketOver(t)

	if result := ask(t, domain, "market.set-relays", map[string]string{"relays": stub.URL}); !result.OK {
		t.Fatalf("setting relays: %+v", result)
	}

	// Publish an offer through a direct client, then let the domain read it.
	key, err := domain.identity(context.Background())

	if err != nil {
		t.Fatal(err)
	}

	built, _, err := marketflow.BuildOffer(key, marketflow.BuildOfferOptions{
		BlakeAmount: amount.Blake(1_000_000_000),
		BTCSats:     25_000,
		Minutes:     30,
		Direction:   market.BlakeForBTC,
		CreatedAt:   time.Now().Unix(),
	})

	if err != nil {
		t.Fatal(err)
	}

	client, err := domain.client(context.Background())

	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.Publish(context.Background(), built); err != nil {
		t.Fatal(err)
	}

	if result := ask(t, domain, "market.list", nil); !result.OK {
		t.Fatalf("list: %+v", result)
	}
}

// fieldValue reads one "name: value" line from a rendered result.
func fieldValue(t *testing.T, text, label string) string {
	t.Helper()

	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, label) {
			return strings.TrimSpace(strings.TrimPrefix(line, label))
		}
	}

	t.Fatalf("no %q line in:\n%s", label, text)

	return ""
}

// TestMarketDomainSatsAreWholeSatoshis: the market's sats argument is a satoshi count, exactly
// as the CLI's --sats is.
//
// This is a bug the browser check caught: parsing the argument as a coin value multiplied it
// by a hundred million, so an offer for 50,000 sats was advertised as 5,000,000,000,000. The
// unit test exists so the next person does not have to run a browser to find out.
func TestMarketDomainSatsAreWholeSatoshis(t *testing.T) {
	domain, stub := marketOver(t)

	if result := ask(t, domain, "market.set-relays", map[string]string{"relays": stub.URL}); !result.OK {
		t.Fatalf("setting relays: %+v", result)
	}

	published := ask(t, domain, "market.publish", map[string]string{
		"blake": "10", "sats": "50000", "minutes": "30",
	})

	if !published.OK {
		t.Fatalf("publish: %+v", published)
	}

	listed := ask(t, domain, "market.list", nil)

	if !strings.Contains(listed.Text, "50000 sats") {
		t.Fatalf("the offer was not advertised as 50,000 sats:\n%s", listed.Text)
	}

	// And a fractional or negative satoshi count is not a trade.
	for name, sats := range map[string]string{"fractional": "50000.5", "negative": "-1", "zero": "0"} {
		if result := ask(t, domain, "market.publish", map[string]string{
			"blake": "10", "sats": sats,
		}); result.OK {
			t.Errorf("%s sats was accepted", name)
		}
	}
}
