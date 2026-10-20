package marketops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/coord"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/store"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// ReverseOfferImport is what importing Alice's protocol offer produced.
type ReverseOfferImport struct {
	MatchID string
	SwapID  string
	OfferID string
	TakeID  string

	// Alice and Bob are the protocol roles: in this direction Alice is the taker and Bob is
	// the market's maker.
	Alice string
	Bob   string

	Blake   amount.Blake
	BTCSats int64

	Direction market.Direction

	// Location is where the accept message was sent, on the handshake paths.
	Location string
	// Recovered reports that this call finished a handshake whose Bob keys were already
	// durable and whose accept message had never been sent.
	Recovered bool
	// Repeated reports that the import was already complete and nothing was created.
	Repeated bool
}

// AcceptReverseOffer imports Alice's ordinary protocol offer after a reverse match.
//
// In this direction the current participant is the market maker, which is protocol Bob: he
// published the match, and what he still needs is the other half of the handshake — Alice's
// signed atomic-swap offer, with her contract keys and her deadlines. Importing it is what
// lets him fund, and it must be checked against the economics the match already
// authenticated rather than accepted on its own word.
//
// The interesting state is the middle one. Bob's settlement keys become durable before his
// accept message is sent, so a crash there leaves a record with keys and no counterparty
// identity. Resuming is safe only while nothing has been funded or claimed: completing a
// handshake after settlement activity would be a second history for money that has already
// moved, so that is refused rather than repaired.
func AcceptReverseOffer(
	ctx context.Context, env *Env, matchID string, supplied *event.Event, hooks MatchHooks,
) (*ReverseOfferImport, error) {
	if err := env.check(); err != nil {
		return nil, err
	}

	if len(matchID) != 64 {
		return nil, fmt.Errorf("match_id must be the full 64-character Nostr event ID")
	}

	bob := env.Identity.Public

	candidate := supplied

	if candidate == nil {
		fetched, err := FetchEvent(ctx, env, matchID)

		if err != nil {
			return nil, err
		}

		candidate = fetched
	}

	if err := event.Verify(*candidate, env.now()); err != nil {
		return nil, err
	}

	var message map[string]any

	if err := json.Unmarshal([]byte(candidate.Content), &message); err != nil {
		return nil, fmt.Errorf("Market match content is invalid JSON")
	}

	if kind, _ := message["type"].(string); kind != "market-match" {
		return nil, fmt.Errorf("Referenced event is not a market match")
	}

	// In the reverse direction the current participant must be the market *maker*: the
	// taker is Alice, and she is the one whose offer is being imported here.
	if candidate.Pubkey != bob {
		return nil, fmt.Errorf("SECURITY FAILURE: current participant did not sign this market match")
	}

	if maker, _ := message["maker"].(string); maker != bob {
		return nil, fmt.Errorf("SECURITY FAILURE: market match maker identity mismatch")
	}

	expectedTaker, ok := message["taker"].(string)

	if !ok || expectedTaker == "" {
		return nil, fmt.Errorf("Market match has no valid taker identity")
	}

	item, err := validateMatch(ctx, env, *candidate, expectedTaker)

	if err != nil {
		return nil, err
	}

	if item.Direction != market.BTCForBlake {
		return nil, fmt.Errorf("Market match is not btc-for-blake")
	}

	if item.MakerRole != "bob" {
		return nil, fmt.Errorf("SECURITY FAILURE: maker did not derive to protocol Bob")
	}

	if item.TakerRole != "alice" {
		return nil, fmt.Errorf("SECURITY FAILURE: taker did not derive to protocol Alice")
	}

	if item.Maker != bob {
		return nil, fmt.Errorf("SECURITY FAILURE: validated maker is not current Bob")
	}

	alice := item.Taker

	imported := &ReverseOfferImport{
		MatchID: matchID, SwapID: item.SwapID, OfferID: item.OfferID, TakeID: item.TakeID,
		Alice: alice, Bob: bob,
		Blake: item.BlakeAmount, BTCSats: item.BTCSats,
		Direction: item.Direction,
	}

	record, err := env.Records.Load(ctx, item.SwapID)

	if err == nil {
		return repeatReverseOffer(ctx, env, record, item, imported, hooks)
	}

	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	blake, sats := item.BlakeAmount, item.BTCSats

	if hooks.BeforeAccept != nil {
		hooks.BeforeAccept()
	}

	accepted, err := env.Engine.AcceptOffer(ctx, swap.AcceptRequest{
		SwapID:              item.SwapID,
		ExpectedAlicePub:    alice,
		ExpectedBlakeAmount: &blake,
		ExpectedBTCSats:     &sats,
	})

	if err != nil {
		return nil, err
	}

	if hooks.AfterAccept != nil {
		hooks.AfterAccept(accepted)
	}

	pinned, err := env.Records.Load(ctx, item.SwapID)

	if err != nil {
		return nil, err
	}

	if pinned.SecretHex != nil {
		return nil, fmt.Errorf("SECURITY FAILURE: Bob learned Alice's original secret")
	}

	if pinned.NostrAlicePub == nil || *pinned.NostrAlicePub != alice {
		return nil, fmt.Errorf("SECURITY FAILURE: protocol offer Alice identity mismatch")
	}

	// Bob's Nostr identity was already proven by the market match, so it is persisted here
	// rather than learned again from the message he just consumed.
	if pinned.NostrBobPub == nil || *pinned.NostrBobPub != bob {
		if err := pinBob(ctx, env.Records, pinned, bob); err != nil {
			return nil, err
		}
	}

	if hooks.AfterReverseOffer != nil {
		hooks.AfterReverseOffer(*imported)
	}

	return imported, nil
}

