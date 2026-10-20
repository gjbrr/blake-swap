package marketops

import (
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/model"
)

// This test moved with the code it tests: the resume guard lives here now, so its assertion
// does too. It was not loosened.

// reverseOfferSwap is a partially imported reverse negotiation, before Bob's identity is
// pinned.
func reverseOfferSwap() *model.Swap {
	alice, bob := strings.Repeat("44", 32), strings.Repeat("33", 32)
	btcWindow, blakeWindow, recovery := int64(64_800), int64(79_200), int64(14_400)

	return &model.Swap{
		SwapID:                    "aaaa00000001",
		State:                     "OFFERED",
		SecretHash:                strings.Repeat("00", 32),
		BlakeAmountBaseUnits:      12_500_000_000,
		BTCSats:                   150_000,
		BTCTimeout:                900,
		BlakeTimeout:              800,
		BTCTimeoutWindowSeconds:   &btcWindow,
		BlakeTimeoutWindowSeconds: &blakeWindow,
		BobRecoverySeconds:        &recovery,
		NostrAlicePub:             &alice,
		NostrBobPub:               &bob,
	}
}

// TestReverseResumableRefusesEveryUnsafeResume: a partial handshake may only be finished
// while nothing has moved, and only with keys Bob already created.
func TestReverseResumableRefusesEveryUnsafeResume(t *testing.T) {
	bobKey := strings.Repeat("77", 32)
	txid := strings.Repeat("88", 32)

	fixture := func() *model.Swap {
		record := reverseOfferSwap()
		record.BlakeBobPub = &bobKey
		record.BTCBobPub = &bobKey
		record.NostrBobPub = nil

		return record
	}

	// The resumable shape is accepted, so the test is not asserting refusal only.
	if err := reverseResumable(fixture()); err != nil {
		t.Fatalf("a resumable record was refused: %v", err)
	}

	cases := map[string]struct {
		mutate func(*model.Swap)
		want   string
	}{
		"not offered": {func(s *model.Swap) { s.State = "BLAKE_FUNDED" }, "not OFFERED"},
		"has secret": {func(s *model.Swap) {
			secret := strings.Repeat("66", 32)
			s.SecretHex = &secret
		}, "unexpectedly possesses secret S"},
		"funded":  {func(s *model.Swap) { s.BTCFundingTxID = &txid }, "after settlement activity"},
		"claimed": {func(s *model.Swap) { s.BlakeClaimTxID = &txid }, "after settlement activity"},
		"no keys": {func(s *model.Swap) { s.BlakeBobPub = nil }, "does not contain Bob settlement keys"},
	}

	for name, test := range cases {
		record := fixture()
		test.mutate(record)

		err := reverseResumable(record)

		if err == nil {
			t.Errorf("%s: was resumable", name)

			continue
		}

		if !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: refusal = %v, want it to mention %q", name, err, test.want)
		}
	}
}
