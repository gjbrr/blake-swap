//go:build !js

// The engine domain is checked against a real HTTP server standing in for the app
// server's node proxy. httptest listens on a socket, which the wasm target cannot do,
// so this file is native-only: the wasm suite covers the vault domain, which is the
// part whose behaviour differs between the two platforms.
package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/platform"
	"github.com/gjbrr/blake-swap/pkg/proto"
	"github.com/gjbrr/blake-swap/pkg/store"
)

// fakeProxy is a stand-in for the app server's node proxy: it answers the handful of
// JSON-RPC methods the read-only engine commands call.
//
// It is a proxy rather than a node because that is what the browser talks to, and the
// URL it builds (including the /rpc/<chain> path) is part of what the test is checking.
type fakeProxy struct {
	server  *httptest.Server
	methods []string
	paths   []string
}

// newFakeProxy starts a stand-in proxy with both chains at fixed heights.
func newFakeProxy(t *testing.T) *fakeProxy {
	t.Helper()

	proxy := &fakeProxy{}

	proxy.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.paths = append(proxy.paths, r.URL.Path)

		var request struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)

		proxy.methods = append(proxy.methods, request.Method)

		result := any(nil)

		switch request.Method {
		case "getblockcount":
			// The chain is named by the path, so one proxy can serve both.
			height := 100

			if strings.HasPrefix(r.URL.Path, "/rpc/blake") {
				height = 200
			}

			result = height
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": result, "error": nil, "id": request.ID,
		})
	}))

	t.Cleanup(proxy.server.Close)

	return proxy
}

// engineDomainFor builds an unlocked vault with an identity and a proxy address.
func engineDomainFor(t *testing.T, proxyURL string) (*engineDomain, *vaultDomain) {
	t.Helper()

	vault := NewVaultDomain(platform.NewMemoryKV(), nil)

	if result := run(t, vault, "vault.create", map[string]string{"passphrase": "passphrase"}); !result.OK {
		t.Fatalf("create = %+v", result)
	}

	if result := run(t, vault, "proxy.set", map[string]string{"url": proxyURL}); !result.OK {
		t.Fatalf("proxy.set = %+v", result)
	}

	secret := strings.Repeat("11", 32)

	if result := run(t, vault, "identity.set", map[string]string{"secret": secret}); !result.OK {
		t.Fatalf("identity.set = %+v", result)
	}

	return newEngineDomain(vault), vault
}

// TestEngineStatusThroughTheProxy is the whole browser chain: the domain reads the proxy
// address and the identity from the encrypted vault, builds node clients against the
// proxy, and reports both chain heights.
func TestEngineStatusThroughTheProxy(t *testing.T) {
	proxy := newFakeProxy(t)
	engine, _ := engineDomainFor(t, proxy.server.URL)

	body, err := proto.EncodeRequest(proto.Request{Command: "engine.status"})

	if err != nil {
		t.Fatal(err)
	}

	result := handleCommand(context.Background(), nil, engine, nil, body)

	if !result.OK {
		t.Fatalf("result = %+v", result)
	}

	if !strings.Contains(result.Text, "BLAKE height: 200") ||
		!strings.Contains(result.Text, "Bitcoin height: 100") {
		t.Fatalf("text = %q", result.Text)
	}

	// Both chains were addressed through the proxy's paths, with no credentials.
	if len(proxy.paths) < 2 || proxy.paths[0] != "/rpc/blake" || proxy.paths[1] != "/rpc/btc" {
		t.Fatalf("paths = %v", proxy.paths)
	}
}

// TestEngineRefusesWithoutAProxyOrIdentity: the engine domain reports what is missing
// instead of guessing at a default.
func TestEngineRefusesWithoutAProxyOrIdentity(t *testing.T) {
	ctx := context.Background()

	vault := NewVaultDomain(platform.NewMemoryKV(), nil)

	if result := run(t, vault, "vault.create", map[string]string{"passphrase": "p"}); !result.OK {
		t.Fatalf("create = %+v", result)
	}

	engine := newEngineDomain(vault)

	result := engine.Handle(ctx, proto.Request{Command: "engine.status"})

	if result.OK || !strings.Contains(result.Error, "no proxy address") {
		t.Fatalf("result = %+v", result)
	}

	if result := run(t, vault, "proxy.set", map[string]string{"url": "http://127.0.0.1:1"}); !result.OK {
		t.Fatalf("proxy.set = %+v", result)
	}

	result = engine.Handle(ctx, proto.Request{Command: "engine.status"})

	if result.OK || !strings.Contains(result.Error, "no participant identity") {
		t.Fatalf("result = %+v", result)
	}
}

