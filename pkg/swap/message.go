package swap

import (
	"fmt"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/model"
)

//
// One place builds the wire body of every settlement message, and one place says
// which protocol role signs and receives each of them.
//
// It is deliberately exported: the browser worker has to publish the same seven
// messages the CLI does, and a second copy of these field maps would be a second
// wire format wearing the first one's name. The engine's own announce and accept
// commands call these too, so there is exactly one definition of "what an
// `accept` looks like".
//

// senderRole is the protocol role a message type must be signed by.
//
// The roles follow the protocol, not the chain: Alice makes the offer and
// reveals the secret on Bitcoin; Bob accepts, funds Bitcoin, claims BLAKE and
// refunds Bitcoin. A message signed by the other role is refused before it is
// signed, because a settlement announcement with the wrong author is not a
// typo — it is a claim to a role the signer does not hold.
var senderRole = map[string]string{
	"offer":         "alice",
	"accept":        "bob",
	"blake-funded":  "alice",
	"btc-funded":    "bob",
	"btc-claimed":   "alice",
	"blake-claimed": "bob",
	"btc-refunded":  "bob",
}

// SenderRole returns the protocol role that signs one message type.
func SenderRole(kind string) (string, error) {
	role, known := senderRole[kind]

	if !known {
		return "", fmt.Errorf("unknown settlement message type: %q", kind)
	}

	return role, nil
}

// ReceiverRole returns the protocol role that receives one message type.
//
// It is the other participant, and it is derived rather than listed so the two
// tables cannot disagree: whichever role does not sign a message receives it.
func ReceiverRole(kind string) (string, error) {
	sender, err := SenderRole(kind)

	if err != nil {
		return "", err
	}

	if sender == "alice" {
		return "bob", nil
	}

	return "alice", nil
}

// MessageBody builds the wire body of one settlement message from a swap record.
//
// Nothing comes from the caller but the record and the type, which is the point:
// a body assembled anywhere else could describe a swap the record does not hold.
// A message whose facts the record does not have is refused rather than filled
// with a placeholder, and engine-internal fields (and the secret) are never part
// of it.
func MessageBody(swap *model.Swap, kind string) (map[string]any, error) {
	if swap == nil {
		return nil, fmt.Errorf("no swap record to build a %q message from", kind)
	}

	if _, err := SenderRole(kind); err != nil {
		return nil, err
	}

	body := map[string]any{
		"type":        kind,
		"swap_id":     swap.SwapID,
		"secret_hash": swap.SecretHash,
	}

	switch kind {
	case "offer":
		blakeAlice, err := requiredKey(swap.BlakeAlicePub, "blake_alice_pub", "BLAKE contract key")

		if err != nil {
			return nil, err
		}

		btcAlice, err := requiredKey(swap.BTCAlicePub, "btc_alice_pub", "Bitcoin contract key")

		if err != nil {
			return nil, err
		}

		body["blake_amount"] = amount.Blake(swap.BlakeAmountBaseUnits).Format()
		body["btc_sats"] = swap.BTCSats
		body["btc_timeout"] = swap.BTCTimeout
		body["blake_timeout"] = swap.BlakeTimeout
		body["created_at"] = swap.CreatedAt
		body["blake_alice_pub"] = blakeAlice
		body["btc_alice_pub"] = btcAlice

		//
		// The v1.5 metadata is advertised exactly when the record has it: a peer
		// refuses an offer without it, and inventing windows for a record that
		// never had a plan would advertise a deadline nobody computed.
		//
		if swap.TimeoutPolicyVersion != nil && *swap.TimeoutPolicyVersion == "1.5" {
			body["timeout_policy_version"] = "1.5"

			if swap.BTCTimeoutWindowSeconds != nil {
				body["btc_timeout_window_seconds"] = *swap.BTCTimeoutWindowSeconds
			}

			if swap.BlakeTimeoutWindowSeconds != nil {
				body["blake_timeout_window_seconds"] = *swap.BlakeTimeoutWindowSeconds
			}

			if swap.BobRecoverySeconds != nil {
				body["bob_recovery_seconds"] = *swap.BobRecoverySeconds
			}
		}

	case "accept":
		blakeBob, err := requiredKey(swap.BlakeBobPub, "blake_bob_pub", "BLAKE contract key")

		if err != nil {
			return nil, err
		}

		btcBob, err := requiredKey(swap.BTCBobPub, "btc_bob_pub", "Bitcoin contract key")

		if err != nil {
			return nil, err
		}

		body["blake_bob_pub"] = blakeBob
		body["btc_bob_pub"] = btcBob

	case "blake-funded":
		txid, vout, err := requiredFunding(swap, true)

		if err != nil {
			return nil, err
		}

		body["blake_amount"] = amount.Blake(swap.BlakeAmountBaseUnits).Format()
		body["blake_timeout"] = swap.BlakeTimeout
		body["blake_funding_txid"] = txid
		body["blake_funding_vout"] = vout

	case "btc-funded":
		txid, vout, err := requiredFunding(swap, false)

		if err != nil {
			return nil, err
		}

		body["btc_sats"] = swap.BTCSats
		body["btc_timeout"] = swap.BTCTimeout
		body["btc_funding_txid"] = txid
		body["btc_funding_vout"] = vout

	case "btc-claimed":
		txid, blockHash, err := requiredSettlement(
			swap.BTCCLaimTxID, swap.BTCCLaimBlockHash, "btc_claim_txid", "btc_claim_blockhash",
			"Bitcoin claim",
		)

		if err != nil {
			return nil, err
		}

		body["btc_claim_txid"] = txid
		body["btc_claim_blockhash"] = blockHash

	case "blake-claimed":
		txid, blockHash, err := requiredSettlement(
			swap.BlakeClaimTxID, swap.BlakeClaimBlockHash, "blake_claim_txid",
			"blake_claim_blockhash", "BLAKE claim",
		)

		if err != nil {
			return nil, err
		}

		body["blake_claim_txid"] = txid
		body["blake_claim_blockhash"] = blockHash

	case "btc-refunded":
		txid, blockHash, err := requiredSettlement(
			swap.BTCRefundTxID, swap.BTCRefundBlockHash, "btc_refund_txid",
			"btc_refund_blockhash", "Bitcoin refund",
		)

		if err != nil {
			return nil, err
		}

		body["btc_refund_txid"] = txid
		body["btc_refund_blockhash"] = blockHash
	}

	return body, nil
}

