package view_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/decimal"
	"github.com/gjbrr/blake-swap/pkg/market"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/proto"
	"github.com/gjbrr/blake-swap/pkg/reconcile"
	"github.com/gjbrr/blake-swap/pkg/state"
	"github.com/gjbrr/blake-swap/pkg/swap"
	"github.com/gjbrr/blake-swap/pkg/view"
)

// record is a swap with both sides pinned, so the role derivation has something to
// derive from.
func record() *model.Swap {
	alice, bob := strings.Repeat("a", 64), strings.Repeat("b", 64)
	blakeTx, btcTx := strings.Repeat("c", 64), strings.Repeat("d", 64)

	vout := int32(1)

	return &model.Swap{
		SwapID:               "0123456789ab",
		State:                state.BTCFunded,
		CreatedAt:            1_700_000_000,
		BlakeAmountBaseUnits: 1_000_000,
		BTCSats:              25_000,
		SecretHash:           strings.Repeat("e", 64),
		BlakeTimeout:         900,
		BTCTimeout:           800,
		BlakeFundingTxID:     &blakeTx,
		BlakeFundingVout:     &vout,
		BTCFundingTxID:       &btcTx,
		NostrAlicePub:        &alice,
		NostrBobPub:          &bob,
	}
}

// TestAmountsAreExactStrings is the rule the whole view layer exists to keep: money
// crosses to the browser as a string produced by pkg/amount, never as a number a
// JavaScript engine could read as a float.
func TestAmountsAreExactStrings(t *testing.T) {
	summary := view.Summary(record(), string(reconcile.Alice), nil)

	if summary.BlakeAmount != "0.01" {
		t.Fatalf("BLAKE amount = %q, want 0.01", summary.BlakeAmount)
	}

	if summary.BTCSats != "25000" {
		t.Fatalf("BTC sats = %q, want 25000", summary.BTCSats)
	}

	encoded, err := json.Marshal(summary)

	if err != nil {
		t.Fatal(err)
	}

	// A JSON number would be a float in the browser. The quoted form is the proof that
	// nothing here is one.
	if !strings.Contains(string(encoded), `"blake_amount":"0.01"`) {
		t.Fatalf("the BLAKE amount is not a JSON string: %s", encoded)
	}

	if !strings.Contains(string(encoded), `"btc_sats":"25000"`) {
		t.Fatalf("the satoshi amount is not a JSON string: %s", encoded)
	}

	// The secret itself must never be in a view: the hashlock is public, the preimage
	// is not.
	secret := strings.Repeat("f", 64)
	record().SecretHex = &secret

	withSecret := view.Summary(record(), string(reconcile.Alice), nil)

	withSecret.SecretHash = ""

	encoded, err = json.Marshal(withSecret)

	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(encoded), secret) {
		t.Fatal("a view carried the swap secret")
	}
}

// TestRoleFollowsThePins is the fix this client is built on: the role is a fact about
// who signed the offer, not a setting.
func TestRoleFollowsThePins(t *testing.T) {
	alice, bob := strings.Repeat("a", 64), strings.Repeat("b", 64)

	cases := []struct {
		name string
		me   string
		want string
	}{
		{"alice", alice, "alice"},
		{"bob", bob, "bob"},
		{"nobody", strings.Repeat("9", 64), "alice"},
		{"no identity", "", "alice"},
	}

	for _, test := range cases {
		if got := view.RoleOf(record(), test.me); got != test.want {
			t.Errorf("%s: role = %q, want %q", test.name, got, test.want)
		}
	}

	// The counterparty follows the role: Alice's counterparty is Bob and Bob's is
	// Alice, because a view that named the wrong one would be read as a different
	// trade.
	if got := view.Summary(record(), "alice", nil).Counterparty; got != bob {
		t.Errorf("Alice's counterparty = %q, want Bob", got)
	}

	if got := view.Summary(record(), "bob", nil).Counterparty; got != alice {
		t.Errorf("Bob's counterparty = %q, want Alice", got)
	}
}

