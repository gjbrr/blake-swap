package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// scriptedTransport returns queued outcomes, so retry behaviour is tested
// without a node or a clock.
type scriptedTransport struct {
	outcomes []outcome
	calls    int
	seen     [][]byte
}

type outcome struct {
	body []byte
	err  error
}

func (s *scriptedTransport) Do(_ context.Context, body []byte) ([]byte, error) {
	s.seen = append(s.seen, body)

	if s.calls >= len(s.outcomes) {
		return nil, errors.New("script exhausted")
	}

	result := s.outcomes[s.calls]
	s.calls++

	return result.body, result.err
}

func noSleep(_ context.Context, _ time.Duration) error { return nil }

func jsonResult(value string) []byte {
	return []byte(`{"result":` + value + `,"error":null}`)
}

func client(t *testing.T, transport Transport) *Client {
	t.Helper()

	return &Client{
		Transport:   transport,
		MaxAttempts: 3,
		Backoff:     time.Millisecond,
		MaxBackoff:  time.Millisecond,
		Sleep:       noSleep,
	}
}

func TestCallSucceeds(t *testing.T) {
	body := jsonResult(`35571`)
	transport := &scriptedTransport{outcomes: []outcome{{body: body}}}

	result, err := client(t, transport).Call(context.Background(), "getblockcount")

	if err != nil {
		t.Fatal(err)
	}

	if string(result) != "35571" {
		t.Fatalf("result = %s", result)
	}

	if transport.calls != 1 {
		t.Fatalf("calls = %d", transport.calls)
	}

	// The request is JSON-RPC with the method and an empty params array.
	var request map[string]any

	if err := json.Unmarshal(transport.seen[0], &request); err != nil {
		t.Fatal(err)
	}

	if request["method"] != "getblockcount" {
		t.Fatalf("method = %v", request["method"])
	}

	if _, ok := request["params"].([]any); !ok {
		t.Fatalf("params = %v (%T)", request["params"], request["params"])
	}
}

func TestReadIsRetriedOnTransientFailure(t *testing.T) {
	transport := &scriptedTransport{outcomes: []outcome{
		{err: Transient(errors.New("connection refused"))},
		{err: Transient(errors.New("connection reset"))},
		{body: jsonResult(`35571`)},
	}}

	result, err := client(t, transport).Call(context.Background(), "getblockcount")

	if err != nil {
		t.Fatal(err)
	}

	if string(result) != "35571" {
		t.Fatalf("result = %s", result)
	}

	if transport.calls != 3 {
		t.Fatalf("calls = %d, want 3", transport.calls)
	}
}

func TestRetriesAreBounded(t *testing.T) {
	transport := &scriptedTransport{outcomes: []outcome{
		{err: Transient(errors.New("down"))},
		{err: Transient(errors.New("down"))},
		{err: Transient(errors.New("down"))},
		{body: jsonResult(`1`)},
	}}

	_, err := client(t, transport).Call(context.Background(), "getblockcount")

	if err == nil {
		t.Fatal("a permanently failing call succeeded")
	}

	if transport.calls != 3 {
		t.Fatalf("calls = %d, want 3 (max attempts)", transport.calls)
	}
}

func TestStateChangingCallIsNeverRetried(t *testing.T) {
	transport := &scriptedTransport{outcomes: []outcome{
		{err: Transient(errors.New("connection reset"))},
		{body: jsonResult(`"txid"`)},
	}}

	_, err := client(t, transport).Call(context.Background(), "sendtoaddress", "addr", 0.1)

	if err == nil {
		t.Fatal("a failing sendtoaddress succeeded")
	}

	if transport.calls != 1 {
		t.Fatalf("calls = %d, want 1: a state-changing call must not be retried", transport.calls)
	}

	// And the refusal must explain the ambiguity rather than looking like a
	// generic failure.
	message := err.Error()

	for _, want := range []string{"sendtoaddress", "NOT retried", "may have succeeded"} {
		if !strings.Contains(message, want) {
			t.Errorf("error %q is missing %q", message, want)
		}
	}
}

func TestUnknownMethodIsNotRetried(t *testing.T) {
	if Retryable("someNewMutatingCall") {
		t.Fatal("an unclassified method is retryable")
	}

	transport := &scriptedTransport{outcomes: []outcome{
		{err: Transient(errors.New("down"))},
		{body: jsonResult(`1`)},
	}}

	if _, err := client(t, transport).Call(context.Background(), "someNewMutatingCall"); err == nil {
		t.Fatal("the call succeeded")
	}

	if transport.calls != 1 {
		t.Fatalf("calls = %d, want 1", transport.calls)
	}
}

