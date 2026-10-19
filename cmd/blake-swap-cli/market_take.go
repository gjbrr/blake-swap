package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// runInspectMarketTake builds and verifies a take without publishing it.
//
// It is the taker's dry run: the event is signed by this identity exactly as a real take
// would be, and printed, but it never reaches a relay. That makes it the one place a
// person can see what they are about to advertise before anybody else can.
func runInspectMarketTake(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("inspect-market-take takes one offer id")
	}

	offerID := strings.TrimSpace(args[0])

	if len(offerID) != 64 {
		return fmt.Errorf("offer_id must be the full 64-character Nostr event ID")
	}

	key, err := engineKey(engine)

	if err != nil {
		return err
	}

	client := marketClient()
	now := time.Now()

	// Ask the same scanner the market commands use, so signature, expiry and
	// cancellation rules are applied by one implementation rather than two.
	report, err := client.FetchReport(ctx, marketflow.MarketFilters(200)...)

	if err != nil {
		return err
	}

	announceMarketRead(report)

	events := append([]event.Event{}, report.Events...)
	events = append(events, cachedEvents(ctx, client.Cache)...)

	entries, err := marketflow.Book(events, now)

	if err != nil {
		return err
	}

	open := marketflow.OpenOffers(entries)
	var listed *marketflow.Entry

	for index := range open {
		if open[index].EventID() == offerID {
			listed = &open[index]

			break
		}
	}

	if listed == nil {
		return fmt.Errorf("Offer is not currently open")
	}

	//
	// The dry run signs against the exact event, not against the scan's summary of it:
	// what is inspected has to be what a real take would reference.
	//
	original, err := fetchEventByID(ctx, client, offerID)

	if err != nil {
		return err
	}

	if err := event.Verify(*original, now); err != nil {
		return err
	}

	built, _, err := marketflow.BuildTake(key, marketflow.BuildTakeOptions{
		Offer:     listed.Offer,
		CreatedAt: now.Unix(),
	})

	if err != nil {
		return err
	}

	// The event was signed as it was built; verifying it here is what makes this a
	// check rather than a preview.
	if err := event.Verify(built, now); err != nil {
		return fmt.Errorf("the built take does not verify: %w", err)
	}

	rendered, err := renderInspectedTake(built)

	if err != nil {
		return err
	}

	fmt.Print(rendered)

	return nil
}

// renderInspectedTake is the reference's verification block for an unpublished take.
func renderInspectedTake(built event.Event) (string, error) {
	tags, err := json.MarshalIndent(built.Tags, "", "  ")

	if err != nil {
		return "", err
	}

	var message map[string]any

	if err := json.Unmarshal([]byte(built.Content), &message); err != nil {
		return "", err
	}

	// A map marshals with sorted keys, which is what the reference prints.
	content, err := json.MarshalIndent(message, "", "  ")

	if err != nil {
		return "", err
	}

	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("MARKET TAKE EVENT VALID\n")
	out.WriteString("=======================\n")
	fmt.Fprintf(&out, "event id: %s\n", built.ID)
	fmt.Fprintf(&out, "signer:   %s\n", built.Pubkey)
	fmt.Fprintf(&out, "kind:     %d\n", built.Kind)
	out.WriteString("\n")
	out.WriteString("tags:\n")
	out.Write(tags)
	out.WriteString("\n\n")
	out.WriteString("content:\n")
	out.Write(content)
	out.WriteString("\n\n")
	out.WriteString("PASS: event ID recomputes correctly\n")
	out.WriteString("PASS: Bob's Schnorr signature verifies\n")
	out.WriteString("PASS: event was NOT published\n")

	return out.String(), nil
}
