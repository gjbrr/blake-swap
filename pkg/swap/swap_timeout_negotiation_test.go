package swap

import (
	"context"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/coord"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/store"
	"github.com/gjbrr/blake-swap/pkg/timeout"
)

// This file mirrors the reference's `tests/swap_timeout_negotiation_tests.py`: Bob's
// independent check of the deadlines he is offered.
//
// The property the suite exists for is not "the numbers are checked" — it is that a refusal
// happens *before* anything durable exists. A Bob who rejects an unsafe offer but leaves a
// swap record or an acceptance behind has created the very state the refusal was meant to
// prevent, and a retry then finds a half-handshake nobody agreed to.

// negotiationEngine is a participant with two chains and a coordination route.
func negotiationEngine(t *testing.T) (*Engine, *recordingTransport) {
	t.Helper()

	identity, err := nostr.NewKey(strings.Repeat("77", 32))

	if err != nil {
		t.Fatal(err)
	}

	transport := newRecordingTransport()

	engine := NewEngine(store.NewMemory(), nil, newChainNode(1000), newChainNode(1000))
	engine.Identity = identity
	engine.Transport = transport
	engine.Mode = "off"
	engine.BlakeWallet = "bob"
	engine.BTCWallet = "bob"
	engine.BlakeWatchWallet = "bob-watch"
	engine.BTCWatchWallet = "bob-watch"

	return engine, transport
}

// offeredBody is a v1.5 offer Bob would accept, at the heights the engine reports.
func offeredBody(t *testing.T) (map[string]any, timeout.Plan) {
	t.Helper()

	plan, err := timeout.BuildPlan(1000, 1000, timeout.DefaultPolicy())

	if err != nil {
		t.Fatal(err)
	}

	return map[string]any{
		"type":            "offer",
		"swap_id":         "0123456789ab",
		"blake_amount":    "0.001",
		"btc_sats":        int64(25000),
		"secret_hash":     strings.Repeat("33", 32),
		"created_at":      int64(1700000000),
		"blake_timeout":   plan.BlakeTimeoutHeight,
		"btc_timeout":     plan.BTCTimeoutHeight,
		"blake_alice_pub": "02" + strings.Repeat("44", 32),
		"btc_alice_pub":   "03" + strings.Repeat("55", 32),

		"timeout_policy_version":       "1.5",
		"btc_timeout_window_seconds":   plan.BTCWindowSeconds,
		"blake_timeout_window_seconds": plan.BlakeWindowSeconds,
		"bob_recovery_seconds":         plan.BobRecoverySeconds,
	}, plan
}

// deliverOne puts one offer on the transport from the named sender.
func deliverOne(transport *recordingTransport, body map[string]any, sender string) {
	transport.incoming["offer"] = &coord.Message{
		Kind:   "offer",
		Sender: sender,
		Body:   body,
	}
}

// acceptAsBob offers one body to Bob and returns what happened.
func acceptAsBob(t *testing.T, engine *Engine, transport *recordingTransport, sender string, body map[string]any) error {
	t.Helper()

	deliverOne(transport, body, sender)

	_, err := engine.AcceptOffer(context.Background(), AcceptRequest{
		SwapID: "0123456789ab", ExpectedAlicePub: sender,
	})

	return err
}

// TestSwapTimeoutNegotiationAcceptsASafeOffer: the baseline every refusal below is a mutation
// of.
func TestSwapTimeoutNegotiationAcceptsASafeOffer(t *testing.T) {
	engine, transport := negotiationEngine(t)

	body, _ := offeredBody(t)

	maker := makeIdentity(t)

	if err := acceptAsBob(t, engine, transport, maker, body); err != nil {
		t.Fatalf("a safe offer was refused: %v", err)
	}
}

