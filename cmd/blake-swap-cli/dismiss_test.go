package main

import (
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/reconcile"
)

// dismissableSwap is a record with nothing at risk: the usual case for archiving.
func dismissableSwap() *model.Swap {
	alice, bob := strings.Repeat("11", 32), strings.Repeat("22", 32)

	return &model.Swap{
		SwapID:        "aaaa00000001",
		State:         "OFFERED",
		NostrAlicePub: &alice,
		NostrBobPub:   &bob,
	}
}

// emptyReport is what reconciliation says about a swap that never funded anything.
func emptyReport() *reconcile.Report {
	return &reconcile.Report{
		SwapID: "aaaa00000001",
		Blake:  reconcile.Observation{Status: reconcile.Absent},
		BTC:    reconcile.Observation{Status: reconcile.Absent},
		Reason: "no HTLC output was ever funded",
	}
}

// TestDismissRefusalAllowsAnUnfundedSwap: the case the command exists for.
func TestDismissRefusalAllowsAnUnfundedSwap(t *testing.T) {
	if err := dismissRefusal(dismissableSwap(), emptyReport()); err != nil {
		t.Fatalf("an unfunded swap was refused: %v", err)
	}
}

// TestDismissRefusalRefusesAnythingWithMoneyAtRisk: every observation that cannot prove
// the money is gone is a refusal, naming the chain and the status.
func TestDismissRefusalRefusesAnythingWithMoneyAtRisk(t *testing.T) {
	cases := map[string]reconcile.Status{
		"funded":        reconcile.Funded,
		"spent":         reconcile.Spent,
		"unknown":       reconcile.NotEvaluated,
		"pending":       reconcile.Pending,
		"pending-spend": reconcile.PendingSpend,
		"invalid":       reconcile.Invalid,
		"ambiguous":     reconcile.Ambiguous,
	}

	for name, status := range cases {
		t.Run(name, func(t *testing.T) {
			report := emptyReport()
			report.Blake.Status = status

			err := dismissRefusal(dismissableSwap(), report)

			if err == nil {
				t.Fatalf("%s was accepted", name)
			}

			if !strings.Contains(err.Error(), "BLAKE") || !strings.Contains(err.Error(), string(status)) {
				t.Fatalf("error does not name the chain and status: %v", err)
			}

			// The other chain is checked the same way.
			other := emptyReport()
			other.BTC.Status = status

			if err := dismissRefusal(dismissableSwap(), other); err == nil ||
				!strings.Contains(err.Error(), "Bitcoin") {
				t.Fatalf("Bitcoin %s: err = %v", name, err)
			}
		})
	}
}

// TestDismissRefusalAllowsTheUnfundableNegotiation: a swap whose counterparty never
// produced settlement pubkeys has nothing to evaluate, so "not evaluated" is the truth
// rather than a gap — and only in that exact case.
func TestDismissRefusalAllowsTheUnfundableNegotiation(t *testing.T) {
	report := emptyReport()
	report.Blake.Status = ""
	report.BTC.Status = ""
	report.NegotiationStatus = "negotiation-incomplete"
	report.NegotiationReason = "protocol Bob settlement pubkeys were never created or imported"

	if err := dismissRefusal(dismissableSwap(), report); err != nil {
		t.Fatalf("an unfundable negotiation was refused: %v", err)
	}

	// Once the pubkeys exist the swap was settlement-capable, so it is not dismissed.
	pub := strings.Repeat("33", 32)
	record := dismissableSwap()
	record.BlakeBobPub = &pub

	if err := dismissRefusal(record, report); err == nil {
		t.Fatal("a settlement-capable swap was accepted as unfundable")
	}

	// A different negotiation reason is not the same exemption.
	other := emptyReport()
	other.Blake.Status = ""
	other.NegotiationStatus = "negotiation-incomplete"
	other.NegotiationReason = "something else"

	if err := dismissRefusal(dismissableSwap(), other); err == nil {
		t.Fatal("an unknown negotiation reason was accepted")
	}
}

// TestDismissRefusalRefusesRecordedSettlement: a swap that did something is not
// paperwork, whatever the chain currently says.
func TestDismissRefusalRefusesRecordedSettlement(t *testing.T) {
	txid := strings.Repeat("ab", 32)

	for name, record := range map[string]*model.Swap{
		"blake funding": {BlakeFundingTxID: &txid},
		"btc funding":   {BTCFundingTxID: &txid},
		"btc claim":     {BTCCLaimTxID: &txid},
		"blake claim":   {BlakeClaimTxID: &txid},
		"btc refund":    {BTCRefundTxID: &txid},
		"blake refund":  {BlakeRefundTxID: &txid},
	} {
		record.SwapID = "aaaa00000001"

		err := dismissRefusal(record, emptyReport())

		if err == nil {
			t.Fatalf("%s was accepted", name)
		}

		if !strings.Contains(err.Error(), "durable settlement/funding fields exist") {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

// TestIsParty: only a named party may retire a record.
func TestIsParty(t *testing.T) {
	record := dismissableSwap()

	if !isParty(record, strings.Repeat("11", 32)) || !isParty(record, strings.Repeat("22", 32)) {
		t.Fatal("a named party was not recognised")
	}

	if isParty(record, strings.Repeat("99", 32)) {
		t.Fatal("a stranger was treated as a party")
	}

	if isParty(&model.Swap{SwapID: "aaaa00000001"}, strings.Repeat("11", 32)) {
		t.Fatal("a record with no participants admitted a party")
	}
}
