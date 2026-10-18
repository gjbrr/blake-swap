// Package swap is the protocol's command surface in Go: one function per CLI
// command, without the CLI.
//
// The Python engine's commands print as they work, which is fine for a terminal
// and wrong for everything else this port has to run on — a browser worker, a
// test, another program. So each command here takes what it needs and returns a
// result; printing is the CLI's job, and it is the only place that has to
// reproduce the reference output byte for byte.
//
// Every command loads the durable record, refuses to act on a state the protocol
// does not allow, and saves what it changed. That order is not incidental: a
// command that saved first and validated second would leave a record that claims
// progress the chain does not agree with.
package swap

import (
	"context"
	"fmt"
	"time"

	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/coord"
	"github.com/gjbrr/blake-swap/pkg/decimal"
	"github.com/gjbrr/blake-swap/pkg/fund"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/observe"
	"github.com/gjbrr/blake-swap/pkg/settle"
	"github.com/gjbrr/blake-swap/pkg/state"
	"github.com/gjbrr/blake-swap/pkg/store"
)

// Node is everything the engine needs from one chain's node.
//
// It is the union of the funding, settlement and observation surfaces, and
// *chain.Node satisfies it — asserted below, because that assertion is what makes
// the interface honest rather than aspirational.
type Node interface {
	fund.Node
	settle.Node
	observe.Node
	// InMempool is the presence check settlement recovery branches on.
	InMempool(ctx context.Context, txid string) (bool, error)
}

// The engine must be wireable to the real node surface, not just to test doubles.
var _ Node = (*chain.Node)(nil)

// Engine is one participant's view of the protocol.
type Engine struct {
	// Store is where swap records live.
	Store store.Store
	// Intents is where funding intents live. Funding refuses to run without it.
	Intents fund.Store
	// Blake and BTC are the two chain nodes.
	Blake, BTC Node
	// Identity is this participant's Nostr identity, when coordination is used.
	Identity Identity
	// Transport carries coordination messages. It is the coordination layer's
	// own interface, so the engine can drive either the local spool or relays
	// without knowing which.
	Transport coord.Transport
	// Mode is the coordination confidentiality mode.
	Mode nostr.Mode
	// Role is this participant's local protocol role for loads and saves.
	Role string
	// Wallets are the signing wallet names per chain.
	BlakeWallet, BTCWallet string
	// WatchWallets are the watch-only wallet names per chain.
	BlakeWatchWallet, BTCWatchWallet string
	// TestMode enables the commands that exist to drive tests, such as Advance.
	TestMode bool
	// Funding is the fee and rig policy funding commands use.
	Funding FundingSettings
	// Settlement is the fee policy settlement commands use.
	Settlement SettlementSettings
	// Now is the clock. Zero means time.Now.
	Now func() time.Time
}

// SettlementSettings is how a settlement command prices its transaction.
type SettlementSettings struct {
	// FeeRate is an explicit feerate in sat/vB, outranking every other knob.
	FeeRate *decimal.Dec
	// FeeSats is an exact fee (the environment's standby override).
	FeeSats *int64
	// Multiplier scales the estimator.
	Multiplier *decimal.Dec
	// MaxPercent and MaxSats bound what a settlement may pay.
	MaxPercent decimal.Dec
	MaxSats    *int64
	// FeeTargetBlocks is the estimator's confirmation target.
	FeeTargetBlocks int64
}

// FundingSettings is how a funding command prices and confirms its transaction.
type FundingSettings struct {
	// FeeRate is an explicit feerate in sat/vB. It outranks the estimator.
	FeeRate *decimal.Dec
	// FeeTargetBlocks is the estimator's confirmation target.
	FeeTargetBlocks int64
	// FeeMultiplier scales the estimator's answer.
	FeeMultiplier *decimal.Dec
	// MinFeeRate is the wallet-construction floor (Bitcoin Core needs 1 sat/vB).
	MinFeeRate *decimal.Dec
	// AutoMine mines blocks after broadcasting. It exists for the regtest rig.
	AutoMine int64
}

