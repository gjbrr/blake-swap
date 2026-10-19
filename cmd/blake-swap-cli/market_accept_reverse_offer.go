package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/coord"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/store"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// runAcceptReverseSwapOffer imports Alice's ordinary protocol offer after a reverse match.
//
// In this direction the current participant is the market maker, which is protocol Bob:
// he published the match, and what he still needs is the other half of the handshake —
// Alice's signed atomic-swap offer, with her contract keys and her deadlines. Importing
// it is what lets him fund, and it must be checked against the economics the match already
// authenticated rather than accepted on its own word.
func runAcceptReverseSwapOffer(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("accept-reverse-swap-offer takes one match id")
	}

	matchID := args[0]

	if len(matchID) != 64 {
		return fmt.Errorf("match_id must be the full 64-character Nostr event ID")
	}

	key, err := engineKey(engine)

	if err != nil {
		return err
	}

	bob := key.PublicKey()

	client := marketClient()

	matchEvent, err := fetchEventByID(ctx, client, matchID)

	if err != nil {
		return err
	}

	if err := event.Verify(*matchEvent, time.Now()); err != nil {
		return err
	}

	var message map[string]any

	if err := json.Unmarshal([]byte(matchEvent.Content), &message); err != nil {
		return fmt.Errorf("Market match content is invalid JSON")
	}

	if kind, _ := message["type"].(string); kind != "market-match" {
		return fmt.Errorf("Referenced event is not a market match")
	}

	// In the reverse direction the current participant must be the market *maker*: the
	// taker is Alice, and she is the one whose offer is being imported here.
	if matchEvent.Pubkey != bob {
		return fmt.Errorf("SECURITY FAILURE: current participant did not sign this market match")
	}

	if maker, _ := message["maker"].(string); maker != bob {
		return fmt.Errorf("SECURITY FAILURE: market match maker identity mismatch")
	}

	expectedTaker, ok := message["taker"].(string)

	if !ok || expectedTaker == "" {
		return fmt.Errorf("Market match has no valid taker identity")
	}

	item, err := validateMarketMatchFor(ctx, client, *matchEvent, expectedTaker)

	if err != nil {
		return err
	}

	if item.Direction != market.BTCForBlake {
		return fmt.Errorf("Market match is not btc-for-blake")
	}

	if item.MakerRole != "bob" {
		return fmt.Errorf("SECURITY FAILURE: maker did not derive to protocol Bob")
	}

	if item.TakerRole != "alice" {
		return fmt.Errorf("SECURITY FAILURE: taker did not derive to protocol Alice")
	}

	if item.Maker != bob {
		return fmt.Errorf("SECURITY FAILURE: validated maker is not current Bob")
	}

	alice := item.Taker

	records := store.NewFile(stateDirectory())

	existing, err := records.Load(ctx, item.SwapID)

	if err == nil {
		return repeatAcceptReverseSwapOffer(
			ctx, engine, existing, item, matchID, alice, bob, records,
		)
	}

	if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	blake, sats := item.BlakeAmount, item.BTCSats

	if _, err := engine.AcceptOffer(ctx, swap.AcceptRequest{
		SwapID:              item.SwapID,
		ExpectedAlicePub:    alice,
		ExpectedBlakeAmount: &blake,
		ExpectedBTCSats:     &sats,
	}); err != nil {
		return err
	}

	imported, err := records.Load(ctx, item.SwapID)

	if err != nil {
		return err
	}

	if imported.SecretHex != nil {
		return fmt.Errorf("SECURITY FAILURE: Bob learned Alice's original secret")
	}

	if imported.NostrAlicePub == nil || *imported.NostrAlicePub != alice {
		return fmt.Errorf("SECURITY FAILURE: protocol offer Alice identity mismatch")
	}

	// Bob's Nostr identity was already proven by the market match, so it is persisted
	// here rather than learned again from the message he just consumed.
	if imported.NostrBobPub == nil || *imported.NostrBobPub != bob {
		if err := pinBob(ctx, records, imported, bob); err != nil {
			return err
		}
	}

	fmt.Print(renderReverseSwapOfferImported(matchID, imported))

	return nil
}

