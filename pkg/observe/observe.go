// Package observe answers one question about a swap: what is true on the
// chain right now?
//
// The answer is deliberately a closed vocabulary rather than a boolean,
// because the engine's next action depends on the difference between "the
// funding output is not there", "it is there but unconfirmed", "it is
// confirmed and unspent", and "it was spent, by this branch". Collapsing any
// two of those would let the engine broadcast a second settlement of the same
// HTLC, which is the one mistake this layer exists to make impossible.
//
// Nothing here mutates the swap: the durable record is read, and a discovered
// funding outpoint lives only in the returned Observation until the caller
// decides to record it.
package observe

import (
	"context"
	"errors"
	"fmt"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/htlc"
	"github.com/gjbrr/blake-swap/pkg/model"
)

// Node is the chain surface observation needs. It is an interface so the
// behaviour can be tested without a node, and *chain.Node satisfies it.
type Node interface {
	Height(ctx context.Context) (int64, error)
	BlockHash(ctx context.Context, height int64) (string, error)
	BlockTxs(ctx context.Context, hash string) (*chain.VerboseBlock, error)
	RawTx(ctx context.Context, txid, blockHash string) (*chain.Tx, error)
	TxOut(ctx context.Context, txid string, vout int64) (*chain.TxOut, error)
	WalletTx(ctx context.Context, txid string) (*chain.WalletTx, error)
	DecodeRaw(ctx context.Context, hexTx string) (*chain.Tx, error)
	DescriptorInfo(ctx context.Context, body string) (*chain.DescriptorInfo, error)
	DeriveAddresses(ctx context.Context, descriptor string) ([]string, error)
}

// Status is what is known about one leg of a swap.
type Status string

// The statuses. Every one of them is a distinct next action for the engine:
// see the package comment.
const (
	// Absent means no confirmed output matching the contract was found.
	Absent Status = "absent"
	// Pending means the funding output exists unconfirmed.
	Pending Status = "pending"
	// Funded means the funding output is confirmed and unspent.
	Funded Status = "funded"
	// Spent means a confirmed transaction spends the HTLC.
	Spent Status = "spent"
	// PendingSpend means this participant's settlement transaction is in the
	// mempool, proven to spend this HTLC and use the expected branch.
	PendingSpend Status = "pending-spend"
	// Invalid means something was found but it is not the contract.
	Invalid Status = "invalid"
	// Unknown means the outpoint could not be read and no spender was found.
	Unknown Status = "unknown"
	// PendingOrAmbiguous means a spender is known but cannot be proven yet.
	// The engine must wait, never act.
	PendingOrAmbiguous Status = "pending-or-ambiguous"
)

// Observation is the answer for one chain's leg.
type Observation struct {
	Status         Status
	TxID           string
	Vout           int64
	BlockHash      string
	Confirmations  int64
	Discovered     bool
	SpendTxID      string
	SpendBlockHash string
	Branch         htlc.Branch
	Reason         string
}

// Failure classifies an error the way the engine has to react to it.
type Failure int

// The two classes. Rejected means act no further; Pending means wait.
const (
	// Rejected is a fail-closed condition: the observed chain disagrees with
	// the durable record, or the record cannot be proven.
	Rejected Failure = iota
	// Wait is a wait condition. It is an error only so that callers cannot
	// mistake it for success and broadcast a competing transaction.
	Wait
)

// Error is a classified failure.
type Error struct {
	Failure Failure
	Message string
}

// Error renders the failure with the prefix the Python engine used, so logs
// and operator instructions read the same on both sides of the port.
func (e *Error) Error() string {
	if e.Failure == Wait {
		return "PENDING: " + e.Message
	}

	return "SECURITY FAILURE: " + e.Message
}

// rejectf returns a fail-closed error.
func rejectf(format string, args ...any) error {
	return &Error{Failure: Rejected, Message: fmt.Sprintf(format, args...)}
}

// pendf returns a wait-condition error.
func pendf(format string, args ...any) error {
	return &Error{Failure: Wait, Message: fmt.Sprintf(format, args...)}
}

// ClassOf reports an error's class. It is false for errors that did not come
// from this package; those are ordinary bugs or transport failures.
func ClassOf(err error) (Failure, bool) {
	var failure *Error

	if errors.As(err, &failure) {
		return failure.Failure, true
	}

	return Rejected, false
}

// Config is what observation needs to know beyond the swap itself.
type Config struct {
	// Wallet is the wallet that created this participant's transactions. It
	// is the fallback that recovers a settlement transaction on a node
	// without txindex, where getrawtransaction cannot see a wallet's own
	// transaction any more.
	Wallet string

	// AllowUnanchoredScan permits a funding scan from genesis when the swap
	// has no recorded anchor. It exists for legacy durable state; a new swap
	// always records an anchor, and an unbounded scan is a denial of service
	// against the participant's own node.
	AllowUnanchoredScan bool

	// MaxScanBlocks bounds one funding scan. Zero means DefaultMaxScanBlocks.
	MaxScanBlocks int64
}

// DefaultMaxScanBlocks bounds a funding scan when Config does not.
const DefaultMaxScanBlocks int64 = 100_000

