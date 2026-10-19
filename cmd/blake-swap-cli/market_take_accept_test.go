package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/store"
)

// TestRenderMarketTakeAcceptedMatchesTheReference pins the block an operator reads after
// the maker's one irreversible market act.
func TestRenderMarketTakeAcceptedMatchesTheReference(t *testing.T) {
	offerID := strings.Repeat("ab", 32)
	takeID := strings.Repeat("cd", 32)
	alice := strings.Repeat("11", 32)
	bob := strings.Repeat("22", 32)

	got := renderMarketTakeAccepted(offerID, takeID, "aaaa00000001",
		alice, bob, 12_500_000_000, 150_000, 900, 800)

	want := "\nMARKET TAKE ACCEPTED\n" +
		"====================\n" +
		"offer id: " + offerID + "\n" +
		"take id:  " + takeID + "\n" +
		"swap id:  aaaa00000001\n" +
		"\n" +
		"Alice Nostr: " + alice + "\n" +
		"Bob Nostr:   " + bob + "\n" +
		"\n" +
		"BLAKE: 125.0\n" +
		"BTC sats: 150000\n" +
		"\n" +
		"BTC timeout:   900\n" +
		"BLAKE timeout: 800\n" +
		"\n" +
		"PASS: fresh secret/hashlock created\n" +
		"PASS: Bob pinned from exact signed take\n" +
		"PASS: Alice pinned from exact signed offer\n" +
		"PASS: no funds have moved\n"

	if got != want {
		t.Fatalf("renderMarketTakeAccepted mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestRenderMarketTakeAlreadyAcceptedMatchesTheReference: the duplicate path is not an
// error, and it must read as a successful no-op rather than as a second acceptance.
func TestRenderMarketTakeAlreadyAcceptedMatchesTheReference(t *testing.T) {
	offerID := strings.Repeat("ab", 32)
	takeID := strings.Repeat("cd", 32)

	got := renderMarketTakeAlreadyAccepted(offerID, takeID, "aaaa00000001")

	want := "\nMARKET TAKE ALREADY ACCEPTED\n" +
		"============================\n" +
		"offer id: " + offerID + "\n" +
		"take id:  " + takeID + "\n" +
		"swap id:  aaaa00000001\n" +
		"\n" +
		"PASS: duplicate command did not create another swap\n" +
		"PASS: offer/take selection records are mutually consistent\n" +
		"PASS: missing selection record, if any, was repaired\n"

	if got != want {
		t.Fatalf("renderMarketTakeAlreadyAccepted mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestRenderMarketBridgeMatchesTheReference pins the handover block, which is what tells
// an operator that the market stopped and the settlement engine took over.
func TestRenderMarketBridgeMatchesTheReference(t *testing.T) {
	offerID := strings.Repeat("ab", 32)
	takeID := strings.Repeat("cd", 32)
	matchID := strings.Repeat("ef", 32)

	got := renderMarketBridge(offerID, takeID, matchID, "aaaa00000001")

	want := "\nMARKET -> SWAP BRIDGE CREATED\n" +
		"=============================\n" +
		"market offer: " + offerID + "\n" +
		"market take:  " + takeID + "\n" +
		"market match: " + matchID + "\n" +
		"atomic swap:  aaaa00000001\n" +
		"\n" +
		"PASS: settlement engine was not modified\n" +
		"PASS: HTLC funding remains a separate explicit action\n"

	if got != want {
		t.Fatalf("renderMarketBridge mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestRenderReverseTakeAcceptedMatchesTheReference pins the block that says, in as many
// words, that Bob did not create Alice's secret.
func TestRenderReverseTakeAcceptedMatchesTheReference(t *testing.T) {
	offerID := strings.Repeat("ab", 32)
	takeID := strings.Repeat("cd", 32)
	matchID := strings.Repeat("ef", 32)

	got := renderReverseTakeAccepted(
		offerID, takeID, matchID, "aaaa00000001", "btc-for-blake", "bob", "alice",
	)

	want := "\nREVERSE MARKET TAKE ACCEPTED\n" +
		"============================\n" +
		"offer id: " + offerID + "\n" +
		"take id:  " + takeID + "\n" +
		"match id: " + matchID + "\n" +
		"swap id:  aaaa00000001\n" +
		"\n" +
		"direction: btc-for-blake\n" +
		"maker is protocol: bob\n" +
		"taker is protocol: alice\n" +
		"\n" +
		"PASS: maker selected exact signed take\n" +
		"PASS: public swap_id reserved\n" +
		"PASS: Bob did NOT create secret S\n" +
		"PASS: no atomic-swap state exists yet\n" +
		"PASS: no funds moved\n"

	if got != want {
		t.Fatalf("renderReverseTakeAccepted mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestRenderReverseTakeAlreadyAcceptedMatchesTheReference: a repeated reverse acceptance
// must not reserve a second swap id.
func TestRenderReverseTakeAlreadyAcceptedMatchesTheReference(t *testing.T) {
	offerID := strings.Repeat("ab", 32)
	takeID := strings.Repeat("cd", 32)

	got := renderReverseTakeAlreadyAccepted(offerID, takeID, "aaaa00000001")

	want := "\nREVERSE MARKET TAKE ALREADY ACCEPTED\n" +
		"====================================\n" +
		"offer id: " + offerID + "\n" +
		"take id:  " + takeID + "\n" +
		"swap id:  aaaa00000001\n" +
		"\n" +
		"PASS: no second swap ID was created\n" +
		"PASS: no secret was created by Bob\n"

	if got != want {
		t.Fatalf("renderReverseTakeAlreadyAccepted mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestPrefixHintNamesTheCommandThatFillsTheCache: a resolver refusal is only useful if it
// says what would have made the prefix resolvable.
func TestPrefixHintNamesTheCommandThatFillsTheCache(t *testing.T) {
	cases := map[string]string{
		"market-offer": "blake-swap market",
		"market-take":  "market-takes <offer>",
		"market-match": "match discovery",
		"":             "refresh the relevant Nostr view",
	}

	for messageType, want := range cases {
		if got := prefixHint(messageType); !strings.Contains(got, want) {
			t.Errorf("%q: hint = %q, want it to mention %q", messageType, got, want)
		}
	}
}

// TestRenderAmbiguousPrefixListsEveryCandidate: an ambiguous prefix must show all of the
// events it matched, or the operator cannot choose more characters.
func TestRenderAmbiguousPrefixListsEveryCandidate(t *testing.T) {
	first := strings.Repeat("ab", 32)
	second := strings.Repeat("ab", 31) + "cd"

	got := renderAmbiguousPrefix("abab", []string{first, second})

	for _, want := range []string{
		"\nAMBIGUOUS ID PREFIX\n",
		"===================\n",
		"abab\n\n",
		first + "\n",
		second + "\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output is missing %q:\n%s", want, got)
		}
	}
}

// TestResolveCachedPrefixIsPerMessageType: the same cached event must not resolve a
// prefix for a different message type, and a full id never consults the cache.
func TestResolveCachedPrefixIsPerMessageType(t *testing.T) {
	take := event.Event{
		ID:      strings.Repeat("ab", 32),
		Content: `{"type":"market-take","offer_id":"x"}`,
	}
	offer := event.Event{
		ID:      strings.Repeat("cd", 32),
		Content: `{"type":"market-offer"}`,
	}

	// The directory cache is the one the CLI uses; it is also the one that stores
	// unsigned fixtures for a resolver test without pretending they are evidence.
	cache := dirCache{dir: t.TempDir()}

	ctx := context.Background()

	for _, e := range []event.Event{take, offer} {
		if err := cache.Put(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	client := &nostr.Client{Cache: cache}

	resolved, err := resolveCachedPrefix(ctx, client, strings.ToUpper(take.ID[:8]), "market-take", "market take", false)

	if err != nil {
		t.Fatalf("a cached take was not resolved: %v", err)
	}

	if resolved != take.ID {
		t.Fatalf("resolved %q, want %q", resolved, take.ID)
	}

	// The offer's prefix is in the cache, but not as a take.
	_, err = resolveCachedPrefix(ctx, client, offer.ID[:8], "market-take", "market take", false)

	if err == nil {
		t.Fatal("an offer prefix resolved as a take")
	}

	if !strings.Contains(err.Error(), "run `market-takes <offer>` first") {
		t.Fatalf("refusal does not name the fill command: %v", err)
	}

	// Two takes sharing a prefix are ambiguous rather than first-wins.
	if err := cache.Put(ctx, event.Event{
		ID:      strings.Repeat("ab", 31) + "cd",
		Content: `{"type":"market-take","offer_id":"y"}`,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := resolveCachedPrefix(ctx, client, "abababab", "market-take", "market take", false); err == nil ||
		!strings.Contains(err.Error(), "more than one verified event") {
		t.Fatalf("an ambiguous prefix was not refused as ambiguous: %v", err)
	}

	// A full id is authoritative and needs no cache.
	full, err := resolveCachedPrefix(ctx, client, strings.Repeat("9f", 32), "market-take", "market take", false)

	if err != nil || full != strings.Repeat("9f", 32) {
		t.Fatalf("full id resolve = %q, %v", full, err)
	}
}

// TestRepeatAcceptRefusesDisagreeingRecords: two durable records that name different
// swaps are a fact that must not be repaired away.
func TestRepeatAcceptRefusesDisagreeingRecords(t *testing.T) {
	t.Setenv("BLAKE_SWAP_STATE_DIR", t.TempDir())

	alice, bob := strings.Repeat("11", 32), strings.Repeat("22", 32)

	err := repeatAcceptMarketTake(context.Background(), repeatAccept{
		OfferID:          strings.Repeat("ab", 32),
		TakeID:           strings.Repeat("cd", 32),
		Alice:            alice,
		Bob:              bob,
		OfferSelection:   map[string]any{"swap_id": "aaaa00000001"},
		TakeSelection:    map[string]any{"swap_id": "aaaa00000002"},
		OfferSelectionOn: true,
		TakeSelectionOn:  true,
	})

	if err == nil || !strings.Contains(err.Error(), "disagree about swap_id") {
		t.Fatalf("disagreeing records were not refused: %v", err)
	}
}

// TestRepeatAcceptRefusesAForeignIdentity: a record written by another participant must
// not be adopted by repairing it.
func TestRepeatAcceptRefusesAForeignIdentity(t *testing.T) {
	t.Setenv("BLAKE_SWAP_STATE_DIR", t.TempDir())

	err := repeatAcceptMarketTake(context.Background(), repeatAccept{
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
	})

	if err == nil || !strings.Contains(err.Error(), "Alice identity mismatch") {
		t.Fatalf("a foreign Alice identity was not refused: %v", err)
	}
}

// TestRepeatAcceptRepairsTheMissingHalf: an interrupted pair of writes is finished rather
// than left as a negotiation with no usable record.
func TestRepeatAcceptRepairsTheMissingHalf(t *testing.T) {
	dir := t.TempDir()

	t.Setenv("BLAKE_SWAP_STATE_DIR", dir)

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

	if err := store.NewFile(dir).Save(context.Background(), store.Alice, record); err != nil {
		t.Fatal(err)
	}

	canonical := map[string]any{
		"version":         1,
		"offer_id":        offerID,
		"take_id":         takeID,
		"swap_id":         "aaaa00000001",
		"nostr_alice_pub": alice,
		"nostr_bob_pub":   bob,
	}

	takePath := filepath.Join(dir, selectionsDirectory, takeID+".json")
	offerPath := filepath.Join(dir, selectionsDirectory, "offer-"+offerID+".json")

	if err := writeSelection(takePath, canonical); err != nil {
		t.Fatal(err)
	}

	if err := repeatAcceptMarketTake(context.Background(), repeatAccept{
		OfferID:          offerID,
		TakeID:           takeID,
		Alice:            alice,
		Bob:              bob,
		OfferSelection:   nil,
		TakeSelection:    canonical,
		TakePath:         takePath,
		OfferPath:        offerPath,
		OfferSelectionOn: false,
		TakeSelectionOn:  true,
	}); err != nil {
		t.Fatalf("repair failed: %v", err)
	}

	raw, err := os.ReadFile(offerPath)

	if err != nil {
		t.Fatalf("the missing record was not repaired: %v", err)
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

// TestRenderOfferPublishedMatchesTheReference: the swap offer that follows an accepted
// take is the ordinary publish-offer block, so its text has to be the reference's.
func TestRenderOfferPublishedMatchesTheReference(t *testing.T) {
	got := renderOfferPublished("aaaa00000001", ".blake-swaps-v2/messages/aaaa00000001.offer.json")

	want := "\nOFFER PUBLISHED\n" +
		"===============\n" +
		"swap id: aaaa00000001\n" +
		"message: .blake-swaps-v2/messages/aaaa00000001.offer.json\n" +
		"\n" +
		"PASS: secret_hex was NOT published\n"

	if got != want {
		t.Fatalf("renderOfferPublished mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}
