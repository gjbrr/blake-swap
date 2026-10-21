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
	"encoding/hex"
	"fmt"
	"strconv"

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

// Terms are a contract's facts before a compiler touches them: the two public keys, the hash
// of the preimage and the locktime. They are what a local wallet needs, and what a descriptor
// is only one rendering of.
type Terms struct {
	// Claimer takes the money with the preimage; Refunder takes it back after Locktime.
	Claimer  []byte
	Refunder []byte
	// SecretHash is the SHA-256 of the swap secret.
	SecretHash [32]byte
	// Locktime is the refund branch's absolute locktime.
	Locktime uint32
}

// LocalContract is a contract derived without a node descriptor.
type LocalContract struct {
	// Address is the P2WSH address on the chain.
	Address string
	// Script is the witness script.
	Script []byte
}

// Local is a node surface that can derive a contract from its terms.
//
// The local wallet has the script builder; a node has the descriptor compiler. Derive prefers
// the local one when the node offers it, because a build with no node wallet has no compiler to
// ask.
type Local interface {
	DeriveHTLC(ctx context.Context, ch chain.Chain, terms Terms) (LocalContract, error)
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

	expected := swap.BTCSats

	if ch == chain.Blake {
		expected = swap.BlakeAmountBaseUnits
	}

	// A local wallet derives the contract from its terms; a node compiles a descriptor. The
	// two must agree, which is what the byte-for-byte script vector pins.
	if local, ok := node.(Local); ok {
		terms, err := bondTerms(bond, ch)

		if err != nil {
			return nil, err
		}

		derived, err := local.DeriveHTLC(ctx, ch, terms)

		if err != nil {
			return nil, fmt.Errorf("%s contract: %w", Name(ch), err)
		}

		return &Contract{Chain: ch, Address: derived.Address, Amount: expected}, nil
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

// bondTerms renders a bond as the terms one chain's contract is built from.
func bondTerms(bond htlc.Bond, ch chain.Chain) (Terms, error) {
	var (
		claimer, refunder string
		locktime          int64
	)

	switch ch {
	case chain.Blake:
		claimer, refunder, locktime = bond.BlakeBobPub, bond.BlakeAlicePub, bond.BlakeTimeout
	case chain.BTC:
		claimer, refunder, locktime = bond.BTCAlicePub, bond.BTCBobPub, bond.BTCTimeout
	default:
		return Terms{}, fmt.Errorf("unknown HTLC chain: %s", ch)
	}

	key := func(text string) ([]byte, error) {
		raw, err := hex.DecodeString(text)

		if err != nil {
			return nil, fmt.Errorf("%s public key: %w", Name(ch), err)
		}

		return raw, nil
	}

	rawClaimer, err := key(claimer)

	if err != nil {
		return Terms{}, err
	}

	rawRefunder, err := key(refunder)

	if err != nil {
		return Terms{}, err
	}

	hash, err := hex.DecodeString(bond.SecretHash)

	if err != nil || len(hash) != 32 {
		return Terms{}, fmt.Errorf("%s secret hash is not 32 bytes", Name(ch))
	}

	if locktime <= 0 || locktime > int64(^uint32(0)) {
		return Terms{}, fmt.Errorf("%s timeout %d is not a locktime", Name(ch), locktime)
	}

	var terms Terms

	terms.Claimer = rawClaimer
	terms.Refunder = rawRefunder
	copy(terms.SecretHash[:], hash)
	terms.Locktime = uint32(locktime)

	return terms, nil
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
// Check is what a funding verification proved, in the terms an operator reads.
type Check struct {
	TxID string
	Vout int64
	// Amount is the funded output's value, rendered the way the chain renders it:
	// a decimal for BLAKE, whole satoshis for Bitcoin.
	Amount string
	// Address is where the funding actually went.
	Address string
	// Expected is where this swap's HTLC should be.
	Expected      string
	Confirmations int64
}

// CheckFunding proves the funding output is this swap's HTLC and reports what it
// found.
//
// The facts are returned rather than printed because two commands read them: the
// verification command prints them for an operator, and the funding command uses
// the same proof before it will spend anything.
func CheckFunding(
	ctx context.Context, node Node, swap *model.Swap, ch chain.Chain,
) (*Check, error) {
	txid, vout, err := Outpoint(swap, ch)

	if err != nil {
		return nil, err
	}

	derived, err := Derive(ctx, node, swap, ch)

	if err != nil {
		return nil, err
	}

	utxo, err := node.TxOut(ctx, txid, vout)

	if err != nil {
		return nil, err
	}

	if utxo == nil {
		return nil, fmt.Errorf("%s HTLC is already spent or unavailable", Name(ch))
	}

	if err := derived.CheckOutput(utxo.Value, utxo.ScriptPubKey.Address); err != nil {
		return nil, err
	}

	if utxo.Confirmations < 1 {
		return nil, fmt.Errorf("%s funding transaction is not confirmed", Name(ch))
	}

	amount := utxo.Value.String()

	if ch == chain.BTC {
		sats, err := utxo.Value.Sats()

		if err != nil {
			return nil, err
		}

		amount = strconv.FormatInt(int64(sats), 10)
	}

	return &Check{
		TxID:          txid,
		Vout:          vout,
		Amount:        amount,
		Address:       utxo.ScriptPubKey.Address,
		Expected:      derived.Address,
		Confirmations: utxo.Confirmations,
	}, nil
}

// VerifyFunding is CheckFunding without the report.
func VerifyFunding(ctx context.Context, node Node, swap *model.Swap, ch chain.Chain) error {
	_, err := CheckFunding(ctx, node, swap, ch)

	return err
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
