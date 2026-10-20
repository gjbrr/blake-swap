package marketops

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/model"
)

// reverseTakeEvidence is the validated evidence a reverse acceptance runs on.
//
// It is what AcceptTake has already read and judged by the time it knows the direction, so the
// reverse path never reads the relay a second time — which is both a cost the browser would
// pay twice and, in the CLI, a second scan announcement that the reference does not print.
type reverseTakeEvidence struct {
	Offer *event.Event
	Terms *marketflow.ListedOffer
	Take  *event.Event
	// Selected is the take as the canonical validator judged it.
	Selected        *marketflow.TakeEntry
	OfferID, TakeID string
}

// acceptReverseTake accepts a take in the btc-for-blake direction.
//
// The difference from the forward direction is not cosmetic: here the offer's maker is
// protocol Bob, the side that funds BTC, and protocol Alice is the taker. Bob must never
// create Alice's secret — a swap where the payer of the BLAKE leg's counterparty also chose
// the hashlock would let Bob claim without Alice's cooperation — so this path reserves the
// public swap id, publishes the match, and creates no atomic-swap state at all. Alice will
// create the swap from the match on her side.
func acceptReverseTake(
	ctx context.Context, env *Env, evidence reverseTakeEvidence, hooks TakeHooks,
) (*TakeAcceptance, error) {
	terms := evidence.Terms
	direction := terms.Direction

	if direction != market.BTCForBlake {
		return nil, fmt.Errorf("Reverse-market helper called for wrong direction")
	}

	makerRole, takerRole, err := direction.ProtocolRoles()

	if err != nil {
		return nil, err
	}

	if makerRole != "bob" || takerRole != "alice" {
		return nil, fmt.Errorf("SECURITY FAILURE: reverse direction did not derive Bob/Alice roles")
	}

	maker := evidence.Offer.Pubkey
	taker := evidence.Take.Pubkey

	if env.Identity.Public != maker {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: current participant did not create this market offer",
		)
	}

	if evidence.Selected.Taker != taker {
		return nil, fmt.Errorf("SECURITY FAILURE: selected taker identity mismatch")
	}

	takeName := selectionPath(evidence.TakeID)
	offerName := selectionPath("offer-" + evidence.OfferID)

	existing, err := reverseSelection(ctx, env, takeName, offerName)

	if err != nil {
		return nil, err
	}

	acceptance := TakeAcceptance{
		OfferID: evidence.OfferID, TakeID: evidence.TakeID,
		Alice: taker, Bob: maker,
		Blake: terms.BlakeAmount, BTCSats: terms.BTCSats,
		Direction: direction, MakerRole: makerRole, TakerRole: takerRole,
		Reverse: true,
	}

	if existing != nil {
		return repeatReverseTake(existing, acceptance, hooks)
	}

	// A public coordination identifier only: no secret, no hashlock, no swap record.
	if env.Random == nil {
		return nil, fmt.Errorf("the reverse market accept needs a source of entropy")
	}

	swapID, err := model.NewSwapID(env.Random)

	if err != nil {
		return nil, err
	}

	acceptance.SwapID = swapID

	selection := map[string]any{
		"version":    2,
		"offer_id":   evidence.OfferID,
		"take_id":    evidence.TakeID,
		"swap_id":    swapID,
		"direction":  string(direction),
		"maker_role": makerRole,
		"taker_role": takerRole,

		"nostr_bob_pub":   maker,
		"nostr_alice_pub": taker,

		"blake_amount": json.Number(terms.BlakeAmount.Format()),
		"btc_sats":     terms.BTCSats,
	}

	// Both copies of the selection are written before either is announced, and both
	// must survive a crash: losing one would make the two agree on different terms.
	for _, name := range []string{takeName, offerName} {
		if err := env.writeDocument(ctx, name, selection); err != nil {
			return nil, err
		}
	}

	built, _, err := marketflow.BuildMatch(env.Identity, marketflow.BuildMatchOptions{
		Offer:       *terms,
		OfferEvent:  *evidence.Offer,
		TakeEvent:   *evidence.Take,
		Taker:       &evidence.Selected.Take,
		SwapID:      swapID,
		BlakeAmount: terms.BlakeAmount,
		BTCSats:     terms.BTCSats,
		CreatedAt:   env.now().Unix(),
	})

	if err != nil {
		return nil, err
	}

	if hooks.BeforeMatchPublish != nil {
		hooks.BeforeMatchPublish(acceptance)
	}

	report, err := env.Client.Publish(ctx, built)

	if err != nil {
		return nil, err
	}

	// Cached before the selection is rewritten: a match this machine published must stay
	// resolvable from its own cache after the relay window closes.
	if env.Client.Cache != nil {
		_ = env.Client.Cache.Put(ctx, built)
	}

	selection["market_match_id"] = built.ID

	for _, name := range []string{takeName, offerName} {
		if err := env.writeDocument(ctx, name, selection); err != nil {
			return nil, err
		}
	}

	acceptance.MatchID = built.ID

	if hooks.AfterMatchPublish != nil {
		hooks.AfterMatchPublish(acceptance, MatchPublication{
			OfferID: evidence.OfferID, TakeID: evidence.TakeID,
			MatchID: built.ID, SwapID: swapID,
			Published: built, Report: report,
		})
	}

	// The reverse block is the reference's last line for this command: it reports the match
	// that was just published, so it cannot be printed before there is one.
	if hooks.Accepted != nil {
		hooks.Accepted(acceptance)
	}

	return &acceptance, nil
}

