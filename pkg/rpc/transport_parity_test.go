//go:build !js

package rpc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// Parity: tests/chain_cli_transport_tests.py
//
// The reference suite guards the subprocess boundary it uses to talk to a node:
// argv quoting, booleans rendered as JSON literals, a payload too large for argv
// switched to stdin, the exact argv/stdin cutoff, exec failure naming, and the
// retry policy around a stalled node.
//
// This port speaks JSON-RPC, so the argv half does not exist here and is waived
// below. What is portable is the part that is about *talking to a node* rather
// than about shelling out: a large request body, values that must keep their
// exact types, the retry allowlist and its backoff, the rule that a state-changing
// call is never retried, and the wording a refusal reaches an operator in.
//
// parityCase records one reference assertion and what proves it here.
type parityCase struct {
	// reference is the reference assertion's name, as its suite prints it.
	reference string
	// goTest names the Go test that proves the same thing, if one does.
	goTest string
	// waiver explains why no Go test can, when goTest is empty.
	waiver string
}

// transportParity is the disposition of every assertion in the reference suite.
func transportParity() []parityCase {
	return []parityCase{
		{
			reference: "ordinary arguments stay in argv",
			waiver: "this client renders no argv: it POSTs a JSON-RPC body to the " +
				"node's HTTP endpoint, so argument quoting cannot be wrong. The " +
				"request body's shape is proved by TestLargeRequestBodyIsAccepted " +
				"and TestValuesKeepTheirExactTypes.",
		},
		{
			reference: "booleans render as JSON literals",
			goTest:    "TestValuesKeepTheirExactTypes",
		},
		{
			reference: "structured arguments are JSON encoded",
			goTest:    "TestValuesKeepTheirExactTypes",
		},
		{
			reference: "wallet selection is preserved",
			waiver: "the reference passes -rpcwallet on the command line; this " +
				"client addresses the wallet in the URL path instead, and " +
				"pkg/chain's TestWalletBindingProvesTheWallet covers that seam.",
		},
		{
			reference: "an oversized argument switches to stdin",
			goTest:    "TestLargeRequestBodyIsAccepted",
		},
		{
			reference: "the stdin payload keeps order and types",
			goTest:    "TestValuesKeepTheirExactTypes",
		},
		{
			reference: "the argv/stdin boundary is exact",
			waiver: "there is no argv/stdin boundary in a JSON-RPC client, so " +
				"there is no cutoff to place. The property the cutoff protected -- " +
				"a large request is delivered whole -- is proved by " +
				"TestLargeRequestBodyIsAccepted.",
		},
		{
			reference: "a failed exec becomes a named error",
			goTest:    "TestConnectionFailureNamesTheEndpoint",
		},
		{
			reference: "a timeout names the override",
			goTest:    "TestTimeoutIsNotRetried",
		},
		{
			reference: "a slow read-only call is retried",
			goTest:    "TestReadIsRetriedOnTransientFailure",
		},
		{
			reference: "a slow state-changing call is not retried",
			goTest:    "TestStateChangingCallIsNeverRetried",
		},
		{
			reference: "an unknown method is not retried",
			goTest:    "TestUnknownMethodIsNotRetried",
		},
		{
			reference: "transient connection failures are retried",
			goTest:    "TestReadIsRetriedOnTransientFailure",
		},
		{
			reference: "retries are bounded and configured",
			goTest:    "TestRetriesAreBounded",
		},
		{
			reference: "a warning before the JSON does not hide it",
			waiver: "the reference's node occasionally writes a warning line " +
				"before its JSON; this client reads an HTTP body that is JSON and " +
				"nothing else, and a non-JSON body is refused. " +
				"TestNonJSONResponseIsTransient covers the refusal.",
		},
		{
			reference: "results are decoded, text passes through",
			goTest:    "TestValuesKeepTheirExactTypes",
		},
		{
			reference: "a non-zero exit raises the node error",
			goTest:    "TestRefusalKeepsTheNodesWording",
		},
	}
}

