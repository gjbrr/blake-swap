package swap

import (
	"context"
	"fmt"

	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/decimal"
	"github.com/gjbrr/blake-swap/pkg/fund"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/state"
)

// Identity derivation and the role guard.
//
// Every economic command starts here. The identity pinned into a swap is what
// decides which side of it this participant is on, and an action taken as the
// wrong side is worse than not taken at all: it funds the contract the
// counterparty can already claim, or claims one that is not due.

// LocalRole derives this participant's protocol role from the identities pinned
// into the swap.
//
// It fails closed in all three of the ways that matter: pinned as neither role
// (this participant has no business acting), pinned as both (the record cannot
// say which side it is), and no identity configured at all.
func LocalRole(swap *model.Swap, me string) (string, string, error) {
	if swap == nil {
		return "", "", fmt.Errorf("swap is nil")
	}

	if me == "" {
		return "", "", fmt.Errorf("no local Nostr identity is configured")
	}

	alice := swap.NostrAlicePub != nil && *swap.NostrAlicePub == me
	bob := swap.NostrBobPub != nil && *swap.NostrBobPub == me

	switch {
	case alice && bob:
		return "", "", fmt.Errorf(
			"SECURITY FAILURE: current participant is pinned as both protocol roles",
		)
	case alice:
		peer := ""

		if swap.NostrBobPub != nil {
			peer = *swap.NostrBobPub
		}

		return "alice", peer, nil
	case bob:
		peer := ""

		if swap.NostrAlicePub != nil {
			peer = *swap.NostrAlicePub
		}

		return "bob", peer, nil
	}

	return "", "", fmt.Errorf(
		"SECURITY FAILURE: current Nostr identity is not pinned to this swap",
	)
}

// requireRole refuses an economic action taken as the wrong protocol role.
func (e *Engine) requireRole(swap *model.Swap, expected, action string) (string, error) {
	if expected != "alice" && expected != "bob" {
		return "", fmt.Errorf("expected role must be alice or bob")
	}

	if e.Identity == nil {
		return "", fmt.Errorf("no local Nostr identity is configured")
	}

	actual, _, err := LocalRole(swap, e.Identity.PublicKey())

	if err != nil {
		return "", err
	}

	if actual != expected {
		return "", fmt.Errorf(
			"SECURITY FAILURE: refusing %s as protocol %s; protocol requires %s",
			action, actual, expected,
		)
	}

	return actual, nil
}

// FundingResult is what a funding command did.
type FundingResult struct {
	Chain         chain.Chain
	TxID          string
	Vout          int64
	BlockHash     string
	Confirmations int64
	Pending       bool
	Recovered     bool
	Watch         fund.WatchStatus
	FeeRate       decimal.Dec
	FeeSource     string
	// FeeTargetBlocks is the confirmation target the estimator was asked for.
	FeeTargetBlocks int64
	// Address is where the HTLC was funded, as the node compiled it.
	Address string
	// Height is the chain height the contract was built against.
	Height int64
	State  state.State
	// Note is a message the operator should see, such as a superseded intent.
	Note string
}

// fundOptions maps the engine's settings onto the funding layer's.
func (e *Engine) fundOptions(ch chain.Chain) fund.Options {
	wallet := e.BlakeWallet
	watch := e.BlakeWatchWallet

	if ch == chain.BTC {
		wallet = e.BTCWallet
		watch = e.BTCWatchWallet
	}

	return fund.Options{
		Wallet:          wallet,
		WatchWallet:     watch,
		FeeRate:         e.Funding.FeeRate,
		FeeTargetBlocks: e.Funding.FeeTargetBlocks,
		FeeMultiplier:   e.Funding.FeeMultiplier,
		MinFeeRate:      e.Funding.MinFeeRate,
		AutoMine:        e.Funding.AutoMine,
		Intents:         e.Intents,
	}
}

// FundBlake puts Alice's BLAKE HTLC on the chain.
//
// The order is the protocol's: state first, then money. A funding attempted from
// any state but OFFERED is refused before the node is asked to sign anything,
// because a second funding of an already-funded contract pays twice for one
// claim.
func (e *Engine) FundBlake(ctx context.Context, swapID string) (*FundingResult, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	if _, err := e.requireRole(swap, "alice", "BLAKE funding"); err != nil {
		return nil, err
	}

	if swap.State != state.Offered {
		return nil, fmt.Errorf("fund-blake requires swap state OFFERED")
	}

	if e.Blake == nil {
		return nil, fmt.Errorf("the engine has no BLAKE node")
	}

	receipt, err := fund.Fund(ctx, e.Blake, swap, chain.Blake, e.fundOptions(chain.Blake))

	if err != nil {
		return nil, err
	}

	derived, err := contract.Derive(ctx, e.Blake, swap, chain.Blake)

	if err != nil {
		return nil, err
	}

	height, err := e.Blake.Height(ctx)

	if err != nil {
		return nil, err
	}

	result := &FundingResult{
		Chain:           chain.Blake,
		Address:         derived.Address,
		Height:          height,
		TxID:            receipt.TxID,
		Vout:            receipt.Vout,
		BlockHash:       receipt.BlockHash,
		Confirmations:   receipt.Confirmations,
		Pending:         receipt.Pending,
		Recovered:       receipt.Recovered,
		Watch:           receipt.Watch,
		FeeRate:         receipt.FeeRate,
		FeeSource:       receipt.FeeSource,
		FeeTargetBlocks: e.Funding.FeeTargetBlocks,
		State:           swap.State,
	}

	//
	// A pending BLAKE funding deliberately leaves the record at OFFERED. The
	// intent file is what remembers the attempt; advancing durable protocol state
	// on an unconfirmed funding would claim progress the chain has not made.
	//
	if receipt.Pending {
		return result, nil
	}

	txid := receipt.TxID
	vout := int32(receipt.Vout)
	blockHash := receipt.BlockHash

	swap.BlakeFundingTxID = &txid
	swap.BlakeFundingVout = &vout
	swap.BlakeFundingBlockHash = &blockHash

	if err := swap.Transition(state.BlakeFunded); err != nil {
		return nil, err
	}

	if err := e.save(ctx, swap); err != nil {
		return nil, err
	}

	if err := fund.ClearIntent(ctx, e.Intents, swap.SwapID, chain.Blake); err != nil {
		return nil, err
	}

	result.State = swap.State

	return result, nil
}

