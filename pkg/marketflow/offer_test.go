package marketflow

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/keys"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/nostr"
)

// maker is the identity that signs market offers in these tests.
func maker(t *testing.T) *nostr.Key {
	t.Helper()

	key, err := nostr.NewKey(strings.Repeat("11", 32))

	if err != nil {
		t.Fatal(err)
	}

	return key
}

// buildTestOffer builds one offer at a fixed time.
func buildTestOffer(t *testing.T) (event.Event, market.Offer) {
	t.Helper()

	built, offer, err := BuildOffer(maker(t), BuildOfferOptions{
		BlakeAmount: amount.Blake(1_000_000),
		BTCSats:     25_000,
		Minutes:     30,
		Direction:   market.BlakeForBTC,
		CreatedAt:   1_700_000_000,
	})

	if err != nil {
		t.Fatal(err)
	}

	return built, offer
}

func TestOfferRoundTrip(t *testing.T) {
	built, offer := buildTestOffer(t)

	parsed, err := ParseOffer(built, time.Unix(1_700_000_100, 0))

	if err != nil {
		t.Fatal(err)
	}

	if parsed.EventID != built.ID || parsed.Maker != built.Pubkey {
		t.Fatalf("parsed = %+v", parsed)
	}

	if parsed.BlakeAmount != offer.BlakeAmount || parsed.BTCSats != offer.BTCSats {
		t.Fatalf("economics changed: %+v", parsed.Offer)
	}

	if parsed.Direction != market.BlakeForBTC {
		t.Fatalf("direction = %q", parsed.Direction)
	}

	// The message and the event agree about time, which is what stops a relay from
	// redating a signed advertisement.
	if parsed.CreatedAt != built.CreatedAt {
		t.Fatalf("created_at = %d, event = %d", parsed.CreatedAt, built.CreatedAt)
	}

	if parsed.MakerRole != "alice" || parsed.TakerRole != "bob" {
		t.Fatalf("roles = %s/%s", parsed.MakerRole, parsed.TakerRole)
	}

	if parsed.ExpiredAt(1_700_000_100) {
		t.Fatal("a fresh offer is already expired")
	}

	if !parsed.ExpiredAt(1_700_000_000 + 30*60) {
		t.Fatal("an offer did not expire at its lifetime")
	}
}

// TestOfferTagsMatchContent: the direction is in the message *and* in a signed tag.
// A mismatch is how a mis-signed advertisement would change which side funds which
// chain, so it is refused rather than resolved in favour of one of them.
func TestOfferTagsMatchContent(t *testing.T) {
	built, _ := buildTestOffer(t)

	mangled := built
	mangled.Tags = [][]string{
		{"t", MarketTag},
		{"base", "BLAKE"},
		{"quote", "BTC"},
		{"direction", string(market.BTCForBlake)},
	}

	// Re-sign, so the only thing wrong is the disagreement.
	resign(t, &mangled)

	if _, err := ParseOffer(mangled, time.Unix(1_700_000_100, 0)); err == nil ||
		!strings.Contains(err.Error(), "direction does not match") {
		t.Fatalf("err = %v", err)
	}
}

// TestOfferRefusesAForgedSignature: a relay can hand over anything, and the
// signature is the only thing that says who wrote it.
func TestOfferRefusesAForgedSignature(t *testing.T) {
	built, _ := buildTestOffer(t)

	forged := built
	forged.Sig = strings.Repeat("00", 64)

	if _, err := ParseOffer(forged, time.Unix(1_700_000_100, 0)); err == nil {
		t.Fatal("a forged signature was accepted")
	}
}

