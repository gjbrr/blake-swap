//go:build !js

// The engine domain's write surface, its structured results, and the role derivation that
// replaces the test rig's hardcoded mapping.
package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/platform"
	"github.com/gjbrr/blake-swap/pkg/proto"
	"github.com/gjbrr/blake-swap/pkg/store"
)

// putRecord seeds a sealed record through the vault, the way a test or a migration would.
func putRecord(t *testing.T, vault *vaultDomain, record *model.Swap, role string) {
	t.Helper()

	encoded, err := json.Marshal(record)

	if err != nil {
		t.Fatal(err)
	}

	result := run(t, vault, "state.put", map[string]string{"json": string(encoded), "role": role})

	if !result.OK {
		t.Fatalf("state.put = %+v", result)
	}
}

// identityPublic is the public key of the identity the harness sets.
func identityPublic(t *testing.T) string {
	t.Helper()

	key, err := nostr.NewKey(strings.Repeat("11", 32))

	if err != nil {
		t.Fatal(err)
	}

	return key.Public
}

// pinnedRecord is a record created by the local identity (Alice) with an optional peer.
func pinnedRecord(t *testing.T, id, alice, bob string) *model.Swap {
	t.Helper()

	record, err := model.New(strings.NewReader(strings.Repeat("ab", 64)), model.NewParams{
		SwapID:       id,
		BlakeAmount:  amount.Blake(1_000_000),
		BTCSats:      25_000,
		BlakeTimeout: 500,
		BTCTimeout:   400,
	})

	if err != nil {
		t.Fatal(err)
	}

	if alice != "" {
		record.NostrAlicePub = &alice
	}

	if bob != "" {
		record.NostrBobPub = &bob
	}

	return record
}

// TestStatusCarriesAView: the page renders the view and never the text, so every command
// that has a view must return one that parses.
func TestStatusCarriesAView(t *testing.T) {
	proxy := newFakeProxy(t)
	engine, _ := engineDomainFor(t, proxy.server.URL)

	result := engine.Handle(context.Background(), proto.Request{Command: "engine.status"})

	if !result.OK {
		t.Fatalf("status = %+v", result)
	}

	if !json.Valid(result.Data) {
		t.Fatalf("status data is not JSON: %q", result.Data)
	}

	var view proto.SetupView

	if err := proto.DecodeData(result, &view); err != nil {
		t.Fatal(err)
	}

	if view.Heights.Blake != 200 || view.Heights.BTC != 100 {
		t.Fatalf("heights = %+v", view.Heights)
	}

	// The text is the CLI's shape and must not have moved.
	if !strings.Contains(result.Text, "BLAKE height: 200") {
		t.Fatalf("text = %q", result.Text)
	}
}

// TestShowCarriesTheSwapView: amounts cross as exact strings, and the id in the view is the
// record's.
func TestShowCarriesTheSwapView(t *testing.T) {
	proxy := newFakeProxy(t)
	engine, vault := engineDomainFor(t, proxy.server.URL)

	record := pinnedRecord(t, "aaaa00000011", identityPublic(t), strings.Repeat("22", 32))

	putRecord(t, vault, record, store.Alice)

	result := engine.Handle(context.Background(), proto.Request{
		Command: "engine.show", Args: map[string]string{"swap_id": record.SwapID},
	})

	if !result.OK {
		t.Fatalf("show = %+v", result)
	}

	var view proto.SwapSummary

	if err := proto.DecodeData(result, &view); err != nil {
		t.Fatal(err)
	}

	if view.SwapID != record.SwapID || view.State != string(record.State) {
		t.Fatalf("view = %+v", view)
	}

	if view.BlakeAmount != "0.01" || view.BTCSats != "25000" {
		t.Fatalf("amounts = %q / %q", view.BlakeAmount, view.BTCSats)
	}

	if view.Role != store.Alice {
		t.Fatalf("role = %q, want alice", view.Role)
	}

	// The secret hash is public and the secret is not.
	if view.SecretHash != record.SecretHash {
		t.Fatalf("secret hash = %q", view.SecretHash)
	}

	if strings.Contains(string(result.Data), *record.SecretHex) {
		t.Fatal("the show view carried the swap secret")
	}
}

