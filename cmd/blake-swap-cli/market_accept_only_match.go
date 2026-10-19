package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/coord"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/store"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// runAcceptMatch imports the only pending valid match for this participant.
//
// Discovery is read-only: it asks the relays (and the verified cache) for matches
// addressed to this identity, resolves the offer and take each one names, and validates
// every claim before anything is imported. What it will not do is choose: with more than
// one pending match it lists them and stops, because which counterparty to trade with is
// the participant's decision and not a client's.
func runAcceptMatch(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("accept-match takes at most one match id or prefix")
	}

	key, err := engineKey(engine)

	if err != nil {
		return err
	}

	me := key.PublicKey()

	client := marketClient()
	now := time.Now()

	report, err := client.FetchReport(ctx, marketflow.MarketFilters(200)...)

	if err != nil {
		return err
	}

	events := append([]event.Event{}, report.Events...)
	events = append(events, cachedEvents(ctx, client.Cache)...)
	events = completeMatchEvidence(ctx, client, events, me)

	records := store.NewFile(stateDirectory())

	candidates := pendingMatches(
		me,
		marketflow.MatchesFor(me, events, now),
		localSwapExists,
		func(swapID string) bool {
			retired, err := records.Retired(ctx, swapID)

			return err == nil && retired
		},
	)

	if requested := strings.ToLower(strings.TrimSpace(firstArg(args))); requested != "" {
		if len(requested) < 8 {
			return fmt.Errorf("match ID prefix is too short; use at least 8 characters")
		}

		filtered := make([]marketflow.MatchEntry, 0, len(candidates))

		for _, entry := range candidates {
			if strings.HasPrefix(entry.Event.ID, requested) {
				filtered = append(filtered, entry)
			}
		}

		if len(filtered) == 0 {
			// The match may simply not be pending any more because this participant
			// already imported it. A retry after a crash runs the same command again,
			// and "no pending match" would be a false and alarming report about a swap
			// that exists and is fine.
			swapID, matchID, ok := locallyBoundMatch(requested)

			if ok {
				fmt.Print(renderMatchAlreadyBound(matchID, swapID))

				return nil
			}

			return fmt.Errorf("No pending valid match begins with '%s'", requested)
		}

		if len(filtered) > 1 {
			fmt.Print(renderAmbiguousMatches(filtered))

			return fmt.Errorf("Match prefix is ambiguous; use more characters")
		}

		candidates = filtered
	}

	if len(candidates) == 0 {
		fmt.Print(renderNoPendingMatch())

		return nil
	}

	if len(candidates) > 1 {
		fmt.Print(renderMultiplePendingMatches(candidates))

		return nil
	}

	item := candidates[0]

	fmt.Print(renderOnePendingMatch(item))

	switch item.Match.Direction {
	case market.BlakeForBTC:
		fmt.Print("Importing match as protocol Bob.\n")

		return runAcceptMarketMatch(ctx, engine, []string{item.Event.ID})

	case market.BTCForBlake:
		fmt.Print("Importing reverse-direction match as protocol Alice.\n")

		if err := runAcceptReverseMarketMatch(ctx, engine, []string{item.Event.ID}); err != nil {
			return err
		}

		// Importing a reverse match is not finished until Alice's own offer exists: the
		// other side needs it to fund. This moves no funds, which is why it is safe to
		// do here rather than leaving it to a separate command.
		if offerAvailable(ctx, engine, item.Match.SwapID, me) {
			return nil
		}

		fmt.Print("\nPublishing reverse protocol offer...\n")

		return runPublishOffer(ctx, engine, []string{item.Match.SwapID})
	}

	return fmt.Errorf("Unsupported market direction: '%s'", item.Match.Direction)
}

// pendingMatches keeps the matches this participant could still import.
//
// A match this participant *made* is a receipt they published; accepting it is not a
// thing, so only matches where they are the taker are candidates. A match whose swap
// already exists locally is done, and one whose swap was retired is history — importing
// either would create a second swap for a negotiation that is already settled.
func pendingMatches(
	participant string,
	entries []marketflow.MatchEntry,
	hasLocalSwap func(string) bool,
	isRetired func(string) bool,
) []marketflow.MatchEntry {
	candidates := make([]marketflow.MatchEntry, 0, len(entries))

	for _, entry := range entries {
		if entry.Match.Taker != participant {
			continue
		}

		if hasLocalSwap != nil && hasLocalSwap(entry.Match.SwapID) {
			continue
		}

		if isRetired != nil && isRetired(entry.Match.SwapID) {
			continue
		}

		candidates = append(candidates, entry)
	}

	return candidates
}