// TestOfferRefusesSettlementData: an advertisement must not leak the contract.
func TestOfferRefusesSettlementData(t *testing.T) {
	built, offer := buildTestOffer(t)

	message := map[string]any{
		"type":         "market-offer",
		"version":      2,
		"base":         "BLAKE",
		"quote":        "BTC",
		"direction":    string(offer.Direction),
		"blake_amount": offer.BlakeAmount.Decimal().String(),
		"btc_sats":     offer.BTCSats,
		"created_at":   offer.CreatedAt,
		"expires_at":   offer.ExpiresAt,
		"nonce":        offer.Nonce,
		"secret_hash":  strings.Repeat("ab", 32),
	}

	content, err := event.CanonicalJSON(message)

	if err != nil {
		t.Fatal(err)
	}

	mangled := built
	mangled.Content = string(content)
	resign(t, &mangled)

	if _, err := ParseOffer(mangled, time.Unix(1_700_000_100, 0)); err == nil ||
		!strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("err = %v", err)
	}
}

// TestOfferRefusesAnotherProtocolsEvent: a valid signature does not make an event a
// market offer.
func TestOfferRefusesAnotherProtocolsEvent(t *testing.T) {
	built, _ := buildTestOffer(t)

	mangled := built
	mangled.Tags = [][]string{{"t", "something-else"}}
	resign(t, &mangled)

	if _, err := ParseOffer(mangled, time.Unix(1_700_000_100, 0)); err == nil ||
		!strings.Contains(err.Error(), "market tag") {
		t.Fatalf("err = %v", err)
	}

	// An unrelated kind is refused too.
	kindMangled := built
	kindMangled.Kind = 4
	resign(t, &kindMangled)

	if _, err := ParseOffer(kindMangled, time.Unix(1_700_000_100, 0)); err == nil ||
		!strings.Contains(err.Error(), "kind") {
		t.Fatalf("err = %v", err)
	}
}

// TestOfferRefusesATimeMismatch: the content timestamp and the event timestamp are
// the same fact, and one of them being editable would make the other meaningless.
func TestOfferRefusesATimeMismatch(t *testing.T) {
	built, offer := buildTestOffer(t)

	message := map[string]any{
		"type":         "market-offer",
		"version":      2,
		"base":         "BLAKE",
		"quote":        "BTC",
		"direction":    string(offer.Direction),
		"blake_amount": offer.BlakeAmount.Decimal().String(),
		"btc_sats":     offer.BTCSats,
		"created_at":   offer.CreatedAt,
		"expires_at":   offer.ExpiresAt,
		"nonce":        offer.Nonce,
	}

	content, err := event.CanonicalJSON(message)

	if err != nil {
		t.Fatal(err)
	}

	mangled := built
	mangled.Content = string(content)
	mangled.CreatedAt = built.CreatedAt + 60
	resign(t, &mangled)

	if _, err := ParseOffer(mangled, time.Unix(1_700_000_100, 0)); err == nil ||
		!strings.Contains(err.Error(), "created_at mismatch") {
		t.Fatalf("err = %v", err)
	}
}

