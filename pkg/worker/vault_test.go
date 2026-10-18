package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/platform"
	"github.com/gjbrr/blake-swap/pkg/proto"
)

// domain builds a vault domain over a memory KV.
func domain() *vaultDomain {
	return NewVaultDomain(platform.NewMemoryKV(), nil)
}

// run executes a command and fails on an unexpected error.
func run(t *testing.T, d *vaultDomain, command string, args map[string]string) proto.Result {
	t.Helper()

	return d.Run(context.Background(), command, args)
}

// TestVaultLifecycle: create, lock, unlock, and the state each step reports.
func TestVaultLifecycle(t *testing.T) {
	d := domain()

	status := run(t, d, "vault.status", nil)

	if !status.OK || !strings.Contains(status.Text, "vault: absent") {
		t.Fatalf("status = %+v", status)
	}

	created := run(t, d, "vault.create", map[string]string{"passphrase": "correct horse"})

	if !created.OK {
		t.Fatalf("create = %+v", created)
	}

	status = run(t, d, "vault.status", nil)

	if !status.OK || !strings.Contains(status.Text, "vault: present") ||
		!strings.Contains(status.Text, "unlocked: yes") {
		t.Fatalf("status = %+v", status)
	}

	if locked := run(t, d, "vault.lock", nil); !locked.OK {
		t.Fatalf("lock = %+v", locked)
	}

	// A locked vault refuses state operations rather than silently failing later.
	if result := run(t, d, "state.list", nil); result.OK ||
		!strings.Contains(result.Error, "locked") {
		t.Fatalf("list while locked = %+v", result)
	}

	unlocked := run(t, d, "vault.unlock", map[string]string{"passphrase": "correct horse"})

	if !unlocked.OK {
		t.Fatalf("unlock = %+v", unlocked)
	}

	if list := run(t, d, "state.list", nil); !list.OK {
		t.Fatalf("list after unlock = %+v", list)
	}
}

// TestVaultRefusesAWrongPassphrase: unlocking is where a wrong passphrase must be named.
func TestVaultRefusesAWrongPassphrase(t *testing.T) {
	d := domain()

	run(t, d, "vault.create", map[string]string{"passphrase": "right"})
	run(t, d, "vault.lock", nil)

	result := run(t, d, "vault.unlock", map[string]string{"passphrase": "wrong"})

	if result.OK || !strings.Contains(result.Error, "wrong passphrase") {
		t.Fatalf("result = %+v", result)
	}

	// And the correct one still works.
	if result := run(t, d, "vault.unlock", map[string]string{"passphrase": "right"}); !result.OK {
		t.Fatalf("result = %+v", result)
	}
}

// TestStateRoundTrip: the record the worker stores and loads is the same record.
func TestStateRoundTrip(t *testing.T) {
	d := domain()

	run(t, d, "vault.create", map[string]string{"passphrase": "passphrase"})

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

	saved := run(t, d, "state.put", map[string]string{"json": string(encoded)})

	if !saved.OK {
		t.Fatalf("put = %+v", saved)
	}

	loaded := run(t, d, "state.get", map[string]string{"swap_id": record.SwapID})

	if !loaded.OK || !strings.Contains(loaded.Text, record.SwapID) {
		t.Fatalf("get = %+v", loaded)
	}

	list := run(t, d, "state.list", nil)

	if !list.OK || !strings.Contains(list.Text, record.SwapID) {
		t.Fatalf("list = %+v", list)
	}
}

// TestSelfTestRunsInProcess: the browser's self-test is the same code a native test can
// run, which is what makes it worth trusting.
func TestSelfTestRunsInProcess(t *testing.T) {
	result := run(t, domain(), "vault.selftest", nil)

	if !result.OK {
		t.Fatalf("selftest = %+v", result)
	}

	if !strings.Contains(result.Text, "encrypted round trip ok") {
		t.Fatalf("text = %q", result.Text)
	}
}

// TestDispatchEncodesAndDecodes: the envelope body is the contract between the two wasm
// instances, so it is exercised as JSON rather than as a struct.
func TestDispatchEncodesAndDecodes(t *testing.T) {
	d := domain()

	body, err := proto.EncodeRequest(proto.Request{
		Command: "vault.create",
		Args:    map[string]string{"passphrase": "passphrase"},
	})

	if err != nil {
		t.Fatal(err)
	}

	result := handleCommand(context.Background(), d, newEngineDomain(d), body)

	if !result.OK || result.Command != "vault.create" {
		t.Fatalf("result = %+v", result)
	}

	encoded, err := proto.EncodeResult(result)

	if err != nil {
		t.Fatal(err)
	}

	decoded, err := proto.DecodeResult(encoded)

	if err != nil {
		t.Fatal(err)
	}

	if !decoded.OK || decoded.Command != "vault.create" {
		t.Fatalf("decoded = %+v", decoded)
	}
}

// TestUnknownCommandIsRefused: a command the worker does not know is an error, not a
// guess.
func TestUnknownCommandIsRefused(t *testing.T) {
	body, err := proto.EncodeRequest(proto.Request{Command: "not.a.command"})

	if err != nil {
		t.Fatal(err)
	}

	domain := domain()

	result := handleCommand(context.Background(), domain, newEngineDomain(domain), body)

	if result.OK || !strings.Contains(result.Error, "unsupported command") {
		t.Fatalf("result = %+v", result)
	}
}
