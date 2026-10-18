package fund

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/model"
)

// Store is where funding intents live.
//
// It is an interface because the intent has to be durable in two very
// different places: a file on a host, and an encrypted record in the browser's
// IndexedDB. What matters to this package is only that a write that returns nil
// survives a crash, which is the property the whole crash window depends on.
type Store interface {
	Read(ctx context.Context, swapID string, ch chain.Chain) ([]byte, bool, error)
	Write(ctx context.Context, swapID string, ch chain.Chain, data []byte) error
	Delete(ctx context.Context, swapID string, ch chain.Chain) error
}

// Intent is the durable marker written before funds may move.
//
// It carries no secrets. It binds the attempted funding to this swap, chain,
// contract address and amount, so that a later process can tell "money may have
// moved" from "nothing happened".
type Intent struct {
	SwapID     string      `json:"swap_id"`
	Chain      chain.Chain `json:"chain"`
	Address    string      `json:"address"`
	Amount     string      `json:"amount"`
	BaseUnits  int64       `json:"base_units"`
	SecretHash string      `json:"secret_hash"`
	TxID       string      `json:"txid,omitempty"`
	Vout       *int32      `json:"vout,omitempty"`
}

// Resolution is what a stored intent turned out to mean.
type Resolution string

// The resolutions. Only confirmed is resolved automatically; the rest need an
// operator, because the difference between "the broadcast never happened" and
// "the wallet that made it is gone" is not something the engine can see.
const (
	// ResolutionNone means no intent exists.
	ResolutionNone Resolution = "none"
	// ResolutionConfirmed means the exact payment is confirmed; the intent was
	// cleared because the swap record is now the authority.
	ResolutionConfirmed Resolution = "confirmed"
	// ResolutionPending means the exact payment exists but is unconfirmed, so
	// a broadcast is genuinely in flight.
	ResolutionPending Resolution = "pending"
	// ResolutionAbsent means this wallet knows of no payment to the intent's
	// address, which is consistent with a crash before the broadcast.
	ResolutionAbsent Resolution = "absent"
	// ResolutionUnknown means the intent exists but is unreadable or
	// incomplete.
	ResolutionUnknown Resolution = "unknown"
)

// WriteIntent records that a funding is about to start.
func WriteIntent(
	ctx context.Context,
	store Store,
	swap *model.Swap,
	ch chain.Chain,
	address string,
) (*Intent, error) {
	if swap == nil {
		return nil, fmt.Errorf("swap is nil")
	}

	baseUnits := swap.BTCSats

	if ch == chain.Blake {
		baseUnits = swap.BlakeAmountBaseUnits
	}

	intent := &Intent{
		SwapID:     swap.SwapID,
		Chain:      ch,
		Address:    address,
		Amount:     amount.Sats(baseUnits).Coin().String(),
		BaseUnits:  baseUnits,
		SecretHash: swap.SecretHash,
	}

	encoded, err := json.Marshal(intent)

	if err != nil {
		return nil, err
	}

	if err := store.Write(ctx, swap.SwapID, ch, encoded); err != nil {
		return nil, err
	}

	return intent, nil
}

// RecordBroadcast extends a durable intent with the transaction identity.
//
// The protocol checkpoint is deliberately not advanced here: the intent is what
// covers the window between the node broadcasting and the swap record learning
// the outpoint, so it has to name the transaction before that window closes.
func RecordBroadcast(
	ctx context.Context, store Store, swapID string, ch chain.Chain, txid string, vout int64,
) error {
	intent, found, err := ReadIntent(ctx, store, swapID, ch)

	if err != nil {
		return err
	}

	if !found {
		return fmt.Errorf(
			"Funding intent disappeared before broadcast identity could be recorded",
		)
	}

	intent.TxID = txid
	index := int32(vout)
	intent.Vout = &index

	encoded, err := json.Marshal(intent)

	if err != nil {
		return err
	}

	return store.Write(ctx, swapID, ch, encoded)
}

// ReadIntent returns a stored intent.
func ReadIntent(
	ctx context.Context, store Store, swapID string, ch chain.Chain,
) (*Intent, bool, error) {
	data, found, err := store.Read(ctx, swapID, ch)

	if err != nil || !found {
		return nil, false, err
	}

	var intent Intent

	if err := json.Unmarshal(data, &intent); err != nil {
		return nil, true, err
	}

	return &intent, true, nil
}

// ClearIntent removes the marker once the outpoint is durable.
func ClearIntent(ctx context.Context, store Store, swapID string, ch chain.Chain) error {
	return store.Delete(ctx, swapID, ch)
}

// ResolveIntent decides what a leftover intent means.
//
// Read-only with respect to the chain: it never broadcasts, never mines and
// never clears an intent it could not resolve. An intent that a crash left
// behind would otherwise block timeout handling forever, because an existing
// intent means a spend may have started.
func ResolveIntent(
	ctx context.Context,
	node Node,
	swap *model.Swap,
	ch chain.Chain,
	wallet string,
	store Store,
) (Resolution, error) {
	intent, found, err := ReadIntent(ctx, store, swap.SwapID, ch)

	if err != nil {
		return ResolutionUnknown, nil
	}

	if !found {
		return ResolutionNone, nil
	}

	if intent.Address == "" || intent.SecretHash != swap.SecretHash {
		return ResolutionUnknown, nil
	}

	if wallet == "" {
		return ResolutionUnknown, nil
	}

	payment, err := FindPayment(ctx, node.Bind(wallet), intent.Address, intent.BaseUnits)

	if err != nil {
		return ResolutionUnknown, nil
	}

	if payment == nil {
		//
		// No payment this wallet ever made to that address. That is
		// consistent with a crash before the broadcast, but it is not proof: a
		// replaced or restored wallet would look the same. Do not clear it
		// here; the operator's explicit "clear stale intent" does that.
		//
		return ResolutionAbsent, nil
	}

	if payment.BlockHash != "" {
		if err := ClearIntent(ctx, store, swap.SwapID, ch); err != nil {
			return ResolutionUnknown, nil
		}

		return ResolutionConfirmed, nil
	}

	return ResolutionPending, nil
}

// DefaultWallet is the engine's conventional signing wallet per chain.
//
// These names match the Python engine's defaults so a rig built for it works
// unchanged.
func DefaultWallet(ch chain.Chain) string {
	if ch == chain.Blake {
		return "alice"
	}

	return "bob"
}

// MemStore is a Store in memory, for tests and for a dry run.
type MemStore struct {
	mu    sync.Mutex
	items map[string][]byte
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{items: map[string][]byte{}}
}

func intentKey(swapID string, ch chain.Chain) string {
	return string(ch) + "/" + swapID
}

// Read implements Store.
func (m *MemStore) Read(_ context.Context, swapID string, ch chain.Chain) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	data, found := m.items[intentKey(swapID, ch)]

	return data, found, nil
}

// Write implements Store.
func (m *MemStore) Write(_ context.Context, swapID string, ch chain.Chain, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	copied := make([]byte, len(data))
	copy(copied, data)
	m.items[intentKey(swapID, ch)] = copied

	return nil
}

// Delete implements Store.
func (m *MemStore) Delete(_ context.Context, swapID string, ch chain.Chain) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.items, intentKey(swapID, ch))

	return nil
}

// Keys returns the stored intent keys, sorted. It exists so a test can prove a
// delete actually deleted something rather than silently finding nothing.
func (m *MemStore) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	keys := make([]string, 0, len(m.items))

	for key := range m.items {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}