// TestOfferRefusesBadEconomics covers the amounts and the lifetime.
func TestOfferRefusesBadEconomics(t *testing.T) {
	cases := map[string]struct {
		mutate func(map[string]any)
		want   string
	}{
		"zero BLAKE": {
			mutate: func(m map[string]any) { m["blake_amount"] = "0" },
			want:   "BLAKE amount",
		},
		"negative BTC": {
			mutate: func(m map[string]any) { m["btc_sats"] = -1 },
			want:   "BTC amount",
		},
		"expiry before creation": {
			mutate: func(m map[string]any) { m["expires_at"] = m["created_at"].(int64) },
			want:   "expiry",
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			built, offer := buildTestOffer(t)

			message := map[string]any{
				"type":         "market-offer",
				"version":      2,
				"base":         "BLAKE",
				"quote":        "BTC",
				"direction":    string(offer.Direction),
				"blake_amount": offer.BlakeAmount.Decimal().String(),
				"btc_sats":     offer.BTCSats,
				"created_at":   offer.CreatedAt,
				"expires_at":   offer.ExpiresAt,
				"nonce":        offer.Nonce,
			}

			c.mutate(message)

			content, err := event.CanonicalJSON(message)

			if err != nil {
				t.Fatal(err)
			}

			mangled := built
			mangled.Content = string(content)
			resign(t, &mangled)

			if _, err := ParseOffer(mangled, time.Unix(1_700_000_100, 0)); err == nil ||
				!strings.Contains(strings.ToLower(err.Error()), strings.ToLower(c.want)) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// TestBuildRefusesAnUnknownDirection: the direction decodes to protocol roles, and
// an unknown one has none.
func TestBuildRefusesAnUnknownDirection(t *testing.T) {
	_, _, err := BuildOffer(maker(t), BuildOfferOptions{
		BlakeAmount: amount.Blake(1_000_000),
		BTCSats:     25_000,
		Minutes:     30,
		Direction:   market.Direction("blake-for-nothing"),
	})

	if err == nil {
		t.Fatal("an unknown direction was accepted")
	}
}

// TestOfferNonceIsFresh: two identical trades published in the same second must not
// collapse into one event.
func TestOfferNonceIsFresh(t *testing.T) {
	first, firstOffer := buildTestOffer(t)
	second, secondOffer := buildTestOffer(t)

	if firstOffer.Nonce == secondOffer.Nonce {
		t.Fatal("two offers share a nonce")
	}

	if first.ID == second.ID {
		t.Fatal("two offers are the same event")
	}
}

// resign signs an event again after a test has changed it.
func resign(t *testing.T, e *event.Event) {
	t.Helper()

	key := maker(t)

	signer, err := keys.FromSecretHex(key.Secret)

	if err != nil {
		t.Fatal(err)
	}

	e.Pubkey = key.Public

	id, err := e.ComputeID()

	if err != nil {
		t.Fatal(err)
	}

	e.ID = id

	if err := event.Sign(e, signer); err != nil {
		t.Fatal(err)
	}
}

// TestOfferCarriesAnExpirationTag: an offer is a stored advertisement, so it says when a relay
// may forget it. NIP-40's expiration tag repeats the content's expires_at, and the two are held
// together: a maker cannot keep an offer on a relay past the terms it signed.
func TestOfferCarriesAnExpirationTag(t *testing.T) {
	built, offer := buildTestOffer(t)

	value, ok := built.TagValue("expiration")

	if !ok {
		t.Fatal("the offer carries no expiration tag")
	}

	if value != strconv.FormatInt(offer.ExpiresAt, 10) {
		t.Fatalf("expiration tag %q, want %d", value, offer.ExpiresAt)
	}

	parsed, err := ParseOffer(built, time.Unix(1_700_000_100, 0))

	if err != nil {
		t.Fatalf("an offer with its own expiration tag was refused: %v", err)
	}

	if parsed.ExpiresAt != offer.ExpiresAt {
		t.Fatalf("parsed expires_at %d", parsed.ExpiresAt)
	}

	// A tag that disagrees with the content is refused even though the signature is valid:
	// the signer is the same key, the terms are not.
	forged := built
	forged.Tags = make([][]string, len(built.Tags))

	for i, tag := range built.Tags {
		forged.Tags[i] = append([]string(nil), tag...)

		if tag[0] == "expiration" {
			forged.Tags[i][1] = strconv.FormatInt(offer.ExpiresAt+3600, 10)
		}
	}

	id, err := forged.ComputeID()

	if err != nil {
		t.Fatal(err)
	}

	forged.ID = id

	signer, err := keys.FromSecretHex(maker(t).Secret)

	if err != nil {
		t.Fatal(err)
	}

	if err := event.Sign(&forged, signer); err != nil {
		t.Fatal(err)
	}

	if _, err := ParseOffer(forged, time.Unix(1_700_000_100, 0)); err == nil {
		t.Fatal("an offer whose expiration tag disagrees with its content was accepted")
	}
}
