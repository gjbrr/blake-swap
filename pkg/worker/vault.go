package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	// defaultProxy is the page's own origin, offered as the proxy when none is
	// configured. It is memory only: where this document was loaded from is not a
	// setting worth sealing, and it must never overwrite one the operator chose.
	defaultProxy string
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

		//
		// The role is the caller's, because this command exists to seed a record in a
		// test and a seeded Bob record must be stored as Bob's. The store itself refuses a
		// save that would change an existing record's side.
		//
		role := strings.TrimSpace(request.Args["role"])

		if role == "" {
			role = store.Alice
		}

		if err := d.store.Save(ctx, role, record); err != nil {
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

	case "wallets.set":
		return d.setWallets(ctx, request)

	case "identity.generate":
		return d.generateIdentity(ctx, request)

	case "proxy.default":
		//
		// The page supplies its own origin so that an app served by a server can reach
		// the nodes it is already talking to. It is a fact about where this document was
		// loaded from, not a protocol decision, and it never overwrites a proxy the
		// operator configured.
		//
		d.defaultProxy = strings.TrimRight(strings.TrimSpace(request.Args["url"]), "/")

		return proto.Result{
			Command: request.Command, OK: true, Text: "proxy default: " + d.defaultProxy,
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

// proxyAddress is the proxy the engine should use.
//
// A configured proxy always wins. The page's origin is the fallback, which is what makes
// a browser served by cmd/server work with no setup: the server already proxies the two
// chains on its own origin.
func (d *vaultDomain) proxyAddress(ctx context.Context) (string, error) {
	stored, err := d.readSecret(ctx, SecretProxy)

	if err != nil {
		return "", err
	}

	if stored != "" {
		return stored, nil
	}

	return d.defaultProxy, nil
}

// setWallets stores the four wallet names.
//
// The names are validated as names rather than as a set of known values: a node may call
// its wallets anything, and the client's job is to refuse what a node cannot accept, not
// to choose for it. What is refused is the shapes that turn into a URL path or a shell
// token: empty, whitespace, slashes, dots-only, and anything unexpected in a wallet name.
func (d *vaultDomain) setWallets(ctx context.Context, request proto.Request) proto.Result {
	if d.store == nil {
		return proto.Result{Command: request.Command, Error: "the vault is locked"}
	}

	pairs := []struct {
		argument string
		name     string
		label    string
	}{
		{"blake", SecretWalletBlake, "BLAKE wallet"},
		{"btc", SecretWalletBTC, "BTC wallet"},
		{"blake_watch", SecretWalletBlakeWatch, "BLAKE watch-only wallet"},
		{"btc_watch", SecretWalletBTCWatch, "BTC watch-only wallet"},
	}

	for _, pair := range pairs {
		value := strings.TrimSpace(request.Args[pair.argument])

		if value == "" {
			return proto.Result{
				Command: request.Command,
				Error:   pair.label + " name is required",
			}
		}

		if err := validateWalletName(value); err != nil {
			return proto.Result{
				Command: request.Command,
				Error:   pair.label + ": " + err.Error(),
			}
		}

		if err := d.writeSecret(ctx, pair.name, value); err != nil {
			return failed(request.Command, err)
		}
	}

	return proto.Result{
		Command: request.Command, OK: true, Text: "wallets set",
	}
}

// validateWalletName refuses a name a node would not accept.
func validateWalletName(name string) error {
	if len(name) > 64 {
		return fmt.Errorf("name is longer than 64 characters")
	}

	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
			continue
		default:
			return fmt.Errorf("name may contain only letters, digits, '-' and '_'")
		}
	}

	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("name may not start with '-'")
	}

	return nil
}

// generateIdentity creates a new Nostr key, seals it, and returns the secret exactly
// once.
//
// The secret is in the reply because a person must be able to write their key down, and
// that is the only moment it can be shown: after this reply the worker holds it sealed
// and the page holds nothing. Storing it nowhere else is the whole design.
func (d *vaultDomain) generateIdentity(ctx context.Context, request proto.Request) proto.Result {
	if d.store == nil {
		return proto.Result{Command: request.Command, Error: "the vault is locked"}
	}

	existing, err := d.readSecret(ctx, SecretIdentity)

	if err != nil {
		return failed(request.Command, err)
	}

	//
	// Replacing a key silently would orphan every swap pinned to the old one, so a
	// second generate is refused unless the caller says it means it.
	//
	if existing != "" && !strings.EqualFold(strings.TrimSpace(request.Args["replace"]), "true") {
		key, err := nostr.NewKey(existing)

		if err != nil {
			return failed(request.Command, err)
		}

		return proto.Result{
			Command: request.Command,
			Error: "an identity is already configured (" + key.Public +
				"); pass replace=true to create a new one",
		}
	}

	secret, err := generateSecret()
	key, err := nostr.NewKey(secret)

	if err != nil {
		return failed(request.Command, err)
	}

	if err := d.writeSecret(ctx, SecretIdentity, key.Secret); err != nil {
		return failed(request.Command, err)
	}

	return withView(proto.Result{
		Command: request.Command,
		OK:      true,
		Text:    "identity generated: " + key.Public,
	}, proto.IdentityView{Public: key.Public, Secret: key.Secret})
}

// generateSecret makes a fresh 32-byte key as hex.
func generateSecret() (string, error) {
	buffer := make([]byte, 32)

	if _, err := io.ReadFull(randomReader{}, buffer); err != nil {
		return "", err
	}

	return hex.EncodeToString(buffer), nil
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
	ctx context.Context,
	vault *vaultDomain,
	engine *engineDomain,
	market *marketDomain,
	settlement *coordDomain,
	body string,
) proto.Result {
	request, err := proto.DecodeRequest(body)

	if err != nil {
		return proto.Result{Command: "", Error: err.Error()}
	}

	switch {
	case request.Command == "wallets.status":
		return engine.Handle(ctx, request)
	case strings.HasPrefix(request.Command, "vault."),
		strings.HasPrefix(request.Command, "state."),
		strings.HasPrefix(request.Command, "identity."),
		strings.HasPrefix(request.Command, "proxy."),
		strings.HasPrefix(request.Command, "wallets."):
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
	case strings.HasPrefix(request.Command, "coord."):
		if settlement == nil {
			return proto.Result{
				Command: request.Command,
				Error:   "this worker has no coordination domain",
			}
		}

		return settlement.Handle(ctx, request)
	}

	return proto.Result{
		Command: request.Command,
		Error:   "unsupported command: " + request.Command,
	}
}
