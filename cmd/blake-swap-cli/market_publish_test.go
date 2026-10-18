package main

import (
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// TestRenderPublishedOfferMatchesTheReference pins the confirmation block, including
// the reference's uneven label spacing (it comes from print's separating space after a
// label that already ends in spaces).
func TestRenderPublishedOfferMatchesTheReference(t *testing.T) {
	offer := market.Offer{
		Direction:   market.BlakeForBTC,
		BlakeAmount: amount.Blake(10_000_000),
		BTCSats:     25_000,
		CreatedAt:   1_700_000_000,
		ExpiresAt:   1_700_003_600,
	}

	want := `
MARKET OFFER PUBLISHED
======================
offer id: ` + strings.Repeat("ab", 32) + `
maker:    ` + strings.Repeat("cd", 32) + `
giving:   0.1 BLAKE
wanting:  25,000 sats
direction: blake-for-btc
maker protocol role: alice
lifetime: 60 minute(s)
expires:  1700003600

PASS: advertisement contains no secret, hashlock, HTLC, or chain wallet keys
`

	got := renderPublishedOffer(strings.Repeat("ab", 32), strings.Repeat("cd", 32), offer, 60)

	if got != want {
		t.Fatalf("renderPublishedOffer mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRenderPublishedOfferReversedDirection: the terms swap when the maker gives BTC.
func TestRenderPublishedOfferReversedDirection(t *testing.T) {
	offer := market.Offer{
		Direction:   market.BTCForBlake,
		BlakeAmount: amount.Blake(250_000_000),
		BTCSats:     1_000_000,
		ExpiresAt:   1_700_003_600,
	}

	got := renderPublishedOffer("id", "maker", offer, 30)

	for _, want := range []string{
		"giving:   1,000,000 sats\n",
		"wanting:  2.5 BLAKE\n",
		"direction: btc-for-blake\n",
		"maker protocol role: bob\n",
		"lifetime: 30 minute(s)\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output is missing %q:\n%s", want, got)
		}
	}
}

// TestRenderCancelledOfferMatchesTheReference pins the cancellation block.
func TestRenderCancelledOfferMatchesTheReference(t *testing.T) {
	offerID := strings.Repeat("ab", 32)
	cancelID := strings.Repeat("ef", 32)
	maker := strings.Repeat("cd", 32)

	want := `
MARKET OFFER CANCELLED
======================
offer id:  ` + offerID + `
cancel id: ` + cancelID + `
maker:     ` + maker + `

PASS: cancellation was signed by the original offer maker
`

	if got := renderCancelledOffer(offerID, cancelID, maker); got != want {
		t.Fatalf("renderCancelledOffer mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestEngineKeyRefusesWithoutAnIdentity: signing a market event needs the secret, and a
// nil or public-only identity is refused rather than producing an unsigned event.
func TestEngineKeyRefusesWithoutAnIdentity(t *testing.T) {
	if _, err := engineKey(nil); err == nil {
		t.Fatal("a nil engine produced a key")
	}

	if _, err := engineKey(&swap.Engine{}); err == nil {
		t.Fatal("an engine with no identity produced a key")
	}
}
