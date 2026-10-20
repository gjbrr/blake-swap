//go:build !js

package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/platform"
	"github.com/gjbrr/blake-swap/pkg/proto"
)

// walletNode is a stand-in for the app server's proxy that answers the wallet RPCs.
//
// It remembers which wallets exist and which of them are loaded, because those are two
// different facts and the client reports them separately: a wallet that exists but is not
// loaded is what a node that restarted leaves behind.
type walletNode struct {
	server *httptest.Server

	mu     sync.Mutex
	onDisk map[string]bool // name -> privateKeysEnabled
	loaded map[string]bool
	// created is every createwallet call, in order.
	created []walletCreateCall
	// loads is every loadwallet call.
	loads []string
	// failMethod, when set, answers that method with an RPC error.
	failMethod string
}

// walletCreateCall is one createwallet request's parameters.
type walletCreateCall struct {
	Chain  string
	Params []any
}

// newWalletNode starts a proxy whose node has no wallets.
func newWalletNode(t *testing.T) *walletNode {
	t.Helper()

	node := &walletNode{onDisk: map[string]bool{}, loaded: map[string]bool{}}

	node.server = httptest.NewServer(http.HandlerFunc(node.serve))

	t.Cleanup(node.server.Close)

	return node
}

// serve answers one JSON-RPC call.
func (w *walletNode) serve(rw http.ResponseWriter, r *http.Request) {
	var request struct {
		ID     any    `json:"id"`
		Method string `json:"method"`
		Params []any  `json:"params"`
	}

	_ = json.NewDecoder(r.Body).Decode(&request)

	w.mu.Lock()
	defer w.mu.Unlock()

	// The wallet is in the path: /rpc/<chain>/wallet/<name>.
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	chain := ""

	if len(parts) >= 2 {
		chain = parts[1]
	}

	wallet := ""

	if len(parts) >= 4 && parts[2] == "wallet" {
		wallet = parts[3]
	}

	var (
		result  any
		failure any
	)

	switch request.Method {
	case "listwallets":
		names := make([]string, 0, len(w.loaded))

		for name := range w.loaded {
			names = append(names, name)
		}

		result = names

	case "loadwallet":
		name, _ := request.Params[0].(string)

		w.loads = append(w.loads, name)

		if _, ok := w.onDisk[name]; !ok {
			failure = map[string]any{"code": -18, "message": "Wallet file not found."}
		} else {
			w.loaded[name] = true
			result = map[string]any{"name": name}
		}

	case "createwallet":
		name, _ := request.Params[0].(string)
		disablePrivateKeys, _ := request.Params[1].(bool)

		w.created = append(w.created, walletCreateCall{Chain: chain, Params: request.Params})
		w.onDisk[name] = !disablePrivateKeys
		w.loaded[name] = true

		result = map[string]any{"name": name}

	case "getwalletinfo":
		signing, ok := w.onDisk[wallet]

		if !ok {
			failure = map[string]any{"code": -18, "message": "Wallet file not found."}

			break
		}

		result = map[string]any{
			"walletname":            wallet,
			"walletversion":         169900,
			"private_keys_enabled":  signing,
			"private_keys_disabled": !signing,
			"descriptors":           true,
		}

	default:
		result = 0
	}

	response := map[string]any{"result": result, "error": failure, "id": request.ID}

	if w.failMethod == request.Method && failure == nil {
		response = map[string]any{
			"result": nil,
			"error":  map[string]any{"code": -32601, "message": request.Method + " is unavailable"},
			"id":     request.ID,
		}
	}

	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(response)
}

// addOnDisk puts a wallet on the node without loading it, which is what a restart leaves.
func (w *walletNode) addOnDisk(name string, signing bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.onDisk[name] = signing
}

// addLoaded puts a wallet on the node and loads it.
//
// The type of a wallet is only a question the node answers for a loaded one, so a wallet
// that is merely on disk is "absent" as far as the client can tell.
func (w *walletNode) addLoaded(name string, signing bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.onDisk[name] = signing
	w.loaded[name] = true
}

// walletDomain builds an unlocked vault and an engine domain pointed at a wallet node.
func walletDomain(t *testing.T, node *walletNode) (*engineDomain, *vaultDomain) {
	t.Helper()

	engine, vault := engineDomainFor(t, node.server.URL)

	if result := run(t, vault, "identity.set", map[string]string{
		"secret": strings.Repeat("11", 32),
	}); !result.OK {
		t.Fatalf("identity.set = %+v", result)
	}

	return engine, vault
}

