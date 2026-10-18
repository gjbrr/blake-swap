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

	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/contract"
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

// chainName is the upper-case name the messages use.
func chainName(ch chain.Chain) string { return contract.Name(ch) }

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
