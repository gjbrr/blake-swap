// Package client is the browser client's state layer: a store.Store whose records
// are encrypted before they reach IndexedDB.
//
// The point of the encryption is narrow and worth stating plainly. A browser has no
// filesystem and no OS key store, so the swap records — which include the swap secret,
// the value that can move funds — would otherwise sit in IndexedDB in the clear, readable
// by anything that can run script on the origin. So each record is sealed with a key
// derived from a user passphrase, and the storage key is bound into the plaintext so a
// sealed record cannot be moved to another record's slot and still open.
//
// Everything above this layer sees the ordinary store.Store interface: the engine does
// not know whether a record is on a disk or in an encrypted browser database.
package client

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/store"
	"github.com/gjbrr/blake-swap/pkg/vault"
)

// KV is the key-value surface the encrypted store needs.
//
// It is an interface because the durable implementation is IndexedDB behind a
// JavaScript bridge, and a test should not need a browser to prove that a record
// round-trips, that a wrong passphrase fails, or that tampering is detected.
type KV interface {
	// Get returns a value, or false when the key is absent.
	Get(ctx context.Context, key string) ([]byte, bool, error)
	// Put stores a value.
	Put(ctx context.Context, key string, value []byte) error
	// Delete removes a value.
	Delete(ctx context.Context, key string) error
	// Keys returns every key with the given prefix, sorted.
	Keys(ctx context.Context, prefix string) ([]string, error)
}

// Encrypted is a store.Store over an encrypted key-value store.
type Encrypted struct {
	kv  KV
	key []byte
	// header is the KDF parameters, stored so the vault can be reopened later.
	header vault.Params
	// random is the entropy source for salt and nonces. Nil means crypto/rand on a
	// host; the browser passes WebCrypto, because a wasm build's own crypto/rand is
	// not an entropy guarantee this code can make.
	random io.Reader
}

// entropy returns the store's random source.
func (e *Encrypted) entropy() io.Reader {
	if e.random != nil {
		return e.random
	}

	return rand.Reader
}

// Key layout. The prefixes keep the vault's own metadata out of the record and
// message scans, which is what lets the store's List see exactly the records.
const (
	recordPrefix  = "record/"
	rolePrefix    = "record-role/"
	messagePrefix = "message/"
	headerKey     = "vault/header"
	verifierKey   = "vault/verifier"
)

// Create initialises a new encrypted store and returns it unlocked.
//
// The verifier is a sealed constant: opening a vault with the wrong passphrase must fail
// here, at unlock time, rather than later when the first record fails to decrypt and the
// operator is left wondering which of their records is corrupt.
func Create(ctx context.Context, kv KV, passphrase string) (*Encrypted, error) {
	return CreateWith(ctx, kv, passphrase, rand.Reader)
}

// CreateWith is Create with an explicit entropy source.
func CreateWith(ctx context.Context, kv KV, passphrase string, random io.Reader) (*Encrypted, error) {
	if passphrase == "" {
		return nil, fmt.Errorf("a vault needs a passphrase")
	}

	params, err := vault.NewParams(random)

	if err != nil {
		return nil, err
	}

	key, err := vault.DeriveKey(passphrase, params)

	if err != nil {
		return nil, err
	}

	encodedHeader, err := params.MarshalHeader()

	if err != nil {
		return nil, err
	}

	verifier, err := vault.Seal(key, verifierKey, []byte("blake-swap vault v1"), random)

	if err != nil {
		return nil, err
	}

	if err := kv.Put(ctx, headerKey, encodedHeader); err != nil {
		return nil, err
	}

	if err := kv.Put(ctx, verifierKey, []byte(verifier)); err != nil {
		return nil, err
	}

	return &Encrypted{kv: kv, key: key, header: params}, nil
}

// Open unlocks an existing encrypted store.
func Open(ctx context.Context, kv KV, passphrase string) (*Encrypted, error) {
	return OpenWith(ctx, kv, passphrase, rand.Reader)
}

