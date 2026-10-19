package worker

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"strings"

	"github.com/gjbrr/blake-swap/pkg/client"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/nostr"
	"github.com/gjbrr/blake-swap/pkg/platform"
	"github.com/gjbrr/blake-swap/pkg/proto"
	"github.com/gjbrr/blake-swap/pkg/store"
)

// vaultDomain is the worker's encrypted-state domain.
//
// It owns the one thing the page must never hold: the derived key. The page asks for
// operations; the key stays in this instance, and the storage underneath it is
// IndexedDB sealed by pkg/client.
//
// The command names are the engine's, not a second vocabulary: "vault.create",
// "vault.unlock", "vault.lock", "vault.status", "state.put", "state.get". A page that
// can speak these can do nothing the CLI could not, which is the property that keeps
// two front ends from drifting.
type vaultDomain struct {
	kv       platform.KV
	random   io.Reader
	store    *client.Encrypted
	unlocked bool
}

// NewVaultDomain builds a vault domain over any KV and entropy source.
//
// The browser passes IndexedDB and WebCrypto; a test passes memory and crypto/rand. The
// dispatch below is the same code either way, which is the only way a browser-only
// command can be tested.
func NewVaultDomain(kv platform.KV, random io.Reader) *vaultDomain {
	if random == nil {
		random = rand.Reader
	}

	return &vaultDomain{kv: kv, random: random}
}

// Run executes one command, for callers that hold the domain directly.
func (d *vaultDomain) Run(ctx context.Context, command string, args map[string]string) proto.Result {
	return d.handle(ctx, proto.Request{Command: command, Args: args})
}

