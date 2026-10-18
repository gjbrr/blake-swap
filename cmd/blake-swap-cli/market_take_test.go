package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/nostr"
)

// TestRenderInspectedTakeShowsTheEventAndProvesIt: the preview must contain the event's
// own facts, its tags and its content, and must say the event was not published.
func TestRenderInspectedTakeShowsTheEventAndProvesIt(t *testing.T) {
	key, err := nostr.NewKey(strings.Repeat("22", 32))

	if err != nil {
		t.Fatal(err)
	}

	maker, err := nostr.NewKey(strings.Repeat("11", 32))

	if err != nil {
		t.Fatal(err)
	}

	offerEvent, _, err := marketflow.BuildOffer(maker, marketflow.BuildOfferOptions{
		BlakeAmount: amount.Blake(10_000_000),
		BTCSats:     25_000,
		Minutes:     30,
		Direction:   market.BlakeForBTC,
		CreatedAt:   1_700_000_000,
		Nonce:       "0011223344556677",
	})

	if err != nil {
		t.Fatal(err)
	}

	listed, err := marketflow.ParseOffer(offerEvent, time.Unix(1_700_000_100, 0))

	if err != nil {
		t.Fatal(err)
	}

	built, _, err := marketflow.BuildTake(key, marketflow.BuildTakeOptions{
		Offer:     *listed,
		CreatedAt: 1_700_000_200,
		Nonce:     "8899aabbccddeeff",
	})

	if err != nil {
		t.Fatal(err)
	}

	rendered, err := renderInspectedTake(built)

	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"\nMARKET TAKE EVENT VALID\n=======================\n",
		"event id: " + built.ID + "\n",
		"signer:   " + key.PublicKey() + "\n",
		"kind:     1\n",
		"tags:\n[\n",
		`"blake-swap-market-take"`,
		"content:\n{\n",
		`"offer_id": "` + offerEvent.ID + `"`,
		"PASS: event ID recomputes correctly\n",
		"PASS: Bob's Schnorr signature verifies\n",
		"PASS: event was NOT published\n",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("output is missing %q:\n%s", want, rendered)
		}
	}

	// Sorted keys, as the reference prints with sort_keys=True.
	first := strings.Index(rendered, "content:\n")
	last := strings.LastIndex(rendered, "\n\nPASS: event ID")
	content := rendered[first+len("content:\n") : last]

	keys := []string{"\"blake_amount\"", "\"btc_sats\"", "\"created_at\"", "\"offer_id\""}

	for index := 1; index < len(keys); index++ {
		if strings.Index(content, keys[index-1]) > strings.Index(content, keys[index]) {
			t.Fatalf("content keys are not sorted:\n%s", content)
		}
	}
}