// FundBTC puts Bob's Bitcoin HTLC on the chain.
//
// Two extra checks come first, and both are the protocol rather than caution:
// Bob acts only in BLAKE_FUNDED, and only after he has verified Alice's BLAKE
// contract against the chain. Funding Bitcoin against a BLAKE side that is not
// actually there is the worst trade in the protocol — Bob pays sats for a claim
// that cannot be completed.
func (e *Engine) FundBTC(ctx context.Context, swapID string) (*FundingResult, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	if _, err := e.requireRole(swap, "bob", "BTC funding"); err != nil {
		return nil, err
	}

	if swap.State != state.BlakeFunded {
		return nil, fmt.Errorf("fund-btc requires state BLAKE_FUNDED")
	}

	if e.Blake == nil || e.BTC == nil {
		return nil, fmt.Errorf("the engine needs both chain nodes")
	}

	if err := contract.VerifyFunding(ctx, e.Blake, swap, chain.Blake); err != nil {
		return nil, fmt.Errorf("the BLAKE contract is not acceptable: %w", err)
	}

	receipt, err := fund.Fund(ctx, e.BTC, swap, chain.BTC, e.fundOptions(chain.BTC))

	if err != nil {
		return nil, err
	}

	txid := receipt.TxID
	vout := int32(receipt.Vout)

	swap.BTCFundingTxID = &txid
	swap.BTCFundingVout = &vout

	derived, err := contract.Derive(ctx, e.BTC, swap, chain.BTC)

	if err != nil {
		return nil, err
	}

	height, err := e.BTC.Height(ctx)

	if err != nil {
		return nil, err
	}

	result := &FundingResult{
		Chain:           chain.BTC,
		Address:         derived.Address,
		Height:          height,
		TxID:            receipt.TxID,
		Vout:            receipt.Vout,
		BlockHash:       receipt.BlockHash,
		Confirmations:   receipt.Confirmations,
		Pending:         receipt.Pending,
		Recovered:       receipt.Recovered,
		Watch:           receipt.Watch,
		FeeRate:         receipt.FeeRate,
		FeeSource:       receipt.FeeSource,
		FeeTargetBlocks: e.Funding.FeeTargetBlocks,
		State:           swap.State,
	}

	if receipt.Pending {
		//
		// The exact outpoint is recorded even while pending, and the protocol
		// checkpoint deliberately stays at BLAKE_FUNDED. Reconciliation can then
		// tell a known pending funding from genuinely absent Bitcoin funding,
		// which is the difference between waiting and funding again.
		//
		swap.BTCFundingBlockHash = nil

		if err := e.save(ctx, swap); err != nil {
			return nil, err
		}

		return result, nil
	}

	blockHash := receipt.BlockHash
	swap.BTCFundingBlockHash = &blockHash

	if err := swap.Transition(state.BTCFunded); err != nil {
		return nil, err
	}

	if err := e.save(ctx, swap); err != nil {
		return nil, err
	}

	if err := fund.ClearIntent(ctx, e.Intents, swap.SwapID, chain.BTC); err != nil {
		return nil, err
	}

	result.State = swap.State

	return result, nil
}

// VerifyBlake proves the BLAKE funding output is this swap's HTLC.
func (e *Engine) VerifyBlake(ctx context.Context, swapID string) error {
	_, err := e.CheckBlakeFunding(ctx, swapID)

	return err
}

// CheckBlakeFunding verifies the BLAKE funding and reports what it proves.
//
// The report exists because the verification command prints it for an operator and
// the funding command uses the same proof before it will spend anything.
func (e *Engine) CheckBlakeFunding(ctx context.Context, swapID string) (*contract.Check, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	return contract.CheckFunding(ctx, e.Blake, swap, chain.Blake)
}

// CheckBTCFunding verifies the Bitcoin funding and reports what it proves.
func (e *Engine) CheckBTCFunding(ctx context.Context, swapID string) (*contract.Check, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	return contract.CheckFunding(ctx, e.BTC, swap, chain.BTC)
}

// VerifyBTC proves the Bitcoin funding output is this swap's HTLC.
func (e *Engine) VerifyBTC(ctx context.Context, swapID string) error {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return err
	}

	return contract.VerifyFunding(ctx, e.BTC, swap, chain.BTC)
}
