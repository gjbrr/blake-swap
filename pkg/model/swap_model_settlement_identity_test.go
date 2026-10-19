package model

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// This file mirrors the reference's `tests/swap_model_settlement_identity_tests.py`: what a
// record has to carry before it may claim a settlement happened.
//
// A settlement transaction is the only durable evidence that money moved, so a record that
// names signed hex without the identity it belongs to is not "slightly incomplete" — it is a
// record that cannot tell its own transaction apart from a reorged one.

// settlementSwap is a funded record with the v1.5 metadata a validated swap carries.
func settlementSwap(t *testing.T) *Swap {
	t.Helper()

	btcWindow, blakeWindow, recovery := int64(12_600), int64(27_600), int64(14_700)

	return &Swap{
		SwapID:                    "0123456789ab",
		State:                     "BTC_FUNDED",
		CreatedAt:                 1,
		BlakeAmountBaseUnits:      10_000,
		BTCSats:                   2730,
		SecretHash:                strings.Repeat("11", 32),
		BlakeTimeout:              100,
		BTCTimeout:                200,
		TimeoutPolicyVersion:      strPtr("1.5"),
		BTCTimeoutWindowSeconds:   &btcWindow,
		BlakeTimeoutWindowSeconds: &blakeWindow,
		BobRecoverySeconds:        &recovery,
	}
}

// TestSwapModelSettlementIdentityAcceptsALegacyRecord: a record from before signed hex was
// stored is still a valid record — the absence of evidence is not a false claim.
func TestSwapModelSettlementIdentityAcceptsALegacyRecord(t *testing.T) {
	if err := settlementSwap(t).Validate(); err != nil {
		t.Fatalf("a record without signed hex was refused: %v", err)
	}
}

// TestSwapModelSettlementIdentityRequiresAnAnchor: signed hex without the transaction id it
// belongs to cannot be checked against the chain, so it is refused by name.
func TestSwapModelSettlementIdentityRequiresAnAnchor(t *testing.T) {
	signed := strPtr("00")

	for name, field := range map[string]func(*Swap){
		"btc_claim_hex":  func(s *Swap) { s.BTCCLaimHex = signed },
		"btc_refund_hex": func(s *Swap) { s.BTCRefundHex = signed },
	} {
		record := settlementSwap(t)
		field(record)

		err := record.Validate()

		if err == nil {
			t.Errorf("%s without an anchor was accepted", name)

			continue
		}

		if !strings.Contains(err.Error(), name+" requires") {
			t.Errorf("%s: refusal = %v", name, err)
		}
	}
}

// TestSwapModelSettlementIdentityAcceptsATXIDAnchor: with the txid present the signed hex is
// checkable, and the record is valid.
func TestSwapModelSettlementIdentityAcceptsATXIDAnchor(t *testing.T) {
	record := settlementSwap(t)
	record.BlakeClaimTxID = strPtr(strings.Repeat("22", 32))
	record.BlakeClaimHex = strPtr("00")

	if err := record.Validate(); err != nil {
		t.Fatalf("a signed BLAKE claim with a txid anchor was refused: %v", err)
	}
}

// TestSwapModelSettlementIdentityAcceptsAReorgHint: after a rollback the canonical txid is
// gone, and the reorg hint is what still identifies the transaction.
func TestSwapModelSettlementIdentityAcceptsAReorgHint(t *testing.T) {
	claim := settlementSwap(t)
	claim.State = "BTC_CLAIMED"
	claim.BlakeReorgClaimTxID = strPtr(strings.Repeat("22", 32))
	claim.BlakeClaimHex = strPtr("00")

	if err := claim.Validate(); err != nil {
		t.Fatalf("a BLAKE claim with only its reorg hint was refused: %v", err)
	}

	refund := settlementSwap(t)
	refund.State = "BTC_REFUNDED"
	refund.BlakeReorgRefundTxID = strPtr(strings.Repeat("33", 32))
	refund.BlakeRefundHex = strPtr("00")

	if err := refund.Validate(); err != nil {
		t.Fatalf("a BLAKE refund with only its reorg hint was refused: %v", err)
	}
}

// TestSwapModelSettlementIdentityRefusesMalformedEvidence: every identity field is hex of the
// right length, and the signed transaction is hex at all.
func TestSwapModelSettlementIdentityRefusesMalformedEvidence(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Swap)
		want   string
	}{
		"signed hex is not hex": {func(s *Swap) {
			s.BTCCLaimTxID = strPtr(strings.Repeat("44", 32))
			s.BTCCLaimHex = strPtr("not-hex")
		}, "btc_claim_hex must be valid hex"},
		"settlement txid is malformed": {func(s *Swap) {
			s.BTCCLaimTxID = strPtr("banana")
		}, "btc_claim_txid must be 32-byte hex"},
		"funding txid is malformed": {func(s *Swap) {
			s.BTCFundingTxID = strPtr("1234")
			vout := int32(0)
			s.BTCFundingVout = &vout
		}, "btc_funding_txid must be 32-byte hex"},
		"settlement blockhash is malformed": {func(s *Swap) {
			s.BTCCLaimBlockHash = strPtr("banana")
		}, "btc_claim_blockhash must be 32-byte hex"},
	}

	for name, test := range cases {
		record := settlementSwap(t)
		test.mutate(record)

		err := record.Validate()

		if err == nil {
			t.Errorf("%s: was accepted", name)

			continue
		}

		if !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: refusal = %v, want it to mention %q", name, err, test.want)
		}
	}
}

// TestSwapModelSettlementIdentityChecksTheRevealedSecret: a revealed secret is only evidence
// when it hashes to the swap's own hashlock, and a short one is not a secret at all.
func TestSwapModelSettlementIdentityChecksTheRevealedSecret(t *testing.T) {
	secret := strings.Repeat("77", 32)
	raw, err := hex.DecodeString(secret)

	if err != nil {
		t.Fatal(err)
	}

	digest := sha256.Sum256(raw)

	record := settlementSwap(t)
	record.SecretHash = hex.EncodeToString(digest[:])
	record.RevealedSecretHex = &secret

	if err := record.Validate(); err != nil {
		t.Fatalf("a matching revealed secret was refused: %v", err)
	}

	mismatched := *record
	other := strings.Repeat("88", 32)
	mismatched.RevealedSecretHex = &other

	err = mismatched.Validate()

	if err == nil || !strings.Contains(err.Error(), "revealed_secret_hex does not match secret_hash") {
		t.Fatalf("a mismatched revealed secret was accepted: %v", err)
	}

	short := *record
	short.RevealedSecretHex = strPtr("11")

	err = short.Validate()

	if err == nil || !strings.Contains(err.Error(), "revealed_secret_hex must be 32 bytes") {
		t.Fatalf("a short revealed secret was accepted: %v", err)
	}
}
