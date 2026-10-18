// Package proxy is the node RPC proxy the browser talks to.
//
// A browser cannot hold node credentials: anything that can run script on the origin can
// read them, and a node's credentials are the wallet. So the credentials stay on the
// server, and the browser sends JSON-RPC bodies to a path that names a chain — and
// optionally a wallet — which the proxy resolves to the node's own endpoint.
//
// Two rules make it a boundary rather than a pipe. The method is checked against an
// allowlist, so a proxy that is reachable from a browser cannot be used as a general RPC
// client; and requests are rate-limited per remote address, because a browser client that
// reconnects in a loop is a denial of service against the participant's own node.
//
// The allowlist deliberately includes state-changing methods: the client's whole purpose
// is to fund, claim and refund. What it excludes is everything the engine never calls, so
// an exploit has to be an exploit through the engine's own surface.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Endpoint is one node.
type Endpoint struct {
	// URL is the node's JSON-RPC root, such as http://127.0.0.1:18443.
	URL string
	// User and Password are the node's credentials. They never leave this process.
	User     string
	Password string
}

// Config is how the proxy is built.
type Config struct {
	// Chains maps a path segment ("btc", "blake") to a node.
	Chains map[string]Endpoint
	// Allow overrides the method allowlist. Nil uses DefaultAllowlist.
	Allow map[string]bool
	// RatePerSecond and Burst bound one remote address. Zero uses defaults.
	RatePerSecond float64
	Burst         int
	// Client is an optional HTTP client.
	Client *http.Client
}

// Proxy forwards allowlisted JSON-RPC calls to the configured nodes.
type Proxy struct {
	chains  map[string]Endpoint
	allow   map[string]bool
	limiter *limiter
	client  *http.Client
}

// DefaultAllowlist is every method the engine calls, and nothing else.
//
// It is spelled out rather than derived from the engine so that adding a method to the
// engine does not silently widen what a browser can reach: the new call fails until
// someone adds it here, which is the right direction for a security boundary to fail.
var DefaultAllowlist = map[string]bool{
	// Chain and block reads.
	"getblockcount":    true,
	"getblockhash":     true,
	"getblock":         true,
	"getbestblockhash": true,

	// Transaction reads.
	"getrawtransaction":    true,
	"gettxout":             true,
	"decoderawtransaction": true,
	"getmempoolentry":      true,
	"getmempoolinfo":       true,

	// Fee estimation.
	"estimatesmartfee": true,

	// Wallet lifecycle and reads.
	"listwallets":       true,
	"loadwallet":        true,
	"createwallet":      true,
	"getbalances":       true,
	"listunspent":       true,
	"listtransactions":  true,
	"gettransaction":    true,
	"getnewaddress":     true,
	"getaddressinfo":    true,
	"getdescriptorinfo": true,
	"deriveaddresses":   true,
	"importdescriptors": true,

	// Spending and settlement.
	"sendtoaddress":        true,
	"sendrawtransaction":   true,
	"createrawtransaction": true,
	"converttopsbt":        true,
	"utxoupdatepsbt":       true,
	"walletprocesspsbt":    true,
	"finalizepsbt":         true,

	// Mining, for the regtest rig only. A public deployment should drop it.
	"generatetoaddress": true,
}

// New builds a proxy, or returns an error when it would not be safe.
func New(cfg Config) (*Proxy, error) {
	if len(cfg.Chains) == 0 {
		return nil, fmt.Errorf("proxy has no chains configured")
	}

	allow := cfg.Allow

	if allow == nil {
		allow = DefaultAllowlist
	}

	rate := cfg.RatePerSecond

	if rate <= 0 {
		rate = 20
	}

	burst := cfg.Burst

	if burst <= 0 {
		burst = 40
	}

	client := cfg.Client

	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	return &Proxy{
		chains:  cfg.Chains,
		allow:   allow,
		limiter: newLimiter(rate, burst),
		client:  client,
	}, nil
}

