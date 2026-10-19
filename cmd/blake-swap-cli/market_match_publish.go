package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/store"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// selectionsDirectory holds the local record that binds an accepted take to a swap.
const selectionsDirectory = "market-selections"

// matchPublication is what a published match bound together.
type matchPublication struct {
	OfferID string
	TakeID  string
	MatchID string
	SwapID  string
}

// runPublishMarketMatch publishes the maker's match for an accepted take and prints it.
func runPublishMarketMatch(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("publish-market-match takes one take id")
	}

	published, err := publishMarketMatch(ctx, engine, args[0])

	if err != nil {
		return err
	}

	fmt.Print(renderMatchPublished(
		published.OfferID, published.TakeID, published.MatchID, published.SwapID,
	))

	return nil
}

// publishMarketMatch signs and publishes the maker's match for an accepted take.
//
// The match is the terminal market event: it binds an offer, a take and a local swap,
// and it adds no new terms — everything in it is re-derived from the two events it names
// and checked against the swap. The local selection record is what makes that possible,
// and it is rewritten with the match id afterwards, because a crash that lost the link
// would leave a published match unresolvable from this machine.
func publishMarketMatch(
	ctx context.Context, engine *swap.Engine, takeID string,
) (matchPublication, error) {
	if len(takeID) != 64 {
		return matchPublication{}, fmt.Errorf(
			"take_id must be the full 64-character Nostr event ID",
		)
	}

	key, err := engineKey(engine)

	if err != nil {
		return matchPublication{}, err
	}

	selectionPath := filepath.Join(stateDirectory(), selectionsDirectory, takeID+".json")

	raw, err := os.ReadFile(selectionPath)

	if os.IsNotExist(err) {
		return matchPublication{}, fmt.Errorf("Alice has not accepted this market take")
	}

	if err != nil {
		return matchPublication{}, err
	}

	var selection map[string]any

	if err := json.Unmarshal(raw, &selection); err != nil {
		return matchPublication{}, err
	}

	swapID, _ := selection["swap_id"].(string)

	if swapID == "" {
		return matchPublication{}, fmt.Errorf("Market selection has no swap_id")
	}

	offerID, _ := selection["offer_id"].(string)

	records := store.NewFile(stateDirectory())

	record, err := records.Load(ctx, swapID)

	if err != nil {
		return matchPublication{}, err
	}

	client := marketClient()

	takeEvent, err := fetchEventByID(ctx, client, takeID)

	if err != nil {
		return matchPublication{}, err
	}

	take, err := marketflow.ParseTake(*takeEvent, time.Now())

	if err != nil {
		return matchPublication{}, fmt.Errorf("Referenced event is not a market take")
	}

	// The take must name the offer the selection recorded: a selection that has been
	// edited, or a take that has been swapped underneath it, is not the negotiation that
	// was accepted.
	if take.OfferID != offerID {
		return matchPublication{}, fmt.Errorf("SECURITY FAILURE: take references a different market offer")
	}

	offerEvent, err := fetchEventByID(ctx, client, offerID)

	if err != nil {
		return matchPublication{}, err
	}

	listed, err := marketflow.ParseOffer(*offerEvent, time.Now())

	if err != nil {
		return matchPublication{}, fmt.Errorf("Referenced event is not a market offer")
	}

	if err := event.Verify(*offerEvent, time.Now()); err != nil {
		return matchPublication{}, err
	}

	if err := event.Verify(*takeEvent, time.Now()); err != nil {
		return matchPublication{}, err
	}

	// A durable selection may allow recovery of an old negotiation, but it must not
	// create a newly signed post-expiry match: counterparties reject those.
	if listed.ExpiresAt <= time.Now().Unix() {
		return matchPublication{}, fmt.Errorf("Market offer expired before match publication; publish a fresh offer instead")
	}

	alice, bob := key.PublicKey(), takeEvent.Pubkey

	if offerEvent.Pubkey != alice {
		return matchPublication{}, fmt.Errorf("SECURITY FAILURE: market offer was not signed by current Alice")
	}

	if value, _ := selection["nostr_alice_pub"].(string); value != alice {
		return matchPublication{}, fmt.Errorf("SECURITY FAILURE: stored Alice identity mismatch")
	}

	if value, _ := selection["nostr_bob_pub"].(string); value != bob {
		return matchPublication{}, fmt.Errorf("SECURITY FAILURE: stored Bob identity mismatch")
	}

	if record.NostrAlicePub == nil || *record.NostrAlicePub != alice {
		return matchPublication{}, fmt.Errorf("SECURITY FAILURE: swap Alice identity mismatch")
	}

	if record.NostrBobPub == nil || *record.NostrBobPub != bob {
		return matchPublication{}, fmt.Errorf("SECURITY FAILURE: swap Bob identity mismatch")
	}

	if listed.BlakeAmount != record.BlakeAmount() {
		return matchPublication{}, fmt.Errorf("SECURITY FAILURE: BLAKE terms changed")
	}

	if listed.BTCSats != record.BTCSats {
		return matchPublication{}, fmt.Errorf("SECURITY FAILURE: BTC terms changed")
	}

	built, _, err := marketflow.BuildMatch(key, marketflow.BuildMatchOptions{
		Offer:       *listed,
		OfferEvent:  *offerEvent,
		TakeEvent:   *takeEvent,
		Taker:       take,
		SwapID:      record.SwapID,
		BlakeAmount: listed.BlakeAmount,
		BTCSats:     listed.BTCSats,
		CreatedAt:   time.Now().Unix(),
	})

	if err != nil {
		return matchPublication{}, err
	}

	if _, err := client.Publish(ctx, built); err != nil {
		return matchPublication{}, err
	}

	selection["market_match_id"] = built.ID

	encoded, err := json.MarshalIndent(selection, "", "  ")

	if err != nil {
		return matchPublication{}, err
	}

	if err := writeAtomically(selectionPath, append(encoded, '\n')); err != nil {
		return matchPublication{}, err
	}

	return matchPublication{
		OfferID: offerID,
		TakeID:  takeID,
		MatchID: built.ID,
		SwapID:  record.SwapID,
	}, nil
}

// renderMatchPublished is the reference's confirmation block.
func renderMatchPublished(offerID, takeID, matchID, swapID string) string {
	return "\nMARKET MATCH PUBLISHED\n" +
		"======================\n" +
		"offer id: " + offerID + "\n" +
		"take id:  " + takeID + "\n" +
		"match id: " + matchID + "\n" +
		"swap id:  " + swapID + "\n\n" +
		"PASS: match is signed by the offer's maker\n" +
		"PASS: the exact signed take is referenced\n" +
		"PASS: no secret was published\n" +
		"PASS: no funds moved\n"
}

// writeAtomically publishes a file by rename, so a crash cannot leave it half written.
func writeAtomically(path string, content []byte) error {
	temporary := path + ".tmp"

	if err := os.WriteFile(temporary, content, 0o600); err != nil {
		return err
	}

	return os.Rename(temporary, path)
}