// Contract is the on-chain form of one leg of a swap.
//
// Descriptor is the node's own compilation of the HTLC body, checksummed:
// the engine never computes a checksum itself, because a checksum it computed
// could disagree with the node that has to import the descriptor.
type Contract struct {
	Chain      chain.Chain
	Descriptor string
	Address    string
	// Amount is the expected value in the chain's smallest unit: satoshis on
	// Bitcoin, BLAKE base units on BLAKE. Both are 10^-8 of a coin.
	Amount int64
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

// checkOutput proves a chain output is this swap's HTLC.
//
// Amount and derived address are what make a recorded outpoint trustworthy:
// the txid in the durable record is a claim, and this is the check that turns
// it into evidence. A mismatch is fail-closed — the caller must not go looking
// for some other transaction that would match.
func (c *Contract) checkOutput(value amount.Coin, address string) error {
	actual, err := value.Sats()

	if err != nil {
		if c.Chain == chain.Blake {
			return rejectf("BLAKE funding output has an unrepresentable amount: %v", err)
		}

		return rejectf("Bitcoin funding output has an unrepresentable amount: %v", err)
	}

	if int64(actual) != c.Amount {
		if c.Chain == chain.Blake {
			return rejectf(
				"Wrong BLAKE amount: expected %s (%d base units), found %s",
				amount.Blake(c.Amount).Format(), c.Amount, value,
			)
		}

		return rejectf(
			"Wrong Bitcoin amount: expected %d sats, found %d sats",
			c.Amount, int64(actual),
		)
	}

	if address != c.Address {
		return rejectf(
			"%s funding output does not match the expected HTLC address",
			chainName(c.Chain),
		)
	}

	return nil
}

// VerifyFunding proves a confirmed funding output is the contract.
//
// This is the check a participant runs before treating a swap as funded, and
// the one the observation layer runs before reporting `funded`: a confirmed
// outpoint is not the same claim as a confirmed *HTLC*.
func VerifyFunding(ctx context.Context, node Node, swap *model.Swap, ch chain.Chain) error {
	txid, vout, err := fundingOutpoint(swap, ch)

	if err != nil {
		return err
	}

	contract, err := Derive(ctx, node, swap, ch)

	if err != nil {
		return err
	}

	utxo, err := node.TxOut(ctx, txid, vout)

	if err != nil {
		return err
	}

	if utxo == nil {
		return fmt.Errorf("%s HTLC is already spent or unavailable", chainName(ch))
	}

	if err := contract.checkOutput(utxo.Value, utxo.ScriptPubKey.Address); err != nil {
		return err
	}

	if utxo.Confirmations < 1 {
		return fmt.Errorf("%s funding transaction is not confirmed", chainName(ch))
	}

	return nil
}

// chainName is the upper-case name the messages use.
func chainName(ch chain.Chain) string {
	if ch == chain.Blake {
		return "BLAKE"
	}

	return "Bitcoin"
}

// fundingOutpoint reads the recorded funding outpoint for one chain.
func fundingOutpoint(swap *model.Swap, ch chain.Chain) (string, int64, error) {
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
		return "", 0, fmt.Errorf("swap has no %s funding txid", chainName(ch))
	}

	if vout == nil {
		return "", 0, fmt.Errorf("swap has no %s funding vout", chainName(ch))
	}

	return *txid, int64(*vout), nil
}

// fundingAnchors reads the recorded funding outpoint and its block anchor.
type fundingAnchor struct {
	TxID      *string
	Vout      *int32
	BlockHash *string
}

// anchorFor returns one chain's recorded funding anchors.
func anchorFor(swap *model.Swap, ch chain.Chain) fundingAnchor {
	if ch == chain.Blake {
		return fundingAnchor{
			TxID:      swap.BlakeFundingTxID,
			Vout:      swap.BlakeFundingVout,
			BlockHash: swap.BlakeFundingBlockHash,
		}
	}

	return fundingAnchor{
		TxID:      swap.BTCFundingTxID,
		Vout:      swap.BTCFundingVout,
		BlockHash: swap.BTCFundingBlockHash,
	}
}

// settlementSpenders describes the settlement transactions the durable record
// already knows about, so a mempool settlement is not mistaken for absence.
func settlementSpenders(swap *model.Swap, ch chain.Chain) []knownSpender {
	if ch == chain.Blake {
		return []knownSpender{
			{branch: htlc.Claim, txid: swap.BlakeClaimTxID, blockHash: swap.BlakeClaimBlockHash},
			{branch: htlc.Refund, txid: swap.BlakeRefundTxID, blockHash: swap.BlakeRefundBlockHash},
		}
	}

	return []knownSpender{
		{branch: htlc.Claim, txid: swap.BTCCLaimTxID, blockHash: swap.BTCCLaimBlockHash},
		{branch: htlc.Refund, txid: swap.BTCRefundTxID, blockHash: swap.BTCRefundBlockHash},
	}
}

// knownSpender is one settlement the durable record claims to have broadcast.
type knownSpender struct {
	branch    htlc.Branch
	txid      *string
	blockHash *string
}

// populated reports whether the record claims this settlement exists.
func (k knownSpender) populated() bool { return k.txid != nil && *k.txid != "" }

// confirmed reports whether the record also knows the block it landed in.
func (k knownSpender) confirmed() bool {
	return k.blockHash != nil && *k.blockHash != ""
}

// spendsHTLC reports whether a transaction spends the exact funding outpoint.
func spendsHTLC(tx *chain.Tx, txid string, vout int64) bool {
	for _, in := range tx.Vin {
		if in.TxID == txid && in.Vout == vout {
			return true
		}
	}

	return false
}

// classify asks the HTLC classifier which branch a transaction used.
func classify(tx *chain.Tx, txid string, vout int64, secretHash string) (htlc.Branch, error) {
	inputs := make([]htlc.Input, 0, len(tx.Vin))

	for _, in := range tx.Vin {
		inputs = append(inputs, htlc.Input{
			TxID:    in.TxID,
			Vout:    in.Vout,
			Witness: in.Witness,
		})
	}

	return htlc.ClassifySpend(
		htlc.Tx{Vin: inputs}, txid, vout, secretHash,
	)
}