// OpenWith is Open with an explicit entropy source, for later writes.
func OpenWith(ctx context.Context, kv KV, passphrase string, random io.Reader) (*Encrypted, error) {
	encodedHeader, found, err := kv.Get(ctx, headerKey)

	if err != nil {
		return nil, err
	}

	if !found {
		return nil, fmt.Errorf("no vault in this store")
	}

	params, err := vault.UnmarshalHeader(encodedHeader)

	if err != nil {
		return nil, err
	}

	key, err := vault.DeriveKey(passphrase, params)

	if err != nil {
		return nil, err
	}

	encodedVerifier, found, err := kv.Get(ctx, verifierKey)

	if err != nil {
		return nil, err
	}

	if !found {
		return nil, fmt.Errorf("the vault has no verifier")
	}

	if _, err := vault.Open(key, verifierKey, string(encodedVerifier)); err != nil {
		return nil, fmt.Errorf("wrong passphrase (the vault verifier did not open)")
	}

	return &Encrypted{kv: kv, key: key, header: params, random: random}, nil
}

// Exists reports whether a vault is present in this store.
func Exists(ctx context.Context, kv KV) (bool, error) {
	_, found, err := kv.Get(ctx, headerKey)

	return found, err
}

// Header returns the vault's KDF parameters, for display and for migration.
func (e *Encrypted) Header() vault.Params { return e.header }

// Save implements store.Store.
//
// The role is not decorative here. A vault holds one participant's records, and a
// participant is Alice in one swap and Bob in another; the sealed record is keyed by swap
// id alone, so a save that disagreed with the role already recorded for that swap would
// overwrite one participant's record with the other's view of the same swap. The role tag
// is written on the first save and checked on every later one.
//
// A vault written before the tag existed has no tag, and the first save of an existing
// record writes it: that is the migration, and it is deliberate rather than silent,
// because the alternative is refusing to open a vault that worked yesterday.
func (e *Encrypted) Save(ctx context.Context, role string, swap *model.Swap) error {
	if err := store.ValidateRole(role); err != nil {
		return err
	}

	if swap == nil {
		return fmt.Errorf("swap is nil")
	}

	if err := swap.Validate(); err != nil {
		return err
	}

	recorded, found, err := e.get(ctx, rolePrefix+swap.SwapID)

	if err != nil {
		return err
	}

	if found && string(recorded) != role {
		return fmt.Errorf(
			"SECURITY FAILURE: swap %s is stored as protocol %s; refusing to save it as %s",
			swap.SwapID, string(recorded), role,
		)
	}

	encoded, err := swap.Marshal()

	if err != nil {
		return err
	}

	if err := e.put(ctx, recordPrefix+swap.SwapID, encoded); err != nil {
		return err
	}

	if !found {
		if err := e.put(ctx, rolePrefix+swap.SwapID, []byte(role)); err != nil {
			return err
		}
	}

	return nil
}

// Role is the protocol role this vault recorded for one swap.
//
// An empty role with a nil error means the record predates the tag. The caller falls back
// to the record's identity pins, which are the authority anyway; the tag exists so that a
// save cannot silently change sides.
func (e *Encrypted) Role(ctx context.Context, swapID string) (string, error) {
	if err := store.ValidateSwapID(swapID); err != nil {
		return "", err
	}

	recorded, found, err := e.get(ctx, rolePrefix+swapID)

	if err != nil {
		return "", err
	}

	if !found {
		return "", nil
	}

	return string(recorded), nil
}

// Load implements store.Store.
func (e *Encrypted) Load(ctx context.Context, swapID string) (*model.Swap, error) {
	if err := store.ValidateSwapID(swapID); err != nil {
		return nil, err
	}

	key := recordPrefix + swapID

	plaintext, found, err := e.get(ctx, key)

	if err != nil {
		return nil, err
	}

	if !found {
		return nil, fmt.Errorf("%s record %s: %w", swapID, swapID, store.ErrNotFound)
	}

	return model.Decode(plaintext, nil)
}

