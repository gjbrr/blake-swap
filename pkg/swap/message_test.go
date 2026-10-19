package swap

import (
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/wire"
)

// messageRecord is a record with every fact the seven messages need.
func messageRecord() *model.Swap {
	blakePub := "02" + strings.Repeat("ab", 32)
	btcPub := "03" + strings.Repeat("cd", 32)
	txid := strings.Repeat("11", 32)
	blockHash := strings.Repeat("22", 32)
	version := "1.5"

	sats := int64(50_000)
	vout := int32(1)

	btcWindow := int64(86_400)
	blakeWindow := int64(90_000)
	recovery := int64(3_600)

	return &model.Swap{
		SwapID:                    "0123456789ab",
		BlakeAmountBaseUnits:      int64(amount.Blake(1_000_000_000)),
		BTCSats:                   sats,
		SecretHash:                strings.Repeat("33", 32),
		BlakeTimeout:              1_200,
		BTCTimeout:                1_000,
		CreatedAt:                 1_700_000_000,
		TimeoutPolicyVersion:      &version,
		BTCTimeoutWindowSeconds:   &btcWindow,
		BlakeTimeoutWindowSeconds: &blakeWindow,
		BobRecoverySeconds:        &recovery,
		BlakeAlicePub:             &blakePub,
		BTCAlicePub:               &btcPub,
		BlakeBobPub:               &blakePub,
		BTCBobPub:                 &btcPub,
		BlakeFundingTxID:          &txid,
		BlakeFundingVout:          &vout,
		BTCFundingTxID:            &txid,
		BTCFundingVout:            &vout,
		BTCCLaimTxID:              &txid,
		BTCCLaimBlockHash:         &blockHash,
		BlakeClaimTxID:            &txid,
		BlakeClaimBlockHash:       &blockHash,
		BTCRefundTxID:             &txid,
		BTCRefundBlockHash:        &blockHash,
	}
}

// TestMessageBodySatisfiesTheWireSchema: the builder both clients use produces messages
// the canonical schema accepts, for all seven types.
//
// The schema is the authority, so this is the check that keeps the builder honest; a
// field renamed here and not there fails on the type whose body changed, not on a
// downstream relay.
func TestMessageBodySatisfiesTheWireSchema(t *testing.T) {
	record := messageRecord()

	for _, kind := range []string{
		"offer", "accept", "blake-funded", "btc-funded",
		"btc-claimed", "blake-claimed", "btc-refunded",
	} {
		body, err := MessageBody(record, kind)

		if err != nil {
			t.Errorf("%s: %v", kind, err)

			continue
		}

		if body["type"] != kind || body["swap_id"] != record.SwapID {
			t.Errorf("%s: body = %v", kind, body)
		}

		if _, present := body["secret_hex"]; present {
			t.Errorf("%s: the body carries secret_hex", kind)
		}

		expected := wire.Kind(kind)

		if _, err := wire.Validate(body, &expected); err != nil {
			t.Errorf("%s: the wire schema refused the built body: %v", kind, err)
		}
	}
}

// TestMessageBodyRefusesAFactTheRecordDoesNotHave: a message about a contract the
// record does not hold is refused by name rather than filled with a placeholder.
func TestMessageBodyRefusesAFactTheRecordDoesNotHave(t *testing.T) {
	record := messageRecord()
	record.BlakeFundingTxID = nil

	if _, err := MessageBody(record, "blake-funded"); err == nil ||
		!strings.Contains(err.Error(), "funding txid") {
		t.Fatalf("err = %v", err)
	}

	record = messageRecord()
	record.BTCCLaimBlockHash = nil

	if _, err := MessageBody(record, "btc-claimed"); err == nil ||
		!strings.Contains(err.Error(), "blockhash") {
		t.Fatalf("err = %v", err)
	}

	record = messageRecord()
	record.BlakeAlicePub = nil

	if _, err := MessageBody(record, "offer"); err == nil ||
		!strings.Contains(err.Error(), "contract key") {
		t.Fatalf("err = %v", err)
	}

	if _, err := MessageBody(record, "not-a-type"); err == nil {
		t.Fatal("an unknown message type produced a body")
	}
}

// TestSenderAndReceiverRoles: each type belongs to one role, and the other role
// receives it.
func TestSenderAndReceiverRoles(t *testing.T) {
	want := map[string][2]string{
		"offer":         {"alice", "bob"},
		"accept":        {"bob", "alice"},
		"blake-funded":  {"alice", "bob"},
		"btc-funded":    {"bob", "alice"},
		"btc-claimed":   {"alice", "bob"},
		"blake-claimed": {"bob", "alice"},
		"btc-refunded":  {"bob", "alice"},
	}

	for kind, roles := range want {
		sender, err := SenderRole(kind)

		if err != nil {
			t.Errorf("%s: %v", kind, err)

			continue
		}

		receiver, err := ReceiverRole(kind)

		if err != nil {
			t.Errorf("%s: %v", kind, err)

			continue
		}

		if sender != roles[0] || receiver != roles[1] {
			t.Errorf("%s: signed by %q received by %q, want %v", kind, sender, receiver, roles)
		}
	}

	if _, err := SenderRole("btc-melted"); err == nil {
		t.Fatal("an unknown message type had a sender role")
	}
}

// TestCheckReceivedRefusesTheSecretAndForeignSwaps: the local half every receive runs.
func TestCheckReceivedRefusesTheSecretAndForeignSwaps(t *testing.T) {
	record := messageRecord()

	body, err := MessageBody(record, "accept")

	if err != nil {
		t.Fatal(err)
	}

	if err := CheckReceived(record, "accept", body); err != nil {
		t.Fatalf("a valid accept was refused: %v", err)
	}

	if err := CheckReceived(record, "blake-funded", body); err == nil {
		t.Fatal("a message of the wrong type was accepted")
	}

	other := messageRecord()
	other.SwapID = "baddcafe1234"

	if err := CheckReceived(other, "accept", body); err == nil {
		t.Fatal("a message for another swap was accepted")
	}

	tainted := map[string]any{}

	for key, value := range body {
		tainted[key] = value
	}

	tainted["secret_hex"] = strings.Repeat("44", 32)

	if err := CheckReceived(record, "accept", tainted); err == nil ||
		!strings.Contains(err.Error(), "SECURITY FAILURE") {
		t.Fatalf("a message carrying the secret was accepted: %v", err)
	}
}