// repeatAcceptReverseSwapOffer handles a record that already exists.
//
// The interesting case is the crash window: Bob's settlement keys are durable but the
// accept message never reached Alice, so the record exists without his authenticated
// identity. Resuming is safe only while nothing has been funded or claimed, because a
// handshake completed after settlement activity would be a second history for money that
// has already moved.
func repeatAcceptReverseSwapOffer(
	ctx context.Context,
	engine *swap.Engine,
	record *model.Swap,
	item *marketflow.ParsedMatch,
	matchID, alice, bob string,
	records store.Store,
) error {
	if record.NostrAlicePub == nil || *record.NostrAlicePub != alice {
		return fmt.Errorf("SECURITY FAILURE: existing Bob state has different Alice identity")
	}

	if record.NostrBobPub != nil && *record.NostrBobPub != bob {
		return fmt.Errorf("SECURITY FAILURE: existing Bob state has different Bob identity")
	}

	if record.NostrBobPub == nil {
		if err := resumeReverseBobHandshake(ctx, engine, records, record, alice, bob, matchID); err != nil {
			return err
		}

		return nil
	}

	if record.BlakeAmount() != item.BlakeAmount {
		return fmt.Errorf("SECURITY FAILURE: existing Bob state has different BLAKE terms")
	}

	if record.BTCSats != item.BTCSats {
		return fmt.Errorf("SECURITY FAILURE: existing Bob state has different BTC terms")
	}

	fmt.Print(renderReverseSwapOfferAlreadyImported(matchID, record.SwapID))

	return nil
}

// resumeReverseBobHandshake finishes a handshake whose accept message was never sent.
func resumeReverseBobHandshake(
	ctx context.Context,
	engine *swap.Engine,
	records store.Store,
	record *model.Swap,
	alice, bob, matchID string,
) error {
	if err := reverseBobResumable(record); err != nil {
		return err
	}

	record.NostrBobPub = &bob

	if err := records.Save(ctx, store.Bob, record); err != nil {
		return err
	}

	if engine.Transport == nil {
		return fmt.Errorf("the engine has no coordination transport")
	}

	if record.BlakeBobPub == nil || record.BTCBobPub == nil {
		return fmt.Errorf("SECURITY FAILURE: partial reverse Bob state does not contain Bob settlement keys")
	}

	location, err := engine.Transport.Send(ctx, record.SwapID, "accept", map[string]any{
		"type":          "accept",
		"swap_id":       record.SwapID,
		"secret_hash":   record.SecretHash,
		"blake_bob_pub": *record.BlakeBobPub,
		"btc_bob_pub":   *record.BTCBobPub,
	}, coord.SendOptions{
		LocalRole:    store.Bob,
		ExpectedRole: store.Bob,
		PeerPubkey:   alice,
		Mode:         engine.Mode,
	})

	if err != nil {
		return err
	}

	fmt.Print(renderReverseBobRecovered(record.SwapID, location))

	return nil
}

// reverseBobResumable reports whether an unfunded record may finish its handshake.
func reverseBobResumable(record *model.Swap) error {
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

// renderReverseSwapOfferImported is the reference's confirmation for a fresh import.
func renderReverseSwapOfferImported(matchID string, record *model.Swap) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("REVERSE SWAP OFFER IMPORTED\n")
	out.WriteString("===========================\n")
	fmt.Fprintf(&out, "match id: %s\n", matchID)
	fmt.Fprintf(&out, "swap id: %s\n", record.SwapID)
	out.WriteString("\n")

	if record.NostrAlicePub != nil {
		fmt.Fprintf(&out, "Alice Nostr: %s\n", *record.NostrAlicePub)
	}

	if record.NostrBobPub != nil {
		fmt.Fprintf(&out, "Bob Nostr:   %s\n", *record.NostrBobPub)
	}

	out.WriteString("\n")
	out.WriteString("PASS: market maker derived as protocol Bob\n")
	out.WriteString("PASS: market taker derived as protocol Alice\n")
	out.WriteString("PASS: protocol offer signed by matched Alice\n")
	out.WriteString("PASS: economic terms match signed market match\n")
	out.WriteString("PASS: v1.5 timeout safety independently validated\n")
	out.WriteString("PASS: Bob does NOT know secret S\n")

	return out.String()
}

// renderReverseSwapOfferAlreadyImported is the duplicate block.
func renderReverseSwapOfferAlreadyImported(matchID, swapID string) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("REVERSE SWAP OFFER ALREADY IMPORTED\n")
	out.WriteString("===================================\n")
	fmt.Fprintf(&out, "match id: %s\n", matchID)
	fmt.Fprintf(&out, "swap id:  %s\n", swapID)
	out.WriteString("\n")
	out.WriteString("PASS: existing Bob state reused\n")
	out.WriteString("PASS: no new Bob settlement keys created\n")
	out.WriteString("PASS: Bob still does NOT know secret S\n")

	return out.String()
}

// renderReverseBobRecovered is the block for a resumed handshake.
func renderReverseBobRecovered(swapID, location string) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("REVERSE BOB HANDSHAKE RECOVERED\n")
	out.WriteString("===============================\n")
	fmt.Fprintf(&out, "swap id:        %s\n", swapID)
	fmt.Fprintf(&out, "accept message: %s\n", location)
	out.WriteString("\n")
	out.WriteString("PASS: existing Bob settlement keys reused\n")
	out.WriteString("PASS: authenticated Bob identity pinned\n")
	out.WriteString("PASS: Bob still does NOT know secret S\n")
	out.WriteString("PASS: no funds moved\n")

	return out.String()
}
