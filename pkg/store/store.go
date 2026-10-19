// Package store is where a participant's swaps, messages and archives live.
//
// Two properties motivate the interface rather than a file path. The record of
// a swap is the protocol's authoritative local state, so writing it must be
// atomic and durable: a crash may leave the previous version or the new one,
// never a prefix of either. And the same engine has to run on a host and in a
// browser, where the durable place is an encrypted IndexedDB store rather than a
// directory — so everything above this layer asks for "save this record" rather
// than for a file.
//
// The layout is the Python engine's, because a port that cannot read the
// existing state tree is not a drop-in replacement:
//
//	<base>/<swap-id>.json                 one record per swap
//	<base>/archive/<swap-id>.json         a superseded or retired record
//	<base>/messages/<swap-id>.<kind>.json one transport message
package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/gjbrr/blake-swap/pkg/model"
)

// Roles. The role is a protocol role, not a filesystem identity: the state tree
// is already participant-local, so Alice's and Bob's records share one base.
const (
	Alice = "alice"
	Bob   = "bob"
)

// Store is the durable surface the engine needs.
type Store interface {
	// Save writes one swap record, atomically and durably.
	Save(ctx context.Context, role string, swap *model.Swap) error
	// Load reads one swap record, applying the legacy migration.
	Load(ctx context.Context, swapID string) (*model.Swap, error)
	// List returns the canonical swap IDs, sorted.
	List(ctx context.Context) ([]string, error)
	// Retired reports whether this swap was archived (or superseded).
	Retired(ctx context.Context, swapID string) (bool, error)
	// Archive moves a record out of the live set without deleting it.
	Archive(ctx context.Context, role, swapID string) error

	// SendMessage writes one transport message for a swap.
	SendMessage(ctx context.Context, swapID, kind, sender string, body map[string]any) error
	// ReceiveMessage reads one transport message for a swap.
	ReceiveMessage(ctx context.Context, swapID, kind string) (*Message, error)
	// ListMessages returns the message kinds present for a swap, sorted.
	ListMessages(ctx context.Context, swapID string) ([]string, error)
}

// Message is one transport message as the spool stores it.
type Message struct {
	Kind   string
	Sender string
	Body   map[string]any
	// Path is where it was found, for operator output. It is empty for a
	// store that has no paths.
	Path string
}

// ErrNotFound is returned when a record or message does not exist.
var ErrNotFound = fmt.Errorf("not found")

// ValidateRole refuses a role the protocol does not have.
func ValidateRole(role string) error {
	if role != Alice && role != Bob {
		return fmt.Errorf("Unknown swap role: %s", role)
	}

	return nil
}

// ValidateSwapID refuses a name that is not a canonical swap ID.
//
// The rule lives in the record type, which owns the externally visible schema;
// a scan that accepted a name the wire rejects would load a record that can
// never be named in a message.
func ValidateSwapID(swapID string) error {
	if !model.IsSwapID(swapID) {
		return fmt.Errorf("not a canonical swap ID: %q", swapID)
	}

	return nil
}

// ValidateSender refuses a transport sender identity that is not a public key.
//
// The file transport is a local test backend, not an authenticated network, but
// it still carries the identity the sender selected so the same identity-binding
// code runs on both backends. A sender that is not 32 bytes of hex could not be
// that identity.
func ValidateSender(sender string) error {
	if len(sender) != 64 {
		return fmt.Errorf("Invalid FILE transport sender identity")
	}

	if _, err := hex.DecodeString(sender); err != nil {
		return fmt.Errorf("Invalid FILE transport sender identity")
	}

	return nil
}

// CanonicalID returns the swap ID a file name encodes, or false.
func CanonicalID(name string) (string, bool) {
	stem := strings.TrimSuffix(name, ".json")

	if stem == name || !model.IsSwapID(stem) {
		return "", false
	}

	return stem, true
}

// SortIDs sorts a list of swap IDs the way a directory listing would.
func SortIDs(ids []string) { sort.Strings(ids) }