// TestTimelineOnlyShowsTheBranchTaken: a swap that is funding normally must not show the
// refund steps as pending work.
func TestTimelineOnlyShowsTheBranchTaken(t *testing.T) {
	steps := view.Timeline(state.BTCFunded)

	byState := map[string]proto.TimelineStep{}

	for _, step := range steps {
		byState[step.State] = step
	}

	for _, want := range []string{"OFFERED", "BLAKE_FUNDED", "BTC_FUNDED"} {
		if !byState[want].Reached {
			t.Errorf("%s is not marked reached", want)
		}
	}

	if !byState["BTC_FUNDED"].Current {
		t.Error("the current checkpoint is not marked current")
	}

	for _, want := range []string{"SECRET_REVEALED", "BTC_CLAIMED", "BLAKE_CLAIMED", "COMPLETE"} {
		if byState[want].Reached {
			t.Errorf("%s is marked reached before it happened", want)
		}
	}

	for _, absent := range []string{"BTC_REFUNDED", "BLAKE_REFUNDED"} {
		if _, present := byState[absent]; present {
			t.Errorf("the untaken refund branch shows %s", absent)
		}
	}

	// The refund branch, when it is the one taken, is shown and marked.
	refund := map[string]proto.TimelineStep{}

	for _, step := range view.Timeline(state.BlakeRefunded) {
		refund[step.State] = step
	}

	for _, want := range []string{"OFFERED", "BLAKE_FUNDED", "BTC_REFUNDED", "BLAKE_REFUNDED"} {
		if !refund[want].Reached {
			t.Errorf("refund path: %s is not marked reached", want)
		}
	}

	if !refund["BLAKE_REFUNDED"].Current {
		t.Error("refund path: the current checkpoint is not marked current")
	}

	if !refund["BTC_REFUNDED"].Refund {
		t.Error("refund path: the refund branch is not marked")
	}
}

// TestSummaryCarriesTheReport: the next action comes from reconciliation and is empty
// when there is no report, because the one field that must never be invented is the one
// that tells an operator what to do.
func TestSummaryCarriesTheReport(t *testing.T) {
	plain := view.Summary(record(), "alice", nil)

	if plain.NextAction != "" || plain.DecisionClass != "" {
		t.Fatalf("a summary with no report claimed an action: %+v", plain)
	}

	report := &reconcile.Report{
		SwapID:          "0123456789ab",
		ProtocolRole:    reconcile.Alice,
		CheckpointState: state.BTCFunded,
		DecisionClass:   reconcile.ClassActionable,
		NextAction:      reconcile.ActionClaimBTC,
		Reason:          "Bitcoin is funded and the timelock has not passed",
		Heights:         reconcile.Heights{BTC: 120, Blake: 900},
		Blake:           reconcile.Observation{Status: reconcile.Funded},
		BTC:             reconcile.Observation{Status: reconcile.Funded, TxID: "abc"},
	}

	summary := view.Summary(record(), "alice", report)

	if summary.NextAction != string(reconcile.ActionClaimBTC) {
		t.Fatalf("next action = %q", summary.NextAction)
	}

	if summary.Chains.BTC.Status != "funded" || summary.Chains.BTC.TxID != "abc" {
		t.Fatalf("BTC observation = %+v", summary.Chains.BTC)
	}

	if summary.Chains.BTC.Height != 120 || summary.Chains.Blake.Height != 900 {
		t.Fatalf("heights = %+v", summary.Chains)
	}

	if summary.Funding.BlakeTxID == "" || summary.Funding.BTCVout != 0 {
		t.Fatalf("funding = %+v", summary.Funding)
	}

	// An unconfirmed settlement has no block hash and says so.
	if summary.Settlement.BTCClaim.Confirmed {
		t.Fatal("an unset settlement is reported as confirmed")
	}
}

