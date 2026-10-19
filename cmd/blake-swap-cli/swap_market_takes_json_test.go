package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
)

// This file mirrors the reference's `tests/swap_market_takes_json_tests.py`: the machine-readable
// take list a UI or a script reads.
//
// Both amounts are emitted — the display value and the exact base units — because a UI needs the
// first and a settlement needs the second, and deriving one from the other is where rounding
// bugs live. The one deliberate difference from the reference is that this output is *only* the
// document: the reference prints a human "Nostr take read:" line to stdout first, which makes its
// own JSON flag unparseable.

// jsonTake is one take with the fields the JSON renderer reads.
func jsonTake() marketflow.TakeEntry {
	return marketflow.TakeEntry{
		Event: event.Event{
			ID:        strings.Repeat("c", 64),
			Pubkey:    strings.Repeat("d", 64),
			CreatedAt: 1234567890,
		},
		Take: market.Take{
			BlakeAmount: amount.Blake(1_000_000_000),
			BTCSats:     25_000,
			Direction:   market.BlakeForBTC,
			MakerRole:   "alice",
			TakerRole:   "bob",
		},
		EventID: strings.Repeat("c", 64),
		Taker:   strings.Repeat("d", 64),
	}
}

// TestSwapMarketTakesJSONEmitsTheValidatedTake: the document's shape, field by field.
func TestSwapMarketTakesJSONEmitsTheValidatedTake(t *testing.T) {
	offerID := strings.Repeat("a", 64)

	listed := &marketflow.ListedOffer{
		Offer: market.Offer{
			BlakeAmount: amount.Blake(1_000_000_000),
			BTCSats:     25_000,
			Direction:   market.BlakeForBTC,
		},
		EventID: offerID,
		Maker:   strings.Repeat("b", 64),
	}

	text, err := marketTakesJSON(offerID, listed, []marketflow.TakeEntry{jsonTake()})

	if err != nil {
		t.Fatal(err)
	}

	var parsed struct {
		OfferID string           `json:"offer_id"`
		Takes   []map[string]any `json:"takes"`
	}

	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("the JSON document does not parse: %v\n%s", err, text)
	}

	if parsed.OfferID != offerID {
		t.Fatalf("offer_id = %q", parsed.OfferID)
	}

	if len(parsed.Takes) != 1 {
		t.Fatalf("takes = %d", len(parsed.Takes))
	}

	take := parsed.Takes[0]

	for field, want := range map[string]any{
		"event_id":                strings.Repeat("c", 64),
		"taker":                   strings.Repeat("d", 64),
		"blake_amount":            10.0,
		"blake_amount_base_units": float64(1_000_000_000),
		"btc_sats":                float64(25_000),
		"created_at":              float64(1234567890),
		"direction":               "blake-for-btc",
		"maker_role":              "alice",
		"taker_role":              "bob",
	} {
		if take[field] != want {
			t.Errorf("%s = %v, want %v", field, take[field], want)
		}
	}
}

// TestSwapMarketTakesJSONIsOnlyTheDocument: no human line pollutes the machine-readable mode.
func TestSwapMarketTakesJSONIsOnlyTheDocument(t *testing.T) {
	offerID := strings.Repeat("a", 64)
	listed := &marketflow.ListedOffer{EventID: offerID}

	text, err := marketTakesJSON(offerID, listed, nil)

	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(text, "Nostr take read") {
		t.Fatalf("a human line was emitted into JSON mode:\n%s", text)
	}

	var parsed map[string]any

	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("the document does not parse: %v\n%s", err, text)
	}
}

// TestSwapMarketTakesHumanOutputIsPreserved: the human-readable view keeps the table, the count
// and the full ids, because the ids are what a person copies into the next command.
func TestSwapMarketTakesHumanOutputIsPreserved(t *testing.T) {
	offerID := strings.Repeat("a", 64)
	take := jsonTake()

	listed := &marketflow.ListedOffer{
		Offer:   market.Offer{BlakeAmount: 1_000_000_000, BTCSats: 25_000},
		EventID: offerID,
		Maker:   strings.Repeat("b", 64),
	}

	text := renderMarketTakes(offerID, listed, []marketflow.TakeEntry{take})

	for _, want := range []string{
		"TAKES FOR MARKET OFFER\n",
		"1 valid take(s)\n",
		"FULL TAKE ID(S)\n",
		take.EventID + "\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("human output is missing %q:\n%s", want, text)
		}
	}
}
