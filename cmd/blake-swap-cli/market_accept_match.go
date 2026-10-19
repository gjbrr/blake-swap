package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/marketops"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/store"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// matchBindingDirectory is where Bob durably binds a market negotiation to his swap.
const matchBindingDirectory = "market-match-bindings"

// runAcceptMarketMatch imports the swap a market match binds, as the taker.
//
// The match is the maker's receipt, and this is the taker's side of it: Bob validates the
// match against the exact offer and take it names, then binds that immutable negotiation
// tuple to the swap id before creating any local state. A later signed match may be new
// evidence for the same tuple but may not redefine it — offer, take, swap, identities and
// terms are what "the same negotiation" means, so a second match that disagrees with the
// first is refused rather than preferred.
func runAcceptMarketMatch(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("accept-market-match takes one match id")
	}

	if _, err := engineKey(engine); err != nil {
		return err
	}

	client := marketClient()

	matchID, err := resolveCachedPrefix(ctx, client, args[0], "market-match", "market match", true)

	if err != nil {
		return err
	}

	if len(matchID) != 64 {
		return fmt.Errorf("match_id must be the full 64-character Nostr event ID")
	}

	found, err := fetchEventByID(ctx, client, matchID)

	if err != nil {
		return err
	}

	env, err := marketEnvOrError(engine, client, func(line string) { fmt.Print(line) })

	if err != nil {
		return err
	}

	_, err = marketops.AcceptMatch(ctx, env, matchID, found, marketops.MatchHooks{
		AfterAccept: func(result *swap.AcceptResult) {
			fmt.Printf("accept message: %s\n", messageLocation(result.Location))
			fmt.Print(renderOfferAccepted(
				result.Swap.SwapID, recordPath(result.Swap.SwapID), result.Timeout,
			))
		},
		AfterImport: func(imported marketops.MatchImport) {
			if imported.Repeated {
				fmt.Print(renderMatchAlreadyImported(imported.MatchID, imported.SwapID))

				return
			}

			fmt.Print(renderMatchImported(imported.MatchID, imported.SwapID))
		},
	})

	return err
}

// pinBob records Bob's authenticated identity on his own swap.
func pinBob(ctx context.Context, records store.Store, record *model.Swap, bob string) error {
	if record.NostrBobPub != nil && *record.NostrBobPub == bob {
		return nil
	}

	record.NostrBobPub = &bob

	return records.Save(ctx, store.Bob, record)
}

// validateMarketMatchFor validates one match against the events it references.
//
// The two references are immutable, so they are satisfied from the verified cache when
// possible and only fetched when this machine has never seen them.
func validateMarketMatchFor(
	ctx context.Context, client *nostr.Client, candidate event.Event, taker string,
) (*marketflow.ParsedMatch, error) {
	if err := event.Verify(candidate, time.Now()); err != nil {
		return nil, err
	}

	var message map[string]any

	if err := json.Unmarshal([]byte(candidate.Content), &message); err != nil {
		return nil, fmt.Errorf("Market match contains invalid JSON")
	}

	offerID, _ := message["offer_id"].(string)

	if len(offerID) != 64 {
		return nil, fmt.Errorf("Invalid market offer ID")
	}

	takeID, _ := message["take_id"].(string)

	if len(takeID) != 64 {
		return nil, fmt.Errorf("Invalid market take ID")
	}

	offer, err := referencedEvent(ctx, client, offerID)

	if err != nil {
		return nil, err
	}

	take, err := referencedEvent(ctx, client, takeID)

	if err != nil {
		return nil, err
	}

	parsed, err := marketflow.ParseMatch(candidate, *offer, *take, time.Now())

	if err != nil {
		return nil, err
	}

	if parsed.Taker != taker {
		return nil, fmt.Errorf("Market match is for another taker")
	}

	return parsed, nil
}

// referencedEvent reads one immutable referenced event, cache first.
func referencedEvent(ctx context.Context, client *nostr.Client, id string) (*event.Event, error) {
	if client.Cache != nil {
		cached, ok, err := client.Cache.Get(ctx, id)

		if err == nil && ok {
			if verifyErr := event.Verify(cached, time.Now()); verifyErr == nil {
				return &cached, nil
			}
		}
	}

	return fetchEventByID(ctx, client, id)
}

// readBinding reads one durable match binding, or nil when there is none.
func readBinding(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)

	if os.IsNotExist(err) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	var binding map[string]any

	if err := json.Unmarshal(raw, &binding); err != nil {
		return nil, fmt.Errorf("Existing market match binding is unreadable")
	}

	return binding, nil
}

// writeBinding writes one durable match binding atomically.
func writeBinding(path string, binding map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	encoded, err := json.MarshalIndent(binding, "", "  ")

	if err != nil {
		return err
	}

	return writeAtomically(path, append(encoded, '\n'))
}

// renderMatchImported is the reference's confirmation for a fresh import.
func renderMatchImported(matchID, swapID string) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("MARKET MATCH IMPORTED\n")
	out.WriteString("=====================\n")
	fmt.Fprintf(&out, "match id: %s\n", matchID)
	fmt.Fprintf(&out, "swap id:  %s\n", swapID)
	out.WriteString("\n")
	out.WriteString("PASS: swap offer signer == matched Alice\n")
	out.WriteString("PASS: Bob identity == signed take\n")
	out.WriteString("PASS: market negotiation tuple durably bound to swap\n")
	out.WriteString("PASS: Bob still does NOT know secret S\n")
	out.WriteString("PASS: no funds moved\n")

	return out.String()
}

// renderMatchAlreadyImported is what a repeated import prints.
func renderMatchAlreadyImported(matchID, swapID string) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("MARKET MATCH ALREADY IMPORTED\n")
	out.WriteString("=============================\n")
	fmt.Fprintf(&out, "match id: %s\n", matchID)
	fmt.Fprintf(&out, "swap id:  %s\n", swapID)
	out.WriteString("\n")
	out.WriteString("PASS: duplicate command did not create new Bob keys\n")
	out.WriteString("PASS: immutable market negotiation binding preserved\n")
	out.WriteString("PASS: missing binding, if any, was repaired\n")

	return out.String()
}
