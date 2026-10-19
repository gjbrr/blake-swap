package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// ignoreDirectory holds the local tombstones for matches a participant has dismissed.
const ignoreDirectory = "ignored-market-matches"

// runIgnoreMatch records that a valid, unimported match should stop being offered.
//
// The tombstone is local on purpose. Nothing is deleted, no Nostr event is touched and
// no swap state is created: a match is somebody else's signed statement, and the only
// thing this participant can truthfully say is "not this one, not now". The marker
// outlives the process because that is the entire point of writing it.
func runIgnoreMatch(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("ignore-match takes one match id or prefix")
	}

	key, err := engineKey(engine)

	if err != nil {
		return err
	}

	requested := strings.ToLower(strings.TrimSpace(args[0]))

	if len(requested) < 8 {
		return fmt.Errorf("match ID prefix is too short; use at least 8 characters")
	}

	matches, err := fetchMatches(ctx, key, 200)

	if err != nil {
		return err
	}

	var candidates []marketflow.MatchEntry

	for _, entry := range matches {
		if strings.HasPrefix(entry.Event.ID, requested) {
			candidates = append(candidates, entry)
		}
	}

	if len(candidates) == 0 {
		return fmt.Errorf("No valid market match begins with '%s'", requested)
	}

	if len(candidates) > 1 {
		fmt.Print(renderAmbiguousMatches(candidates))

		return fmt.Errorf("Match prefix is ambiguous; use more characters")
	}

	entry := candidates[0]
	matchID := entry.Event.ID
	swapID := entry.Match.SwapID

	// A match that already has local swap state is being negotiated here; ignoring it
	// would hide a trade in progress.
	if swapID != "" && localSwapExists(swapID) {
		return fmt.Errorf("Refusing to ignore: this match already has local swap state")
	}

	dir := filepath.Join(stateDirectory(), ignoreDirectory)
	path := filepath.Join(dir, matchID+".json")

	if _, err := os.Stat(path); err == nil {
		fmt.Print(renderAlreadyIgnored(matchID))

		return nil
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	if err := writeTombstone(path, matchID, entry); err != nil {
		return err
	}

	fmt.Print(renderIgnored(matchID, swapID))

	return nil
}

// localSwapExists reports whether this participant already has state for a swap.
func localSwapExists(swapID string) bool {
	if swapID == "" {
		return false
	}

	_, err := os.Stat(filepath.Join(stateDirectory(), swapID+".json"))

	return err == nil
}

// writeTombstone writes the local record atomically.
//
// A half-written tombstone is worse than none: the next run would see the file, say
// "already ignored", and never retry — so the rename is what publishes it.
func writeTombstone(path, matchID string, entry marketflow.MatchEntry) error {
	record := map[string]any{
		"match_id":     matchID,
		"swap_id":      nilIfEmpty(entry.Match.SwapID),
		"offer_id":     entry.Match.OfferID,
		"take_id":      entry.Match.TakeID,
		"maker":        entry.Match.Maker,
		"taker":        entry.Match.Taker,
		"direction":    string(entry.Match.Direction),
		"blake_amount": json.RawMessage(pythonFloat(blakeFloat(entry.Match.BlakeAmount))),
		"btc_sats":     entry.Match.BTCSats,
		"ignored_at":   time.Now().Unix(),
		"local_only":   true,
	}

	encoded, err := json.MarshalIndent(record, "", "  ")

	if err != nil {
		return err
	}

	temporary := path + ".tmp"

	if err := os.WriteFile(temporary, append(encoded, '\n'), 0o600); err != nil {
		return err
	}

	return os.Rename(temporary, path)
}

// renderIgnored is the reference's confirmation for a new tombstone.
func renderIgnored(matchID, swapID string) string {
	swap := swapID

	if swap == "" {
		swap = "None"
	}

	return "\nMARKET MATCH IGNORED\n" +
		"====================\n" +
		"match: " + matchID + "\n" +
		"swap:  " + swap + "\n\n" +
		"PASS: local tombstone created\n" +
		"PASS: no Nostr event was deleted or modified\n" +
		"PASS: no swap state was created\n" +
		"PASS: no funds moved\n"
}

// renderAlreadyIgnored is the block for a tombstone that already exists.
func renderAlreadyIgnored(matchID string) string {
	return "\nMATCH ALREADY IGNORED\n" +
		"=====================\n" +
		"match: " + matchID + "\n"
}

// renderAmbiguousMatches lists the candidates a too-short prefix matched.
func renderAmbiguousMatches(candidates []marketflow.MatchEntry) string {
	var out strings.Builder

	out.WriteString("\nAMBIGUOUS MATCH PREFIX\n")
	out.WriteString("======================\n")

	for _, entry := range candidates {
		swap := entry.Match.SwapID

		if swap == "" {
			swap = "None"
		}

		fmt.Fprintf(&out, "%s swap=%s\n", entry.Event.ID, swap)
	}

	return out.String()
}
