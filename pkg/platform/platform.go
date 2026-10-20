// Package platform is the browser's side of the world, behind interfaces.
//
// Every capability the client needs that a browser provides differently from a
// host is here and nowhere else: the clock, the log, durable storage, an HTTP
// transport, and a WebSocket to a relay. The engine and the market layer depend on
// the interfaces, so the same code runs on a host (where the implementations are
// ordinary Go) and in a wasm worker (where they are JavaScript bridges).
//
// The interfaces are small on purpose. A seam that offers more than the code above it
// uses is a seam that will grow one convenience at a time until the two platforms
// disagree about something that mattered.
package platform

import (
	"context"
	"sync"
	"time"
)

// Clock is the time source.
//
// It is an interface because two clocks that disagree by minutes make two
// participants disagree about whether an offer expired, and a test needs to move
// time without sleeping.
type Clock interface {
	Now() time.Time
}

// Console is the log sink.
type Console interface {
	Log(message string)
}

// KV is durable key-value storage.
//
// The browser implementation is IndexedDB, which is asynchronous, which is why every
// method takes a context: a synchronous API here would be a lie that the browser
// cannot keep.
type KV interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Put(ctx context.Context, key string, value []byte) error
	Delete(ctx context.Context, key string) error
	// Keys returns the keys with a prefix, sorted, so iteration is deterministic
	// on both platforms.
	Keys(ctx context.Context, prefix string) ([]string, error)
	// ReplaceAll makes the store hold exactly these entries and nothing else.
	//
	// It is a batch form of Put rather than a second storage API, and it exists for one
	// caller: restoring a vault backup. A restore done as a loop of Put and Delete calls
	// has a state in the middle that is neither the old vault nor the new one, and a
	// browser tab that dies there leaves records sealed with a key that is no longer in
	// the store. An implementation either commits every entry or leaves the old contents
	// untouched.
	ReplaceAll(ctx context.Context, entries map[string][]byte) error
}

// Reader is an io.Reader over the platform's entropy source.
//
// It exists so a caller that wants randomness as a stream — a salt, a nonce — gets it
// from the same place everything else on that platform does.
type Reader struct{}

// Read fills p from the platform's cryptographic random source.
func (Reader) Read(p []byte) (int, error) { return readRandom(p) }

// MemoryKV is a KV in memory.
//
// It is used by tests and by a session-only vault: a client that has not asked for
// durable storage should still work, and losing that state with the session is exactly
// what it asked for. The worker's self-test uses it so a browser check never touches a
// user's vault.
type MemoryKV struct {
	mu    sync.Mutex
	items map[string][]byte
}

// NewMemoryKV returns an empty store.
func NewMemoryKV() *MemoryKV { return &MemoryKV{items: map[string][]byte{}} }

// Get implements KV.
func (m *MemoryKV) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	value, found := m.items[key]

	return value, found, nil
}

// Put implements KV.
func (m *MemoryKV) Put(_ context.Context, key string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	copied := make([]byte, len(value))
	copy(copied, value)
	m.items[key] = copied

	return nil
}

// Delete implements KV.
func (m *MemoryKV) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.items, key)

	return nil
}

// ReplaceAll implements KV.
//
// The new map is built first and swapped in under the lock, so a caller that fails while
// building it (it cannot, here, but an implementation that can must) leaves the old one
// untouched, and a reader never sees a half-replaced store.
func (m *MemoryKV) ReplaceAll(_ context.Context, entries map[string][]byte) error {
	replacement := make(map[string][]byte, len(entries))

	for key, value := range entries {
		copied := make([]byte, len(value))
		copy(copied, value)

		replacement[key] = copied
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.items = replacement

	return nil
}

// Keys implements KV.
func (m *MemoryKV) Keys(_ context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	keys := make([]string, 0, len(m.items))

	for key := range m.items {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			keys = append(keys, key)
		}
	}

	sortStrings(keys)

	return keys, nil
}

// ReadRandom fills p from the platform's cryptographic random source.
//
// It is a function rather than an io.Reader so the browser implementation can use
// WebCrypto, where a wasm build's own crypto/rand has no entropy guarantee.
func ReadRandom(p []byte) (int, error) { return readRandom(p) }

// RealClock is the system clock.
//
// It is a distinct type rather than a function so it can be embedded and so a zero
// RealClock is usable.
type RealClock struct{}

// Now implements Clock.
func (RealClock) Now() time.Time { return time.Now() }

// FixedClock is a clock that does not move unless it is told to.
type FixedClock struct {
	Current time.Time
}

// Now implements Clock.
func (c *FixedClock) Now() time.Time { return c.Current }

// Advance moves a fixed clock forward.
func (c *FixedClock) Advance(d time.Duration) { c.Current = c.Current.Add(d) }

// PrintConsole writes to the platform's standard logger.
type PrintConsole struct{}

// Log implements Console.
func (PrintConsole) Log(message string) { println(message) }

// sortStrings sorts in place.
//
// Both platforms must hand keys back in the same order, or a scan over records would
// depend on which platform it ran on.
func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
