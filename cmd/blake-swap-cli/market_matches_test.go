package main

import (
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
)

// TestRenderMarketMatchesMatchesTheReference pins the listing, including the banner that
// names the identity and the role note that explains the two role vocabularies.
func TestRenderMarketMatchesMatchesTheReference(t *testing.T) {
	me := strings.Repeat("22", 32)

	entry := marketflow.MatchEntry{
		Match: marketflow.ParsedMatch{
			Match: market.Match{
				OfferID:     strings.Repeat("ab", 32),
				TakeID:      strings.Repeat("cd", 32),
				SwapID:      "aaaa00000001",
				Maker:       strings.Repeat("11", 32),
				Taker:       me,
				Direction:   market.BlakeForBTC,
				MakerRole:   "alice",
				TakerRole:   "bob",
				BlakeAmount: amount.Blake(10_000_000),
				BTCSats:     25_000,
			},
			EventID: strings.Repeat("ef", 32),
		},
	}

	want := `
MARKET MATCHES FOR TAKER 222222222222
========================================

Market role: taker. The maker/taker roles are market roles; the protocol role each one binds depends on the direction.

MATCH 1
----------------------------------------
match id: ` + strings.Repeat("ef", 32) + `
offer id: ` + strings.Repeat("ab", 32) + `
take id:  ` + strings.Repeat("cd", 32) + `
swap id:  aaaa00000001
maker:    ` + strings.Repeat("11", 32) + `
taker:    ` + me + `
direction: blake-for-btc
maker is protocol: alice
taker is protocol: bob
selling:  0.1 BLAKE
asking:   25,000 sats

1 valid match(es)

PASS: every match signature verifies
PASS: maker identity verified
PASS: taker identity verified
PASS: market terms preserved
`

	if got := renderMarketMatches(me, []marketflow.MatchEntry{entry}); got != want {
		t.Fatalf("renderMarketMatches mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRenderMarketMatchesWithNone: an empty inbox says so and claims nothing.
func TestRenderMarketMatchesWithNone(t *testing.T) {
	me := strings.Repeat("22", 32)
	got := renderMarketMatches(me, nil)

	if !strings.Contains(got, "No valid matches found.\n") {
		t.Fatalf("output:\n%s", got)
	}

	if strings.Contains(got, "PASS:") {
		t.Fatalf("an empty listing made claims:\n%s", got)
	}
}
