package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// acceptReverseMarketTake accepts a take in the btc-for-blake direction.
//
// The difference from the forward direction is not cosmetic: here the offer's maker is
// protocol Bob, the side that funds BTC, and protocol Alice is the taker. Bob must never
// create Alice's secret — a swap where the payer of the BLAKE leg's counterparty also
// chose the hashlock would let Bob claim without Alice's cooperation — so this path
// reserves the public swap id, publishes the match, and creates no atomic-swap state at
// all. Alice will create the swap from the match on her side.
func acceptReverseMarketTake(
	ctx context.Context,
	engine *swap.Engine,
	key *nostr.Key,
	client *nostr.Client,
	input takeAcceptInput,
) error {
	direction := input.OfferTerms.Direction

	if direction != market.BTCForBlake {
		return fmt.Errorf("Reverse-market helper called for wrong direction")
	}

	makerRole, takerRole, err := direction.ProtocolRoles()

	if err != nil {
		return err
	}

	if makerRole != "bob" || takerRole != "alice" {
		return fmt.Errorf("SECURITY FAILURE: reverse direction did not derive Bob/Alice roles")
	}

	maker := input.Offer.Pubkey
	taker := input.TakeEvent.Pubkey

	if key.PublicKey() != maker {
		return fmt.Errorf("SECURITY FAILURE: current participant did not create this market offer")
	}

	take, err := marketflow.ParseTake(*input.TakeEvent, time.Now())

	if err != nil {
		return fmt.Errorf("Referenced event is not a market take")
	}

	if take.Taker != taker {
		return fmt.Errorf("SECURITY FAILURE: selected taker identity mismatch")
	}

	if err := os.MkdirAll(input.Directory, 0o700); err != nil {
		return err
	}

	takeSelectionPath := filepath.Join(input.Directory, input.TakeID+".json")
	offerSelectionPath := filepath.Join(input.Directory, "offer-"+input.OfferID+".json")

	var existing map[string]any

	for _, path := range []string{takeSelectionPath, offerSelectionPath} {
		candidate, err := readSelection(path)

		if err != nil {
			return err
		}

		if candidate == nil {
			continue
		}

		if existing == nil {
			existing = candidate

			continue
		}

		if !sameSelection(candidate, existing) {
			return fmt.Errorf("SECURITY FAILURE: reverse market selection records disagree")
		}
	}

	if existing != nil {
		if v, _ := existing["offer_id"].(string); v != input.OfferID {
			return fmt.Errorf("SECURITY FAILURE: existing reverse market selection is inconsistent")
		}

		if v, _ := existing["take_id"].(string); v != input.TakeID {
			return fmt.Errorf("SECURITY FAILURE: existing reverse market selection is inconsistent")
		}

		if v, _ := existing["direction"].(string); v != string(direction) {
			return fmt.Errorf("SECURITY FAILURE: existing reverse market selection is inconsistent")
		}

		if v, _ := existing["maker_role"].(string); v != "bob" {
			return fmt.Errorf("SECURITY FAILURE: existing reverse market selection is inconsistent")
		}

		if v, _ := existing["taker_role"].(string); v != "alice" {
			return fmt.Errorf("SECURITY FAILURE: existing reverse market selection is inconsistent")
		}

		if v, _ := existing["nostr_bob_pub"].(string); v != maker {
			return fmt.Errorf("SECURITY FAILURE: existing reverse market selection is inconsistent")
		}

		if v, _ := existing["nostr_alice_pub"].(string); v != taker {
			return fmt.Errorf("SECURITY FAILURE: existing reverse market selection is inconsistent")
		}

		swapID, _ := existing["swap_id"].(string)

		if swapID == "" {
			return fmt.Errorf("Existing reverse selection has no swap_id")
		}

		fmt.Print(renderReverseTakeAlreadyAccepted(input.OfferID, input.TakeID, swapID))

		return nil
	}

	// A public coordination identifier only: no secret, no hashlock, no swap record.
	swapID, err := model.NewSwapID(randReader())

	if err != nil {
		return err
	}

	selection := map[string]any{
		"version":    2,
		"offer_id":   input.OfferID,
		"take_id":    input.TakeID,
		"swap_id":    swapID,
		"direction":  string(direction),
		"maker_role": makerRole,
		"taker_role": takerRole,

		"nostr_bob_pub":   maker,
		"nostr_alice_pub": taker,

		"blake_amount": json.Number(pythonFloat(blakeFloat(input.OfferTerms.BlakeAmount))),
		"btc_sats":     input.OfferTerms.BTCSats,
	}

	// Both copies of the selection are written before either is announced, and both
	// must survive a crash: losing one would make the two agree on different terms.
	for _, path := range []string{takeSelectionPath, offerSelectionPath} {
		if err := writeSelection(path, selection); err != nil {
			return err
		}
	}

	built, _, err := marketflow.BuildMatch(key, marketflow.BuildMatchOptions{
		Offer:       *input.OfferTerms,
		OfferEvent:  *input.Offer,
		TakeEvent:   *input.TakeEvent,
		Taker:       take,
		SwapID:      swapID,
		BlakeAmount: input.OfferTerms.BlakeAmount,
		BTCSats:     input.OfferTerms.BTCSats,
		CreatedAt:   time.Now().Unix(),
	})

	if err != nil {
		return err
	}

	if _, err := client.Publish(ctx, built); err != nil {
		return err
	}

	selection["market_match_id"] = built.ID

	for _, path := range []string{takeSelectionPath, offerSelectionPath} {
		if err := writeSelection(path, selection); err != nil {
			return err
		}
	}

	fmt.Print(renderReverseTakeAccepted(
		input.OfferID, input.TakeID, built.ID, swapID, string(direction), makerRole, takerRole,
	))

	return nil
}

