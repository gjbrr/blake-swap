// Package contract is the on-chain form of one leg of a swap.
//
// Both the funding layer and the observation layer need the same three facts
// about an HTLC: the descriptor the node compiled from the negotiated terms,
// the address that descriptor derives, and the exact amount the swap says it
// should hold. Deriving them in one place matters because the checks that use
// them are what turn a txid in a durable record into evidence, and two copies
// of "the expected address" is two chances to disagree.
//
// The descriptor comes back from the node already checksummed. The engine never
// computes a checksum itself: a checksum it computed could disagree with the
// node that has to import the descriptor.
package contract

import (
	"context"
	"fmt"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/htlc"
	"github.com/gjbrr/blake-swap/pkg/model"
)

// Node is the node surface derivation needs.
type Node interface {
	DescriptorInfo(ctx context.Context, body string) (*chain.DescriptorInfo, error)
	DeriveAddresses(ctx context.Context, descriptor string) ([]string, error)
	TxOut(ctx context.Context, txid string, vout int64) (*chain.TxOut, error)
}

// Contract is one leg's on-chain facts.
type Contract struct {
	Chain      chain.Chain
	Descriptor string
	Address    string
	// Amount is the expected value in the chain's smallest unit: satoshis on
	// Bitcoin, BLAKE base units on BLAKE. Both are 10^-8 of a coin.
	Amount int64
}

// Name is the chain's name as the operator-facing messages spell it.
func Name(ch chain.Chain) string {
	if ch == chain.Blake {
		return "BLAKE"
	}

	return "Bitcoin"
}

// Bond is the swap's HTLC facts as the descriptor builder wants them.
func Bond(swap *model.Swap) (htlc.Bond, error) {
	if swap == nil {
		return htlc.Bond{}, fmt.Errorf("swap is nil")
	}

	fields := []struct {
		name  string
		value *string
	}{
		{"blake_bob_pub", swap.BlakeBobPub},
		{"blake_alice_pub", swap.BlakeAlicePub},
		{"btc_alice_pub", swap.BTCAlicePub},
		{"btc_bob_pub", swap.BTCBobPub},
	}

	pubkeys := make([]string, len(fields))

	for i, field := range fields {
		if field.value == nil || *field.value == "" {
			return htlc.Bond{}, fmt.Errorf("swap has no %s", field.name)
		}

		pubkeys[i] = *field.value
	}

	return htlc.Bond{
		SecretHash:    swap.SecretHash,
		BlakeBobPub:   pubkeys[0],
		BlakeAlicePub: pubkeys[1],
		BTCAlicePub:   pubkeys[2],
		BTCBobPub:     pubkeys[3],
		BlakeTimeout:  swap.BlakeTimeout,
		BTCTimeout:    swap.BTCTimeout,
	}, nil
}

// Derive asks the node to compile the HTLC and derive its address.
func Derive(ctx context.Context, node Node, swap *model.Swap, ch chain.Chain) (*Contract, error) {
	bond, err := Bond(swap)

	if err != nil {
		return nil, err
	}

	body, err := bond.Descriptor(htlc.Chain(ch))

	if err != nil {
		return nil, err
	}

	info, err := node.DescriptorInfo(ctx, body)

	if err != nil {
		return nil, fmt.Errorf("getdescriptorinfo: %w", err)
	}

	if info.Descriptor == "" {
		return nil, fmt.Errorf("the node returned no descriptor for the HTLC")
	}

	addresses, err := node.DeriveAddresses(ctx, info.Descriptor)

	if err != nil {
		return nil, fmt.Errorf("deriveaddresses: %w", err)
	}

	if len(addresses) != 1 {
		return nil, fmt.Errorf(
			"the node derived %d addresses for the HTLC, want exactly one",
			len(addresses),
		)
	}

	expected := swap.BTCSats

	if ch == chain.Blake {
		expected = swap.BlakeAmountBaseUnits
	}

	return &Contract{
		Chain:      ch,
		Descriptor: info.Descriptor,
		Address:    addresses[0],
		Amount:     expected,
	}, nil
}

// CheckOutput proves a chain output is this swap's HTLC.
//
// Amount and derived address are what make a recorded outpoint trustworthy:
// the txid in the durable record is a claim, and this is the check that turns
// it into evidence. A mismatch is fail-closed — the caller must not go looking
// for some other transaction that would match.
func (c *Contract) CheckOutput(value amount.Coin, address string) error {
	actual, err := value.Sats()

	if err != nil {
		return fmt.Errorf(
			"%s funding output has an unrepresentable amount: %w",
			Name(c.Chain), err,
		)
	}

	if int64(actual) != c.Amount {
		if c.Chain == chain.Blake {
			return fmt.Errorf(
				"Wrong BLAKE amount: expected %s (%d base units), found %s",
				amount.Blake(c.Amount).Format(), c.Amount, value,
			)
		}

		return fmt.Errorf(
			"Wrong Bitcoin amount: expected %d sats, found %d sats",
			c.Amount, int64(actual),
		)
	}

	if address != c.Address {
		return fmt.Errorf(
			"%s funding output does not match the expected HTLC address",
			Name(c.Chain),
		)
	}

	return nil
}

// VerifyFunding proves a confirmed funding output is the contract.
//
// This is the check a participant runs before treating a swap as funded: a
// confirmed outpoint is not the same claim as a confirmed *HTLC*.
func VerifyFunding(ctx context.Context, node Node, swap *model.Swap, ch chain.Chain) error {
	txid, vout, err := Outpoint(swap, ch)

	if err != nil {
		return err
	}

	derived, err := Derive(ctx, node, swap, ch)

	if err != nil {
		return err
	}

	utxo, err := node.TxOut(ctx, txid, vout)

	if err != nil {
		return err
	}

	if utxo == nil {
		return fmt.Errorf("%s HTLC is already spent or unavailable", Name(ch))
	}

	if err := derived.CheckOutput(utxo.Value, utxo.ScriptPubKey.Address); err != nil {
		return err
	}

	if utxo.Confirmations < 1 {
		return fmt.Errorf("%s funding transaction is not confirmed", Name(ch))
	}

	return nil
}

// Outpoint reads the recorded funding outpoint for one chain.
func Outpoint(swap *model.Swap, ch chain.Chain) (string, int64, error) {
	if swap == nil {
		return "", 0, fmt.Errorf("swap is nil")
	}

	var (
		txid *string
		vout *int32
	)

	if ch == chain.Blake {
		txid, vout = swap.BlakeFundingTxID, swap.BlakeFundingVout
	} else {
		txid, vout = swap.BTCFundingTxID, swap.BTCFundingVout
	}

	if txid == nil || *txid == "" {
		return "", 0, fmt.Errorf("swap has no %s funding txid", Name(ch))
	}

	if vout == nil {
		return "", 0, fmt.Errorf("swap has no %s funding vout", Name(ch))
	}

	return *txid, int64(*vout), nil
}
