package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
)

// ignoreEntry builds one match entry for the renderers.
func ignoreEntry(matchID, swapID string) marketflow.MatchEntry {
	return marketflow.MatchEntry{
		Event: eventWithID(matchID),
		Match: marketflow.ParsedMatch{
			Match: market.Match{
				OfferID:     strings.Repeat("ab", 32),
				TakeID:      strings.Repeat("cd", 32),
				SwapID:      swapID,
				Maker:       strings.Repeat("11", 32),
				Taker:       strings.Repeat("22", 32),
				Direction:   market.BlakeForBTC,
				BlakeAmount: amount.Blake(10_000_000),
				BTCSats:     25_000,
			},
			EventID: matchID,
		},
	}
}

// TestRenderIgnoredMatchesTheReference pins the tombstone confirmation.
func TestRenderIgnoredMatchesTheReference(t *testing.T) {
	matchID := strings.Repeat("ef", 32)

	want := `
MARKET MATCH IGNORED
====================
match: ` + matchID + `
swap:  aaaa00000001

PASS: local tombstone created
PASS: no Nostr event was deleted or modified
PASS: no swap state was created
PASS: no funds moved
`

	if got := renderIgnored(matchID, "aaaa00000001"); got != want {
		t.Fatalf("renderIgnored mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	// A match with no swap id prints Python's None, not an empty field.
	if got := renderIgnored(matchID, ""); !strings.Contains(got, "swap:  None\n") {
		t.Fatalf("missing swap id rendered as:\n%s", got)
	}
}

// TestRenderAlreadyIgnoredMatchesTheReference: a second ignore says nothing happened.
func TestRenderAlreadyIgnoredMatchesTheReference(t *testing.T) {
	matchID := strings.Repeat("ef", 32)

	want := "\nMATCH ALREADY IGNORED\n" +
		"=====================\n" +
		"match: " + matchID + "\n"

	if got := renderAlreadyIgnored(matchID); got != want {
		t.Fatalf("renderAlreadyIgnored mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestRenderAmbiguousMatchesListsCandidates: an ambiguous prefix must show what it
// matched, or the operator has no way to choose more characters.
func TestRenderAmbiguousMatchesListsCandidates(t *testing.T) {
	first := ignoreEntry(strings.Repeat("ef", 32), "aaaa00000001")
	second := ignoreEntry(strings.Repeat("ef", 31)+"ff", "")

	got := renderAmbiguousMatches([]marketflow.MatchEntry{first, second})

	for _, want := range []string{
		"AMBIGUOUS MATCH PREFIX\n",
		first.Event.ID + " swap=aaaa00000001\n",
		second.Event.ID + " swap=None\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output is missing %q:\n%s", want, got)
		}
	}
}

// eventWithID builds a bare event carrying an id, for renderers that print one.
func eventWithID(id string) event.Event { return event.Event{ID: id} }

// TestWriteTombstoneIsAtomicAndComplete: the marker must exist whole or not at all, and
// it must carry the facts the next run needs to recognise the match.
func TestWriteTombstoneIsAtomicAndComplete(t *testing.T) {
	dir := t.TempDir()
	entry := ignoreEntry(strings.Repeat("ef", 32), "aaaa00000001")
	path := dir + "/" + entry.Event.ID + ".json"

	if err := writeTombstone(path, entry.Event.ID, entry); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)

	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		`"match_id"`, `"swap_id": "aaaa00000001"`, `"offer_id"`, `"take_id"`,
		`"direction": "blake-for-btc"`, `"blake_amount": 0.1`, `"btc_sats": 25000`,
		`"local_only": true`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("tombstone is missing %q:\n%s", want, raw)
		}
	}

	// No temporary file is left behind, which is what makes the rename the publish.
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("directory holds %d files, want 1", len(entries))
	}
}

// TestLocalSwapExists: the refusal that protects a trade in progress.
func TestLocalSwapExists(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BLAKE_SWAP_STATE_DIR", dir)

	if localSwapExists("aaaa00000001") {
		t.Fatal("a swap with no record was reported as existing")
	}

	if err := os.WriteFile(filepath.Join(dir, "aaaa00000001.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	if !localSwapExists("aaaa00000001") {
		t.Fatal("a swap record was not seen")
	}

	if localSwapExists("") {
		t.Fatal("an empty swap id was reported as existing")
	}
}
