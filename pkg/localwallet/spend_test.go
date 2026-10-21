package localwallet

import (
	"encoding/hex"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/htlc"
)

// TestParseDescriptorReadsTheContractsTerms: the descriptor the swap renders is what a local
// wallet parses back into terms, because it has no descriptor compiler to ask.
func TestParseDescriptorReadsTheContractsTerms(t *testing.T) {
	const (
		hash  = "0000000000000000000000000000000000000000000000000000000000000001"
		alice = "03a34b99f22c790c4e36b2b3c2c35a36db06226e41c692fc82b8b56ac1c540c5bd"
		bob   = "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
	)

	bond := htlc.Bond{
		SecretHash:    hash,
		BlakeBobPub:   bob,
		BlakeAlicePub: alice,
		BTCAlicePub:   alice,
		BTCBobPub:     bob,
		BlakeTimeout:  600,
		BTCTimeout:    500,
	}

	descriptor, err := bond.Descriptor(htlc.BTC)

	if err != nil {
		t.Fatal(err)
	}

	terms, err := ParseDescriptor(descriptor)

	if err != nil {
		t.Fatalf("ParseDescriptor: %v", err)
	}

	if hex.EncodeToString(terms.Claimer) != alice {
		t.Fatalf("claimer %x", terms.Claimer)
	}

	if hex.EncodeToString(terms.Refunder) != bob {
		t.Fatalf("refunder %x", terms.Refunder)
	}

	if hex.EncodeToString(terms.SecretHash[:]) != hash {
		t.Fatalf("hash %x", terms.SecretHash)
	}

	if terms.Locktime != 500 {
		t.Fatalf("locktime %d", terms.Locktime)
	}

	// The other chain's descriptor names its keys in the other roles.
	blake, err := bond.Descriptor(htlc.Blake)

	if err != nil {
		t.Fatal(err)
	}

	other, err := ParseDescriptor(blake)

	if err != nil {
		t.Fatalf("ParseDescriptor(blake): %v", err)
	}

	if hex.EncodeToString(other.Claimer) != bob || hex.EncodeToString(other.Refunder) != alice || other.Locktime != 600 {
		t.Fatalf("blake terms %+v", other)
	}
}