// TestRoleFollowsTheRecordNotTheClient is the fix for the test rig's mapping: the same
// client is Alice in one swap and Bob in another, and the role comes from the record's own
// identity pins.
func TestRoleFollowsTheRecordNotTheClient(t *testing.T) {
	proxy := newFakeProxy(t)
	engine, vault := engineDomainFor(t, proxy.server.URL)

	me := identityPublic(t)
	peer := strings.Repeat("22", 32)

	mine := pinnedRecord(t, "aaaa00000021", me, peer)
	theirs := pinnedRecord(t, "aaaa00000022", peer, me)

	putRecord(t, vault, mine, store.Alice)
	putRecord(t, vault, theirs, store.Bob)

	for _, test := range []struct {
		record *model.Swap
		want   string
	}{{mine, store.Alice}, {theirs, store.Bob}} {
		result := engine.Handle(context.Background(), proto.Request{
			Command: "engine.show", Args: map[string]string{"swap_id": test.record.SwapID},
		})

		if !result.OK {
			t.Fatalf("%s: show = %+v", test.record.SwapID, result)
		}

		var view proto.SwapSummary

		if err := proto.DecodeData(result, &view); err != nil {
			t.Fatal(err)
		}

		if view.Role != test.want {
			t.Fatalf("%s: role = %q, want %q", test.record.SwapID, view.Role, test.want)
		}
	}

	// And the engine the command ran on was given that role, so its own save cannot
	// change the record's side.
	built, err := engine.buildFor(context.Background(), mine.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if built.Role != store.Alice {
		t.Fatalf("the engine's role for an Alice swap is %q", built.Role)
	}

	built, err = engine.buildFor(context.Background(), theirs.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if built.Role != store.Bob {
		t.Fatalf("the engine's role for a Bob swap is %q", built.Role)
	}
}

// TestAnUnpinnedRecipientIsStillAlice: Bob's identity is learned when he accepts, so before
// that a record is Alice's. The fallback is a fact about who creates swaps, not a default
// that could be wrong about an accepted one.
func TestAnUnpinnedRecipientIsStillAlice(t *testing.T) {
	proxy := newFakeProxy(t)
	engine, vault := engineDomainFor(t, proxy.server.URL)

	record := pinnedRecord(t, "aaaa00000023", identityPublic(t), "")

	putRecord(t, vault, record, store.Alice)

	result := engine.Handle(context.Background(), proto.Request{
		Command: "engine.show", Args: map[string]string{"swap_id": record.SwapID},
	})

	var view proto.SwapSummary

	if err := proto.DecodeData(result, &view); err != nil {
		t.Fatal(err)
	}

	if view.Role != store.Alice || view.Counterparty != "" {
		t.Fatalf("view = %+v", view)
	}
}

// TestListCarriesEverySwap: the list view is what the Swaps screen renders before any
// report is built, so it must not need a chain read.
func TestListCarriesEverySwap(t *testing.T) {
	proxy := newFakeProxy(t)
	engine, vault := engineDomainFor(t, proxy.server.URL)

	me := identityPublic(t)

	putRecord(t, vault, pinnedRecord(t, "aaaa00000031", me, ""), store.Alice)
	putRecord(t, vault, pinnedRecord(t, "aaaa00000032", me, ""), store.Alice)

	result := engine.Handle(context.Background(), proto.Request{Command: "engine.list"})

	if !result.OK {
		t.Fatalf("list = %+v", result)
	}

	var view proto.SwapList

	if err := proto.DecodeData(result, &view); err != nil {
		t.Fatal(err)
	}

	if len(view.Swaps) != 2 {
		t.Fatalf("listed %d swaps, want 2", len(view.Swaps))
	}

	for _, summary := range view.Swaps {
		if summary.BlakeAmount != "0.01" || summary.BTCSats != "25000" {
			t.Fatalf("summary amounts = %+v", summary)
		}
	}
}

// TestWalletNamesAreStoredAndReported: the four names are this participant's own, and an
// invalid name is refused rather than stored.
func TestWalletNamesAreStoredAndReported(t *testing.T) {
	proxy := newFakeProxy(t)
	engine, vault := engineDomainFor(t, proxy.server.URL)

	// Before anything is set, the neutral defaults are reported.
	status := engine.Handle(context.Background(), proto.Request{Command: "wallets.status"})

	if !status.OK {
		t.Fatalf("wallets.status = %+v", status)
	}

	var view proto.WalletView

	if err := proto.DecodeData(status, &view); err != nil {
		t.Fatal(err)
	}

	if view.Blake != defaultWalletBlake || view.BTC != defaultWalletBTC ||
		view.BlakeWatch != defaultWalletBlakeWatch || view.BTCWatch != defaultWalletBTCWatch {
		t.Fatalf("defaults = %+v", view)
	}

	set := run(t, vault, "wallets.set", map[string]string{
		"blake": "alice", "btc": "alice", "blake_watch": "alice-watch", "btc_watch": "alice-watch",
	})

	if !set.OK {
		t.Fatalf("wallets.set = %+v", set)
	}

	status = engine.Handle(context.Background(), proto.Request{Command: "wallets.status"})

	if err := proto.DecodeData(status, &view); err != nil {
		t.Fatal(err)
	}

	if view.Blake != "alice" || view.BTC != "alice" || view.BlakeWatch != "alice-watch" {
		t.Fatalf("configured wallets = %+v", view)
	}

	// A name that would become a URL path or a shell token is refused.
	for _, bad := range []string{"../escape", "with space", "-leading", ""} {
		result := run(t, vault, "wallets.set", map[string]string{
			"blake": bad, "btc": "btc", "blake_watch": "w", "btc_watch": "w",
		})

		if result.OK {
			t.Fatalf("wallet name %q was accepted", bad)
		}
	}
}

// TestIdentityGenerateShowsTheSecretOnce: a new key is sealed, its public form is
// reported from then on, and the secret is in the reply that created it and nowhere else.
func TestIdentityGenerateShowsTheSecretOnce(t *testing.T) {
	vault := NewVaultDomain(platform.NewMemoryKV(), nil)

	if result := run(t, vault, "vault.create", map[string]string{"passphrase": "p"}); !result.OK {
		t.Fatalf("create = %+v", result)
	}

	generated := run(t, vault, "identity.generate", nil)

	if !generated.OK {
		t.Fatalf("identity.generate = %+v", generated)
	}

	var view proto.IdentityView

	if err := proto.DecodeData(generated, &view); err != nil {
		t.Fatal(err)
	}

	if len(view.Public) != 64 || len(view.Secret) != 64 {
		t.Fatalf("generated identity = %+v", view)
	}

	// It is a real key: the public form is derived from the secret.
	key, err := nostr.NewKey(view.Secret)

	if err != nil {
		t.Fatal(err)
	}

	if key.Public != view.Public {
		t.Fatalf("public %q is not derived from the secret", view.Public)
	}

	// Status reports the public key and no secret.
	status := run(t, vault, "identity.status", nil)

	if !status.OK || !strings.Contains(status.Text, view.Public) {
		t.Fatalf("identity.status = %+v", status)
	}

	if strings.Contains(status.Text, view.Secret) || len(status.Data) != 0 {
		t.Fatal("identity.status reported the secret")
	}

	// Replacing an identity silently would orphan every swap pinned to it.
	again := run(t, vault, "identity.generate", nil)

	if again.OK || !strings.Contains(again.Error, "replace=true") {
		t.Fatalf("a second generate did not ask: %+v", again)
	}

	replaced := run(t, vault, "identity.generate", map[string]string{"replace": "true"})

	if !replaced.OK {
		t.Fatalf("identity.generate replace = %+v", replaced)
	}

	var second proto.IdentityView

	if err := proto.DecodeData(replaced, &second); err != nil {
		t.Fatal(err)
	}

	if second.Secret == view.Secret {
		t.Fatal("replace produced the same key")
	}
}

// TestProxyDefaultIsThePagesOrigin: a browser served by the app server needs no proxy
// setting, but a configured proxy always wins.
func TestProxyDefaultIsThePagesOrigin(t *testing.T) {
	proxy := newFakeProxy(t)

	vault := NewVaultDomain(platform.NewMemoryKV(), nil)

	if result := run(t, vault, "vault.create", map[string]string{"passphrase": "p"}); !result.OK {
		t.Fatalf("create = %+v", result)
	}

	if result := run(t, vault, "proxy.default", map[string]string{"url": proxy.server.URL}); !result.OK {
		t.Fatalf("proxy.default = %+v", result)
	}

	if result := run(t, vault, "identity.set", map[string]string{
		"secret": strings.Repeat("11", 32),
	}); !result.OK {
		t.Fatalf("identity.set = %+v", result)
	}

	engine := newEngineDomain(vault, nil)

	result := engine.Handle(context.Background(), proto.Request{Command: "engine.status"})

	if !result.OK || !strings.Contains(result.Text, "BLAKE height: 200") {
		t.Fatalf("status through the default proxy = %+v", result)
	}

	// A configured proxy outranks the origin.
	if result := run(t, vault, "proxy.set", map[string]string{"url": "http://127.0.0.1:1"}); !result.OK {
		t.Fatalf("proxy.set = %+v", result)
	}

	result = engine.Handle(context.Background(), proto.Request{Command: "engine.status"})

	if result.OK {
		t.Fatal("the configured proxy did not outrank the page origin")
	}
}

// TestUnsupportedEngineCommandIsNamed: a typo is reported as the command that was not
// understood, not as a silent success.
func TestUnsupportedEngineCommandIsNamed(t *testing.T) {
	proxy := newFakeProxy(t)
	engine, _ := engineDomainFor(t, proxy.server.URL)

	result := engine.Handle(context.Background(), proto.Request{Command: "engine.explode"})

	if result.OK || !strings.Contains(result.Error, "engine.explode") {
		t.Fatalf("result = %+v", result)
	}

	// The test-only surface is not exposed to the client at all.
	advance := engine.Handle(context.Background(), proto.Request{
		Command: "engine.advance", Args: map[string]string{"swap_id": "aaaa00000001"},
	})

	if advance.OK || !strings.Contains(advance.Error, "unsupported") {
		t.Fatalf("advance = %+v", advance)
	}
}

// TestValidationRefusalsCarryNoView: a refused command has no data, because a view would
// describe something that did not happen.
func TestValidationRefusalsCarryNoView(t *testing.T) {
	proxy := newFakeProxy(t)
	engine, _ := engineDomainFor(t, proxy.server.URL)

	cases := []struct {
		command string
		args    map[string]string
		want    string
	}{
		{"engine.new", map[string]string{"blake": "notanumber", "sats": "1"}, "BLAKE amount"},
		{"engine.new", map[string]string{"blake": "1", "sats": "0"}, "positive"},
		{"engine.new", map[string]string{"blake": "1", "sats": "1", "btc_timeout": "10"},
			"either supply both"},
		{"engine.accept-offer", map[string]string{}, "requires a swap id"},
		{"engine.claim-btc", map[string]string{"swap_id": "aaaa00000001", "fee_rate": "0"},
			"fee_rate"},
	}

	for _, test := range cases {
		result := engine.Handle(context.Background(), proto.Request{
			Command: test.command, Args: test.args,
		})

		if result.OK {
			t.Fatalf("%s %v was accepted", test.command, test.args)
		}

		if !strings.Contains(result.Error, test.want) {
			t.Fatalf("%s %v: error = %q, want %q", test.command, test.args, result.Error, test.want)
		}

		if len(result.Data) != 0 {
			t.Fatalf("%s carried a view for a refusal: %s", test.command, result.Data)
		}
	}
}

// TestExtractSecretIsNotReturned: the preimage belongs in the sealed record. A browser
// that printed it would be a browser with a secret in the DOM.
func TestExtractSecretIsNotReturned(t *testing.T) {
	proxy := newFakeProxy(t)
	engine, vault := engineDomainFor(t, proxy.server.URL)

	record := pinnedRecord(t, "aaaa00000041", identityPublic(t), strings.Repeat("22", 32))

	putRecord(t, vault, record, store.Alice)

	result := engine.Handle(context.Background(), proto.Request{
		Command: "engine.extract-secret", Args: map[string]string{"swap_id": record.SwapID},
	})

	// With no Bitcoin claim there is no revealed secret to extract, so the engine
	// refuses: what matters here is that the refusal does not carry the local secret
	// either.
	if strings.Contains(result.Text, *record.SecretHex) ||
		strings.Contains(string(result.Data), *record.SecretHex) {
		t.Fatal("extract-secret returned the swap secret")
	}
}