// TestEngineListAndShowReadTheVault: the commands read the same sealed records the vault
// domain wrote.
func TestEngineListAndShowReadTheVault(t *testing.T) {
	proxy := newFakeProxy(t)
	engine, vault := engineDomainFor(t, proxy.server.URL)

	record, err := model.New(strings.NewReader(strings.Repeat("ab", 64)), model.NewParams{
		SwapID:       "aaaa00000001",
		BlakeAmount:  amount.Blake(1_000_000),
		BTCSats:      25_000,
		BlakeTimeout: 500,
		BTCTimeout:   400,
	})

	if err != nil {
		t.Fatal(err)
	}

	encoded, err := json.Marshal(record)

	if err != nil {
		t.Fatal(err)
	}

	if result := run(t, vault, "state.put", map[string]string{"json": string(encoded)}); !result.OK {
		t.Fatalf("state.put = %+v", result)
	}

	list := engine.Handle(context.Background(), proto.Request{Command: "engine.list"})

	if !list.OK || !strings.Contains(list.Text, record.SwapID) {
		t.Fatalf("list = %+v", list)
	}

	show := engine.Handle(context.Background(), proto.Request{
		Command: "engine.show", Args: map[string]string{"swap_id": record.SwapID},
	})

	if !show.OK || !strings.Contains(show.Text, record.SwapID) ||
		!strings.Contains(show.Text, record.SecretHash) {
		t.Fatalf("show = %+v", show)
	}
}

// TestEngineRefusesALockedVault: everything the engine does needs the key, so a locked
// vault is a refusal rather than an empty answer.
func TestEngineRefusesALockedVault(t *testing.T) {
	proxy := newFakeProxy(t)
	engine, vault := engineDomainFor(t, proxy.server.URL)

	if result := run(t, vault, "vault.lock", nil); !result.OK {
		t.Fatalf("lock = %+v", result)
	}

	result := engine.Handle(context.Background(), proto.Request{Command: "engine.list"})

	if result.OK || !strings.Contains(result.Error, "locked") {
		t.Fatalf("result = %+v", result)
	}
}

// TestIdentityAndProxyStatus: the two facts the engine needs are readable once set.
func TestIdentityAndProxyStatus(t *testing.T) {
	vault := NewVaultDomain(platform.NewMemoryKV(), nil)

	if result := run(t, vault, "vault.create", map[string]string{"passphrase": "p"}); !result.OK {
		t.Fatalf("create = %+v", result)
	}

	if result := run(t, vault, "identity.status", nil); !result.OK ||
		!strings.Contains(result.Text, "absent") {
		t.Fatalf("identity.status = %+v", result)
	}

	secret := strings.Repeat("22", 32)

	set := run(t, vault, "identity.set", map[string]string{"secret": secret})

	if !set.OK || !strings.Contains(set.Text, "identity set") {
		t.Fatalf("identity.set = %+v", set)
	}

	status := run(t, vault, "identity.status", nil)

	if !status.OK || !strings.Contains(status.Text, "identity: ") {
		t.Fatalf("status = %+v", status)
	}

	// A value that is not a key is refused rather than stored.
	if result := run(t, vault, "identity.set", map[string]string{"secret": "nothex"}); result.OK {
		t.Fatal("a non-key identity was accepted")
	}
}

// TestRecordSurvivesTheVaultReload: the record the engine lists comes back after a
// lock/unlock cycle, which is what makes the browser's state durable.
func TestRecordSurvivesTheVaultReload(t *testing.T) {
	ctx := context.Background()

	kv := platform.NewMemoryKV()
	vault := NewVaultDomain(kv, nil)

	if result := run(t, vault, "vault.create", map[string]string{"passphrase": "p"}); !result.OK {
		t.Fatalf("create = %+v", result)
	}

	record, err := model.New(strings.NewReader(strings.Repeat("cd", 64)), model.NewParams{
		SwapID:       "aaaa00000002",
		BlakeAmount:  amount.Blake(2_000_000),
		BTCSats:      30_000,
		BlakeTimeout: 600,
		BTCTimeout:   500,
	})

	if err != nil {
		t.Fatal(err)
	}

	encoded, _ := json.Marshal(record)

	if result := run(t, vault, "state.put", map[string]string{"json": string(encoded)}); !result.OK {
		t.Fatalf("put = %+v", result)
	}

	// A second vault over the same storage is what a page reload looks like.
	reopened := NewVaultDomain(kv, nil)

	if result := run(t, reopened, "vault.unlock", map[string]string{"passphrase": "p"}); !result.OK {
		t.Fatalf("unlock = %+v", result)
	}

	loaded, err := reopened.store.Load(ctx, record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if loaded.SecretHash != record.SecretHash {
		t.Fatal("the record did not survive the reload")
	}

	_ = store.Alice
}
