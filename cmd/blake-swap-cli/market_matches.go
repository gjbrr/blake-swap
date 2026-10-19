package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// runMarketMatches lists the matches addressed to this participant.
//
// A match names the offer and the take it settles, and the protocol requires both to be
// re-validated rather than trusted — so this fetches the match events, then whatever
// offer and take they reference and it does not already hold, and lets MatchesFor decide
// what is real. A match whose dependencies cannot be fetched is not reported.
func runMarketMatches(ctx context.Context, engine *swap.Engine, args []string) error {
	flags := flag.NewFlagSet("market-matches", flag.ContinueOnError)

	limit := flags.Int("limit", 100, "how many matches to ask each relay for")

	if err := flags.Parse(args); err != nil {
		return err
	}

	key, err := engineKey(engine)

	if err != nil {
		return err
	}

	me := key.PublicKey()

	matches, err := fetchMatches(ctx, key, *limit)

	if err != nil {
		return err
	}

	// A match this participant dismissed is not a decision waiting for them any more.
	// Filtering here rather than in MatchesFor keeps the suppression local: it is this
	// machine's opinion, not a fact about the evidence.
	matches, err = withoutIgnoredMatches(matches)

	if err != nil {
		return err
	}

	fmt.Print(renderMarketMatches(me, matches))

	return nil
}

// withoutIgnoredMatches drops the matches this participant has dismissed.
func withoutIgnoredMatches(matches []marketflow.MatchEntry) ([]marketflow.MatchEntry, error) {
	ignored, err := ignoredMatchIDs()

	if err != nil {
		return nil, err
	}

	if len(ignored) == 0 {
		return matches, nil
	}

	kept := make([]marketflow.MatchEntry, 0, len(matches))

	for _, entry := range matches {
		if ignored[entry.Event.ID] {
			continue
		}

		kept = append(kept, entry)
	}

	return kept, nil
}

// missingReferences lists the offer and take ids that match events name but the evidence
// does not contain.
func missingReferences(evidence []event.Event) []string {
	have := make(map[string]bool, len(evidence))

	for _, e := range evidence {
		have[e.ID] = true
	}

	seen := map[string]bool{}
	var missing []string

	for _, e := range evidence {
		if !e.HasTag("t", marketflow.MatchTag) {
			continue
		}

		var message map[string]any

		if err := json.Unmarshal([]byte(e.Content), &message); err != nil {
			continue
		}

		for _, field := range []string{"offer_id", "take_id"} {
			id, _ := message[field].(string)

			if id == "" || have[id] || seen[id] {
				continue
			}

			seen[id] = true

			missing = append(missing, id)
		}
	}

	return missing
}

// renderMarketMatches is the reference's match listing.
func renderMarketMatches(me string, matches []marketflow.MatchEntry) string {
	var out strings.Builder

	out.WriteString("\n")
	fmt.Fprintf(&out, "MARKET MATCHES FOR TAKER %s\n", truncate(me, 12))
	out.WriteString(strings.Repeat("=", 40) + "\n")
	out.WriteString("\n")
	out.WriteString("Market role: taker. The maker/taker roles are market roles; ")
	out.WriteString("the protocol role each one binds depends on the direction.\n")
	out.WriteString("\n")

	if len(matches) == 0 {
		out.WriteString("No valid matches found.\n")

		return out.String()
	}

	for index, entry := range matches {
		match := entry.Match

		fmt.Fprintf(&out, "MATCH %d\n", index+1)
		out.WriteString(strings.Repeat("-", 40) + "\n")
		fmt.Fprintf(&out, "match id: %s\n", match.EventID)
		fmt.Fprintf(&out, "offer id: %s\n", match.OfferID)
		fmt.Fprintf(&out, "take id:  %s\n", match.TakeID)
		fmt.Fprintf(&out, "swap id:  %s\n", match.SwapID)
		fmt.Fprintf(&out, "maker:    %s\n", match.Maker)
		fmt.Fprintf(&out, "taker:    %s\n", match.Taker)
		fmt.Fprintf(&out, "direction: %s\n", match.Direction)
		fmt.Fprintf(&out, "maker is protocol: %s\n", match.MakerRole)
		fmt.Fprintf(&out, "taker is protocol: %s\n", match.TakerRole)
		fmt.Fprintf(&out, "selling:  %s BLAKE\n", match.BlakeAmount.Format())
		fmt.Fprintf(&out, "asking:   %s sats\n", thousands(match.BTCSats))
		out.WriteString("\n")
	}

	fmt.Fprintf(&out, "%d valid match(es)\n", len(matches))
	out.WriteString("\n")
	out.WriteString("PASS: every match signature verifies\n")
	out.WriteString("PASS: maker identity verified\n")
	out.WriteString("PASS: taker identity verified\n")
	out.WriteString("PASS: market terms preserved\n")

	return out.String()
}

// fetchMatches reads the matches addressed to one identity, resolving the offer and take
// each one names. It is shared by every command whose subject is a match.
func fetchMatches(
	ctx context.Context, key *nostr.Key, limit int,
) ([]marketflow.MatchEntry, error) {
	me := key.PublicKey()
	client := marketClient()

	report, err := client.FetchReport(ctx, nostr.Filter{
		Kinds:  []int{1},
		Topics: []string{marketflow.MatchTag},
		PTags:  []string{me},
		Limit:  limit,
	})

	if err != nil {
		return nil, err
	}

	evidence := append([]event.Event{}, report.Events...)
	evidence = append(evidence, cachedEvents(ctx, client.Cache)...)

	for _, found := range report.Events {
		if err := client.Cache.Put(ctx, found); err != nil {
			return nil, err
		}
	}

	// The dependencies a match names but this read did not return: a relay may hold a
	// match and not its offer, and the match is not evidence until both are present.
	missing := missingReferences(evidence)

	if len(missing) > 0 {
		dependencies, err := client.FetchMany(ctx, nostr.Filter{Kinds: []int{1}, Ids: missing})

		if err != nil {
			return nil, err
		}

		for _, found := range dependencies {
			if err := client.Cache.Put(ctx, found); err != nil {
				return nil, err
			}
		}

		evidence = append(evidence, dependencies...)
	}

	return marketflow.MatchesFor(me, evidence, time.Now()), nil
}