func TestTimeoutIsNotRetried(t *testing.T) {
	// The caller's own deadline is not a transient node failure.
	transport := &scriptedTransport{outcomes: []outcome{
		{err: context.DeadlineExceeded},
		{body: jsonResult(`1`)},
	}}

	if _, err := client(t, transport).Call(context.Background(), "getblockcount"); err == nil {
		t.Fatal("a deadline failure was retried into success")
	}

	if transport.calls != 1 {
		t.Fatalf("calls = %d, want 1", transport.calls)
	}
}

func TestNodeErrorIsNotRetried(t *testing.T) {
	transport := &scriptedTransport{outcomes: []outcome{
		{body: []byte(`{"result":null,"error":{"code":-5,"message":"No such mempool transaction"}}`)},
	}}

	_, err := client(t, transport).Call(context.Background(), "getrawtransaction", "aa")

	if err == nil {
		t.Fatal("a node error was returned as success")
	}

	var nodeErr *Error

	if !errors.As(err, &nodeErr) {
		t.Fatalf("error is %T, want *rpc.Error", err)
	}

	if nodeErr.Code != -5 || nodeErr.Method != "getrawtransaction" {
		t.Fatalf("node error = %+v", nodeErr)
	}

	if transport.calls != 1 {
		t.Fatalf("calls = %d: a node error is an answer, not a transient failure", transport.calls)
	}
}

func TestNonJSONResponseIsTransient(t *testing.T) {
	transport := &scriptedTransport{outcomes: []outcome{
		{body: []byte(`<html>bad gateway</html>`)},
		{body: jsonResult(`35571`)},
	}}

	result, err := client(t, transport).Call(context.Background(), "getblockcount")

	if err != nil {
		t.Fatal(err)
	}

	if string(result) != "35571" {
		t.Fatalf("result = %s", result)
	}
}

func TestMissingResultAndError(t *testing.T) {
	// An empty envelope is neither a result nor an error.
	transport := &scriptedTransport{outcomes: []outcome{
		{body: []byte(`{}`)},
	}}

	if _, err := client(t, transport).Call(context.Background(), "getblockcount"); err == nil {
		t.Fatal("an empty response was accepted")
	}

	// A null result is legitimate: several node methods return it.
	transport = &scriptedTransport{outcomes: []outcome{
		{body: []byte(`{"result":null,"error":null}`)},
	}}

	result, err := client(t, transport).Call(context.Background(), "getblockcount")

	if err != nil {
		t.Fatalf("a null result was refused: %v", err)
	}

	if string(result) != "null" {
		t.Fatalf("result = %s", result)
	}
}

func TestBackoffDoublesAndCaps(t *testing.T) {
	var delays []time.Duration

	transport := &scriptedTransport{outcomes: []outcome{
		{err: Transient(errors.New("down"))},
		{err: Transient(errors.New("down"))},
		{err: Transient(errors.New("down"))},
	}}

	c := &Client{
		Transport:   transport,
		MaxAttempts: 4,
		Backoff:     10 * time.Millisecond,
		MaxBackoff:  25 * time.Millisecond,
		Sleep: func(_ context.Context, d time.Duration) error {
			delays = append(delays, d)
			return nil
		},
	}

	if _, err := c.Call(context.Background(), "getblockcount"); err == nil {
		t.Fatal("the call succeeded")
	}

	want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 25 * time.Millisecond}

	if len(delays) != len(want) {
		t.Fatalf("delays = %v, want %v", delays, want)
	}

	for i := range want {
		if delays[i] != want[i] {
			t.Fatalf("delays = %v, want %v", delays, want)
		}
	}
}

func TestAllowlistShape(t *testing.T) {
	methods := RetryableMethods()

	if len(methods) == 0 {
		t.Fatal("the allowlist is empty")
	}

	// The state-changing methods the engine uses must never be on it.
	for _, method := range []string{
		"sendtoaddress", "sendrawtransaction", "importdescriptors",
		"createwallet", "loadwallet", "generatetoaddress", "sendall",
	} {
		if Retryable(method) {
			t.Errorf("%s is retryable", method)
		}
	}

	// And the reads it depends on must be.
	for _, method := range []string{
		"getblockcount", "getrawtransaction", "gettxout", "listunspent",
		"getdescriptorinfo", "walletprocesspsbt",
	} {
		if !Retryable(method) {
			t.Errorf("%s is not retryable", method)
		}
	}
}

func TestString(t *testing.T) {
	if got := String("a", 1, true); got != "a, 1, true" {
		t.Fatalf("String = %q", got)
	}
}