// create runs one wallets.create and decodes its view.
func create(t *testing.T, engine *engineDomain, slot, name string) (proto.Result, proto.WalletCreateView) {
	t.Helper()

	result := engine.Handle(context.Background(), proto.Request{
		Command: "wallets.create",
		Args:    map[string]string{"slot": slot, "name": name},
	})

	var view proto.WalletCreateView

	if result.OK {
		if err := proto.DecodeData(result, &view); err != nil {
			t.Fatal(err)
		}
	}

	return result, view
}

// TestWalletCreateSendsTheFlagsTheSlotNeeds: the two flags a node must be told, checked
// positionally because createwallet takes them positionally.
func TestWalletCreateSendsTheFlagsTheSlotNeeds(t *testing.T) {
	for _, test := range []struct {
		slot       string
		watch      bool
		privateKey bool
	}{
		{"blake", false, true},
		{"btc", false, true},
		{"blake_watch", true, false},
		{"btc_watch", true, false},
	} {
		t.Run(test.slot, func(t *testing.T) {
			node := newWalletNode(t)
			engine, vault := walletDomain(t, node)

			result, view := create(t, engine, test.slot, "participant")

			if !result.OK {
				t.Fatalf("wallets.create = %+v", result)
			}

			if !view.Created || !view.Loaded || view.WatchOnly != test.watch ||
				view.Signing != !test.watch {
				t.Fatalf("view = %+v", view)
			}

			if len(node.created) != 1 {
				t.Fatalf("createwallet was called %d times", len(node.created))
			}

			call := node.created[0]

			// createwallet "<name>" <disable_private_keys> <blank> <passphrase>
			//              <avoid_reuse> <descriptors>
			if len(call.Params) != 6 {
				t.Fatalf("createwallet params = %v", call.Params)
			}

			if call.Params[0] != "participant" {
				t.Fatalf("the name parameter is %v", call.Params[0])
			}

			if got, _ := call.Params[1].(bool); got == test.privateKey {
				// disable_private_keys must be the opposite of signing.
				t.Fatalf("disable_private_keys = %v for a %s slot", got, test.slot)
			}

			if got, _ := call.Params[5].(bool); !got {
				t.Fatalf("descriptors is not true: params = %v", call.Params)
			}

			if !containsWallet(node.loads, "participant") {
				t.Fatalf("loadwallet was not tried first: %v", node.loads)
			}

			// The name is sealed in the slot's own key.
			stored, err := vault.readSecret(context.Background(), walletSlotSecret(test.slot))

			if err != nil || stored != "participant" {
				t.Fatalf("stored name = %q, %v", stored, err)
			}
		})
	}
}

// walletSlotSecret is the vault key one slot stores its name under.
func walletSlotSecret(argument string) string {
	slot, err := walletSlotFor(argument)

	if err != nil {
		panic(err)
	}

	return slot.secret
}

// TestWalletCreateIsIdempotent: a second call reports the existing wallet and creates
// nothing.
func TestWalletCreateIsIdempotent(t *testing.T) {
	node := newWalletNode(t)
	engine, _ := walletDomain(t, node)

	if result, _ := create(t, engine, "blake_watch", "watcher"); !result.OK {
		t.Fatalf("the first create = %+v", result)
	}

	second, view := create(t, engine, "blake_watch", "watcher")

	if !second.OK {
		t.Fatalf("the retry was refused: %+v", second)
	}

	if view.Created {
		t.Fatalf("the retry reported a creation: %+v", view)
	}

	if !view.Loaded || !strings.Contains(second.Text, "already exists") {
		t.Fatalf("the retry = %+v", second)
	}

	if len(node.created) != 1 {
		t.Fatalf("createwallet was called %d times", len(node.created))
	}
}

// TestWalletCreateLoadsAnUnloadedWallet: a wallet that exists and is not loaded is loaded,
// not created again.
func TestWalletCreateLoadsAnUnloadedWallet(t *testing.T) {
	node := newWalletNode(t)
	engine, _ := walletDomain(t, node)

	node.addOnDisk("existing", true)

	result, view := create(t, engine, "btc", "existing")

	if !result.OK {
		t.Fatalf("wallets.create = %+v", result)
	}

	if view.Created {
		t.Fatal("a wallet that was already on disk was reported as created")
	}

	if !view.Loaded || !view.Signing {
		t.Fatalf("view = %+v", view)
	}

	if len(node.created) != 0 {
		t.Fatalf("createwallet was called for an unloaded wallet: %v", node.created)
	}
}