// List implements store.Store.
func (e *Encrypted) List(ctx context.Context) ([]string, error) {
	keys, err := e.kv.Keys(ctx, recordPrefix)

	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(keys))

	for _, key := range keys {
		ids = append(ids, key[len(recordPrefix):])
	}

	return ids, nil
}

// Retired implements store.Store.
//
// A retired record lives under the archive prefix, which is where Archive moves it; the
// sealed value is unchanged, so retiring and restoring are both cheap.
func (e *Encrypted) Retired(ctx context.Context, swapID string) (bool, error) {
	_, found, err := e.kv.Get(ctx, archivePrefix+swapID)

	return found, err
}

// Archive implements store.Store.
func (e *Encrypted) Archive(ctx context.Context, role, swapID string) error {
	if err := store.ValidateRole(role); err != nil {
		return err
	}

	sealed, found, err := e.kv.Get(ctx, recordPrefix+swapID)

	if err != nil {
		return err
	}

	if !found {
		return fmt.Errorf("%s: %w", swapID, store.ErrNotFound)
	}

	if err := e.kv.Put(ctx, archivePrefix+swapID, sealed); err != nil {
		return err
	}

	// The role tag follows the record: an archived swap that is saved again must still
	// be the same participant's record.
	if recorded, found, err := e.get(ctx, rolePrefix+swapID); err == nil && found {
		if err := e.put(ctx, rolePrefix+swapID, recorded); err != nil {
			return err
		}
	}

	return e.kv.Delete(ctx, recordPrefix+swapID)
}

// SendMessage implements store.Store.
func (e *Encrypted) SendMessage(
	ctx context.Context, swapID, kind, sender string, body map[string]any,
) error {
	if err := store.ValidateSwapID(swapID); err != nil {
		return err
	}

	if err := store.ValidateSender(sender); err != nil {
		return err
	}

	if body == nil {
		return fmt.Errorf("a transport message must have a body")
	}

	envelope := map[string]any{
		"_file_transport_version": 2,
		"sender_pubkey":           sender,
		"message":                 body,
	}

	encoded, err := json.Marshal(envelope)

	if err != nil {
		return err
	}

	return e.put(ctx, messagePrefix+swapID+"."+kind, encoded)
}

// ReceiveMessage implements store.Store.
func (e *Encrypted) ReceiveMessage(ctx context.Context, swapID, kind string) (*store.Message, error) {
	if err := store.ValidateSwapID(swapID); err != nil {
		return nil, err
	}

	key := messagePrefix + swapID + "." + kind

	plaintext, found, err := e.get(ctx, key)

	if err != nil {
		return nil, err
	}

	if !found {
		return nil, fmt.Errorf("No %s message: %s: %w", kind, swapID, store.ErrNotFound)
	}

	var envelope struct {
		Version int            `json:"_file_transport_version"`
		Sender  string         `json:"sender_pubkey"`
		Message map[string]any `json:"message"`
	}

	if err := json.Unmarshal(plaintext, &envelope); err != nil {
		return nil, fmt.Errorf("invalid transport message: %w", err)
	}

	if envelope.Message == nil {
		return nil, fmt.Errorf("invalid transport message: no body")
	}

	if envelope.Sender != "" {
		if err := store.ValidateSender(envelope.Sender); err != nil {
			return nil, err
		}
	}

	body := make(map[string]any, len(envelope.Message)+1)

	for key, value := range envelope.Message {
		body[key] = value
	}

	if envelope.Sender != "" {
		body["_nostr_pubkey"] = envelope.Sender
	}

	return &store.Message{Kind: kind, Sender: envelope.Sender, Body: body}, nil
}