// encodeRecord renders a swap record the way the engine stores it: indented,
// keys sorted, with a trailing newline.
//
// Sorting and the trailing newline are not cosmetic. They make two records
// written by different paths — the Go CLI and the Python reference — diffable,
// which is what the parity suites do with them.
func encodeRecord(swap *model.Swap) ([]byte, error) {
	encoded, err := swap.Marshal()

	if err != nil {
		return nil, err
	}

	var record map[string]any

	if err := json.Unmarshal(encoded, &record); err != nil {
		return nil, err
	}

	return encodeJSON(record, true)
}

// encodeJSON renders one JSON document with sorted keys.
func encodeJSON(value any, indent bool) ([]byte, error) {
	var (
		encoded []byte
		err     error
	)

	if indent {
		encoded, err = json.MarshalIndent(value, "", "  ")
	} else {
		encoded, err = json.Marshal(value)
	}

	if err != nil {
		return nil, err
	}

	return append(encoded, '\n'), nil
}

// decodeSwap parses a stored record.
func decodeSwap(data []byte, warn func(string)) (*model.Swap, error) {
	return model.Decode(data, warn)
}

// SupersededPlaceholders lists the local OFFERED records a newly created swap
// makes obsolete.
//
// A placeholder has no counterparty, no contract keys and no funding or
// settlement transaction, so nobody else can be relying on it. Anything that was
// announced is deliberately excluded: it may still be accepted, and archiving it
// behind the operator's back would strand a swap a peer is holding.
func SupersededPlaceholders(ctx context.Context, st Store, keepSwapID string) ([]*model.Swap, error) {
	ids, err := st.List(ctx)

	if err != nil {
		return nil, err
	}

	var placeholders []*model.Swap

	for _, swapID := range ids {
		if swapID == keepSwapID {
			continue
		}

		candidate, err := st.Load(ctx, swapID)

		if err != nil {
			// A record that cannot be read is not a placeholder: skipping it
			// keeps a corrupt record visible instead of quietly archiving it.
			continue
		}

		if !isPlaceholder(candidate) {
			continue
		}

		placeholders = append(placeholders, candidate)
	}

	return placeholders, nil
}

// isPlaceholder reports whether a record is a pure, unannounced placeholder.
func isPlaceholder(swap *model.Swap) bool {
	if string(swap.State) != "OFFERED" {
		return false
	}

	if swap.NostrBobPub != nil || swap.BlakeAlicePub != nil || swap.BTCAlicePub != nil {
		return false
	}

	anchors := []*string{
		swap.BlakeFundingTxID,
		swap.BTCFundingTxID,
		swap.BTCCLaimTxID,
		swap.BlakeClaimTxID,
		swap.BTCRefundTxID,
		swap.BlakeRefundTxID,
	}

	for _, anchor := range anchors {
		if anchor != nil && *anchor != "" {
			return false
		}
	}

	return true
}

// ArchiveSuperseded archives every placeholder the new swap supersedes, and
// returns the IDs it archived.
//
// Archiving is announced by the caller: a swap that disappears silently is the
// failure this is fixing, so the replacement must never be quiet about it.
func ArchiveSuperseded(ctx context.Context, st Store, keepSwapID, role string) ([]string, error) {
	placeholders, err := SupersededPlaceholders(ctx, st, keepSwapID)

	if err != nil {
		return nil, err
	}

	var archived []string

	for _, placeholder := range placeholders {
		if err := st.Archive(ctx, role, placeholder.SwapID); err != nil {
			return archived, err
		}

		archived = append(archived, placeholder.SwapID)
	}

	return archived, nil
}

// ArchiveReader is a store that can list what it retired.
//
// It is separate from Store because only the dashboard needs to enumerate the archive:
// every other command works on live records, and forcing a browser store to materialise a
// retired list it never reads would be a cost for a question nobody asked.
type ArchiveReader interface {
	// Archived returns the retired records, in swap-id order.
	Archived(ctx context.Context) ([]*model.Swap, error)
}