// TestWalletCreateRefusesTheWrongType: a signing wallet in a watch slot and the converse.
func TestWalletCreateRefusesTheWrongType(t *testing.T) {
	for _, test := range []struct {
		name string
		slot string
		disk bool
		want string
	}{
		{"signing wallet in a watch slot", "btc_watch", true,
			"exists as a signing wallet, not a watch-only wallet"},
		{"watch-only wallet in a signing slot", "btc", false,
			"exists as a watch-only wallet, not a signing wallet"},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := newWalletNode(t)
			engine, vault := walletDomain(t, node)

			node.addOnDisk("mismatch", test.disk)

			result, _ := create(t, engine, test.slot, "mismatch")

			if result.OK {
				t.Fatalf("the wrong type was accepted: %+v", result)
			}

			if !strings.Contains(result.Error, test.want) {
				t.Fatalf("refusal = %q, want %q", result.Error, test.want)
			}

			// Nothing was created, and nothing was stored for the slot.
			if len(node.created) != 0 {
				t.Fatalf("createwallet was called: %v", node.created)
			}

			if stored, _ := vault.readSecret(context.Background(), walletSlotSecret(test.slot)); stored != "" {
				t.Fatalf("the refused name was stored: %q", stored)
			}
		})
	}
}

// TestWalletCreateRefusesAnUnusableName: the name rules, before any RPC.
func TestWalletCreateRefusesAnUnusableName(t *testing.T) {
	node := newWalletNode(t)
	engine, _ := walletDomain(t, node)

	for name, want := range map[string]string{
		"":                      "a wallet name is required",
		"with space":            "may contain only",
		"with/slash":            "may contain only",
		"with.control\x01":      "may contain only",
		".leading":              "may not start with",
		"-leading":              "may not start with",
		strings.Repeat("a", 65): "longer than 64 bytes",
	} {
		result, _ := create(t, engine, "blake", name)

		if result.OK {
			t.Fatalf("the name %q was accepted", name)
		}

		if !strings.Contains(result.Error, want) {
			t.Fatalf("name %q: refusal = %q, want %q", name, result.Error, want)
		}
	}

	if len(node.created) != 0 || len(node.loads) != 0 {
		t.Fatalf("an invalid name reached the node: created=%v loads=%v", node.created, node.loads)
	}

	// A slot that is not one of the four is refused, and a dotted name is accepted.
	if result, _ := create(t, engine, "ethereum", "x"); result.OK ||
		!strings.Contains(result.Error, "slot must be one of") {
		t.Fatalf("an unknown slot was accepted: %+v", result)
	}

	if result, _ := create(t, engine, "blake", "alice.wallet-2"); !result.OK {
		t.Fatalf("a valid dotted name was refused: %+v", result)
	}
}

// failingSecretKV refuses to seal anything under one prefix.
type failingSecretKV struct {
	platform.KV
	failPrefix string
}

func (k *failingSecretKV) Put(ctx context.Context, key string, value []byte) error {
	if k.failPrefix != "" && strings.HasPrefix(key, k.failPrefix) {
		return errSealRefused
	}

	return k.KV.Put(ctx, key, value)
}

// errSealRefused is a storage failure, not the operator's.
type sealError struct{}

func (*sealError) Error() string { return "the store refused the sealed value" }

var errSealRefused = &sealError{}

// TestWalletCreateReportsASplitOutcome: the node has the wallet and the vault does not have
// the name, and a retry stores it.
func TestWalletCreateReportsASplitOutcome(t *testing.T) {
	node := newWalletNode(t)

	kv := &failingSecretKV{KV: platform.NewMemoryKV()}

	vault := NewVaultDomain(kv, nil)

	if result := run(t, vault, "vault.create", map[string]string{"passphrase": "p"}); !result.OK {
		t.Fatalf("create = %+v", result)
	}

	if result := run(t, vault, "proxy.set", map[string]string{"url": node.server.URL}); !result.OK {
		t.Fatalf("proxy.set = %+v", result)
	}

	engine := newEngineDomain(vault, nil)

	// Seal the wallet name only from now on.
	kv.failPrefix = "secret/" + SecretWalletBlake

	result, _ := create(t, engine, "blake", "lost-name")

	if result.OK {
		t.Fatalf("a wallet whose name was not saved reported success: %+v", result)
	}

	if !strings.Contains(result.Error, "exists on blake") ||
		!strings.Contains(result.Error, "run the same command again") {
		t.Fatalf("refusal = %q", result.Error)
	}

	if len(node.created) != 1 {
		t.Fatalf("the wallet was not created: %v", node.created)
	}

	// The retry is idempotent: the wallet is loaded now, so the name is simply saved.
	kv.failPrefix = ""

	retry, view := create(t, engine, "blake", "lost-name")

	if !retry.OK {
		t.Fatalf("the retry = %+v", retry)
	}

	if view.Created {
		t.Fatalf("the retry created a second wallet: %+v", view)
	}

	if stored, _ := vault.readSecret(context.Background(), SecretWalletBlake); stored != "lost-name" {
		t.Fatalf("the name was not saved by the retry: %q", stored)
	}

	if len(node.created) != 1 {
		t.Fatalf("createwallet was called %d times", len(node.created))
	}
}

