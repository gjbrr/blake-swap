package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
)

// takesFixture builds a listed offer and one validated take of it.
func takesFixture(t *testing.T) (*marketflow.ListedOffer, []marketflow.TakeEntry) {
	t.Helper()

	listed := &marketflow.ListedOffer{
		Offer: market.Offer{
			Direction:   market.BlakeForBTC,
			BlakeAmount: amount.Blake(10_000_000),
			BTCSats:     25_000,
		},
		EventID: strings.Repeat("ab", 32),
		Maker:   strings.Repeat("11", 32),
	}

	takes := []marketflow.TakeEntry{{
		EventID: strings.Repeat("ef", 32),
		Taker:   strings.Repeat("22", 32),
		Take: market.Take{
			OfferID:     listed.EventID,
			Direction:   market.BlakeForBTC,
			BlakeAmount: amount.Blake(10_000_000),
			BTCSats:     25_000,
			MakerRole:   "alice",
			TakerRole:   "bob",
		},
	}}

	takes[0].Event.CreatedAt = 1_700_000_200

	return listed, takes
}

// TestRenderMarketTakesMatchesTheReference pins the take table and its proof lines.
func TestRenderMarketTakesMatchesTheReference(t *testing.T) {
	listed, takes := takesFixture(t)

	want := `
TAKES FOR MARKET OFFER
======================

offer: ` + listed.EventID + `
selling: 0.1 BLAKE
asking:  25,000 sats

Take          Taker          Selling        Asking
------------  ------------   ------------   ------------
efefefefefef  222222222222   0.1 BLAKE     25,000 sats

1 valid take(s)

FULL TAKE ID(S)
===============
` + takes[0].EventID + `

PASS: original offer is still open
PASS: every take signature verifies
PASS: every take references this exact offer
PASS: maker and economic terms match
PASS: no swap was created
PASS: no money moved
`

	if got := renderMarketTakes(listed.EventID, listed, takes); got != want {
		t.Fatalf("renderMarketTakes mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRenderMarketTakesWithoutTakes: an untaken offer says so and claims nothing else.
func TestRenderMarketTakesWithoutTakes(t *testing.T) {
	listed, _ := takesFixture(t)

	got := renderMarketTakes(listed.EventID, listed, nil)

	for _, want := range []string{
		"No valid takes found.\n",
		"PASS: offer is open, but nobody has submitted a valid take\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output is missing %q:\n%s", want, got)
		}
	}

	if strings.Contains(got, "FULL TAKE ID(S)") {
		t.Fatalf("an empty inbox listed take ids:\n%s", got)
	}
}

// TestMarketTakesJSONCarriesBothAmounts: a UI needs the display value and a settlement
// needs the base units, and the reference emits both.
func TestMarketTakesJSONCarriesBothAmounts(t *testing.T) {
	listed, takes := takesFixture(t)

	text, err := marketTakesJSON(listed.EventID, listed, takes)

	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		`"blake_amount": 0.1`,
		`"blake_amount_base_units": 10000000`,
		`"btc_sats": 25000`,
		`"offer_id": "` + listed.EventID + `"`,
		`"maker_role": "alice"`,
		`"taker_role": "bob"`,
		`"direction": "blake-for-btc"`,
		`"created_at": 1700000200`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("json is missing %q:\n%s", want, text)
		}
	}

	if !strings.HasPrefix(text, "{\n  \"offer_id\":") {
		t.Fatalf("json is not indented and sorted:\n%s", text)
	}

	_ = time.Now
}
