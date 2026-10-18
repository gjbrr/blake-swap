package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// node is a stand-in JSON-RPC node that records what reached it.
type node struct {
	path     string
	user     string
	password string
	method   string
	body     string
	status   int
	response string
	server   *httptest.Server
}

// newNode starts a stand-in node.
func newNode(t *testing.T) *node {
	t.Helper()

	n := &node{status: http.StatusOK, response: `{"result":true,"error":null,"id":1}`}

	n.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.path = r.URL.Path
		n.user, n.password, _ = r.BasicAuth()

		body, _ := io.ReadAll(r.Body)
		n.body = string(body)

		var request struct {
			Method string `json:"method"`
		}

		_ = json.Unmarshal(body, &request)
		n.method = request.Method

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(n.status)
		_, _ = w.Write([]byte(n.response))
	}))

	t.Cleanup(n.server.Close)

	return n
}

// newProxy builds a proxy pointed at one node.
func newProxy(t *testing.T, n *node) *Proxy {
	t.Helper()

	p, err := New(Config{
		Chains: map[string]Endpoint{
			"btc": {URL: n.server.URL, User: "__cookie__", Password: "secret"},
		},
		RatePerSecond: 1000,
		Burst:         1000,
	})

	if err != nil {
		t.Fatal(err)
	}

	return p
}

// call sends one JSON-RPC body through the proxy.
func call(t *testing.T, p *Proxy, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	recorder := httptest.NewRecorder()

	p.ServeHTTP(recorder, request)

	return recorder
}

// TestForwarding: an allowlisted method reaches the node with the credentials and the
// body intact.
func TestForwarding(t *testing.T) {
	n := newNode(t)
	p := newProxy(t, n)

	body := `{"jsonrpc":"1.0","id":1,"method":"getblockcount","params":[]}`

	response := call(t, p, "/rpc/btc", body)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}

	if n.method != "getblockcount" || n.body != body {
		t.Fatalf("node got method=%s body=%s", n.method, n.body)
	}

	if n.user != "__cookie__" || n.password != "secret" {
		t.Fatalf("credentials = %q/%q", n.user, n.password)
	}

	if response.Body.String() != n.response {
		t.Fatalf("body = %s", response.Body.String())
	}
}

// TestWalletPath: the wallet is part of the node's URL, not its parameters.
func TestWalletPath(t *testing.T) {
	n := newNode(t)
	p := newProxy(t, n)

	response := call(t, p, "/rpc/btc/wallet/alice",
		`{"method":"getbalances","params":[]}`)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}

	if n.path != "/wallet/alice" {
		t.Fatalf("node path = %q", n.path)
	}
}

// TestMethodAllowlist: a method the engine never calls is refused, and the node never
// sees it.
func TestMethodAllowlist(t *testing.T) {
	n := newNode(t)
	p := newProxy(t, n)

	response := call(t, p, "/rpc/btc",
		`{"method":"dumpprivkey","params":["addr"]}`)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d", response.Code)
	}

	if n.method != "" {
		t.Fatalf("the node was called with %q", n.method)
	}
}

// TestNodeErrorBodyIsPassedThrough: a JSON-RPC error keeps its status code and message,
// because the engine's error handling reads them.
func TestNodeErrorBodyIsPassedThrough(t *testing.T) {
	n := newNode(t)
	n.status = http.StatusInternalServerError
	n.response = `{"result":null,"error":{"code":-5,"message":"No such transaction"},"id":1}`

	p := newProxy(t, n)

	response := call(t, p, "/rpc/btc", `{"method":"getrawtransaction","params":[]}`)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want the body passed through as 200", response.Code)
	}

	if !strings.Contains(response.Body.String(), "No such transaction") {
		t.Fatalf("body = %s", response.Body.String())
	}
}

// TestPathAndBodyRefusals covers every shape the proxy must not forward.
func TestPathAndBodyRefusals(t *testing.T) {
	n := newNode(t)
	p := newProxy(t, n)

	cases := map[string]struct {
		path string
		body string
		want int
	}{
		"unknown path":     {"/nope", `{"method":"getblockcount"}`, http.StatusNotFound},
		"unknown chain":    {"/rpc/dogecoin", `{"method":"getblockcount"}`, http.StatusNotFound},
		"wallet traversal": {"/rpc/btc/wallet/../x", `{"method":"getblockcount"}`, http.StatusNotFound},
		"extra segments":   {"/rpc/btc/wallet/a/b", `{"method":"getblockcount"}`, http.StatusNotFound},
		"empty body":       {"/rpc/btc", ``, http.StatusBadRequest},
		"not json":         {"/rpc/btc", `method=getblockcount`, http.StatusBadRequest},
		"no method":        {"/rpc/btc", `{"params":[]}`, http.StatusBadRequest},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			response := call(t, p, c.path, c.body)

			if response.Code != c.want {
				t.Fatalf("status = %d, want %d (%s)", response.Code, c.want, response.Body.String())
			}
		})
	}

	if n.method != "" {
		t.Fatalf("a refused request reached the node as %q", n.method)
	}
}

// TestMethodNotAllowed: the proxy is POST-only.
func TestMethodNotAllowed(t *testing.T) {
	p := newProxy(t, newNode(t))

	request := httptest.NewRequest(http.MethodGet, "/rpc/btc", nil)
	recorder := httptest.NewRecorder()

	p.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", recorder.Code)
	}
}

// TestRateLimit: one address's burst is bounded, and the refusal is a 429 rather than a
// silent drop.
func TestRateLimit(t *testing.T) {
	n := newNode(t)

	p, err := New(Config{
		Chains:        map[string]Endpoint{"btc": {URL: n.server.URL}},
		RatePerSecond: 1,
		Burst:         2,
	})

	if err != nil {
		t.Fatal(err)
	}

	body := `{"method":"getblockcount","params":[]}`

	for i := 0; i < 2; i++ {
		if response := call(t, p, "/rpc/btc", body); response.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d", i, response.Code)
		}
	}

	response := call(t, p, "/rpc/btc", body)

	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", response.Code)
	}
}

// TestUnreachableNodeIsABadGateway: a node that is down is a 502, not a hang.
func TestUnreachableNodeIsABadGateway(t *testing.T) {
	p, err := New(Config{
		Chains: map[string]Endpoint{"btc": {URL: "http://127.0.0.1:1"}},
	})

	if err != nil {
		t.Fatal(err)
	}

	response := call(t, p, "/rpc/btc", `{"method":"getblockcount","params":[]}`)

	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", response.Code)
	}
}

// TestNoChainsIsRefused: a proxy with nowhere to forward is a configuration error.
func TestNoChainsIsRefused(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("a proxy with no chains was built")
	}
}

// TestStatus reports what the proxy will do, for an operator's log.
func TestStatus(t *testing.T) {
	p := newProxy(t, newNode(t))

	status := p.Status()

	chains, ok := status["chains"].([]string)

	if !ok || len(chains) != 1 || chains[0] != "btc" {
		t.Fatalf("status = %v", status)
	}

	if methods, ok := status["methods"].(int); !ok || methods != len(DefaultAllowlist) {
		t.Fatalf("methods = %v", status["methods"])
	}
}
