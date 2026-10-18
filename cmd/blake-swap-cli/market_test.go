package main

import (
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
)

// listed builds one book entry for rendering, without signing anything: these tests are
// about the text, and the event that produced it is irrelevant to the layout.
func listed(id, maker string, direction market.Direction, blake float64, sats, createdAt, expiresAt int64) marketflow.Entry {
	return marketflow.Entry{
		Offer: marketflow.ListedOffer{
			Offer: market.Offer{
				Direction:   direction,
				BlakeAmount: amount.Blake(int64(blake * 1e8)),
				BTCSats:     sats,
				CreatedAt:   createdAt,
				ExpiresAt:   expiresAt,
			},
			EventID:   id,
			Maker:     maker,
			MakerRole: "alice",
			TakerRole: "bob",
		},
		State: marketflow.StateOpen,
	}
}

// TestRenderMarketMatchesTheReference pins the listing to the reference's bytes,
// including the two column widths that differ between the header and the rule.
func TestRenderMarketMatchesTheReference(t *testing.T) {
	entries := []marketflow.Entry{
		listed(strings.Repeat("ab", 32), strings.Repeat("cd", 32), market.BlakeForBTC, 0.1, 25_000, 1_700_000_000, 1_700_003_600),
		listed(strings.Repeat("ef", 32), strings.Repeat("01", 32), market.BTCForBlake, 2.5, 1_000_000, 1_700_000_000, 1_700_000_090),
	}

	want := `
AVAILABLE BLAKE/BTC OFFERS
==========================

Offer          Maker          Giving             Wanting            Expires
------------   ------------   --------------   --------------   --------
abababababab   cdcdcdcdcdcd   0.1 BLAKE          25,000 sats        1h
efefefefefef   010101010101   1,000,000 sats     2.5 BLAKE          1m

2 open offer(s)

Expired offers and valid maker cancellations are hidden.
`

	if got := renderMarket(entries, 1_700_000_000); got != want {
		t.Fatalf("renderMarket mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRenderMarketWithNoOffers: the empty listing stops after one sentence, because the
// reference returns before printing a count it would have to explain.
func TestRenderMarketWithNoOffers(t *testing.T) {
	want := `
AVAILABLE BLAKE/BTC OFFERS
==========================

No valid open offers found.
`

	if got := renderMarket(nil, 1_700_000_000); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// TestFormatTTLMatchesTheReference: seconds, then minutes, then hours, then days, with
// no negative time.
func TestFormatTTLMatchesTheReference(t *testing.T) {
	cases := map[int64]string{
		-5:      "0s",
		0:       "0s",
		59:      "59s",
		60:      "1m",
		3599:    "59m",
		3600:    "1h",
		86_399:  "23h",
		86_400:  "1d",
		172_800: "2d",
	}

	for remaining, want := range cases {
		if got := formatTTL(remaining); got != want {
			t.Errorf("formatTTL(%d) = %q, want %q", remaining, got, want)
		}
	}
}

// TestPythonFloat: whole numbers keep a decimal point, because the reference's output
// is a float and its text is compared.
func TestPythonFloat(t *testing.T) {
	cases := map[float64]string{
		0.1:       "0.1",
		1:         "1.0",
		2.5:       "2.5",
		1_000_000: "1e+06",
	}

	for value, want := range cases {
		if got := pythonFloat(value); got != want {
			t.Errorf("pythonFloat(%v) = %q, want %q", value, got, want)
		}
	}
}

// TestRenderMarketStateMatchesTheReference: one line per offer, whatever its state.
func TestRenderMarketStateMatchesTheReference(t *testing.T) {
	open := listed(strings.Repeat("ab", 32), strings.Repeat("cd", 32), market.BlakeForBTC, 0.1, 25_000, 1_700_000_000, 1_700_003_600)
	matched := listed(strings.Repeat("ef", 32), strings.Repeat("01", 32), market.BTCForBlake, 2.5, 1_000_000, 1_700_000_000, 1_700_000_090)
	matched.State = marketflow.StateMatched
	matched.MatchID = strings.Repeat("99", 32)

	want := `
BLAKE/BTC MARKET STATE
======================

abababababab OPEN blake-for-btc 0.1 BLAKE 25,000 sats
efefefefefef MATCHED btc-for-blake 2.5 BLAKE 1,000,000 sats
`

	if got := renderMarketState([]marketflow.Entry{open, matched}); got != want {
		t.Fatalf("renderMarketState mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestMarketStateJSONCarriesTheReferenceKeys: the machine-readable boundary names every
// field the reference names, and nulls the two that may be absent.
func TestMarketStateJSONCarriesTheReferenceKeys(t *testing.T) {
	entry := listed(strings.Repeat("ab", 32), strings.Repeat("cd", 32), market.BlakeForBTC, 1, 25_000, 1_700_000_000, 1_700_003_600)

	text, err := marketStateJSON([]marketflow.Entry{entry})

	if err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{
		`"event_id"`, `"maker"`, `"created_at"`, `"expires_at"`, `"blake_amount"`,
		`"btc_sats"`, `"direction"`, `"maker_role"`, `"taker_role"`, `"state"`,
		`"cancel_event_id"`, `"match_event_id"`,
	} {
		if !strings.Contains(text, key) {
			t.Fatalf("json is missing %s:\n%s", key, text)
		}
	}

	// Sorted keys, two-space indent: the reference prints with sort_keys=True.
	if !strings.HasPrefix(text, "{\n  \"offers\": [\n    {\n      \"blake_amount\": 1.0,") {
		t.Fatalf("json shape:\n%s", text)
	}

	if !strings.Contains(text, `"cancel_event_id": null`) || !strings.Contains(text, `"match_event_id": null`) {
		t.Fatalf("absent ids are not null:\n%s", text)
	}
}
