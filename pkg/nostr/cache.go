package nostr

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/gjbrr/blake-swap/pkg/event"
)

// Cache keeps our own signed events and the events we have verified.
//
// It exists for a specific reason: a relay's acceptance is not storage. Keeping
// our exact signed event before publishing means the coordination record survives
// a relay that drops it, and caching what we verified means a later read does not
// depend on the same relay answering again.
//
// The interface is what makes that possible on both platforms: a directory on a
// host, an encrypted record in the browser.
type Cache interface {
	Put(ctx context.Context, e event.Event) error
	Get(ctx context.Context, id string) (event.Event, bool, error)
	IDs(ctx context.Context) ([]string, error)
}

// MemoryCache is a Cache in memory.
type MemoryCache struct {
	mu     sync.Mutex
	events map[string]event.Event
}

// NewMemoryCache returns an empty cache.
func NewMemoryCache() *MemoryCache {
	return &MemoryCache{events: map[string]event.Event{}}
}

// Put verifies an event before storing it.
//
// A cache that stored unverified events would be a way into the engine: the read
// path trusts the cache, so the write path has to be the place verification
// happens.
func (m *MemoryCache) Put(_ context.Context, e event.Event) error {
	if err := event.Verify(e, time.Now()); err != nil {
		return fmt.Errorf("refusing to cache an event that does not verify: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.events[e.ID] = e

	return nil
}

// Get implements Cache.
func (m *MemoryCache) Get(_ context.Context, id string) (event.Event, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, found := m.events[id]

	return e, found, nil
}

// IDs implements Cache.
func (m *MemoryCache) IDs(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ids := make([]string, 0, len(m.events))

	for id := range m.events {
		ids = append(ids, id)
	}

	return ids, nil
}

// randomSuffix returns a subscription suffix.
//
// A subscription id distinguishes concurrent reads on one connection; it carries
// no protocol meaning, and a collision only costs a confused read, so eight
// random bytes are plenty.
func randomSuffix() string {
	var buffer [8]byte

	if _, err := rand.Read(buffer[:]); err != nil {
		return "0000000000000000"
	}

	return hex.EncodeToString(buffer[:])
}