// TestHoursAreExact: the rendered duration is integer arithmetic, so it does not depend
// on how a platform rounds a binary fraction.
func TestHoursAreExact(t *testing.T) {
	cases := []struct {
		seconds int64
		want    string
	}{
		{0, "0.00"},
		{3600, "1.00"},
		{5400, "1.50"},
		{12600, "3.50"},
		{15000, "4.17"},
		{-3600, "-1.00"},
	}

	for _, test := range cases {
		result := &swap.AcceptResult{Timeout: &swap.TimeoutCheck{BTCRemainingSeconds: test.seconds}}

		action := view.Accept(view.Action("engine.accept-offer", "", ""), result)

		if len(action.Details) == 0 || action.Details[0].Value != test.want {
			t.Errorf("%d seconds = %+v, want %s", test.seconds, action.Details, test.want)
		}
	}
}

// TestSettlementViewReportsRecovery: whether a settlement was adopted from the record or
// signed now is the difference between "we did this" and "we found this", and the page
// shows it.
func TestSettlementViewReportsRecovery(t *testing.T) {
	recovered := view.Settlement(view.Action("engine.claim-btc", "0123456789ab", "claim-btc"),
		&swap.SettlementResult{
			Chain: "btc", Branch: "claim", TxID: strings.Repeat("1", 64),
			BlockHash: strings.Repeat("2", 64), Recovered: true, State: state.BTCClaimed,
			FeeSats: 100, Vsize: 200, FeeRate: decimal.MustParse("1.5"),
		})

	if !recovered.Recovered || !recovered.Confirmed || recovered.Pending {
		t.Fatalf("recovered settlement = %+v", recovered)
	}

	if recovered.FeeSats != "100" || recovered.FeeRate != "1.5" {
		t.Fatalf("fee = %q sats at %q", recovered.FeeSats, recovered.FeeRate)
	}

	pending := view.Settlement(view.Action("engine.refund-btc", "0123456789ab", "refund-btc"),
		&swap.SettlementResult{Chain: "btc", Branch: "refund", TxID: "ab", Pending: true})

	if !pending.Pending || pending.Confirmed {
		t.Fatalf("pending settlement = %+v", pending)
	}
}

// TestMarketViewRendersEvidence: the market view reports what the relays said, so an
// empty market and a market whose relays did not answer do not look the same.
func TestMarketViewRendersEvidence(t *testing.T) {
	maker := strings.Repeat("a", 64)

	terms := amount.Blake(10_000_000)

	offer := marketflow.ListedOffer{
		Offer: market.Offer{
			Version: 2, BlakeAmount: terms, BTCSats: 50_000,
			Direction: market.BlakeForBTC, ExpiresAt: 2_000_000_000,
		},
		EventID: strings.Repeat("3", 64),
		Maker:   maker,
	}

	market := view.Market(
		"4 unique event(s) from 1/1 relay(s)",
		[]string{"ws://127.0.0.1:1"},
		[]marketflow.Entry{{Offer: offer, State: marketflow.StateOpen}},
		nil,
		nil,
		nil,
		nil,
		maker,
	)

	if market.Read == "" || len(market.Relays) != 1 {
		t.Fatalf("market = %+v", market)
	}

	if len(market.Open) != 1 || !market.Open[0].Mine {
		t.Fatalf("open offers = %+v", market.Open)
	}

	if market.Open[0].BlakeAmount != "0.1" || market.Open[0].BTCSats != "50000" {
		t.Fatalf("amounts = %+v", market.Open[0])
	}

	// The error field is how a relay failure is shown instead of an empty market.
	failed := view.Market("", nil, nil, nil, nil, nil, nil, maker)
	failed.Err = "relay refused"

	if failed.Err == "" || len(failed.Open) != 0 {
		t.Fatal("a failed market read lost its error")
	}
}
