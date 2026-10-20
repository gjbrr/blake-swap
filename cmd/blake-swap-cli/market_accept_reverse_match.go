package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/marketops"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/store"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// runAcceptReverseMarketMatch imports a btc-for-blake match as the taker.
//
// In this direction the market maker is protocol Bob and the taker is protocol Alice, so
// this command is where Alice creates the secret. That is the whole asymmetry: Bob
// publishes the match and reserves the swap id, and only Alice generates S, from her own
// machine, against deadlines she derives from her own view of both chains.
func runAcceptReverseMarketMatch(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("accept-reverse-market-match takes one match id")
	}

	matchID := args[0]

	if len(matchID) != 64 {
		return fmt.Errorf("match_id must be the full 64-character Nostr event ID")
	}

	client := marketClient()

	found, err := fetchEventByID(ctx, client, matchID)

	if err != nil {
		return err
	}

	env, err := marketEnvOrError(engine, client, func(line string) { fmt.Print(line) })

	if err != nil {
		return err
	}

	_, err = marketops.ImportReverseMatch(ctx, env, matchID, found, marketops.MatchHooks{
		AfterImport: func(imported marketops.MatchImport) {
			if imported.Repeated {
				fmt.Print(renderReverseMatchAlreadyImported(imported.MatchID, imported.SwapID))

				return
			}

			record, loadErr := store.NewFile(stateDirectory()).Load(ctx, imported.SwapID)

			if loadErr != nil {
				return
			}

			fmt.Print(renderReverseMatchImported(
				imported.MatchID, record, imported.Taker, imported.Maker,
			))
		},
	})

	return err
}

// reverseMatchRoles checks that a validated match is this participant's reverse trade.
//
// The direction implies the roles, so all four checks say the same thing four ways: a
// btc-for-blake match is Bob's advertisement accepted by Alice, and the current
// participant must be that Alice. Each is refused separately because the operator's
// remedy differs — a wrong direction is a wrong command, a wrong taker is a wrong key.
func reverseMatchRoles(item *marketflow.ParsedMatch, alice string) error {
	if item.Direction != market.BTCForBlake {
		return fmt.Errorf("Market match is not btc-for-blake")
	}

	if item.MakerRole != "bob" {
		return fmt.Errorf("SECURITY FAILURE: reverse match maker is not protocol Bob")
	}

	if item.TakerRole != "alice" {
		return fmt.Errorf("SECURITY FAILURE: reverse match taker is not protocol Alice")
	}

	if item.Taker != alice {
		return fmt.Errorf("SECURITY FAILURE: current participant is not the matched taker")
	}

	return nil
}

// repeatAcceptReverseMarketMatch reuses an Alice swap that already exists.
//
// No second secret is generated here, and that is the point: a repeated command after a
// crash must find the swap, not create a rival one that would invalidate the negotiation.
func repeatAcceptReverseMarketMatch(
	record *model.Swap, item *marketflow.ParsedMatch, matchID, alice, bob string,
) error {
	if record.NostrAlicePub == nil || *record.NostrAlicePub != alice {
		return fmt.Errorf("SECURITY FAILURE: existing Alice swap has different Alice identity")
	}

	if record.NostrBobPub == nil || *record.NostrBobPub != bob {
		return fmt.Errorf("SECURITY FAILURE: existing Alice swap has different Bob identity")
	}

	if record.BlakeAmount() != item.BlakeAmount {
		return fmt.Errorf("SECURITY FAILURE: existing Alice swap has different BLAKE terms")
	}

	if record.BTCSats != item.BTCSats {
		return fmt.Errorf("SECURITY FAILURE: existing Alice swap has different BTC terms")
	}

	fmt.Print(renderReverseMatchAlreadyImported(matchID, record.SwapID))

	return nil
}

// reverseMatchBinding is the durable reverse-market record.
func reverseMatchBinding(item *marketflow.ParsedMatch, matchID string) map[string]any {
	return map[string]any{
		"version":   2,
		"direction": string(item.Direction),

		"offer_id": item.OfferID,
		"take_id":  item.TakeID,
		"match_id": matchID,
		"swap_id":  item.SwapID,

		"maker": item.Maker,
		"taker": item.Taker,

		"maker_role": item.MakerRole,
		"taker_role": item.TakerRole,

		"blake_amount": json.Number(pythonFloat(blakeFloat(item.BlakeAmount))),
		"btc_sats":     item.BTCSats,
	}
}

// renderReverseMatchImported is the reference's reverse import confirmation.
func renderReverseMatchImported(matchID string, record *model.Swap, alice, bob string) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("REVERSE MARKET MATCH IMPORTED\n")
	out.WriteString("=============================\n")
	fmt.Fprintf(&out, "match id: %s\n", matchID)
	fmt.Fprintf(&out, "swap id: %s\n", record.SwapID)
	out.WriteString("\n")
	out.WriteString("direction: btc-for-blake\n")
	out.WriteString("maker is protocol: bob\n")
	out.WriteString("taker is protocol: alice\n")
	out.WriteString("\n")
	fmt.Fprintf(&out, "Alice Nostr: %s\n", alice)
	fmt.Fprintf(&out, "Bob Nostr:   %s\n", bob)
	out.WriteString("\n")
	fmt.Fprintf(&out, "BTC timeout:   %d\n", record.BTCTimeout)
	fmt.Fprintf(&out, "BLAKE timeout: %d\n", record.BlakeTimeout)
	out.WriteString("\n")
	fmt.Fprintf(&out, "BTC window: %s h\n", hours(record.BTCTimeoutWindowSeconds))
	fmt.Fprintf(&out, "Bob recovery: %s h\n", hours(record.BobRecoverySeconds))
	fmt.Fprintf(&out, "BLAKE window: %s h\n", hours(record.BlakeTimeoutWindowSeconds))
	out.WriteString("\n")
	out.WriteString("PASS: reverse match independently validated\n")
	out.WriteString("PASS: current participant derived as protocol Alice\n")
	out.WriteString("PASS: reserved swap_id adopted\n")
	out.WriteString("PASS: fresh secret S created only by Alice\n")
	out.WriteString("PASS: timeout plan independently derived\n")
	out.WriteString("PASS: no funds moved\n")

	return out.String()
}

// renderReverseMatchAlreadyImported is the duplicate block.
func renderReverseMatchAlreadyImported(matchID, swapID string) string {
	var out strings.Builder

	out.WriteString("\n")
	out.WriteString("REVERSE MARKET MATCH ALREADY IMPORTED\n")
	out.WriteString("=====================================\n")
	fmt.Fprintf(&out, "match id: %s\n", matchID)
	fmt.Fprintf(&out, "swap id:  %s\n", swapID)
	out.WriteString("\n")
	out.WriteString("PASS: existing Alice swap reused\n")
	out.WriteString("PASS: no second secret was generated\n")
	out.WriteString("PASS: no funds moved\n")

	return out.String()
}

// hours renders a duration in seconds the way the reference does: hours, two decimals.
//
// A nil duration is rendered as zero rather than as a missing value: a swap recorded
// before the windows were described has no window, which is zero, not unknown.
func hours(seconds *int64) string {
	var value int64

	if seconds != nil {
		value = *seconds
	}

	return fmt.Sprintf("%.2f", float64(value)/3600)
}