// completeMatchEvidence fetches the offer and take a candidate match refers to.
//
// A match is evidence only with both events present, so a participant who saw the match
// but never saw the take would be told there is nothing pending while a valid match waits
// for them. The fetch is by id and the events are immutable, so this resolves history
// rather than discovering anything new.
func completeMatchEvidence(
	ctx context.Context, client *nostr.Client, events []event.Event, participant string,
) []event.Event {
	byID := make(map[string]bool, len(events))

	for _, e := range events {
		if e.ID != "" {
			byID[e.ID] = true
		}
	}

	var missing []string

	seen := map[string]bool{}

	for _, candidate := range events {
		if !candidate.HasTag("t", marketflow.MatchTag) {
			continue
		}

		var message map[string]any

		if err := json.Unmarshal([]byte(candidate.Content), &message); err != nil {
			continue
		}

		if taker, _ := message["taker"].(string); taker != participant {
			continue
		}

		for _, key := range []string{"offer_id", "take_id"} {
			id, _ := message[key].(string)

			if id == "" || byID[id] || seen[id] {
				continue
			}

			seen[id] = true

			missing = append(missing, id)
		}
	}

	for _, id := range missing {
		found, err := fetchEventByID(ctx, client, id)

		if err != nil {
			continue
		}

		byID[found.ID] = true

		events = append(events, *found)
	}

	return events
}

// locallyBoundMatch finds a match this machine already imported, by id prefix.
func locallyBoundMatch(prefix string) (swapID, matchID string, ok bool) {
	directory := filepath.Join(stateDirectory(), matchBindingDirectory)

	entries, err := os.ReadDir(directory)

	if err != nil {
		return "", "", false
	}

	names := make([]string, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		names = append(names, entry.Name())
	}

	sort.Strings(names)

	for _, name := range names {
		stem := strings.TrimSuffix(name, ".json")

		if !model.IsSwapID(stem) {
			continue
		}

		binding, err := readBinding(filepath.Join(directory, name))

		if err != nil || binding == nil {
			continue
		}

		// The two directions record different keys: the reverse binding carries
		// `match_id` and the forward one `first_match_id`. Both answer the same
		// question, so both are read rather than duplicating the lookup per direction.
		for _, key := range []string{"match_id", "first_match_id"} {
			value, _ := binding[key].(string)

			if value != "" && strings.HasPrefix(value, prefix) {
				return stem, value, true
			}
		}
	}

	return "", "", false
}

// offerAvailable reports whether this participant's own protocol offer is readable.
//
// The read is non-destructive on both routes, so this asks "did I already publish it"
// without consuming the message the counterparty has yet to read.
func offerAvailable(ctx context.Context, engine *swap.Engine, swapID, peer string) bool {
	if engine.Transport == nil {
		return false
	}

	_, err := engine.Transport.Receive(ctx, swapID, "offer", coord.ReceiveOptions{
		ExpectedPubkey: peer,
		Mode:           engine.Mode,
	})

	return err == nil
}

// firstArg returns the first argument, or the empty string.
func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}

	return args[0]
}

// renderNoPendingMatch is what an empty inbox says.
func renderNoPendingMatch() string {
	return "\nNO PENDING MATCH\n" +
		"================\n" +
		"No unimported valid market match for this participant was found.\n"
}

// renderMultiplePendingMatches lists them and, correctly, declines to choose.
func renderMultiplePendingMatches(candidates []marketflow.MatchEntry) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("MULTIPLE PENDING MATCHES\n")
	out.WriteString("========================\n")
	out.WriteString("More than one valid unimported match belongs to this participant.\n")
	out.WriteString("Refusing to choose automatically.\n")
	out.WriteString("\n")

	for index, entry := range candidates {
		fmt.Fprintf(&out, "%d. match=%s  swap=%s\n",
			index+1, truncate(entry.Event.ID, 12), entry.Match.SwapID,
		)
	}

	out.WriteString("\n")
	out.WriteString("Use `accept-match <match-prefix>` for the one you want.\n")

	return out.String()
}

// renderOnePendingMatch announces the single match there is nothing to decide about.
func renderOnePendingMatch(entry marketflow.MatchEntry) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("ONE PENDING MATCH FOUND\n")
	out.WriteString("=======================\n")
	fmt.Fprintf(&out, "match: %s\n", entry.Event.ID)
	fmt.Fprintf(&out, "swap:  %s\n", entry.Match.SwapID)
	fmt.Fprintf(&out, "trade: %s BLAKE / %s sats\n",
		pythonFloat(blakeFloat(entry.Match.BlakeAmount)), thousands(entry.Match.BTCSats),
	)
	out.WriteString("\n")

	return out.String()
}

// renderMatchAlreadyBound is the block a retried accept-match prints.
func renderMatchAlreadyBound(matchID, swapID string) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("MARKET MATCH ALREADY IMPORTED\n")
	out.WriteString("=============================\n")
	fmt.Fprintf(&out, "match id: %s\n", matchID)
	fmt.Fprintf(&out, "swap id:  %s\n", swapID)
	out.WriteString("\n")
	out.WriteString("PASS: duplicate command did not create a new swap\n")
	out.WriteString("PASS: durable market binding is intact\n")
	out.WriteString("PASS: no funds moved\n")

	return out.String()
}
