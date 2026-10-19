package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/reconcile"
	"github.com/gjbrr/blake-swap/pkg/state"
	"github.com/gjbrr/blake-swap/pkg/swap"
)

// This file mirrors the reference's `tests/swap_dashboard_json_tests.py`: the dashboard's
// document. A dashboard is read by things other than people — a widget, a monitor, a script
// that alerts on "needs attention" — so its shape is a contract, not a rendering choice.

// dashboardEntry is one swap in the shape the dashboard carries.
func dashboardEntry() swap.DashboardEntry {
	funding := strings.Repeat("aa", 32)

	return swap.DashboardEntry{
		SwapID:      "aaaa00000001",
		State:       state.BTCFunded,
		Class:       reconcile.ClassActionable,
		Action:      reconcile.ActionClaimBTC,
		Outcome:     "READY TO CLAIM BTC",
		Reason:      "Bitcoin HTLC is funded and the BLAKE HTLC is funded",
		Role:        reconcile.Alice,
		BlakeStatus: reconcile.Funded,
		BTCStatus:   reconcile.Funded,
		BlakeAmount: amount.Blake(1_000_000_000),
		BTCSats:     25_000,
		TxIDs: map[string]string{
			"blake_funding_txid": funding,
			"btc_funding_txid":   funding,
		},
	}
}

// TestSwapDashboardJSONDocumentShape: the document's every field, and the summary counts.
func TestSwapDashboardJSONDocumentShape(t *testing.T) {
	summary := swap.Dashboard{
		Active:    []swap.DashboardEntry{dashboardEntry()},
		Attention: nil,
		Settled:   nil,
		Archived: []swap.DashboardEntry{{
			SwapID: "bbbb00000002", State: state.Complete, BlakeAmount: amount.Blake(2_000_000_000), BTCSats: 50_000,
		}},
	}

	encoded, err := json.Marshal(dashboardJSON(summary))

	if err != nil {
		t.Fatal(err)
	}

	var parsed struct {
		Version  int `json:"version"`
		Summary  map[string]int
		Active   []map[string]any
		Archived []map[string]any
	}

	if err := json.Unmarshal(encoded, &parsed); err != nil {
		t.Fatalf("%v\n%s", err, encoded)
	}

	if parsed.Version != 1 {
		t.Fatalf("version = %d", parsed.Version)
	}

	for key, want := range map[string]int{"active": 1, "attention": 0, "settled": 0, "archived": 1} {
		if parsed.Summary[key] != want {
			t.Errorf("summary[%s] = %d, want %d", key, parsed.Summary[key], want)
		}
	}

	item := parsed.Active[0]

	for field, want := range map[string]any{
		"swap_id":        "aaaa00000001",
		"state":          "BTC_FUNDED",
		"protocol_role":  "alice",
		"decision_class": "actionable",
		"next_action":    "claim-btc",
		"status":         "READY TO CLAIM BTC",
		"action":         "run resume",
	} {
		if item[field] != want {
			t.Errorf("%s = %v, want %v", field, item[field], want)
		}
	}

	trade, _ := item["trade"].(map[string]any)

	if trade["blake_amount"] != 10.0 || trade["blake_amount_base_units"] != float64(1_000_000_000) ||
		trade["btc_sats"] != float64(25_000) {
		t.Fatalf("trade = %v", trade)
	}

	chains, _ := item["chains"].(map[string]any)

	if chains["blake_status"] != "funded" || chains["btc_status"] != "funded" {
		t.Fatalf("chains = %v", chains)
	}

	// Solvability is a question put to the wallets, not a fact in the record; absent that
	// answer the field is null rather than a guess.
	if chains["blake_solvable"] != nil || chains["btc_solvable"] != nil {
		t.Fatalf("solvability was invented: %v", chains)
	}

	transactions, _ := item["transactions"].(map[string]any)

	if transactions["blake_funding_txid"] != strings.Repeat("aa", 32) {
		t.Errorf("funding txid = %v", transactions["blake_funding_txid"])
	}

	// A transaction this swap does not have is null, not an empty string: the two mean
	// different things to a client.
	if transactions["btc_claim_txid"] != nil {
		t.Errorf("an absent claim was reported as %v", transactions["btc_claim_txid"])
	}

	if parsed.Archived[0]["swap_id"] != "bbbb00000002" {
		t.Fatalf("archived = %v", parsed.Archived[0])
	}
}

// TestSwapDashboardJSONClassifiesByDecision: a swap is grouped by what the reconciler says,
// not by what its record's state field happens to hold.
func TestSwapDashboardJSONClassifiesByDecision(t *testing.T) {
	active := dashboardEntry()

	settled := dashboardEntry()
	settled.SwapID = "bbbb00000002"
	settled.State = state.Complete
	settled.Class = reconcile.ClassSettled
	settled.Action = reconcile.ActionNone
	settled.Outcome = "COMPLETED"

	attention := dashboardEntry()
	attention.SwapID = "cccc00000003"
	attention.Class = reconcile.ClassManualAttention
	attention.Action = reconcile.ActionManualAttention
	attention.Outcome = "NEEDS ATTENTION"

	failed := dashboardEntry()
	failed.SwapID = "dddd00000004"
	failed.Failed = true
	failed.Outcome = "NEEDS ATTENTION"

	summary := swap.Classify([]swap.DashboardEntry{active, settled, attention, failed})

	if len(summary.Active) != 1 || summary.Active[0].SwapID != active.SwapID {
		t.Fatalf("active = %v", summary.Active)
	}

	if len(summary.Settled) != 1 || summary.Settled[0].SwapID != settled.SwapID {
		t.Fatalf("settled = %v", summary.Settled)
	}

	// A report that could not be built is attention, not silence: an unreadable swap is
	// exactly the one a person must look at.
	if len(summary.Attention) != 2 {
		t.Fatalf("attention = %v", summary.Attention)
	}
}

// TestSwapDashboardHumanView: the three sections, and the words a person reads.
func TestSwapDashboardHumanView(t *testing.T) {
	recommendation := map[reconcile.Action]string{
		reconcile.ActionClaimBTC:        "run resume",
		reconcile.ActionFundBlake:       "run fund",
		reconcile.ActionWait:            "wait",
		reconcile.ActionManualAttention: "inspect",
		reconcile.ActionNone:            "-",
	}

	for action, want := range recommendation {
		if got := dashboardActionText(action); got != want {
			t.Errorf("%s = %q, want %q", action, got, want)
		}
	}

	summary := swap.Classify([]swap.DashboardEntry{dashboardEntry()})

	text := renderDashboard(summary)

	for _, want := range []string{
		"BLAKE SWAP DASHBOARD\n",
		"ACTIVE\n------\n",
		"NEEDS ATTENTION\n---------------\n",
		"SETTLED\n-------\n",
		"aaaa00000001",
		"READY TO CLAIM BTC",
		"run resume",
		"10.0 BLAKE / 25,000 sats",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("dashboard is missing %q:\n%s", want, text)
		}
	}

	// And an empty dashboard says so in each section rather than printing bare headings.
	empty := renderDashboard(swap.Classify(nil))

	for _, want := range []string{"No active swaps.", "No swaps need manual attention.", "No settled swaps."} {
		if !strings.Contains(empty, want) {
			t.Fatalf("empty dashboard is missing %q:\n%s", want, empty)
		}
	}
}