// ServeHTTP implements the proxy endpoint.
//
// The path is /rpc/<chain> or /rpc/<chain>/wallet/<name>. Anything else is a 404, so a
// browser cannot address a node path the proxy did not intend.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)

		return
	}

	chainName, wallet, ok := parsePath(r.URL.Path)

	if !ok {
		http.Error(w, "not a proxy path", http.StatusNotFound)

		return
	}

	endpoint, configured := p.chains[chainName]

	if !configured {
		http.Error(w, "unknown chain", http.StatusNotFound)

		return
	}

	if !p.limiter.allow(remoteAddress(r)) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)

		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))

	if err != nil {
		http.Error(w, "could not read the request", http.StatusBadRequest)

		return
	}

	method, err := methodOf(body)

	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	if !p.allow[method] {
		http.Error(w, "method not allowed by this proxy", http.StatusForbidden)

		return
	}

	url := strings.TrimRight(endpoint.URL, "/")

	if wallet != "" {
		url += "/wallet/" + wallet
	}

	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(body))

	if err != nil {
		http.Error(w, "could not build the node request", http.StatusInternalServerError)

		return
	}

	request.Header.Set("Content-Type", "application/json")

	if endpoint.User != "" || endpoint.Password != "" {
		request.SetBasicAuth(endpoint.User, endpoint.Password)
	}

	response, err := p.client.Do(request)

	if err != nil {
		http.Error(w, "the node is unreachable", http.StatusBadGateway)

		return
	}

	defer func() { _ = response.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))

	if err != nil {
		http.Error(w, "could not read the node's answer", http.StatusBadGateway)

		return
	}

	//
	// A node reports JSON-RPC errors in the body with an HTTP status of 500. The body is
	// what the engine's error handling understands, so it is passed through as a 200 with
	// the JSON intact; anything else would turn "no such transaction" into a transport
	// failure and lose the error code.
	//
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

// parsePath reads /rpc/<chain>[/wallet/<name>].
func parsePath(path string) (chainName, wallet string, ok bool) {
	trimmed := strings.TrimPrefix(path, "/")

	parts := strings.Split(trimmed, "/")

	if len(parts) < 2 || parts[0] != "rpc" || parts[1] == "" {
		return "", "", false
	}

	chainName = parts[1]

	switch {
	case len(parts) == 2:
		return chainName, "", true
	case len(parts) == 4 && parts[2] == "wallet" && parts[3] != "":
		return chainName, parts[3], true
	}

	return "", "", false
}

// methodOf reads the method from one JSON-RPC request.
func methodOf(body []byte) (string, error) {
	if len(body) == 0 {
		return "", fmt.Errorf("empty request body")
	}

	var request struct {
		Method string `json:"method"`
	}

	if err := json.Unmarshal(body, &request); err != nil {
		return "", fmt.Errorf("request is not JSON-RPC: %w", err)
	}

	if request.Method == "" {
		return "", fmt.Errorf("request has no method")
	}

	return request.Method, nil
}

// remoteAddress is the client address, without the port.
func remoteAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)

	if err != nil {
		return r.RemoteAddr
	}

	return host
}

// limiter is a token bucket per remote address.
type limiter struct {
	rate  float64
	burst float64

	mu      sync.Mutex
	buckets map[string]*bucket
}

// bucket is one address's allowance.
type bucket struct {
	tokens float64
	last   time.Time
}

// newLimiter builds a limiter.
func newLimiter(rate float64, burst int) *limiter {
	return &limiter{rate: rate, burst: float64(burst), buckets: map[string]*bucket{}}
}

// allow reports whether one more request fits the allowance.
func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()

	current, found := l.buckets[key]

	if !found {
		current = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = current
	}

	current.tokens += now.Sub(current.last).Seconds() * l.rate
	current.last = now

	if current.tokens > l.burst {
		current.tokens = l.burst
	}

	if current.tokens < 1 {
		return false
	}

	current.tokens--

	return true
}

// Handler returns an http.Handler for the proxy, for use in a mux.
func (p *Proxy) Handler() http.Handler { return p }

// Status reports what the proxy will and will not forward, for an operator.
func (p *Proxy) Status() map[string]any {
	chains := make([]string, 0, len(p.chains))

	for name := range p.chains {
		chains = append(chains, name)
	}

	methods := make([]string, 0, len(p.allow))

	for method := range p.allow {
		methods = append(methods, method)
	}

	return map[string]any{
		"chains":  chains,
		"methods": len(methods),
		"rate":    p.limiter.rate,
		"burst":   p.limiter.burst,
	}
}

// Close releases idle connections.
func (p *Proxy) Close() {
	if transport, ok := p.client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
}

// Do is the transport the proxy itself uses, exposed so a caller can share one client.
func (p *Proxy) Do(ctx context.Context, request *http.Request) (*http.Response, error) {
	return p.client.Do(request.WithContext(ctx))
}
