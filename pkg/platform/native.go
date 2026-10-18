//go:build !js

package platform

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// HTTP is the host implementation of rpc.Transport: a plain HTTP POST to a
// JSON-RPC endpoint.
//
// The browser needs the same thing over fetch, which is why the engine depends on the
// transport interface rather than on net/http. The host version exists so the same
// code paths can run outside a browser, and so a proxy can be exercised in a test.
type HTTP struct {
	// URL is the JSON-RPC endpoint, including the wallet path.
	URL string
	// User and Password are basic-auth credentials. A browser-backed client
	// leaves them empty and goes through the app server's proxy.
	User     string
	Password string
	// Timeout bounds one request.
	Timeout time.Duration
	// Client is an optional transport; nil uses a client with Timeout applied.
	Client *http.Client
}

// Do implements rpc.Transport.
func (h *HTTP) Do(ctx context.Context, body []byte) ([]byte, error) {
	client := h.Client

	if client == nil {
		timeout := h.Timeout

		if timeout <= 0 {
			timeout = 30 * time.Second
		}

		client = &http.Client{Timeout: timeout}
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))

	if err != nil {
		return nil, err
	}

	request.Header.Set("Content-Type", "application/json")

	if h.User != "" || h.Password != "" {
		request.SetBasicAuth(h.User, h.Password)
	}

	response, err := client.Do(request)

	if err != nil {
		return nil, err
	}

	defer func() { _ = response.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))

	if err != nil {
		return nil, err
	}

	//
	// A non-2xx answer is still returned to the caller: a JSON-RPC node reports
	// errors in the body with an HTTP status, and the transport's job is to hand the
	// body over rather than to decide what it means.
	//
	if response.StatusCode >= 400 && len(payload) == 0 {
		return nil, fmt.Errorf("rpc: HTTP %d with an empty body", response.StatusCode)
	}

	return payload, nil
}

// MemoryKV is a KV in memory.
//
// It is used by tests and by a session-only vault: a client that has not yet been
// unlocked for durable storage should still work, and losing that state on reload is
// exactly the behaviour it asked for.
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
