package main

import (
	"context"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// The two writing market commands.
//
// Publishing an advertisement is not a swap: it puts a signed offer on relays, which
// anybody may take or ignore, and nothing is funded or committed. Cancelling withdraws
// one, and only the maker may do it — which the protocol enforces by the event's signer,
// not by the command's good intentions.

// runPublishMarketOffer advertises an offer on the configured relays.
func runPublishMarketOffer(ctx context.Context, engine *swap.Engine, args []string) error {
	flags := flag.NewFlagSet("publish-market-offer", flag.ContinueOnError)

	blake := flags.Float64("blake", 0, "BLAKE amount the maker gives or wants")
	sats := flags.Int64("sats", 0, "BTC sat amount")
	minutes := flags.Int64("minutes", 60, "offer lifetime in minutes")
	direction := flags.String("direction", string(market.BlakeForBTC),
		"asset the maker gives: blake-for-btc or btc-for-blake")

	if err := flags.Parse(args); err != nil {
		return err
	}

	if *blake <= 0 {
		return fmt.Errorf("BLAKE amount must be positive")
	}

	if *sats <= 0 {
		return fmt.Errorf("BTC sat amount must be positive")
	}

	// An offer promises that somebody can settle it. Below the settlement cost the
	// advertisement is one nobody could profitably take, so it is refused here.
	minimum, err := engine.BTCTradeMinimumSats(ctx)

	if err != nil {
		return err
	}

	if *sats < minimum {
		return fmt.Errorf(
			"market offer BTC amount %d sats is too small; minimum settlement-feasible "+
				"amount is %d sats under the current settlement fee policy",
			*sats, minimum,
		)
	}

	if *minutes <= 0 {
		return fmt.Errorf("Offer lifetime must be positive")
	}

	parsed, err := market.ParseDirection(*direction)

	if err != nil {
		return err
	}

	key, err := engineKey(engine)

	if err != nil {
		return err
	}

	blakeAmount, err := amount.ParseBlake(strconv.FormatFloat(*blake, 'f', -1, 64))

	if err != nil {
		return err
	}

	built, offer, err := marketflow.BuildOffer(key, marketflow.BuildOfferOptions{
		BlakeAmount: blakeAmount,
		BTCSats:     *sats,
		Minutes:     *minutes,
		Direction:   parsed,
	})

	if err != nil {
		return err
	}

	if _, err := marketClient().Publish(ctx, built); err != nil {
		return err
	}

	fmt.Print(renderPublishedOffer(built.ID, key.PublicKey(), offer, *minutes))

	return nil
}

// runCancelMarketOffer withdraws an offer, refusing if this identity is not its maker.
func runCancelMarketOffer(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("cancel-market-offer takes one offer id")
	}

	offerID := strings.TrimSpace(args[0])

	if len(offerID) != 64 {
		return fmt.Errorf("offer_id must be a 64-character Nostr event ID")
	}

	key, err := engineKey(engine)

	if err != nil {
		return err
	}

	client := marketClient()

	// Fetch the original: the cancel names it by id, and it can only be signed by the
	// identity that signed the offer.
	events, err := client.FetchMany(ctx, nostr.Filter{Kinds: []int{1}, Ids: []string{offerID}})

	if err != nil {
		return err
	}

	var original *event.Event

	for index := range events {
		if events[index].ID == offerID {
			original = &events[index]
		}
	}

	if original == nil {
		return fmt.Errorf("the referenced offer was not found on any configured relay")
	}

	if _, err := marketflow.ParseOffer(*original, time.Now()); err != nil {
		return fmt.Errorf("Referenced event is not a market offer")
	}

	if original.Pubkey != key.PublicKey() {
		return fmt.Errorf(
			"SECURITY FAILURE: current Nostr identity is not the maker of this offer",
		)
	}

	built, _, err := marketflow.BuildCancel(key, marketflow.BuildCancelOptions{OfferID: offerID})

	if err != nil {
		return err
	}

	if _, err := client.Publish(ctx, built); err != nil {
		return err
	}

	fmt.Print(renderCancelledOffer(offerID, built.ID, key.PublicKey()))

	return nil
}

// engineKey is the participant's signing identity.
//
// The engine holds it as an interface because most of the protocol only needs the
// public key; signing a market event needs the secret, so this is where that is asked
// for explicitly rather than type-asserted at three call sites.
func engineKey(engine *swap.Engine) (*nostr.Key, error) {
	if engine == nil || engine.Identity == nil {
		return nil, fmt.Errorf("no participant identity: set BLAKE_SWAP_PARTICIPANT_KEY_FILE")
	}

	key, ok := engine.Identity.(*nostr.Key)

	if !ok {
		return nil, fmt.Errorf("the participant identity cannot sign")
	}

	return key, nil
}

// renderPublishedOffer is the reference's confirmation block, byte for byte.
func renderPublishedOffer(eventID, maker string, offer market.Offer, minutes int64) string {
	give, want, err := offer.Direction.GiveWant(offer.BlakeAmount, amount.Sats(offer.BTCSats))

	if err != nil {
		give, want = "", ""
	}

	makerRole, _, err := offer.Direction.ProtocolRoles()

	if err != nil {
		makerRole = ""
	}

	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("MARKET OFFER PUBLISHED\n")
	out.WriteString("======================\n")
	fmt.Fprintf(&out, "offer id: %s\n", eventID)
	fmt.Fprintf(&out, "maker:    %s\n", maker)
	fmt.Fprintf(&out, "giving:   %s\n", give)
	fmt.Fprintf(&out, "wanting:  %s\n", want)
	fmt.Fprintf(&out, "direction: %s\n", offer.Direction)
	fmt.Fprintf(&out, "maker protocol role: %s\n", makerRole)
	fmt.Fprintf(&out, "lifetime: %d minute(s)\n", minutes)
	fmt.Fprintf(&out, "expires:  %d\n", offer.ExpiresAt)
	out.WriteString("\n")
	out.WriteString("PASS: advertisement contains no secret, hashlock, HTLC, or chain wallet keys\n")

	return out.String()
}

// renderCancelledOffer is the reference's cancellation block, byte for byte.
func renderCancelledOffer(offerID, cancelID, maker string) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("MARKET OFFER CANCELLED\n")
	out.WriteString("======================\n")
	fmt.Fprintf(&out, "offer id:  %s\n", offerID)
	fmt.Fprintf(&out, "cancel id: %s\n", cancelID)
	fmt.Fprintf(&out, "maker:     %s\n", maker)
	out.WriteString("\n")
	out.WriteString("PASS: cancellation was signed by the original offer maker\n")

	return out.String()
}