// handle runs one vault command.
func (d *vaultDomain) handle(ctx context.Context, request proto.Request) proto.Result {
	switch request.Command {
	case "vault.status":
		exists, err := client.Exists(ctx, d.kv)

		if err != nil {
			return failed(request.Command, err)
		}

		text := "vault: absent"

		if exists {
			text = "vault: present"
		}

		if d.unlocked {
			text += "\nunlocked: yes"
		} else {
			text += "\nunlocked: no"
		}

		return proto.Result{Command: request.Command, OK: true, Text: text}

	case "vault.create":
		passphrase := request.Args["passphrase"]

		if passphrase == "" {
			return proto.Result{
				Command: request.Command,
				Error:   "a vault needs a passphrase",
			}
		}

		created, err := client.CreateWith(ctx, d.kv, passphrase, d.random)

		if err != nil {
			return failed(request.Command, err)
		}

		d.store = created
		d.unlocked = true

		return proto.Result{
			Command: request.Command, OK: true, Text: "vault created and unlocked",
		}

	case "vault.unlock":
		passphrase := request.Args["passphrase"]

		if passphrase == "" {
			return proto.Result{Command: request.Command, Error: "a passphrase is required"}
		}

		opened, err := client.OpenWith(ctx, d.kv, passphrase, d.random)

		if err != nil {
			return failed(request.Command, err)
		}

		d.store = opened
		d.unlocked = true

		return proto.Result{Command: request.Command, OK: true, Text: "vault unlocked"}

	case "vault.lock":
		d.store = nil
		d.unlocked = false

		return proto.Result{Command: request.Command, OK: true, Text: "vault locked"}

	case "state.put":
		if d.store == nil {
			return proto.Result{Command: request.Command, Error: "the vault is locked"}
		}

		// The record is a whole swap, because that is the only thing the store holds:
		// a debug "put any blob" command would be a second storage API.
		record, err := decodeSwap(request.Args["json"])

		if err != nil {
			return failed(request.Command, err)
		}

		if err := d.store.Save(ctx, store.Alice, record); err != nil {
			return failed(request.Command, err)
		}

		return proto.Result{
			Command: request.Command, OK: true, Text: "saved " + record.SwapID,
		}

	case "state.get":
		if d.store == nil {
			return proto.Result{Command: request.Command, Error: "the vault is locked"}
		}

		swapID := request.Args["swap_id"]

		record, err := d.store.Load(ctx, swapID)

		if err != nil {
			return failed(request.Command, err)
		}

		return proto.Result{
			Command: request.Command,
			OK:      true,
			Text:    "loaded " + record.SwapID + " state " + string(record.State),
		}

	case "vault.crypto":
		buffer := make([]byte, 32)

		n, err := platform.ReadRandom(buffer)

		if err != nil {
			return failed(request.Command, err)
		}

		return proto.Result{
			Command: request.Command,
			OK:      true,
			Text:    "random ok: " + itoa(uint32(n)) + " bytes, first " + itoa(uint32(buffer[0])),
		}

	case "vault.selftest":
		//
		// A self-test that runs where the client actually runs: it seals a real swap
		// record, reopens it, and checks the two agree. It uses a memory KV so it
		// leaves the user's vault alone, while still exercising the whole encrypted
		// path inside the wasm instance — the one place a native test cannot reach.
		//
		return vaultSelfTest()

	case "identity.set":
		secret := strings.TrimSpace(request.Args["secret"])

		if secret == "" {
			return proto.Result{Command: request.Command, Error: "a participant secret is required"}
		}

		key, err := nostr.NewKey(secret)

		if err != nil {
			return failed(request.Command, err)
		}

		if err := d.writeSecret(ctx, SecretIdentity, key.Secret); err != nil {
			return failed(request.Command, err)
		}

		return proto.Result{
			Command: request.Command, OK: true, Text: "identity set: " + key.Public,
		}

	case "identity.status":
		secret, err := d.readSecret(ctx, SecretIdentity)

		if err != nil {
			return failed(request.Command, err)
		}

		if secret == "" {
			return proto.Result{Command: request.Command, OK: true, Text: "identity: absent"}
		}

		key, err := nostr.NewKey(secret)

		if err != nil {
			return failed(request.Command, err)
		}

		return proto.Result{
			Command: request.Command, OK: true, Text: "identity: " + key.Public,
		}

	case "proxy.set":
		value := strings.TrimRight(strings.TrimSpace(request.Args["url"]), "/")

		if value == "" {
			return proto.Result{Command: request.Command, Error: "a proxy URL is required"}
		}

		if err := d.writeSecret(ctx, SecretProxy, value); err != nil {
			return failed(request.Command, err)
		}

		return proto.Result{
			Command: request.Command, OK: true, Text: "proxy set: " + value,
		}

	case "proxy.status":
		value, err := d.readSecret(ctx, SecretProxy)

		if err != nil {
			return failed(request.Command, err)
		}

		if value == "" {
			return proto.Result{Command: request.Command, OK: true, Text: "proxy: absent"}
		}

		return proto.Result{
			Command: request.Command, OK: true, Text: "proxy: " + value,
		}

	case "state.list":
		if d.store == nil {
			return proto.Result{Command: request.Command, Error: "the vault is locked"}
		}

		ids, err := d.store.List(ctx)

		if err != nil {
			return failed(request.Command, err)
		}

		if len(ids) == 0 {
			return proto.Result{Command: request.Command, OK: true, Text: "no swaps"}
		}

		return proto.Result{
			Command: request.Command, OK: true, Text: strings.Join(ids, "\n"),
		}
	}

	return proto.Result{
		Command: request.Command,
		Error:   fmt.Sprintf("unknown command: %s", request.Command),
	}
}