// Identity is the local Nostr identity the engine signs with.
type Identity interface {
	// PublicKey is the x-only public key, 64 hex characters.
	PublicKey() string
}

// NewEngine returns an engine with the conventional role defaults: a fresh
// record is Alice's, so Alice is the default local role.
func NewEngine(st store.Store, intents fund.Store, blake, btc Node) *Engine {
	return &Engine{
		Store:            st,
		Intents:          intents,
		Blake:            blake,
		BTC:              btc,
		Role:             store.Alice,
		BlakeWallet:      "alice",
		BTCWallet:        "bob",
		BlakeWatchWallet: "alice-watch",
		BTCWatchWallet:   "bob-watch",
	}
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}

	return time.Now()
}

// load reads one swap record as this participant.
func (e *Engine) load(ctx context.Context, swapID string) (*model.Swap, error) {
	if e.Store == nil {
		return nil, fmt.Errorf("the engine has no store")
	}

	if err := store.ValidateRole(e.role()); err != nil {
		return nil, err
	}

	return e.Store.Load(ctx, swapID)
}

// save writes one swap record, validating it first.
func (e *Engine) save(ctx context.Context, swap *model.Swap) error {
	if e.Store == nil {
		return fmt.Errorf("the engine has no store")
	}

	return e.Store.Save(ctx, e.role(), swap)
}

// role is the configured local role, defaulting to Alice.
func (e *Engine) role() string {
	if e.Role == "" {
		return store.Alice
	}

	return e.Role
}

// ChainStatus reports both chain tips.
//
// It is the one command that needs no durable state: an operator asking "what
// height is BLAKE" should not be told a swap is missing first.
type ChainStatus struct {
	Blake int64
	BTC   int64
}

// Status reports both chain tips.
func (e *Engine) Status(ctx context.Context) (*ChainStatus, error) {
	if e.Blake == nil || e.BTC == nil {
		return nil, fmt.Errorf("the engine needs both chain nodes")
	}

	blake, err := e.Blake.Height(ctx)

	if err != nil {
		return nil, err
	}

	btc, err := e.BTC.Height(ctx)

	if err != nil {
		return nil, err
	}

	return &ChainStatus{Blake: blake, BTC: btc}, nil
}

// Show returns one swap record.
func (e *Engine) Show(ctx context.Context, swapID string) (*model.Swap, error) {
	return e.load(ctx, swapID)
}

// List returns every swap record, sorted by id.
func (e *Engine) List(ctx context.Context) ([]*model.Swap, error) {
	if e.Store == nil {
		return nil, fmt.Errorf("the engine has no store")
	}

	ids, err := e.Store.List(ctx)

	if err != nil {
		return nil, err
	}

	var swaps []*model.Swap

	for _, id := range ids {
		record, err := e.Store.Load(ctx, id)

		if err != nil {
			// A record that cannot be read is reported rather than skipping the
			// whole listing: the operator needs to know it is there.
			return nil, fmt.Errorf("%s: %w", id, err)
		}

		swaps = append(swaps, record)
	}

	return swaps, nil
}

// Advance moves a swap to a target state without touching a chain.
//
// It exists only for tests and demos, and it is gated exactly as the reference
// gates it: a command that can teleport a swap past settlement is a foot-gun on a
// real record. The transition itself still goes through the state machine, so a
// forward-only edge that does not exist cannot be invented here either.
func (e *Engine) Advance(ctx context.Context, swapID, role, target string) (state.State, state.State, error) {
	if !e.TestMode {
		return "", "", fmt.Errorf("advance is available only in BLAKE_SWAP_TEST_MODE=1")
	}

	previous := e.Role
	e.Role = role

	defer func() { e.Role = previous }()

	swap, err := e.load(ctx, swapID)

	if err != nil {
		return "", "", err
	}

	from := swap.State

	if err := swap.Transition(state.State(target)); err != nil {
		return from, from, err
	}

	if err := e.save(ctx, swap); err != nil {
		return from, from, err
	}

	return from, swap.State, nil
}