// CheckReceived is the record-consistency check every received message gets.
//
// The transport has already proved the author, the encryption and the schema; this
// is the other half — that the message is about *this* swap and this contract, and
// that it does not carry the one field the coordination layer must never see.
func CheckReceived(swap *model.Swap, kind string, body map[string]any) error {
	if swap == nil {
		return fmt.Errorf("no swap record to check a %q message against", kind)
	}

	if body == nil {
		return fmt.Errorf("a %s message must have a body", kind)
	}

	if body["type"] != kind {
		return fmt.Errorf("message is not %s", kind)
	}

	if body["swap_id"] != swap.SwapID {
		return fmt.Errorf("swap_id mismatch")
	}

	if body["secret_hash"] != swap.SecretHash {
		return fmt.Errorf("secret_hash mismatch")
	}

	if _, present := body["secret_hex"]; present {
		return fmt.Errorf("SECURITY FAILURE: %s message contains secret_hex", kind)
	}

	return nil
}

// requiredKey reads one required contract key off a record.
func requiredKey(value *string, field, label string) (string, error) {
	if value == nil || *value == "" {
		return "", fmt.Errorf("the swap record has no %s (%s)", label, field)
	}

	return *value, nil
}

// requiredFunding reads one chain's recorded funding outpoint.
func requiredFunding(swap *model.Swap, blake bool) (string, int64, error) {
	txid, vout := swap.BTCFundingTxID, swap.BTCFundingVout

	if blake {
		txid, vout = swap.BlakeFundingTxID, swap.BlakeFundingVout
	}

	if txid == nil || *txid == "" {
		return "", 0, fmt.Errorf("the swap record has no funding txid")
	}

	if vout == nil {
		return "", 0, fmt.Errorf("the swap record has no funding vout")
	}

	return *txid, int64(*vout), nil
}

// requiredSettlement reads one recorded settlement's transaction and block.
func requiredSettlement(
	txid, blockHash *string, txidField, blockField, label string,
) (string, string, error) {
	if txid == nil || *txid == "" {
		return "", "", fmt.Errorf("the swap record has no %s txid (%s)", label, txidField)
	}

	if blockHash == nil || *blockHash == "" {
		return "", "", fmt.Errorf("the swap record has no %s blockhash (%s)", label, blockField)
	}

	return *txid, *blockHash, nil
}