// vaultSelfTest runs the encrypted round trip in the browser.
func vaultSelfTest() proto.Result {
	ctx := context.Background()

	logf("selftest: start")

	kv := platform.NewMemoryKV()

	logf("selftest: creating the vault (key derivation)")

	created, err := client.CreateWith(ctx, kv, "selftest passphrase", platform.Reader{})

	if err != nil {
		return failed("vault.selftest", err)
	}

	logf("selftest: creating a record")

	record, err := model.New(randomReader{}, model.NewParams{
		BlakeAmount:  1_000_000,
		BTCSats:      25_000,
		BlakeTimeout: 500,
		BTCTimeout:   400,
	})

	if err != nil {
		return failed("vault.selftest", err)
	}

	logf("selftest: sealing the record")

	if err := created.Save(ctx, store.Alice, record); err != nil {
		return failed("vault.selftest", err)
	}

	logf("selftest: loading it back")

	loaded, err := created.Load(ctx, record.SwapID)

	if err != nil {
		return failed("vault.selftest", err)
	}

	if loaded.SwapID != record.SwapID || loaded.SecretHash != record.SecretHash {
		return proto.Result{
			Command: "vault.selftest",
			Error:   "the record did not survive the encrypted round trip",
		}
	}

	logf("selftest: checking a wrong passphrase is refused")

	// The same store must refuse a wrong passphrase, or the vault's guarantee is
	// decorative.
	if _, err := client.Open(ctx, kv, "wrong passphrase"); err == nil {
		return proto.Result{
			Command: "vault.selftest",
			Error:   "a wrong passphrase opened the vault",
		}
	}

	logf("selftest: done")

	return proto.Result{
		Command: "vault.selftest",
		OK:      true,
		Text:    "encrypted round trip ok: " + loaded.SwapID,
	}
}

// randomReader is the platform's entropy source.
type randomReader struct{}

// Read fills p from the platform's cryptographic random source.
func (randomReader) Read(p []byte) (int, error) { return platform.ReadRandom(p) }

// readSecret reads a named sealed value from the vault.
func (d *vaultDomain) readSecret(ctx context.Context, name string) (string, error) {
	if d.store == nil {
		return "", fmt.Errorf("the vault is locked")
	}

	value, found, err := d.store.GetSecret(ctx, name)

	if err != nil {
		return "", err
	}

	if !found {
		return "", nil
	}

	return string(value), nil
}

// writeSecret stores a named sealed value.
func (d *vaultDomain) writeSecret(ctx context.Context, name, value string) error {
	if d.store == nil {
		return fmt.Errorf("the vault is locked")
	}

	return d.store.PutSecret(ctx, name, []byte(value))
}

// itoa renders an unsigned integer without pulling in formatting paths.
func itoa(value uint32) string {
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

// logf writes one diagnostic line through the platform console.
func logf(message string) { platform.PrintConsole{}.Log("worker: " + message) }

// failed renders an error result.
func failed(command string, err error) proto.Result {
	return proto.Result{Command: command, Error: err.Error()}
}

// decodeSwap parses a swap record from JSON.
func decodeSwap(encoded string) (*model.Swap, error) {
	if encoded == "" {
		return nil, fmt.Errorf("no record supplied")
	}

	return model.Decode([]byte(encoded), nil)
}

// handleCommand dispatches one command envelope.
//
// It exists so the worker's loop has one place that knows how a command becomes a
// result, and so a new domain is a case in a dispatch rather than a second protocol.
func handleCommand(
	ctx context.Context, vault *vaultDomain, engine *engineDomain, market *marketDomain, body string,
) proto.Result {
	request, err := proto.DecodeRequest(body)

	if err != nil {
		return proto.Result{Command: "", Error: err.Error()}
	}

	switch {
	case strings.HasPrefix(request.Command, "vault."),
		strings.HasPrefix(request.Command, "state."),
		strings.HasPrefix(request.Command, "identity."),
		strings.HasPrefix(request.Command, "proxy."):
		return vault.handle(ctx, request)
	case strings.HasPrefix(request.Command, "engine."):
		return engine.Handle(ctx, request)
	case strings.HasPrefix(request.Command, "market."):
		if market == nil {
			return proto.Result{
				Command: request.Command,
				Error:   "this worker has no market domain",
			}
		}

		return market.Handle(ctx, request)
	}

	return proto.Result{
		Command: request.Command,
		Error:   "unsupported command: " + request.Command,
	}
}
