// Package htlc is the swap's HTLC contract: its descriptor and the structural
// classification of a spend of it.
//
// It is the port of htlc_descriptor_body and classify_htlc_spend. Two decisions
// here are load-bearing:
//
//   - the descriptor *body* is returned without a checksum, because substituting
//     a private key changes the string and therefore its checksum, and the node
//     must be asked to compile the substituted form; and
//   - a spend's branch is decided from the witness *structure*, not from the
//     absence of a preimage. A witness matching neither shape is unknown, and
//     callers must fail closed rather than assume a refund.
package htlc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Chain is which chain's HTLC is being described. The two differ in which key
// claims and which refunds.
type Chain string

// The two chains.
const (
	BTC   Chain = "btc"
	Blake Chain = "blake"
)

// Branch is which HTLC path a spend used.
type Branch string

// The classifier's answers, plus the two the engine treats as a mismatch.
const (
	// Claim is the hashlock branch: a preimage plus the claimer's signature.
	Claim Branch = "claim"
	// Refund is the timelock branch: the refunder's signature only.
	Refund Branch = "refund"
	// Unknown is a witness that matches neither shape. Callers must treat it
	// as a mismatch and fail closed.
	Unknown Branch = "unknown"
)

// Bond is the set of facts a descriptor needs.
type Bond struct {
	SecretHash string // 64 lowercase hex characters

	BlakeBobPub   string // claims BLAKE
	BlakeAlicePub string // refunds BLAKE
	BTCAlicePub   string // claims Bitcoin
	BTCBobPub     string // refunds Bitcoin

	BlakeTimeout int64 // CLTV height on BLAKE
	BTCTimeout   int64 // CLTV height on Bitcoin
}

// Validate checks the fields the descriptor will interpolate.
func (b Bond) Validate() error {
	if !isHex32(b.SecretHash) {
		return fmt.Errorf("secret_hash must be 32-byte lowercase hex")
	}

	for field, value := range map[string]string{
		"blake_bob_pub":   b.BlakeBobPub,
		"blake_alice_pub": b.BlakeAlicePub,
		"btc_alice_pub":   b.BTCAlicePub,
		"btc_bob_pub":     b.BTCBobPub,
	} {
		if !isPubkey(value) {
			return fmt.Errorf("%s must be a compressed secp256k1 public key", field)
		}
	}

	if b.BlakeTimeout <= 0 {
		return fmt.Errorf("blake_timeout must be positive")
	}

	if b.BTCTimeout <= 0 {
		return fmt.Errorf("btc_timeout must be positive")
	}

	return nil
}

// Descriptor returns the canonical wsh(or_i(...)) descriptor body for one
// chain, with no checksum.
//
// On BLAKE Alice refunds and Bob claims; on Bitcoin Alice claims and Bob
// refunds. That inversion is the protocol, not a coincidence: Alice funds
// BLAKE and holds the secret, Bob funds Bitcoin.
func (b Bond) Descriptor(chain Chain) (string, error) {
	return b.DescriptorWithKeys(chain, nil)
}

// DescriptorWithKeys is Descriptor with some public keys replaced by the
// private key that controls them.
//
// `keys` maps a compressed public key to the WIF (or extended private key) that
// controls it. Substituting a key changes the descriptor string, and therefore
// its checksum, so the caller must ask the node to compile the substituted form
// rather than reusing a checksum computed from the public body.
func (b Bond) DescriptorWithKeys(chain Chain, keys map[string]string) (string, error) {
	if err := b.Validate(); err != nil {
		return "", err
	}

	render := func(pubkey string) string {
		if replacement, ok := keys[pubkey]; ok {
			return replacement
		}

		return pubkey
	}

	switch chain {
	case Blake:
		return fmt.Sprintf(
			"wsh(or_i(and_v(v:pk(%s),sha256(%s)),and_v(v:after(%d),pk(%s))))",
			render(b.BlakeBobPub), b.SecretHash, b.BlakeTimeout,
			render(b.BlakeAlicePub),
		), nil

	case BTC:
		return fmt.Sprintf(
			"wsh(or_i(and_v(v:pk(%s),sha256(%s)),and_v(v:after(%d),pk(%s))))",
			render(b.BTCAlicePub), b.SecretHash, b.BTCTimeout,
			render(b.BTCBobPub),
		), nil

	default:
		return "", fmt.Errorf("unknown HTLC chain: %s", chain)
	}
}

// Input is one transaction input as the node reports it.
type Input struct {
	TxID    string
	Vout    int64
	Witness []string
}

// Tx is the part of a decoded transaction the classifier needs.
type Tx struct {
	Vin []Input
}

// ClassifySpend decides which branch a confirmed spend of the exact HTLC
// outpoint used.
//
// The witness shapes are:
//
//	claim:   <preimage(32B)> <signature> <01> <witnessScript>
//	refund:  <signature> <0-or-empty> <witnessScript>
//
// The branch is decided from the structure: the selector is the second-to-last
// witness item, and the claim branch must carry a preimage hashing to the swap's
// secret hash while the refund branch must not. The spend has already been
// accepted by consensus and tied to this outpoint by the caller, so the script
// bytes are not re-derived.
func ClassifySpend(tx Tx, fundingTxID string, fundingVout int64, secretHash string) (Branch, error) {
	input, found := findInput(tx, fundingTxID, fundingVout)

	if !found {
		return "", fmt.Errorf(
			"internal error: candidate transaction does not spend the expected HTLC outpoint",
		)
	}

	if len(input.Witness) == 0 {
		return "", fmt.Errorf("SECURITY FAILURE: confirmed HTLC spend has no witness")
	}

	// Need at least: <arg> <selector> <witnessScript>.
	if len(input.Witness) < 3 {
		return Unknown, nil
	}

	selector := input.Witness[len(input.Witness)-2]

	preimagePresent := false

	for _, item := range input.Witness {
		if len(item) != 64 {
			continue
		}

		candidate, err := hex.DecodeString(item)

		if err != nil {
			continue
		}

		sum := sha256.Sum256(candidate)

		if hex.EncodeToString(sum[:]) == secretHash {
			preimagePresent = true

			break
		}
	}

	if selector == "01" {
		// OP_IF taken: the claim branch, which must carry the preimage.
		if preimagePresent {
			return Claim, nil
		}

		return Unknown, nil
	}

	if selector == "" || selector == "00" {
		// OP_ELSE taken: the refund branch, which must not carry the
		// preimage.
		if preimagePresent {
			return Unknown, nil
		}

		return Refund, nil
	}

	return Unknown, nil
}

func findInput(tx Tx, fundingTxID string, fundingVout int64) (Input, bool) {
	for _, input := range tx.Vin {
		if input.TxID == fundingTxID && input.Vout == fundingVout {
			return input, true
		}
	}

	return Input{}, false
}

// isHex32 reports whether s is 32 bytes of lowercase hex.
func isHex32(s string) bool {
	if len(s) != 64 {
		return false
	}

	for _, r := range s {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}

	return true
}

// isPubkey reports whether s is a compressed secp256k1 public key.
func isPubkey(s string) bool {
	if len(s) != 66 {
		return false
	}

	if s[:2] != "02" && s[:2] != "03" {
		return false
	}

	for _, r := range s[2:] {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}

	return true
}
