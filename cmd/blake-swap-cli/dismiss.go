package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/reconcile"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// runDismiss archives a local swap record that never became a trade.
//
// Archiving is not a protocol action — nothing is published, nothing is spent — but it
// removes the record from every later listing, so the safety rules matter: a swap that
// ever had funds at risk must not disappear from the participant's view by accident.
func runDismiss(ctx context.Context, engine *swap.Engine, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("dismiss takes one swap id or prefix")
	}

	key, err := engineKey(engine)

	if err != nil {
		return err
	}

	prefix := strings.ToLower(strings.TrimSpace(args[0]))

	if len(prefix) < 4 {
		return fmt.Errorf("swap ID prefix is too short; use at least 4 characters")
	}

	records, err := engine.List(ctx)

	if err != nil {
		return err
	}

	var matches []*model.Swap

	for _, record := range records {
		if strings.HasPrefix(record.SwapID, prefix) {
			matches = append(matches, record)
		}
	}

	if len(matches) == 0 {
		return fmt.Errorf("No local swap matches prefix '%s'", prefix)
	}

	if len(matches) > 1 {
		fmt.Print(renderAmbiguousSwaps(matches))

		return fmt.Errorf("Swap prefix is ambiguous; use more characters")
	}

	record := matches[0]

	// Dismissal archives a local record. Refusing to archive a swap this participant is
	// not a party to stops a shared or mis-pointed state root from retiring somebody
	// else's trade.
	if !isParty(record, key.PublicKey()) {
		return fmt.Errorf(
			"SECURITY FAILURE: refusing to dismiss a swap this participant is not a party to",
		)
	}

	report, err := engine.Reconcile(ctx, record.SwapID)

	if err != nil {
		return fmt.Errorf("Cannot safely dismiss because read-only reconciliation failed: %w", err)
	}

	if err := dismissRefusal(record, report); err != nil {
		return err
	}

	fmt.Print(renderSafeToArchive(record, report))

	if err := engine.Store.Archive(ctx, engine.Role, record.SwapID); err != nil {
		return err
	}

	fmt.Print(renderArchived(record.SwapID))

	return nil
}

// isParty reports whether this participant is named in the record.
func isParty(record *model.Swap, me string) bool {
	if record.NostrAlicePub != nil && *record.NostrAlicePub == me {
		return true
	}

	if record.NostrBobPub != nil && *record.NostrBobPub == me {
		return true
	}

	return false
}

// dismissRefusal is the safety rule, kept separate from the reading and moving.
//
// It refuses on three grounds, in the order the reference uses: an observation that
// cannot prove the money is gone, a status that is not "nothing is there", and any
// durable settlement field at all. The last is deliberately blunt — a recorded txid means
// this swap did something, and a swap that did something is not dismissed as paperwork.
func dismissRefusal(record *model.Swap, report *reconcile.Report) error {
	negotiationIncomplete := report.NegotiationStatus == "negotiation-incomplete" &&
		report.NegotiationReason == "protocol Bob settlement pubkeys were never created or imported" &&
		record.BlakeBobPub == nil && record.BTCBobPub == nil

	for _, chain := range []struct {
		name   string
		status reconcile.Status
	}{
		{"BLAKE", report.Blake.Status},
		{"Bitcoin", report.BTC.Status},
	} {
		switch chain.status {
		case reconcile.Funded, reconcile.Spent, reconcile.NotEvaluated:
			return fmt.Errorf("Refusing to dismiss: %s status is '%s'", chain.name, chain.status)
		case reconcile.Absent:
			continue
		case "":
			if negotiationIncomplete {
				continue
			}
		}

		return fmt.Errorf("Refusing to dismiss: %s status is '%s'", chain.name, chain.status)
	}

	var populated []string

	for _, field := range []struct {
		name  string
		value *string
	}{
		{"blake_funding_txid", record.BlakeFundingTxID},
		{"btc_funding_txid", record.BTCFundingTxID},
		{"btc_claim_txid", record.BTCCLaimTxID},
		{"blake_claim_txid", record.BlakeClaimTxID},
		{"btc_refund_txid", record.BTCRefundTxID},
		{"blake_refund_txid", record.BlakeRefundTxID},
	} {
		if field.value != nil && *field.value != "" {
			populated = append(populated, field.name)
		}
	}

	if len(populated) > 0 {
		return fmt.Errorf(
			"Refusing to dismiss: durable settlement/funding fields exist: %s",
			strings.Join(populated, ", "),
		)
	}

	return nil
}

// renderSafeToArchive is the block printed before the record moves.
func renderSafeToArchive(record *model.Swap, report *reconcile.Report) string {
	var out strings.Builder

	blake, btc := string(report.Blake.Status), string(report.BTC.Status)

	if blake == "" {
		blake = "absent"
	}

	if btc == "" {
		btc = "absent"
	}

	out.WriteString("\nSAFE TO ARCHIVE\n")
	out.WriteString("===============\n")
	fmt.Fprintf(&out, "swap:    %s\n", record.SwapID)
	fmt.Fprintf(&out, "state:   %s\n", record.State)
	fmt.Fprintf(&out, "BLAKE:   %s\n", blake)
	fmt.Fprintf(&out, "BTC:     %s\n", btc)
	fmt.Fprintf(&out, "reason:  %s\n", report.Reason)
	out.WriteString("\n")

	if report.NegotiationStatus == "negotiation-incomplete" {
		out.WriteString("PASS: negotiation never became settlement-capable\n")
		out.WriteString("PASS: missing Bob settlement pubkeys make HTLC evaluation inapplicable\n")
	} else {
		out.WriteString("PASS: no funded HTLCs were found\n")
	}

	out.WriteString("PASS: no settlement transaction is recorded\n")
	out.WriteString("PASS: archive is local-only; nothing is published or spent\n")

	return out.String()
}

// renderArchived is the block printed once the record has moved.
func renderArchived(swapID string) string {
	return "\nSWAP ARCHIVED\n" +
		"=============\n" +
		"swap: " + swapID + "\n\n" +
		"PASS: original JSON preserved intact\n"
}

// renderAmbiguousSwaps lists the candidates a too-short prefix matched.
func renderAmbiguousSwaps(matches []*model.Swap) string {
	var out strings.Builder

	out.WriteString("\nAMBIGUOUS SWAP PREFIX\n")
	out.WriteString("=====================\n")

	for _, record := range matches {
		fmt.Fprintf(&out, "%s\n", record.SwapID)
	}

	return out.String()
}