// sameSelection reports whether two decoded selection records agree on every field.
func sameSelection(left, right map[string]any) bool {
	if len(left) != len(right) {
		return false
	}

	for key, value := range left {
		other, ok := right[key]

		if !ok || fmt.Sprint(value) != fmt.Sprint(other) {
			return false
		}
	}

	return true
}

// renderReverseTakeAccepted is the reference's confirmation block for the reverse path.
func renderReverseTakeAccepted(
	offerID, takeID, matchID, swapID, direction, makerRole, takerRole string,
) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("REVERSE MARKET TAKE ACCEPTED\n")
	out.WriteString("============================\n")
	fmt.Fprintf(&out, "offer id: %s\n", offerID)
	fmt.Fprintf(&out, "take id:  %s\n", takeID)
	fmt.Fprintf(&out, "match id: %s\n", matchID)
	fmt.Fprintf(&out, "swap id:  %s\n", swapID)
	out.WriteString("\n")
	fmt.Fprintf(&out, "direction: %s\n", direction)
	fmt.Fprintf(&out, "maker is protocol: %s\n", makerRole)
	fmt.Fprintf(&out, "taker is protocol: %s\n", takerRole)
	out.WriteString("\n")
	out.WriteString("PASS: maker selected exact signed take\n")
	out.WriteString("PASS: public swap_id reserved\n")
	out.WriteString("PASS: Bob did NOT create secret S\n")
	out.WriteString("PASS: no atomic-swap state exists yet\n")
	out.WriteString("PASS: no funds moved\n")

	return out.String()
}

// renderReverseTakeAlreadyAccepted is the duplicate-reverse block.
func renderReverseTakeAlreadyAccepted(offerID, takeID, swapID string) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("REVERSE MARKET TAKE ALREADY ACCEPTED\n")
	out.WriteString("====================================\n")
	fmt.Fprintf(&out, "offer id: %s\n", offerID)
	fmt.Fprintf(&out, "take id:  %s\n", takeID)
	fmt.Fprintf(&out, "swap id:  %s\n", swapID)
	out.WriteString("\n")
	out.WriteString("PASS: no second swap ID was created\n")
	out.WriteString("PASS: no secret was created by Bob\n")

	return out.String()
}
