package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/marketops"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// ignoreDirectory holds the local tombstones for matches a participant has dismissed.
//
// The directory name is the reference's, and so is the shape of one tombstone: a participant
// who runs the Python engine one day and this one the next must find the matches they
// dismissed still dismissed.
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

	if _, err := engineKey(engine); err != nil {
		return err
	}

	client := marketClient()

	env, err := marketEnvOrError(engine, client, func(line string) { fmt.Print(line) })

	if err != nil {
		return err
	}

	ttl, err := ignoredMatchTTL()

	if err != nil {
		return err
	}

	ignored, err := marketops.IgnoreMatch(ctx, env, args[0], time.Duration(ttl)*time.Second)

	if ambiguous, ok := marketops.IsAmbiguous(err); ok {
		fmt.Print(renderAmbiguousMatches(ambiguous.Candidates))

		return err
	}

	if err != nil {
		return err
	}

	if ignored.AlreadyIgnored {
		fmt.Print(renderAlreadyIgnored(ignored.MatchID))

		return nil
	}

	fmt.Print(renderIgnored(ignored.MatchID, ignored.SwapID))

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

// ignoredMatchTTL is how long an ignored match stays suppressed.
//
// Zero means forever, which is the original behaviour. A positive value bounds it: an expired
// tombstone is both ignored and removed when the tombstones are next read, so a match that was
// dismissed by mistake comes back rather than disappearing for good.
//
// Override with BLAKE_SWAP_IGNORED_MATCH_TTL_SECONDS. The browser client has no process
// environment, so it always uses zero; its tombstones last until they are removed.
func ignoredMatchTTL() (int64, error) {
	raw := strings.TrimSpace(os.Getenv("BLAKE_SWAP_IGNORED_MATCH_TTL_SECONDS"))

	if raw == "" {
		return 0, nil
	}

	value, err := strconv.ParseInt(raw, 10, 64)

	if err != nil {
		return 0, fmt.Errorf("BLAKE_SWAP_IGNORED_MATCH_TTL_SECONDS must be an integer")
	}

	if value < 0 {
		return 0, fmt.Errorf("BLAKE_SWAP_IGNORED_MATCH_TTL_SECONDS must not be negative")
	}

	return value, nil
}

// ignoredMatchIDs is the set of match ids this participant has dismissed.
//
// The predicate and the expiry check are the shared ones: the browser filters its match list
// with the same code, so the two clients cannot disagree about which matches were dismissed.
func ignoredMatchIDs() (map[string]bool, error) {
	ttl, err := ignoredMatchTTL()

	if err != nil {
		return nil, err
	}

	env := &marketops.Env{Files: stateFiles{root: stateDirectory()}}

	return marketops.IgnoredMatchIDs(context.Background(), env, time.Duration(ttl)*time.Second)
}