// repeatReverseOffer handles a record that already exists for this negotiation.
func repeatReverseOffer(
	ctx context.Context,
	env *Env,
	record *model.Swap,
	item *marketflow.ParsedMatch,
	imported *ReverseOfferImport,
	hooks MatchHooks,
) (*ReverseOfferImport, error) {
	if record.NostrAlicePub == nil || *record.NostrAlicePub != item.Taker {
		return nil, fmt.Errorf("SECURITY FAILURE: existing Bob state has different Alice identity")
	}

	if record.NostrBobPub != nil && *record.NostrBobPub != imported.Bob {
		return nil, fmt.Errorf("SECURITY FAILURE: existing Bob state has different Bob identity")
	}

	if record.NostrBobPub == nil {
		return resumeReverseHandshake(ctx, env, record, imported, hooks)
	}

	if record.BlakeAmount() != item.BlakeAmount {
		return nil, fmt.Errorf("SECURITY FAILURE: existing Bob state has different BLAKE terms")
	}

	if record.BTCSats != item.BTCSats {
		return nil, fmt.Errorf("SECURITY FAILURE: existing Bob state has different BTC terms")
	}

	imported.Repeated = true

	if hooks.AfterReverseOffer != nil {
		hooks.AfterReverseOffer(*imported)
	}

	return imported, nil
}

// resumeReverseHandshake finishes a handshake whose accept message was never sent.
//
// The keys are already durable, so they are reused: generating a second set would leave Alice
// holding an offer for keys this machine no longer intends to use, which is a second history
// rather than a retry.
func resumeReverseHandshake(
	ctx context.Context,
	env *Env,
	record *model.Swap,
	imported *ReverseOfferImport,
	hooks MatchHooks,
) (*ReverseOfferImport, error) {
	if err := reverseResumable(record); err != nil {
		return nil, err
	}

	record.NostrBobPub = &imported.Bob

	if err := env.Records.Save(ctx, store.Bob, record); err != nil {
		return nil, err
	}

	transport, mode := env.Engine.Coordination()

	if transport == nil {
		return nil, fmt.Errorf("the engine has no coordination transport")
	}

	// The key check is repeated here rather than trusted from reverseResumable: this is the
	// dereference the message is built from.
	if record.BlakeBobPub == nil || record.BTCBobPub == nil {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: partial reverse Bob state does not contain Bob settlement keys",
		)
	}

	location, err := transport.Send(ctx, record.SwapID, "accept", map[string]any{
		"type":          "accept",
		"swap_id":       record.SwapID,
		"secret_hash":   record.SecretHash,
		"blake_bob_pub": *record.BlakeBobPub,
		"btc_bob_pub":   *record.BTCBobPub,
	}, coord.SendOptions{
		LocalRole:    store.Bob,
		ExpectedRole: store.Bob,
		PeerPubkey:   imported.Alice,
		Mode:         mode,
	})

	if err != nil {
		return nil, err
	}

	imported.Recovered = true
	imported.Location = location

	if hooks.AfterReverseOffer != nil {
		hooks.AfterReverseOffer(*imported)
	}

	return imported, nil
}

// reverseResumable reports whether an unfunded record may finish its handshake.
func reverseResumable(record *model.Swap) error {
	if string(record.State) != "OFFERED" {
		return fmt.Errorf("SECURITY FAILURE: partial reverse Bob state is not OFFERED")
	}

	if record.SecretHex != nil {
		return fmt.Errorf("SECURITY FAILURE: protocol Bob unexpectedly possesses secret S")
	}

	for _, value := range []*string{
		record.BlakeFundingTxID,
		record.BTCFundingTxID,
		record.BTCCLaimTxID,
		record.BlakeClaimTxID,
		record.BTCRefundTxID,
		record.BlakeRefundTxID,
	} {
		if value != nil && *value != "" {
			return fmt.Errorf(
				"SECURITY FAILURE: refusing partial-handshake recovery after settlement activity",
			)
		}
	}

	if record.BlakeBobPub == nil || record.BTCBobPub == nil {
		return fmt.Errorf(
			"SECURITY FAILURE: partial reverse Bob state does not contain Bob settlement keys",
		)
	}

	return nil
}