// TestWalletCreateNeedsAProxy: the error is the engine's own, and nothing is written.
func TestWalletCreateNeedsAProxy(t *testing.T) {
	ctx := context.Background()

	vault := NewVaultDomain(platform.NewMemoryKV(), nil)

	if result := run(t, vault, "vault.create", map[string]string{"passphrase": "p"}); !result.OK {
		t.Fatalf("create = %+v", result)
	}

	engine := newEngineDomain(vault, nil)

	result, _ := create(t, engine, "blake", "needs-proxy")

	if result.OK || !strings.Contains(result.Error, "no proxy address") {
		t.Fatalf("result = %+v", result)
	}

	if stored, _ := vault.readSecret(ctx, SecretWalletBlake); stored != "" {
		t.Fatalf("a name was stored without a node: %q", stored)
	}
}

// TestWalletCreateReportsANodeRefusal: the node's own words, not a generic failure.
func TestWalletCreateReportsANodeRefusal(t *testing.T) {
	node := newWalletNode(t)
	engine, vault := walletDomain(t, node)

	node.failMethod = "createwallet"

	result, _ := create(t, engine, "blake", "refused")

	if result.OK {
		t.Fatalf("a node refusal was accepted: %+v", result)
	}

	if !strings.Contains(result.Error, "is unavailable") {
		t.Fatalf("refusal = %q", result.Error)
	}

	if stored, _ := vault.readSecret(context.Background(), SecretWalletBlake); stored != "" {
		t.Fatalf("a name was stored for a wallet the node refused: %q", stored)
	}
}

// TestWalletStatusShowsTheFourSlots: the rows are the slots, with the node's answer about
// each, including the wrong-type case.
func TestWalletStatusShowsTheFourSlots(t *testing.T) {
	node := newWalletNode(t)
	engine, vault := walletDomain(t, node)

	if result := run(t, vault, "wallets.set", map[string]string{
		"blake": "signer", "btc": "gone", "blake_watch": "watcher", "btc_watch": "mismatch",
	}); !result.OK {
		t.Fatalf("wallets.set = %+v", result)
	}

	// A signing wallet loaded, a watch-only one loaded, one on disk but not loaded, and
	// one whose kind is wrong for its slot.
	if _, view := create(t, engine, "blake", "signer"); !view.Loaded {
		t.Fatalf("the signing wallet was not created: %+v", view)
	}

	if _, view := create(t, engine, "blake_watch", "watcher"); !view.Loaded {
		t.Fatalf("the watch-only wallet was not created: %+v", view)
	}

	node.addLoaded("mismatch", true)

	result := engine.Handle(context.Background(), proto.Request{Command: "wallets.status"})

	var view proto.WalletView

	if err := proto.DecodeData(result, &view); err != nil {
		t.Fatal(err)
	}

	if len(view.Slots) != 4 {
		t.Fatalf("slots = %+v", view.Slots)
	}

	states := map[string]string{}

	for _, slot := range view.Slots {
		states[slot.Slot] = slot.State
	}

	if states["blake"] != "loaded" || states["blake_watch"] != "loaded" ||
		states["btc"] != "absent" || states["btc_watch"] != "wrong type" {
		t.Fatalf("states = %v", states)
	}

	// The old fields are still there, so an older page keeps working.
	if view.Blake != "signer" || view.BTCWatch != "mismatch" || !view.Visible["signer"] {
		t.Fatalf("the wallet view lost its own fields: %+v", view)
	}

	// Each row names its chain and whether it is watch-only, because that is what the
	// confirmation has to say.
	for _, slot := range view.Slots {
		if slot.Chain == "" || slot.Label == "" {
			t.Fatalf("slot %+v is missing its own description", slot)
		}
	}
}
