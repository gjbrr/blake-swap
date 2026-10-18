//go:build !js

package platform

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gjbrr/blake-swap/pkg/rpc"
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

// NewTransport returns the host's transport for one JSON-RPC endpoint.
//
// This is the factory the chain package uses, so the choice of transport is a property
// of the platform rather than a decision each caller makes and one of them gets wrong.
func NewTransport(url, user, password string, timeout time.Duration) rpc.Transport {
	return &HTTP{URL: url, User: user, Password: password, Timeout: timeout}
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

// readRandom fills p from the system entropy source.
func readRandom(p []byte) (int, error) { return rand.Read(p) }
