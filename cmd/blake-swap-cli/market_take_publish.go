package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// runTakeMarketOffer publishes this participant's take for an open offer.
//
// Taking is the taker's only irreversible act before any money moves: the take is a
// signed statement that they want this trade on these terms. Two rules follow from
// that, and both are enforced here rather than trusted:
//
//   - the offer must still be open according to the same scanner everyone else uses,
//     so an expired or cancelled offer cannot be taken by a client that looked at a
//     stale answer; and
//   - a take that was already signed is reused, never re-signed. Publishing is
//     retried with the identical event, because two takes for one offer would be two
//     statements from the same person.
func runTakeMarketOffer(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("take-market-offer takes one offer id")
	}

	key, err := engineKey(engine)

	if err != nil {
		return err
	}

	client := marketClient()
	now := time.Now()

	offerID, err := resolveCachedOfferPrefix(ctx, client, args[0])

	if err != nil {
		return err
	}

	// The same scanner the market commands use, so signature, expiry and cancellation
	// rules are applied once.
	report, err := client.FetchReport(ctx, marketflow.MarketFilters(200)...)

	if err != nil {
		return err
	}

	events := append([]event.Event{}, report.Events...)
	events = append(events, cachedEvents(ctx, client.Cache)...)

	entries, err := marketflow.Book(events, now)

	if err != nil {
		return err
	}

	open := marketflow.OpenOffers(entries)

	var selected *marketflow.Entry

	for index := range open {
		if open[index].EventID() == offerID {
			selected = &open[index]

			break
		}
	}

	if selected == nil {
		return fmt.Errorf(
			"Offer is not currently open. It may be expired, cancelled, invalid, " +
				"or unavailable from this relay.",
		)
	}

	// The selected offer's BTC leg has to be feasible to settle, or taking it would
	// commit both sides to a trade that cannot be closed out.
	minimum, err := engine.BTCTradeMinimumSats(ctx)

	if err != nil {
		return err
	}

	if selected.Offer.BTCSats < minimum {
		return fmt.Errorf(
			"selected market offer BTC amount %d sats is too small; minimum "+
				"settlement-feasible amount is %d sats under the current settlement fee policy",
			selected.Offer.BTCSats, minimum,
		)
	}

	if prior := priorTake(events, key.PublicKey(), offerID); prior != nil {
		return reuseTake(ctx, client, offerID, prior)
	}

	built, _, err := marketflow.BuildTake(key, marketflow.BuildTakeOptions{
		Offer:     selected.Offer,
		CreatedAt: now.Unix(),
	})

	if err != nil {
		return err
	}

	if _, err := client.Publish(ctx, built); err != nil {
		return err
	}

	fmt.Print(renderTaken(
		offerID,
		built.ID,
		selected.Offer.Maker,
		key.PublicKey(),
		selected.Offer.BlakeAmount,
		selected.Offer.BTCSats,
	))

	return nil
}

// reuseTake republishes an already-signed take and says so.
//
// The alternative — signing a second take — would be two statements of intent for one
// offer from one taker, and a maker receiving both has no way to know which is real.
func reuseTake(ctx context.Context, client *nostr.Client, offerID string, prior *event.Event) error {
	fmt.Print(renderAlreadyTaken(offerID, prior.ID, prior.Pubkey))

	if _, err := client.Publish(ctx, *prior); err != nil {
		fmt.Print(renderPublishUnconfirmed(err))

		return nil
	}

	return nil
}

// priorTake finds this participant's cached take for an offer.
func priorTake(events []event.Event, taker, offerID string) *event.Event {
	for index := range events {
		candidate := &events[index]

		if candidate.Pubkey != taker {
			continue
		}

		var message map[string]any

		if err := json.Unmarshal([]byte(candidate.Content), &message); err != nil {
			continue
		}

		kind, _ := message["type"].(string)
		offer, _ := message["offer_id"].(string)

		if kind == "market-take" && offer == offerID {
			return candidate
		}
	}

	return nil
}

// resolveCachedOfferPrefix resolves an offer id, or a prefix of at least eight hex
// characters, against the verified cache.
//
// A prefix is a convenience with a price: it is only resolvable against events this
// participant has actually seen, so a fresh cache cannot resolve one. The reference has
// the same limit, and says so.
func resolveCachedOfferPrefix(ctx context.Context, client *nostr.Client, raw string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(raw))

	if len(value) == 64 {
		if err := hexOnly(value); err != nil {
			return "", fmt.Errorf("market offer ID is not hexadecimal")
		}

		return value, nil
	}

	if len(value) < 8 {
		return "", fmt.Errorf("market offer ID prefix is too short; use at least 8 hex characters")
	}

	if err := hexOnly(value); err != nil {
		return "", fmt.Errorf("market offer ID prefix is not hexadecimal")
	}

	var matches []string

	for _, cached := range cachedEvents(ctx, client.Cache) {
		if !strings.HasPrefix(cached.ID, value) {
			continue
		}

		var message map[string]any

		if err := json.Unmarshal([]byte(cached.Content), &message); err != nil {
			continue
		}

		if kind, _ := message["type"].(string); kind != "market-offer" {
			continue
		}

		matches = append(matches, cached.ID)
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no cached market offer matches that prefix")
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("that prefix matches %d cached market offers", len(matches))
	}
}

// hexOnly reports whether a string is hexadecimal characters only.
func hexOnly(value string) error {
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return fmt.Errorf("not hexadecimal")
		}
	}

	return nil
}

// renderTaken is the reference's confirmation block for an accepted take.
func renderTaken(offerID, takeID, maker, taker string, blake amount.Blake, sats int64) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("MARKET OFFER TAKEN\n")
	out.WriteString("==================\n")
	fmt.Fprintf(&out, "offer id: %s\n", offerID)
	fmt.Fprintf(&out, "take id:  %s\n", takeID)
	fmt.Fprintf(&out, "maker:    %s\n", maker)
	fmt.Fprintf(&out, "taker:    %s\n", taker)
	fmt.Fprintf(&out, "selling:  %s BLAKE\n", blake.Format())
	fmt.Fprintf(&out, "asking:   %s sats\n", thousands(sats))
	out.WriteString("\n")
	out.WriteString("PASS: offer was open when taker took it\n")
	out.WriteString("PASS: take request is signed by taker\n")
	out.WriteString("PASS: no money has moved\n")

	return out.String()
}

// renderAlreadyTaken is the block printed when a signed take is being republished.
func renderAlreadyTaken(offerID, takeID, taker string) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("MARKET OFFER ALREADY TAKEN\n")
	out.WriteString("==========================\n")
	fmt.Fprintf(&out, "offer id: %s\n", offerID)
	fmt.Fprintf(&out, "take id:  %s\n", takeID)
	fmt.Fprintf(&out, "taker:    %s\n", taker)
	out.WriteString("\n")
	out.WriteString("PASS: existing signed take reused\n")
	out.WriteString("PASS: no duplicate Nostr event created\n")
	out.WriteString("PASS: no money has moved\n")
	out.WriteString("\n")
	out.WriteString("Retrying publication of the exact same signed take...\n")

	return out.String()
}

// renderPublishUnconfirmed reports a retry that reached no relay.
func renderPublishUnconfirmed(err error) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("WARNING: relay publication was not confirmed\n")
	fmt.Fprintf(&out, "%s\n", err)
	out.WriteString("\n")
	out.WriteString("PASS: original signed take remains durably cached\n")
	out.WriteString("PASS: retry did not create a new take event\n")

	return out.String()
}