// TestTransportParityDisposition requires every reference assertion to be either
// proved here or waived with a specific reason.
//
// A waiver has to name the mechanism that makes the assertion impossible to
// violate in this design, not merely say that it is absent: "not applicable" is
// how a gap gets forgotten.
func TestTransportParityDisposition(t *testing.T) {
	seen := map[string]bool{}

	for _, c := range transportParity() {
		if seen[c.reference] {
			t.Fatalf("%q is listed twice", c.reference)
		}

		seen[c.reference] = true

		switch {
		case c.goTest != "" && c.waiver != "":
			t.Fatalf("%q both names a test and waives itself", c.reference)
		case c.goTest == "" && c.waiver == "":
			t.Fatalf("%q is neither proved nor waived", c.reference)
		case c.waiver != "" && len(strings.Fields(c.waiver)) < 8:
			t.Fatalf("%q is waived too vaguely: %q", c.reference, c.waiver)
		}
	}

	// Seventeen assertions, which is what the reference suite prints as passes.
	if len(transportParity()) != 17 {
		t.Fatalf("the disposition table has %d rows, want 17", len(transportParity()))
	}
}

// TestLargeRequestBodyIsAccepted: a request body far larger than a command line
// can carry must arrive whole.
//
// The reference suite's oversized case exists because argv has a hard limit
// (MAX_ARG_STRLEN, 128 KiB on Linux) and a chain call can exceed it. A JSON-RPC
// body has no such limit, but "no limit" is a claim worth proving rather than
// assuming: it is proved by an endpoint that sees the whole payload.
func TestLargeRequestBodyIsAccepted(t *testing.T) {
	const size = 256 << 10 // twice the argv limit, so the old boundary is well behind us

	payload := strings.Repeat("A", size)

	var received int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)

		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}

		received = len(body)

		// Echo the length back so the client can prove it read the response from a
		// request that really carried the payload.
		_, _ = w.Write([]byte(`{"result":` + itoa(len(body)) + `,"error":null}`))
	}))
	defer server.Close()

	client := &Client{
		Transport:   &HTTPTransport{URL: server.URL},
		MaxAttempts: 1,
		Sleep:       noSleep,
	}

	result, err := client.Call(context.Background(), "getblock", payload)

	if err != nil {
		t.Fatalf("a %d-byte request failed: %v", size, err)
	}

	if received < size {
		t.Fatalf("the endpoint received %d bytes, want at least %d", received, size)
	}

	var echoed int

	if err := json.Unmarshal(result, &echoed); err != nil {
		t.Fatalf("the result is not a number: %v", err)
	}

	if echoed != received {
		t.Fatalf("the endpoint counted %d bytes but the client saw %d", received, echoed)
	}
}

// TestValuesKeepTheirExactTypes: a boolean stays a boolean, an amount keeps every
// digit, and an integer large enough to lose precision in a float survives.
//
// The reference suite has the same concern for the opposite reason: it renders
// values into JSON on a command line, and a Python `True` or a float satoshi
// value that reached argv as "True" or "1e-05" would be rejected by the node, or
// worse, accepted as something else.
func TestValuesKeepTheirExactTypes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)

		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}

		var request struct {
			Params []json.RawMessage `json:"params"`
		}

		if err := json.Unmarshal(body, &request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		// Echo the parameters back verbatim as the result. A value that was
		// rewritten on the way in shows up as a different literal on the way out.
		result, err := json.Marshal(request.Params)

		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}

		_, _ = w.Write([]byte(`{"result":` + string(result) + `,"error":null}`))
	}))
	defer server.Close()

	client := &Client{
		Transport:   &HTTPTransport{URL: server.URL},
		MaxAttempts: 1,
		Sleep:       noSleep,
	}

	const (
		// One satoshi above 21 million BTC in satoshis: past the range where a
		// float64 can represent every integer.
		bigSats = int64(2100000000000001)
		// An eight-decimal amount, which is how the node renders BLAKE.
		amount = "0.00100000"
	)

	result, err := client.Call(context.Background(), "gettxout",
		true, false, bigSats, amount, "null", []string{"a", "b"}, map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("the call failed: %v", err)
	}

	echoed := string(result)

	for _, want := range []string{
		"true",
		"false",
		"2100000000000001",
		`"0.00100000"`,
		`["a","b"]`,
		`{"k":"v"}`,
	} {
		if !strings.Contains(echoed, want) {
			t.Errorf("the node received %s, not %s", echoed, want)
		}
	}

	// And the value really is a number, not a string that happens to match.
	var decoded []json.RawMessage

	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatal(err)
	}

	var sats int64

	if err := json.Unmarshal(decoded[2], &sats); err != nil {
		t.Fatalf("the satoshi parameter is not an integer: %v", err)
	}

	if sats != bigSats {
		t.Fatalf("the satoshi parameter round-tripped as %d, want %d", sats, bigSats)
	}

	var flag bool

	if err := json.Unmarshal(decoded[0], &flag); err != nil || !flag {
		t.Fatalf("the boolean parameter is not true: %v", err)
	}
}

