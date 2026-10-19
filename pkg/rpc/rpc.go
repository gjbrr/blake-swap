// Package rpc is the chain node transport: JSON-RPC calls with a bounded,
// allowlisted retry policy.
//
// It is the port of run_chain_cli's transport concerns, adapted from
// subprocess-plus-bitcoin-cli to HTTP JSON-RPC, which is what a browser-backed
// client needs (the browser talks to the app server, the app server talks to the
// node). The policy is unchanged and is the important part:
//
//   - only a read-only method may be retried. Retrying a state-changing call
//     after a timeout can fund a swap twice, so the set is an allowlist and an
//     unclassified method gets no retry at all; and
//   - a failure is only retried when it is transient (a timeout, a refused or
//     reset connection, an HTTP gateway error). A node that answers with a
//     protocol error has answered.
package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Error is a node-side protocol error: the node answered, and the answer was
// no. It is never retried.
type Error struct {
	Code    int
	Message string
	Method  string
	Data    string
}

// Error renders the node's answer. It does not repeat the method: every caller
// that reports one of these already prefixes it with the call it made, and
// "getnewaddress: getnewaddress: node error -18" is a worse sentence than the same
// fact said once.
func (e *Error) Error() string {
	out := fmt.Sprintf("node error %d: %s", e.Code, e.Message)

	if e.Data != "" {
		out += " (" + e.Data + ")"
	}

	return out
}

// TransientError is a transport failure that is worth retrying.
type TransientError struct {
	Err error
}

func (e *TransientError) Error() string { return e.Err.Error() }

func (e *TransientError) Unwrap() error { return e.Err }

// Transient wraps err as retryable.
func Transient(err error) error {
	if err == nil {
		return nil
	}

	return &TransientError{Err: err}
}

// IsTransient reports whether err is worth retrying.
func IsTransient(err error) bool {
	var transient *TransientError

	if errors.As(err, &transient) {
		return true
	}

	// Context deadlines and cancellations are not the node's fault and are not
	// retried: the caller asked to stop.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	return false
}

// Transport sends one JSON-RPC request body and returns the response body.
//
// It exists so the retry policy can be tested without a node, and so a browser
// bridge can implement it over postMessage later.
type Transport interface {
	Do(ctx context.Context, body []byte) ([]byte, error)
}

// retryable is the allowlist of methods that may be retried automatically.
//
// Everything here is a read of chain, wallet, mempool or descriptor state, or a
// deterministic local transformation. This is copied deliberately from the
// Python engine's RETRYABLE_RPC_METHODS so the two implementations agree about
// which calls are safe to repeat.
var retryable = map[string]bool{
	// Chain reads.
	"getblockcount":     true,
	"getblockhash":      true,
	"getblock":          true,
	"getrawtransaction": true,
	"gettxout":          true,
	"getmempoolentry":   true,
	"getmempoolinfo":    true,
	// Fee, descriptor and address queries.
	"estimatesmartfee":  true,
	"getdescriptorinfo": true,
	"deriveaddresses":   true,
	"getaddressinfo":    true,
	// Wallet reads.
	"gettransaction":   true,
	"getbalances":      true,
	"getwalletinfo":    true,
	"listwallets":      true,
	"listtransactions": true,
	"listunspent":      true,
	"listdescriptors":  true,
	// Pure construction and decoding: no node state is involved.
	"decoderawtransaction": true,
	"createrawtransaction": true,
	"converttopsbt":        true,
	"finalizepsbt":         true,
	"utxoupdatepsbt":       true,
	// Deterministic local signing of a PSBT the caller already has.
	"walletprocesspsbt": true,
}

// Retryable reports whether a method is on the allowlist.
func Retryable(method string) bool { return retryable[method] }

// RetryableMethods returns the allowlist, for documentation and tests.
func RetryableMethods() []string {
	out := make([]string, 0, len(retryable))

	for method := range retryable {
		out = append(out, method)
	}

	return out
}

