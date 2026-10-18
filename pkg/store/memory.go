package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/gjbrr/blake-swap/pkg/model"
)

// Memory is a Store in memory.
//
// It is what the browser will wrap in the vault, and what the tests use: the
// interesting behaviour is the record semantics — validation, migration,
// archival, the message envelope — not the filesystem calls around them.
type Memory struct {
	mu       sync.Mutex
	records  map[string][]byte
	archived map[string][]byte
	messages map[string][]byte
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		records:  map[string][]byte{},
		archived: map[string][]byte{},
		messages: map[string][]byte{},
	}
}

func messageKey(swapID, kind string) string { return swapID + "." + kind }

// Save implements Store.
func (m *Memory) Save(_ context.Context, role string, swap *model.Swap) error {
	if err := ValidateRole(role); err != nil {
		return err
	}

	if swap == nil {
		return fmt.Errorf("swap is nil")
	}

	if err := swap.Validate(); err != nil {
		return err
	}

	encoded, err := encodeRecord(swap)

	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.records[swap.SwapID] = encoded

	return nil
}

// Load implements Store.
func (m *Memory) Load(_ context.Context, swapID string) (*model.Swap, error) {
	if err := ValidateSwapID(swapID); err != nil {
		return nil, err
	}

	m.mu.Lock()
	data, found := m.records[swapID]
	m.mu.Unlock()

	if !found {
		return nil, fmt.Errorf("%s record %s: %w", swapID, swapID, ErrNotFound)
	}

	return decodeSwap(data, nil)
}

// List implements Store.
func (m *Memory) List(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ids := make([]string, 0, len(m.records))

	for id := range m.records {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	return ids, nil
}

// Retired implements Store.
func (m *Memory) Retired(_ context.Context, swapID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	_, found := m.archived[swapID]

	return found, nil
}

// Archive implements Store.
func (m *Memory) Archive(_ context.Context, role, swapID string) error {
	if err := ValidateRole(role); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	data, found := m.records[swapID]

	if !found {
		return fmt.Errorf("%s: %w", swapID, ErrNotFound)
	}

	m.archived[swapID] = data
	delete(m.records, swapID)

	return nil
}

// fileEnvelope is the on-disk message shape.
type fileEnvelope struct {
	Version      int            `json:"_file_transport_version"`
	SenderPubkey string         `json:"sender_pubkey"`
	Message      map[string]any `json:"message"`
}

// SendMessage implements Store.
func (m *Memory) SendMessage(
	_ context.Context, swapID, kind, sender string, body map[string]any,
) error {
	if err := ValidateSwapID(swapID); err != nil {
		return err
	}

	if err := ValidateSender(sender); err != nil {
		return err
	}

	if body == nil {
		return fmt.Errorf("a transport message must have a body")
	}

	encoded, err := encodeJSON(fileEnvelope{
		Version:      2,
		SenderPubkey: sender,
		Message:      body,
	}, true)

	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.messages[messageKey(swapID, kind)] = encoded

	return nil
}

// ReceiveMessage implements Store.
func (m *Memory) ReceiveMessage(_ context.Context, swapID, kind string) (*Message, error) {
	if err := ValidateSwapID(swapID); err != nil {
		return nil, err
	}

	m.mu.Lock()
	data, found := m.messages[messageKey(swapID, kind)]
	m.mu.Unlock()

	if !found {
		return nil, fmt.Errorf("No %s message: %s: %w", kind, swapID, ErrNotFound)
	}

	message, err := decodeMessage(data, kind)

	if err != nil {
		return nil, err
	}

	return message, nil
}

// ListMessages implements Store.
func (m *Memory) ListMessages(_ context.Context, swapID string) ([]string, error) {
	if err := ValidateSwapID(swapID); err != nil {
		return nil, err
	}

	prefix := swapID + "."

	m.mu.Lock()
	defer m.mu.Unlock()

	var kinds []string

	for key := range m.messages {
		if strings.HasPrefix(key, prefix) {
			kinds = append(kinds, strings.TrimPrefix(key, prefix))
		}
	}

	sort.Strings(kinds)

	return kinds, nil
}

// StoreRawMessage stores an envelope verbatim, so a test can present the parser
// with a spool file as a crash or a foreign writer left it.
func (m *Memory) StoreRawMessage(swapID, kind string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.messages[messageKey(swapID, kind)] = data
}

// StoredMessages returns every stored message key, sorted. It exists so a test
// can prove a write happened rather than inferring it from a read.
func (m *Memory) StoredMessages() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	keys := make([]string, 0, len(m.messages))

	for key := range m.messages {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}

// decodeMessage parses a stored envelope.
//
// The sender identity is restored as transport metadata, exactly as the Nostr
// backend attaches it after signature verification, so the identity-binding code
// above this layer runs against one shape on both transports.
func decodeMessage(data []byte, kind string) (*Message, error) {
	var envelope fileEnvelope

	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("invalid transport message: %w", err)
	}

	if envelope.Version == 2 && envelope.Message != nil {
		if err := ValidateSender(envelope.SenderPubkey); err != nil {
			return nil, err
		}

		body := make(map[string]any, len(envelope.Message)+1)

		for key, value := range envelope.Message {
			body[key] = value
		}

		body["_nostr_pubkey"] = envelope.SenderPubkey

		return &Message{Kind: kind, Sender: envelope.SenderPubkey, Body: body}, nil
	}

	// A message written before participant identity was recorded is still a
	// message: the schema above it does not depend on the metadata.
	if envelope.Message != nil {
		return &Message{Kind: kind, Body: envelope.Message}, nil
	}

	return nil, fmt.Errorf("invalid transport message: no body")
}