// ListMessages implements store.Store.
func (e *Encrypted) ListMessages(ctx context.Context, swapID string) ([]string, error) {
	if err := store.ValidateSwapID(swapID); err != nil {
		return nil, err
	}

	prefix := messagePrefix + swapID + "."

	keys, err := e.kv.Keys(ctx, prefix)

	if err != nil {
		return nil, err
	}

	kinds := make([]string, 0, len(keys))

	for _, key := range keys {
		kinds = append(kinds, key[len(prefix):])
	}

	return kinds, nil
}

// archivePrefix holds retired records.
const archivePrefix = "archive/"

// PutSecret seals an arbitrary named value.
//
// It exists for the two things the vault must hold besides swaps: the participant's
// Nostr key and the proxy address. Both are secrets of a sort — one is a signing key,
// the other reveals which node a participant uses — and both belong under the same
// passphrase.
func (e *Encrypted) PutSecret(ctx context.Context, name string, value []byte) error {
	if name == "" {
		return fmt.Errorf("a secret needs a name")
	}

	return e.put(ctx, secretPrefix+name, value)
}

// GetSecret reads a sealed named value.
func (e *Encrypted) GetSecret(ctx context.Context, name string) ([]byte, bool, error) {
	if name == "" {
		return nil, false, fmt.Errorf("a secret needs a name")
	}

	return e.get(ctx, secretPrefix+name)
}

// SecretNames lists the stored secret names.
func (e *Encrypted) SecretNames(ctx context.Context) ([]string, error) {
	keys, err := e.kv.Keys(ctx, secretPrefix)

	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(keys))

	for _, key := range keys {
		names = append(names, key[len(secretPrefix):])
	}

	return names, nil
}

// secretPrefix holds named sealed values.
const secretPrefix = "secret/"

// put seals a value and stores it.
func (e *Encrypted) put(ctx context.Context, key string, plaintext []byte) error {
	if e.key == nil {
		return fmt.Errorf("the vault is locked")
	}

	sealed, err := vault.Seal(e.key, key, plaintext, e.entropy())

	if err != nil {
		return err
	}

	return e.kv.Put(ctx, key, []byte(sealed))
}

// get loads and opens a sealed value.
func (e *Encrypted) get(ctx context.Context, key string) ([]byte, bool, error) {
	if e.key == nil {
		return nil, false, fmt.Errorf("the vault is locked")
	}

	sealed, found, err := e.kv.Get(ctx, key)

	if err != nil || !found {
		return nil, found, err
	}

	plaintext, err := vault.Open(e.key, key, string(sealed))

	if err != nil {
		//
		// A record that will not open is one of two things: tampered with, or sealed
		// under a different key. Both are reported against the record rather than as a
		// generic failure, because the operator needs to know which record is affected.
		//
		return nil, false, fmt.Errorf("record %q did not open: %w", key, err)
	}

	return plaintext, true, nil
}

// Memory is a KV in memory, for tests and for a session-only vault.
type Memory struct {
	mu    sync.Mutex
	items map[string][]byte
}

// NewMemory returns an empty in-memory KV.
func NewMemory() *Memory { return &Memory{items: map[string][]byte{}} }

// Get implements KV.
func (m *Memory) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	value, found := m.items[key]

	return value, found, nil
}

// Put implements KV.
func (m *Memory) Put(_ context.Context, key string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	copied := make([]byte, len(value))
	copy(copied, value)
	m.items[key] = copied

	return nil
}

// Delete implements KV.
func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.items, key)

	return nil
}

// Keys implements KV.
func (m *Memory) Keys(_ context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var keys []string

	for key := range m.items {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			keys = append(keys, key)
		}
	}

	sortStrings(keys)

	return keys, nil
}

// Raw returns the stored bytes for a key, so a test can prove what is on the "disk".
func (m *Memory) Raw(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	value, found := m.items[key]

	return value, found
}

// sortStrings sorts in place.
func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