// Client is a JSON-RPC client with the retry policy.
type Client struct {
	Transport Transport

	// MaxAttempts is the total number of attempts, including the first.
	// A value below 1 is treated as 1.
	MaxAttempts int

	// Backoff is the delay before the second attempt; it doubles after that,
	// up to MaxBackoff.
	Backoff    time.Duration
	MaxBackoff time.Duration

	// Sleep is injectable so tests do not wait.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Defaults matching the Python engine.
const (
	DefaultMaxAttempts = 3
	DefaultBackoff     = 1 * time.Second
	DefaultMaxBackoff  = 30 * time.Second
)

func (c *Client) normalize() {
	if c.MaxAttempts < 1 {
		c.MaxAttempts = DefaultMaxAttempts
	}

	if c.Backoff <= 0 {
		c.Backoff = DefaultBackoff
	}

	if c.MaxBackoff <= 0 {
		c.MaxBackoff = DefaultMaxBackoff
	}

	if c.Sleep == nil {
		c.Sleep = sleepContext
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Call performs one JSON-RPC call, retrying only when both the method and the
// failure are retryable.
func (c *Client) Call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	if c.Transport == nil {
		return nil, fmt.Errorf("%s: no transport configured", method)
	}

	c.normalize()

	if params == nil {
		params = []any{}
	}

	//
	// The allowlist is enforced twice on purpose: once by capping the loop to
	// a single attempt, and once in the loop's break condition. Either alone
	// would do, and removing either alone leaves the other covering for it,
	// which is what a defence-in-depth pair should look like for a rule whose
	// violation funds a swap twice.
	//
	attempts := 1

	if Retryable(method) {
		attempts = c.MaxAttempts
	}

	var lastErr error

	delay := c.Backoff

	for attempt := 1; attempt <= attempts; attempt++ {
		body, err := requestBody(method, params)

		if err != nil {
			return nil, err
		}

		response, err := c.Transport.Do(ctx, body)

		if err == nil {
			result, parseErr := parseResponse(method, response)

			if parseErr == nil {
				return result, nil
			}

			err = parseErr
		}

		lastErr = err

		if !Retryable(method) || !IsTransient(err) || attempt == attempts {
			break
		}

		if err := c.Sleep(ctx, delay); err != nil {
			return nil, fmt.Errorf("%s: %w", method, err)
		}

		delay *= 2

		if delay > c.MaxBackoff {
			delay = c.MaxBackoff
		}
	}

	if !Retryable(method) {
		return nil, fmt.Errorf(
			"%s: %w (this call can change state, so it is NOT retried: a call that timed out may have succeeded; check the node before repeating it)",
			method, lastErr,
		)
	}

	return nil, fmt.Errorf("%s: %w", method, lastErr)
}

// requestBody builds one JSON-RPC request.
func requestBody(method string, params []any) ([]byte, error) {
	payload := map[string]any{
		"jsonrpc": "1.0",
		"id":      method,
		"method":  method,
		"params":  params,
	}

	var buffer bytes.Buffer

	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(payload); err != nil {
		return nil, fmt.Errorf("%s: encode request: %w", method, err)
	}

	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}

// parseResponse decodes a JSON-RPC response and maps a node error.
func parseResponse(method string, response []byte) (json.RawMessage, error) {
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    string `json:"data"`
		} `json:"error"`
	}

	decoder := json.NewDecoder(bytes.NewReader(response))
	decoder.UseNumber()

	if err := decoder.Decode(&envelope); err != nil {
		return nil, Transient(fmt.Errorf(
			"%s: node returned a response that is not JSON: %w", method, err))
	}

	if envelope.Error != nil {
		return nil, &Error{
			Code:    envelope.Error.Code,
			Message: envelope.Error.Message,
			Method:  method,
			Data:    envelope.Error.Data,
		}
	}

	if len(envelope.Result) == 0 {
		return nil, fmt.Errorf("%s: node returned neither a result nor an error", method)
	}

	return envelope.Result, nil
}

// HTTPTransport talks to a node's JSON-RPC port.
type HTTPTransport struct {
	// URL is the node's RPC endpoint.
	URL string
	// User and Password are basic-auth credentials; both may be empty when the
	// node is reached through a local proxy that holds them.
	User     string
	Password string
	// Timeout is the per-attempt deadline. Zero means no client-side deadline
	// (the caller's context still applies).
	Timeout time.Duration
	// Client is optional; a default is used when nil.
	Client *http.Client
}

// Do posts one request.
func (t *HTTPTransport) Do(ctx context.Context, body []byte) ([]byte, error) {
	if t.URL == "" {
		return nil, fmt.Errorf("rpc: no node URL configured")
	}

	client := t.Client

	if client == nil {
		client = &http.Client{}

		if t.Timeout > 0 {
			client.Timeout = t.Timeout
		}
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(body))

	if err != nil {
		return nil, fmt.Errorf("rpc: build request: %w", err)
	}

	request.Header.Set("Content-Type", "application/json")

	if t.User != "" || t.Password != "" {
		request.SetBasicAuth(t.User, t.Password)
	}

	response, err := client.Do(request)

	if err != nil {
		// A transport-level failure is retryable: the node was not reachable
		// right now, or it stalled.
		return nil, Transient(err)
	}

	defer response.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))

	if err != nil {
		return nil, Transient(err)
	}

	switch {
	case response.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("rpc: node rejected the credentials (401)")
	case response.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("rpc: node refused the request (403)")
	case response.StatusCode >= 500:
		// Gateway and proxy errors are worth retrying; a node that answers
		// with a protocol error body is handled below and is not.
		if !looksLikeJSON(payload) {
			return nil, Transient(fmt.Errorf(
				"rpc: node returned HTTP %d", response.StatusCode))
		}
	case response.StatusCode >= 400:
		return nil, fmt.Errorf("rpc: node returned HTTP %d", response.StatusCode)
	}

	// Bitcoin Core answers JSON-RPC errors with HTTP 500, so the body decides
	// whether this was a protocol error.
	return payload, nil
}

func looksLikeJSON(payload []byte) bool {
	trimmed := bytes.TrimSpace(payload)

	return len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')
}

// String renders a call's params for diagnostics.
func String(params ...any) string {
	parts := make([]string, 0, len(params))

	for _, param := range params {
		parts = append(parts, fmt.Sprint(param))
	}

	return strings.Join(parts, ", ")
}
