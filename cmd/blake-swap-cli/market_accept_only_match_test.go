package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/nostr"
)

// TestRenderNoPendingMatchMatchesTheReference pins the empty-inbox block.
func TestRenderNoPendingMatchMatchesTheReference(t *testing.T) {
	want := "\nNO PENDING MATCH\n" +
		"================\n" +
		"No unimported valid market match for this participant was found.\n"

	if got := renderNoPendingMatch(); got != want {
		t.Fatalf("renderNoPendingMatch mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestRenderOnePendingMatchMatchesTheReference pins the block that names the trade about
// to be imported.
func TestRenderOnePendingMatchMatchesTheReference(t *testing.T) {
	entry := matchEntry("aaaa00000001")

	got := renderOnePendingMatch(entry)

	want := "\nONE PENDING MATCH FOUND\n" +
		"=======================\n" +
		"match: " + entry.Event.ID + "\n" +
		"swap:  aaaa00000001\n" +
		"trade: 125.0 BLAKE / 150,000 sats\n" +
		"\n"

	if got != want {
		t.Fatalf("renderOnePendingMatch mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestRenderMultiplePendingMatchesRefusesToChoose: two counterparties means a decision,
// and the command must not make it.
func TestRenderMultiplePendingMatchesRefusesToChoose(t *testing.T) {
	first := matchEntry("aaaa00000001")
	second := matchEntry("bbbb00000002")

	got := renderMultiplePendingMatches([]marketflow.MatchEntry{first, second})

	want := "\nMULTIPLE PENDING MATCHES\n" +
		"========================\n" +
		"More than one valid unimported match belongs to this participant.\n" +
		"Refusing to choose automatically.\n" +
		"\n" +
		"1. match=" + first.Event.ID[:12] + "  swap=aaaa00000001\n" +
		"2. match=" + second.Event.ID[:12] + "  swap=bbbb00000002\n" +
		"\n" +
		"Use `accept-match <match-prefix>` for the one you want.\n"

	if got != want {
		t.Fatalf("renderMultiplePendingMatches mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestRenderMatchAlreadyBoundMatchesTheReference: a retry after a crash must read as a
// successful no-op, with its own wording.
func TestRenderMatchAlreadyBoundMatchesTheReference(t *testing.T) {
	matchID := strings.Repeat("ab", 32)

	got := renderMatchAlreadyBound(matchID, "aaaa00000001")

	want := "\nMARKET MATCH ALREADY IMPORTED\n" +
		"=============================\n" +
		"match id: " + matchID + "\n" +
		"swap id:  aaaa00000001\n" +
		"\n" +
		"PASS: duplicate command did not create a new swap\n" +
		"PASS: durable market binding is intact\n" +
		"PASS: no funds moved\n"

	if got != want {
		t.Fatalf("renderMatchAlreadyBound mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestLocallyBoundMatchReadsBothBindingShapes: the forward binding records
// `first_match_id` and the reverse one `match_id`, and a retry has to find either.
func TestLocallyBoundMatchReadsBothBindingShapes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BLAKE_SWAP_STATE_DIR", dir)

	bindings := filepath.Join(dir, matchBindingDirectory)

	if err := os.MkdirAll(bindings, 0o700); err != nil {
		t.Fatal(err)
	}

	forward := strings.Repeat("7a", 32)
	reverse := strings.Repeat("8b", 32)

	if err := writeBinding(filepath.Join(bindings, "aaaa00000001.json"), map[string]any{
		"swap_id": "aaaa00000001", "first_match_id": forward,
	}); err != nil {
		t.Fatal(err)
	}

	if err := writeBinding(filepath.Join(bindings, "bbbb00000002.json"), map[string]any{
		"swap_id": "bbbb00000002", "match_id": reverse,
	}); err != nil {
		t.Fatal(err)
	}

	swapID, matchID, ok := locallyBoundMatch(forward[:12])

	if !ok || swapID != "aaaa00000001" || matchID != forward {
		t.Fatalf("forward binding lookup = %q, %q, %v", swapID, matchID, ok)
	}

	swapID, matchID, ok = locallyBoundMatch(reverse[:12])

	if !ok || swapID != "bbbb00000002" || matchID != reverse {
		t.Fatalf("reverse binding lookup = %q, %q, %v", swapID, matchID, ok)
	}

	if _, _, ok := locallyBoundMatch(strings.Repeat("cc", 6)); ok {
		t.Fatal("an unknown prefix was reported as bound")
	}
}

// TestLocallyBoundMatchIgnoresNonSwapFiles: the directory may hold anything, and a file
// that is not named after a swap is not a binding.
func TestLocallyBoundMatchIgnoresNonSwapFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BLAKE_SWAP_STATE_DIR", dir)

	bindings := filepath.Join(dir, matchBindingDirectory)

	if err := os.MkdirAll(bindings, 0o700); err != nil {
		t.Fatal(err)
	}

	matchID := strings.Repeat("9d", 32)
	path := filepath.Join(bindings, "not-a-swap-id.json")

	if err := os.WriteFile(path, []byte(`{"match_id":"`+matchID+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, ok := locallyBoundMatch(matchID[:8]); ok {
		t.Fatal("a file that is not a swap binding was treated as one")
	}
}

// TestCompleteMatchEvidenceFetchesMissingReferences: a match whose take was never seen is
// still evidence, because the two references are immutable and can be fetched by id.
func TestCompleteMatchEvidenceFetchesMissingReferences(t *testing.T) {
	me := strings.Repeat("44", 32)
	offerID := strings.Repeat("11", 32)
	takeID := strings.Repeat("22", 32)

	body, err := json.Marshal(map[string]any{
		"type":     "market-match",
		"taker":    me,
		"offer_id": offerID,
		"take_id":  takeID,
	})

	if err != nil {
		t.Fatal(err)
	}

	candidate := event.Event{
		ID:      strings.Repeat("55", 32),
		Tags:    [][]string{{"t", marketflow.MatchTag}},
		Content: string(body),
	}

	// A client with no relays and an empty cache cannot resolve anything, so the
	// evidence is returned unchanged rather than failing the whole read. It has no
	// relays on purpose: a unit test must not depend on one being reachable.
	client := &nostr.Client{Cache: dirCache{dir: t.TempDir()}}

	before := []event.Event{candidate}

	after := completeMatchEvidence(context.Background(), client, before, me)

	if len(after) != len(before) {
		t.Fatalf("evidence changed without any source: %d -> %d", len(before), len(after))
	}

	// Evidence for another participant is not ours to complete.
	other := candidate
	other.Content = strings.Replace(other.Content, me, strings.Repeat("99", 32), 1)

	if got := completeMatchEvidence(
		context.Background(), client, []event.Event{other}, me,
	); len(got) != 1 {
		t.Fatalf("another participant's match gained evidence: %d", len(got))
	}
}

// TestFirstArg: the optional prefix is the only argument the command accepts.
func TestFirstArg(t *testing.T) {
	if got := firstArg(nil); got != "" {
		t.Errorf("no arguments = %q", got)
	}

	if got := firstArg([]string{"abcd"}); got != "abcd" {
		t.Errorf("one argument = %q", got)
	}
}

// matchEntry is one validated match with the fields the renderers read.
func matchEntry(swapID string) marketflow.MatchEntry {
	return marketflow.MatchEntry{
		Event: event.Event{ID: strings.Repeat("ab", 32), Pubkey: strings.Repeat("33", 32)},
		Match: marketflow.ParsedMatch{Match: market.Match{
			OfferID:     strings.Repeat("11", 32),
			TakeID:      strings.Repeat("22", 32),
			SwapID:      swapID,
			BlakeAmount: 12_500_000_000,
			BTCSats:     150_000,
			Direction:   market.BlakeForBTC,
		}},
	}
}

// TestPendingMatchesKeepsOnlyImportableMatches: the three ways a valid match is not a
// candidate — it is this participant's own receipt, its swap already exists here, or its
// swap was retired.
func TestPendingMatchesKeepsOnlyImportableMatches(t *testing.T) {
	me := strings.Repeat("44", 32)

	mine := matchEntry("aaaa00000001")
	mine.Match.Taker = me

	published := matchEntry("bbbb00000002")
	published.Match.Maker = me
	published.Match.Taker = strings.Repeat("99", 32)

	done := matchEntry("cccc00000003")
	done.Match.Taker = me

	retired := matchEntry("dddd00000004")
	retired.Match.Taker = me

	got := pendingMatches(
		me,
		[]marketflow.MatchEntry{mine, published, done, retired},
		func(swapID string) bool { return swapID == "cccc00000003" },
		func(swapID string) bool { return swapID == "dddd00000004" },
	)

	if len(got) != 1 || got[0].Match.SwapID != "aaaa00000001" {
		t.Fatalf("pending = %v, want only the importable match", swapsOf(got))
	}

	// With no predicates the filter still keeps only taker matches.
	if got := pendingMatches(me, []marketflow.MatchEntry{mine, published}, nil, nil); len(got) != 1 {
		t.Fatalf("taker filter = %v", swapsOf(got))
	}
}

// swapsOf lists the swap ids of a match set, for failure messages.
func swapsOf(entries []marketflow.MatchEntry) []string {
	ids := make([]string, 0, len(entries))

	for _, entry := range entries {
		ids = append(ids, entry.Match.SwapID)
	}

	return ids
}