// TestRefusalKeepsTheNodesWording: a refusal reaches the operator with the node's
// code and message, and this client's method, in one line.
//
// The reference's vocabulary is what an operator greps for, so the shape is a
// contract: `<method>: node error <code>: <message>`.
func TestRefusalKeepsTheNodesWording(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":null,"error":{"code":-18,` +
			`"message":"Requested wallet does not exist or is not loaded"}}`))
	}))
	defer server.Close()

	client := &Client{
		Transport:   &HTTPTransport{URL: server.URL},
		MaxAttempts: 1,
		Sleep:       noSleep,
	}

	_, err := client.Call(context.Background(), "getnewaddress")

	if err == nil {
		t.Fatal("a refusal was accepted as success")
	}

	want := "getnewaddress: node error -18: Requested wallet does not exist or is not loaded"

	if !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("refusal is %q, want it to start with %q", err.Error(), want)
	}

	// A refusal on a state-changing call also says why it was not retried: an
	// operator whose call timed out needs to know the node may still have acted.
	if !strings.Contains(err.Error(), "NOT retried") {
		t.Fatalf("a state-changing refusal does not say it was not retried: %q", err.Error())
	}

	// The structured error survives, so a caller can branch on the code without
	// parsing the message.
	var nodeErr *Error

	if !asNodeError(err, &nodeErr) {
		t.Fatalf("the refusal is not an *rpc.Error: %T", err)
	}

	if nodeErr.Code != -18 || nodeErr.Method != "getnewaddress" {
		t.Fatalf("the refusal lost its code or method: %+v", nodeErr)
	}
}

// TestAReadOnlyRefusalDoesNotClaimTheCallChangedState: the note is specific to
// state-changing calls, and a blanket warning would train an operator to ignore it.
func TestAReadOnlyRefusalDoesNotClaimTheCallChangedState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":null,"error":{"code":-5,` +
			`"message":"No such mempool transaction"}}`))
	}))
	defer server.Close()

	client := &Client{
		Transport:   &HTTPTransport{URL: server.URL},
		MaxAttempts: 1,
		Sleep:       noSleep,
	}

	_, err := client.Call(context.Background(), "getrawtransaction", "00")

	if err == nil {
		t.Fatal("a refusal was accepted as success")
	}

	if strings.Contains(err.Error(), "NOT retried") {
		t.Fatalf("a read-only refusal claims the call may have changed state: %q", err.Error())
	}
}

// TestConnectionFailureNamesTheEndpoint: a transport failure says which endpoint,
// because "connection refused" with no address is not actionable.
func TestConnectionFailureNamesTheEndpoint(t *testing.T) {
	client := &Client{
		Transport:   &HTTPTransport{URL: "http://127.0.0.1:1/"},
		MaxAttempts: 1,
		Sleep:       noSleep,
	}

	_, err := client.Call(context.Background(), "getblockcount")

	if err == nil {
		t.Fatal("a refused connection was accepted as success")
	}

	// Either the client names the endpoint itself or the Go transport does; both
	// are the host and port, which is what an operator needs.
	if !regexp.MustCompile(`127\.0\.0\.1:1`).MatchString(err.Error()) {
		t.Fatalf("the failure does not name the endpoint: %v", err)
	}
}

// asNodeError is errors.As for the client's own error type.
func asNodeError(err error, target **Error) bool {
	for err != nil {
		if nodeErr, ok := err.(*Error); ok {
			*target = nodeErr

			return true
		}

		unwrapper, ok := err.(interface{ Unwrap() error })

		if !ok {
			return false
		}

		err = unwrapper.Unwrap()
	}

	return false
}

// itoa renders a non-negative integer without pulling in strconv for one call.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}

	var digits []byte

	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}

	return string(digits)
}
