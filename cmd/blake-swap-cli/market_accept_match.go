package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
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

	key, err := engineKey(engine)

	if err != nil {
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

	bob := key.PublicKey()

	found, err := fetchEventByID(ctx, client, matchID)

	if err != nil {
		return err
	}

	item, err := validateMarketMatchFor(ctx, client, *found, bob)

	if err != nil {
		return err
	}

	directory := filepath.Join(stateDirectory(), matchBindingDirectory)
	bindingPath := filepath.Join(directory, item.SwapID+".json")

	proposed := map[string]any{
		"version":  1,
		"offer_id": item.OfferID,
		"take_id":  item.TakeID,
		"swap_id":  item.SwapID,
		"maker":    item.Maker,
		"taker":    item.Taker,

		"blake_amount": json.Number(pythonFloat(blakeFloat(item.BlakeAmount))),
		"btc_sats":     item.BTCSats,

		// Informational evidence only: the tuple above is the negotiation.
		"first_match_id": matchID,
	}

	existing, err := readBinding(bindingPath)

	if err != nil {
		return err
	}

	if existing != nil {
		if err := matchBindingConflict(existing, proposed); err != nil {
			return err
		}
	}

	records := store.NewFile(stateDirectory())

	record, err := records.Load(ctx, item.SwapID)

	if err == nil {
		return repeatAcceptMarketMatch(ctx, repeatMatch{
			MatchID: matchID,
			Item:    item,
			Bob:     bob,
			Record:  record,
			Store:   records,
			Binding: existing,
			Path:    bindingPath,
			Write:   proposed,
		})
	}

	if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	// No local swap yet: the offer this match names has to be accepted as an ordinary
	// swap, with the market's terms pinned so a different offer cannot answer instead.
	blake, sats := item.BlakeAmount, item.BTCSats

	if _, err := engine.AcceptOffer(ctx, swap.AcceptRequest{
		SwapID:              item.SwapID,
		ExpectedAlicePub:    item.Maker,
		ExpectedBlakeAmount: &blake,
		ExpectedBTCSats:     &sats,
	}); err != nil {
		return err
	}

	accepted, err := records.Load(ctx, item.SwapID)

	if err != nil {
		return err
	}

	if err := pinBob(ctx, records, accepted, bob); err != nil {
		return err
	}

	// Persist the immutable market-to-swap binding only after Bob's swap state exists:
	// a binding that named no record would be a claim about state this machine does not
	// have.
	if existing == nil {
		if err := writeBinding(bindingPath, proposed); err != nil {
			return err
		}
	}

	fmt.Print(renderMatchImported(matchID, item.SwapID))

	return nil
}

// repeatMatch describes a match whose swap already exists locally.
type repeatMatch struct {
	MatchID string
	Item    *marketflow.ParsedMatch
	Bob     string
	Record  *model.Swap
	Store   store.Store
	Binding map[string]any
	Path    string
	Write   map[string]any
}

// repeatAcceptMarketMatch finishes an import that was already done, or repairs it.
//
// Bob's record must agree with the match on every identity and on the trade, because
// those are the facts the binding is supposed to preserve. A missing binding is the one
// gap that is repaired: a crash between the two writes leaves a valid swap and no
// receipt, and refusing that would make the swap unusable.
func repeatAcceptMarketMatch(ctx context.Context, req repeatMatch) error {
	record := req.Record
	item := req.Item

	if record.NostrAlicePub == nil || *record.NostrAlicePub != item.Maker {
		return fmt.Errorf("SECURITY FAILURE: existing Bob swap has different Alice identity")
	}

	if record.NostrBobPub == nil {
		if err := pinBob(ctx, req.Store, record, req.Bob); err != nil {
			return err
		}
	} else if *record.NostrBobPub != req.Bob {
		return fmt.Errorf("SECURITY FAILURE: existing Bob swap has different Bob identity")
	}

	if record.BlakeAmount() != item.BlakeAmount {
		return fmt.Errorf("SECURITY FAILURE: existing Bob swap has different BLAKE terms")
	}

	if record.BTCSats != item.BTCSats {
		return fmt.Errorf("SECURITY FAILURE: existing Bob swap has different BTC terms")
	}

	if req.Binding == nil {
		if err := writeBinding(req.Path, req.Write); err != nil {
			return err
		}
	}

	fmt.Print(renderMatchAlreadyImported(req.MatchID, item.SwapID))

	return nil
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

// matchBindingConflict refuses a binding that describes a different negotiation.
func matchBindingConflict(existing, proposed map[string]any) error {
	for _, key := range []string{
		"offer_id", "take_id", "swap_id", "maker", "taker", "blake_amount", "btc_sats",
	} {
		if _, ok := existing[key]; !ok {
			return fmt.Errorf("Existing market match binding is malformed")
		}
	}

	for _, key := range []string{"offer_id", "take_id", "swap_id", "maker", "taker"} {
		if fmt.Sprint(existing[key]) != fmt.Sprint(proposed[key]) {
			return fmt.Errorf(
				"SECURITY FAILURE: existing market match binding disagrees with validated match",
			)
		}
	}

	if asFloat(existing["blake_amount"]) != asFloat(proposed["blake_amount"]) {
		return fmt.Errorf(
			"SECURITY FAILURE: existing market match binding disagrees with validated match",
		)
	}

	if asFloat(existing["btc_sats"]) != asFloat(proposed["btc_sats"]) {
		return fmt.Errorf(
			"SECURITY FAILURE: existing market match binding disagrees with validated match",
		)
	}

	return nil
}

// asFloat reads a JSON number, which is how both amounts are stored.
func asFloat(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case json.Number:
		parsed, err := typed.Float64()

		if err != nil {
			return math.NaN()
		}

		return parsed
	case int64:
		return float64(typed)
	case int:
		return float64(typed)
	}

	return math.NaN()
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
