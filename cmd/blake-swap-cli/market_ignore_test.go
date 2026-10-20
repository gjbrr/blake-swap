package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
// Parity: tests/command_surface_tests.py
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

// TestIgnoredMatchTTLValidates: the TTL is a duration in seconds, and a value that is not one
// is refused where it is read.
func TestIgnoredMatchTTLValidates(t *testing.T) {
	t.Setenv("BLAKE_SWAP_IGNORED_MATCH_TTL_SECONDS", "")

	if ttl, err := ignoredMatchTTL(); err != nil || ttl != 0 {
		t.Fatalf("default = %d, %v", ttl, err)
	}

	t.Setenv("BLAKE_SWAP_IGNORED_MATCH_TTL_SECONDS", "60")

	if ttl, err := ignoredMatchTTL(); err != nil || ttl != 60 {
		t.Fatalf("configured = %d, %v", ttl, err)
	}

	for name, bad := range map[string]string{"a word": "soon", "negative": "-1"} {
		t.Setenv("BLAKE_SWAP_IGNORED_MATCH_TTL_SECONDS", bad)

		if _, err := ignoredMatchTTL(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestIgnoredMatchIDsReadsTombstones: a tombstone suppresses exactly the match it names.
func TestIgnoredMatchIDsReadsTombstones(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BLAKE_SWAP_STATE_DIR", dir)
	t.Setenv("BLAKE_SWAP_IGNORED_MATCH_TTL_SECONDS", "")

	matchID := strings.Repeat("ab", 32)
	path := filepath.Join(dir, ignoreDirectory, matchID+".json")

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	record := map[string]any{
		"match_id":   matchID,
		"ignored_at": time.Now().Unix(),
	}

	encoded, err := json.Marshal(record)

	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	ignored, err := ignoredMatchIDs()

	if err != nil {
		t.Fatal(err)
	}

	if !ignored[matchID] {
		t.Fatal("the tombstone does not suppress its match")
	}

	// A file that is not named after the match it records is not a tombstone.
	other := strings.Repeat("cd", 32)

	if err := os.WriteFile(filepath.Join(dir, ignoreDirectory, "not-a-tombstone.json"),
		[]byte(`{"match_id":"`+other+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	ignored, err = ignoredMatchIDs()

	if err != nil {
		t.Fatal(err)
	}

	if ignored["not-a-tombstone"] || ignored[other] {
		t.Fatal("a file that is not a tombstone suppressed something")
	}
}

// TestIgnoredMatchIDsExpireAndAreRemoved: an expired tombstone stops suppressing and stops
// existing, so a match dismissed by mistake comes back rather than disappearing for good.
func TestIgnoredMatchIDsExpireAndAreRemoved(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BLAKE_SWAP_STATE_DIR", dir)
	t.Setenv("BLAKE_SWAP_IGNORED_MATCH_TTL_SECONDS", "60")

	matchID := strings.Repeat("ef", 32)
	path := filepath.Join(dir, ignoreDirectory, matchID+".json")

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	// Ignored two minutes ago under a one-minute TTL: aged out.
	record := map[string]any{
		"match_id":   matchID,
		"ignored_at": time.Now().Add(-2 * time.Minute).Unix(),
	}

	encoded, err := json.Marshal(record)

	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	ignored, err := ignoredMatchIDs()

	if err != nil {
		t.Fatal(err)
	}

	if ignored[matchID] {
		t.Fatal("an expired tombstone still suppresses its match")
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("an expired tombstone was left on disk to be misread later")
	}
}
