package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/gjbrr/blake-swap/pkg/marketops"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// runAcceptReverseSwapOffer imports Alice's ordinary protocol offer after a reverse match.
//
// In this direction the current participant is the market maker, which is protocol Bob:
// he published the match, and what he still needs is the other half of the handshake —
// Alice's signed atomic-swap offer, with her contract keys and her deadlines. The state
// machine is shared with the browser worker, so a crash between Bob's keys being durable and
// his accept being sent is recovered the same way in both clients.
func runAcceptReverseSwapOffer(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("accept-reverse-swap-offer takes one match id")
	}

	matchID := args[0]

	if len(matchID) != 64 {
		return fmt.Errorf("match_id must be the full 64-character Nostr event ID")
	}

	if _, err := engineKey(engine); err != nil {
		return err
	}

	client := marketClient()

	matchEvent, err := fetchEventByID(ctx, client, matchID)

	if err != nil {
		return err
	}

	env, err := marketEnvOrError(engine, client, func(line string) { fmt.Print(line) })

	if err != nil {
		return err
	}

	_, err = marketops.AcceptReverseOffer(ctx, env, matchID, matchEvent, marketops.MatchHooks{
		AfterAccept: func(result *swap.AcceptResult) {
			fmt.Printf("accept message: %s\n", messageLocation(result.Location))
			fmt.Print(renderOfferAccepted(
				result.Swap.SwapID, recordPath(result.Swap.SwapID), result.Timeout,
			))
		},
		AfterReverseOffer: func(imported marketops.ReverseOfferImport) {
			switch {
			case imported.Recovered:
				fmt.Print(renderReverseBobRecovered(
					imported.SwapID, messageLocation(imported.Location),
				))
			case imported.Repeated:
				fmt.Print(renderReverseSwapOfferAlreadyImported(
					imported.MatchID, imported.SwapID,
				))
			default:
				fmt.Print(renderReverseSwapOfferImported(
					imported.MatchID, imported.SwapID, imported.Alice, imported.Bob,
				))
			}
		},
	})

	return err
}

// renderReverseSwapOfferImported is the reference's confirmation for a fresh import.
func renderReverseSwapOfferImported(matchID, swapID, alice, bob string) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("REVERSE SWAP OFFER IMPORTED\n")
	out.WriteString("===========================\n")
	fmt.Fprintf(&out, "match id: %s\n", matchID)
	fmt.Fprintf(&out, "swap id:  %s\n", swapID)
	out.WriteString("\n")
	fmt.Fprintf(&out, "Alice Nostr: %s\n", alice)
	fmt.Fprintf(&out, "Bob Nostr:   %s\n", bob)

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
