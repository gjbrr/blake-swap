package main

import (
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
)

// TestRenderOneValidTakeMatchesTheReference pins the announcement before the only take is
// accepted. The two-space `take:` alignment is the reference's.
func TestRenderOneValidTakeMatchesTheReference(t *testing.T) {
	offerID := strings.Repeat("ab", 32)
	takeID := strings.Repeat("cd", 32)

	got := renderOneValidTake(offerID, takeID)

	want := "\nONE VALID TAKE FOUND\n" +
		"====================\n" +
		"offer: " + offerID + "\n" +
		"take:  " + takeID + "\n" +
		"\n" +
		"Accepting the only valid take.\n"

	if got != want {
		t.Fatalf("renderOneValidTake mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// TestRenderMultipleValidTakesRefusesToChoose: with a choice to make, the command must
// list every candidate and say which command does the choosing.
func TestRenderMultipleValidTakesRefusesToChoose(t *testing.T) {
	offerID := strings.Repeat("ab", 32)
	first := strings.Repeat("11", 32)
	second := strings.Repeat("22", 32)

	got := renderMultipleValidTakes(offerID, []marketflow.TakeEntry{
		takeEntry(first, strings.Repeat("aa", 32)),
		takeEntry(second, strings.Repeat("bb", 32)),
	})

	want := "\nMULTIPLE VALID TAKES\n" +
		"====================\n" +
		"offer: " + offerID + "\n" +
		"\n" +
		"More than one counterparty wants this offer. Refusing to choose automatically.\n" +
		"\n" +
		"1. " + first[:12] + "  taker=" + strings.Repeat("aa", 6) + "\n" +
		"2. " + second[:12] + "  taker=" + strings.Repeat("bb", 6) + "\n" +
		"\n" +
		"Use accept-market-take with the chosen take ID/prefix.\n"

	if got != want {
		t.Fatalf("renderMultipleValidTakes mismatch:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

// takeEntry is one valid take with just the fields the renderers read.
func takeEntry(eventID, taker string) marketflow.TakeEntry {
	return marketflow.TakeEntry{
		Event:   event.Event{ID: eventID, Pubkey: taker},
		Take:    market.Take{Taker: taker},
		EventID: eventID,
		Taker:   taker,
	}
}