// TestSwapTimeoutNegotiationRefusesUnsafeOffers: every way an offer can be unsafe, and the
// requirement that each refusal leaves nothing behind.
func TestSwapTimeoutNegotiationRefusesUnsafeOffers(t *testing.T) {
	type mutation struct {
		mutate func(body map[string]any, plan timeout.Plan)
		want   string
	}

	cases := map[string]mutation{
		"unknown policy": {func(body map[string]any, _ timeout.Plan) {
			body["timeout_policy_version"] = "9.9"
		}, "unsupported timeout policy version"},
		"versionless": {func(body map[string]any, _ timeout.Plan) {
			delete(body, "timeout_policy_version")
		}, "timeout policy version is required"},
		"missing recovery": {func(body map[string]any, _ timeout.Plan) {
			delete(body, "bob_recovery_seconds")
		}, "missing bob_recovery_seconds"},
		"expired Bitcoin": {func(body map[string]any, _ timeout.Plan) {
			body["btc_timeout"] = int64(1000)
		}, "Bitcoin timeout is already expired"},
		"expired BLAKE": {func(body map[string]any, _ timeout.Plan) {
			body["blake_timeout"] = int64(1000)
		}, "BLAKE timeout is already expired"},
		"short Bitcoin window": {func(body map[string]any, _ timeout.Plan) {
			// One block above the current height: a future deadline with no window.
			body["btc_timeout"] = int64(1001)
		}, "BTC safety window"},
		"short recovery gap": {func(body map[string]any, plan timeout.Plan) {
			// Both deadlines in the future, too close together for Bob to recover
			// between them.
			body["btc_timeout"] = plan.BlakeTimeoutHeight - 1
		}, "cross-chain recovery window"},
		"recovery gap one block short": {func(body map[string]any, plan timeout.Plan) {
			// The interesting case: everything is in the future and the Bitcoin window
			// is generous, and the recovery gap is exactly one block short of what Bob
			// requires. A check that only looked at "is it vaguely close" would pass
			// this, which is why the boundary is the test.
			blocks := plan.BobRecoverySeconds/600 - 1

			body["blake_timeout"] = body["btc_timeout"].(int64) + blocks
		}, "cross-chain recovery window"},
	}

	for name, test := range cases {
		engine, transport := negotiationEngine(t)

		body, plan := offeredBody(t)
		maker := makeIdentity(t)

		test.mutate(body, plan)

		err := acceptAsBob(t, engine, transport, maker, body)

		if err == nil {
			t.Errorf("%s: was accepted", name)

			continue
		}

		if !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: refusal = %v, want it to mention %q", name, err, test.want)
		}

		// Nothing durable, and nothing announced: a refused offer must leave no trace.
		if _, loadErr := engine.Store.Load(context.Background(), "0123456789ab"); loadErr == nil {
			t.Errorf("%s: a record was created for a refused offer", name)
		}

		if len(transport.sent) != 0 {
			t.Errorf("%s: an acceptance was sent for a refused offer", name)
		}
	}
}

// TestSwapTimeoutNegotiationRefusesAnUnpinnedAuthor: the offer must be signed by the Alice the
// caller expects, because a timeout check on somebody else's offer is a check on nothing.
func TestSwapTimeoutNegotiationRefusesAnUnpinnedAuthor(t *testing.T) {
	engine, transport := negotiationEngine(t)

	body, _ := offeredBody(t)

	maker := makeIdentity(t)
	impostor := strings.Repeat("99", 64)

	err := acceptAsBob(t, engine, transport, maker, body)

	if err != nil {
		t.Fatalf("the safe offer was refused: %v", err)
	}

	// The same body, delivered by somebody else, is not the same offer.
	engine, transport = negotiationEngine(t)

	deliverOne(transport, body, impostor)

	if _, err := engine.AcceptOffer(context.Background(), AcceptRequest{
		SwapID: "0123456789ab", ExpectedAlicePub: maker,
	}); err == nil {
		t.Fatal("an offer from an unexpected author was accepted")
	}
}

// makeIdentity is one maker identity, fixed for the test.
func makeIdentity(t *testing.T) string {
	t.Helper()

	key, err := nostr.NewKey(strings.Repeat("11", 32))

	if err != nil {
		t.Fatal(err)
	}

	return key.Public
}