// reverseSelection reads the two selection records of one reverse negotiation.
//
// Two records describe one accepted take, so both are read and required to agree. A record
// that exists is the durable fact that this negotiation already happened, and that is what
// makes a retry finish rather than reserve a second swap id.
func reverseSelection(
	ctx context.Context, env *Env, takeName, offerName string,
) (map[string]any, error) {
	var existing map[string]any

	for _, name := range []string{takeName, offerName} {
		candidate, err := env.readDocument(ctx, name)

		if err != nil {
			return nil, err
		}

		if candidate == nil {
			continue
		}

		if existing == nil {
			existing = candidate

			continue
		}

		if !sameSelection(candidate, existing) {
			return nil, fmt.Errorf("SECURITY FAILURE: reverse market selection records disagree")
		}
	}

	return existing, nil
}

// repeatReverseTake reports an acceptance that is already recorded.
//
// Unlike the forward direction there is nothing to repair and no swap to check: the reverse
// selection names a reserved swap id and no record was ever created for it, so the only
// question is whether the record still says what this command is about to do again.
func repeatReverseTake(
	existing map[string]any, acceptance TakeAcceptance, hooks TakeHooks,
) (*TakeAcceptance, error) {
	for _, field := range []struct{ key, want string }{
		{"offer_id", acceptance.OfferID},
		{"take_id", acceptance.TakeID},
		{"direction", string(acceptance.Direction)},
		{"maker_role", acceptance.MakerRole},
		{"taker_role", acceptance.TakerRole},
		{"nostr_bob_pub", acceptance.Bob},
		{"nostr_alice_pub", acceptance.Alice},
	} {
		if value, _ := existing[field.key].(string); value != field.want {
			return nil, fmt.Errorf(
				"SECURITY FAILURE: existing reverse market selection is inconsistent",
			)
		}
	}

	swapID, _ := existing["swap_id"].(string)

	if swapID == "" {
		return nil, fmt.Errorf("Existing reverse selection has no swap_id")
	}

	acceptance.SwapID = swapID
	acceptance.Repeated = true
	acceptance.MatchID, _ = existing["market_match_id"].(string)

	if hooks.Accepted != nil {
		hooks.Accepted(acceptance)
	}

	return &acceptance, nil
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
