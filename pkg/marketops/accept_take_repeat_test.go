package marketops

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/store"
)

// These tests moved with the code they test: the repeat path lives here now, so its
// assertions do too. None of them was loosened.

// repeatFixture is one already-recorded acceptance, as the operation sees it.
func repeatFixture(files memoryFiles, record *model.Swap) (*Env, repeatRequest) {
	env := testEnv(files)

	alice, bob := strings.Repeat("11", 32), strings.Repeat("22", 32)

	if record != nil {
		_ = env.Records.Save(context.Background(), store.Alice, record)
	}

	return env, repeatRequest{
		OfferID:          strings.Repeat("ab", 32),
		TakeID:           strings.Repeat("cd", 32),
		Alice:            alice,
		Bob:              bob,
		OfferSelection:   map[string]any{"swap_id": "aaaa00000001"},
		TakeSelection:    map[string]any{"swap_id": "aaaa00000002"},
		OfferSelectionOn: true,
		TakeSelectionOn:  true,
	}
}

// TestRepeatAcceptRefusesDisagreeingRecords: two durable records that name different
// swaps are a fact that must not be repaired away.
func TestRepeatAcceptRefusesDisagreeingRecords(t *testing.T) {
	env, req := repeatFixture(memoryFiles{}, nil)

	_, err := repeatAccept(context.Background(), env, req, TakeHooks{})

	if err == nil || !strings.Contains(err.Error(), "disagree about swap_id") {
		t.Fatalf("disagreeing records were not refused: %v", err)
	}
}

// TestRepeatAcceptRefusesAForeignIdentity: a record written by another participant must
// not be adopted by repairing it.
func TestRepeatAcceptRefusesAForeignIdentity(t *testing.T) {
	env := testEnv(memoryFiles{})

	_, err := repeatAccept(context.Background(), env, repeatRequest{
		OfferID: strings.Repeat("ab", 32),
		TakeID:  strings.Repeat("cd", 32),
		Alice:   strings.Repeat("11", 32),
		Bob:     strings.Repeat("22", 32),
		TakeSelection: map[string]any{
			"swap_id":         "aaaa00000001",
			"nostr_alice_pub": strings.Repeat("33", 32),
			"nostr_bob_pub":   strings.Repeat("22", 32),
		},
		TakeSelectionOn: true,
	}, TakeHooks{})

	if err == nil || !strings.Contains(err.Error(), "Alice identity mismatch") {
		t.Fatalf("a foreign Alice identity was not refused: %v", err)
	}
}

// TestRepeatAcceptRepairsTheMissingHalf: an interrupted pair of writes is finished rather
// than left as a negotiation with no usable record.
func TestRepeatAcceptRepairsTheMissingHalf(t *testing.T) {
	alice, bob := strings.Repeat("11", 32), strings.Repeat("22", 32)
	offerID, takeID := strings.Repeat("ab", 32), strings.Repeat("cd", 32)

	record := &model.Swap{
		SwapID:               "aaaa00000001",
		State:                "OFFERED",
		SecretHash:           strings.Repeat("00", 32),
		BlakeAmountBaseUnits: 100,
		BTCSats:              1000,
		NostrAlicePub:        &alice,
		NostrBobPub:          &bob,
	}

	canonical := map[string]any{
		"version":         1,
		"offer_id":        offerID,
		"take_id":         takeID,
		"swap_id":         "aaaa00000001",
		"nostr_alice_pub": alice,
		"nostr_bob_pub":   bob,
	}

	encoded, err := json.Marshal(canonical)

	if err != nil {
		t.Fatal(err)
	}

	files := memoryFiles{selectionPath(takeID): encoded}
	env := testEnv(files)

	if err := env.Records.Save(context.Background(), store.Alice, record); err != nil {
		t.Fatal(err)
	}

	if _, err := repeatAccept(context.Background(), env, repeatRequest{
		OfferID:          offerID,
		TakeID:           takeID,
		Alice:            alice,
		Bob:              bob,
		TakeSelection:    canonical,
		TakeName:         selectionPath(takeID),
		OfferName:        selectionPath("offer-" + offerID),
		OfferSelectionOn: false,
		TakeSelectionOn:  true,
		Direction:        market.BlakeForBTC,
	}, TakeHooks{}); err != nil {
		t.Fatalf("repair failed: %v", err)
	}

	raw, ok := files[selectionPath("offer-"+offerID)]

	if !ok {
		t.Fatalf("the missing record was not repaired: %v", files)
	}

	var repaired map[string]any

	if err := json.Unmarshal(raw, &repaired); err != nil {
		t.Fatal(err)
	}

	if repaired["swap_id"] != "aaaa00000001" || repaired["take_id"] != takeID {
		t.Fatalf("repaired record disagrees with the canonical one: %v", repaired)
	}
}

// TestSameSelectionComparesEveryField: record equality is what decides whether two
// selection files describe one negotiation.
func TestSameSelectionComparesEveryField(t *testing.T) {
	base := map[string]any{"offer_id": "aa", "take_id": "bb", "swap_id": "cc"}

	if !sameSelection(base, map[string]any{"offer_id": "aa", "take_id": "bb", "swap_id": "cc"}) {
		t.Fatal("identical records compared unequal")
	}

	for name, other := range map[string]map[string]any{
		"missing field":  {"offer_id": "aa", "take_id": "bb"},
		"extra field":    {"offer_id": "aa", "take_id": "bb", "swap_id": "cc", "x": "d"},
		"different swap": {"offer_id": "aa", "take_id": "bb", "swap_id": "dd"},
	} {
		if sameSelection(base, other) {
			t.Errorf("%s compared equal", name)
		}
	}
}
