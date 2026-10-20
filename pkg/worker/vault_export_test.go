package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/platform"
	"github.com/gjbrr/blake-swap/pkg/proto"
	"github.com/gjbrr/blake-swap/pkg/store"
	"github.com/gjbrr/blake-swap/pkg/vaultbackup"
)

// callKV is a KV that records every mutating call.
//
// A restore is the one operation where "how it wrote" is the safety property: a partial
// write leaves a store whose header and records disagree. This double answers whether the
// import replaced the store in one call, and whether it wrote anything at all when it
// refused.
type callKV struct {
	platform.KV
	replacements int
	puts         int
	deletes      int
	// replacementFailure, when set, makes ReplaceAll fail.
	replacementFailure error
	// last is the entries of the last successful ReplaceAll.
	last map[string][]byte
}

func (k *callKV) Put(ctx context.Context, key string, value []byte) error {
	k.puts++

	return k.KV.Put(ctx, key, value)
}

func (k *callKV) Delete(ctx context.Context, key string) error {
	k.deletes++

	return k.KV.Delete(ctx, key)
}

func (k *callKV) ReplaceAll(ctx context.Context, entries map[string][]byte) error {
	k.replacements++

	if k.replacementFailure != nil {
		return k.replacementFailure
	}

	if err := k.KV.ReplaceAll(ctx, entries); err != nil {
		return err
	}

	k.last = entries

	return nil
}

// snapshot reads every key, for a byte-for-byte comparison of two stores.
func snapshot(t *testing.T, kv platform.KV) map[string][]byte {
	t.Helper()

	ctx := context.Background()

	keys, err := kv.Keys(ctx, "")

	if err != nil {
		t.Fatal(err)
	}

	out := make(map[string][]byte, len(keys))

	for _, key := range keys {
		value, found, err := kv.Get(ctx, key)

		if err != nil || !found {
			t.Fatalf("reading %q: %v", key, err)
		}

		out[key] = value
	}

	return out
}

// sameEntries reports whether two stores hold the same bytes under the same keys.
func sameEntries(left, right map[string][]byte) bool {
	if len(left) != len(right) {
		return false
	}

	for key, value := range left {
		other, ok := right[key]

		if !ok || string(other) != string(value) {
			return false
		}
	}

	return true
}

// populatedVault is a vault with one record, one secret, one identity and one market
// tombstone: every kind of value the client stores.
func populatedVault(t *testing.T) *vaultDomain {
	t.Helper()

	d := domain()

	if result := run(t, d, "vault.create", map[string]string{"passphrase": "export passphrase"}); !result.OK {
		t.Fatalf("create = %+v", result)
	}

	record, err := model.New(randomReader{}, model.NewParams{
		BlakeAmount:  2_000_000,
		BTCSats:      50_000,
		BlakeTimeout: 500,
		BTCTimeout:   400,
	})

	if err != nil {
		t.Fatal(err)
	}

	if err := d.store.Save(context.Background(), store.Alice, record); err != nil {
		t.Fatal(err)
	}

	if result := run(t, d, "identity.set", map[string]string{
		"secret": strings.Repeat("11", 32),
	}); !result.OK {
		t.Fatalf("identity.set = %+v", result)
	}

	if err := d.writeSecret(context.Background(), "custom.setting", "a sealed preference"); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	if err := d.kv.Put(ctx, "market/ignored-market-matches/"+strings.Repeat("ab", 32)+".json",
		[]byte(`{"match_id":"`+strings.Repeat("ab", 32)+`"}`)); err != nil {
		t.Fatal(err)
	}

	return d
}

// exported is one export's view, decoded.
func exported(t *testing.T, result proto.Result) proto.VaultExportView {
	t.Helper()

	if !result.OK {
		t.Fatalf("export = %+v", result)
	}

	var view proto.VaultExportView

	if err := proto.DecodeData(result, &view); err != nil {
		t.Fatal(err)
	}

	return view
}

// TestVaultExportImportRoundTrip: every kind of value survives the file.
func TestVaultExportImportRoundTrip(t *testing.T) {
	source := populatedVault(t)

	view := exported(t, run(t, source, "vault.export", nil))

	if len(view.JSON) == 0 || view.Entries == 0 || view.Bytes != len(view.JSON) {
		t.Fatalf("export view = %+v", view)
	}

	if !strings.HasSuffix(view.Name, "Z.json") || !strings.HasPrefix(view.Name, "blake-swap-vault-") {
		t.Fatalf("export name = %q", view.Name)
	}

	// The backup holds every key, not a subset.
	envelope, entries, err := vaultbackup.Parse(view.JSON)

	if err != nil {
		t.Fatal(err)
	}

	if envelope.Kind != vaultbackup.Kind || envelope.Version != vaultbackup.Version {
		t.Fatalf("envelope = %+v", envelope)
	}

	before := snapshot(t, source.kv)

	if !sameEntries(before, entries) {
		t.Fatalf("the backup dropped or changed keys:\nbefore=%d after=%d", len(before), len(entries))
	}

	for _, required := range []string{"vault/header", "vault/verifier", "record/", "record-role/"} {
		found := false

		for key := range entries {
			if strings.HasPrefix(key, required) {
				found = true

				break
			}
		}

		if !found {
			t.Fatalf("the backup has no %q", required)
		}
	}

	// Restore it into an empty store and prove the values read back.
	target := NewVaultDomain(platform.NewMemoryKV(), nil)

	restored := run(t, target, "vault.import", map[string]string{
		"confirm": "replace", "passphrase": "export passphrase", "json": view.JSON,
	})

	if !restored.OK {
		t.Fatalf("import = %+v", restored)
	}

	var importView proto.VaultImportView

	if err := proto.DecodeData(restored, &importView); err != nil {
		t.Fatal(err)
	}

	if importView.Entries != len(entries) || importView.Bytes == 0 {
		t.Fatalf("import view = %+v", importView)
	}

	// A restore locks the vault: the key that was open belonged to the store that is gone.
	if result := run(t, target, "vault.status", nil); !strings.Contains(result.Text, "unlocked: no") {
		t.Fatalf("the imported vault is not locked: %+v", result)
	}

	unlocked := run(t, target, "vault.unlock", map[string]string{"passphrase": "export passphrase"})

	if !unlocked.OK {
		t.Fatalf("unlock after import = %+v", unlocked)
	}

	record, err := target.store.Load(context.Background(), recordID(t, source))

	if err != nil {
		t.Fatalf("the record did not survive: %v", err)
	}

	if record.BlakeAmountBaseUnits != 2_000_000 {
		t.Fatalf("restored record = %+v", record)
	}

	secret, err := target.readSecret(context.Background(), "custom.setting")

	if err != nil || secret != "a sealed preference" {
		t.Fatalf("restored secret = %q, %v", secret, err)
	}

	identity, err := target.readSecret(context.Background(), SecretIdentity)

	if err != nil || identity != strings.Repeat("11", 32) {
		t.Fatalf("restored identity = %q, %v", identity, err)
	}

	if result := run(t, target, "identity.status", nil); !result.OK ||
		!strings.Contains(result.Text, "identity: ") {
		t.Fatalf("identity.status after import = %+v", result)
	}

	tombstones, err := target.kv.Keys(context.Background(), "market/ignored-market-matches/")

	if err != nil || len(tombstones) != 1 {
		t.Fatalf("restored tombstones = %v, %v", tombstones, err)
	}
}

// recordID is the one swap id in a vault.
func recordID(t *testing.T, d *vaultDomain) string {
	t.Helper()

	ids, err := d.store.List(context.Background())

	if err != nil || len(ids) != 1 {
		t.Fatalf("vault records = %v, %v", ids, err)
	}

	return ids[0]
}

// tamperFirstValue changes one stored value and leaves the document's checksum alone,
// which is what an edited file looks like.
func tamperFirstValue(t *testing.T, text string) string {
	t.Helper()

	var document map[string]any

	if err := json.Unmarshal([]byte(text), &document); err != nil {
		t.Fatal(err)
	}

	entries, ok := document["entries"].([]any)

	if !ok || len(entries) == 0 {
		t.Fatal("the document has no entries")
	}

	first, ok := entries[0].(map[string]any)

	if !ok {
		t.Fatal("the first entry is not an object")
	}

	value, _ := first["value"].(string)

	first["value"] = base64.StdEncoding.EncodeToString(append([]byte{0x00}, value...))

	encoded, err := json.Marshal(document)

	if err != nil {
		t.Fatal(err)
	}

	return string(encoded)
}

// TestVaultImportRefusesBeforeWriting: a wrong passphrase, a tampered file and a missing
// confirmation all leave the store byte-identical and call nothing that writes.
func TestVaultImportRefusesBeforeWriting(t *testing.T) {
	source := populatedVault(t)
	view := exported(t, run(t, source, "vault.export", nil))

	targetKV := &callKV{KV: platform.NewMemoryKV()}
	target := NewVaultDomain(targetKV, nil)

	if result := run(t, target, "vault.create", map[string]string{
		"passphrase": "the current passphrase",
	}); !result.OK {
		t.Fatalf("create = %+v", result)
	}

	// The writes the create made are not part of what the refusals are judged on.
	targetKV.puts = 0
	targetKV.deletes = 0
	targetKV.replacements = 0

	before := snapshot(t, targetKV)

	tampered := tamperFirstValue(t, view.JSON)

	for name, args := range map[string]map[string]string{
		"no confirmation": {"passphrase": "export passphrase", "json": view.JSON},
		"wrong confirmation": {
			"confirm": "yes", "passphrase": "export passphrase", "json": view.JSON,
		},
		"no passphrase": {"confirm": "replace", "json": view.JSON},
		"wrong passphrase": {
			"confirm": "replace", "passphrase": "not the passphrase", "json": view.JSON,
		},
		"tampered file": {
			"confirm": "replace", "passphrase": "export passphrase", "json": tampered,
		},
		"not a vault": {"confirm": "replace", "passphrase": "x", "json": "{}"},
	} {
		result := run(t, target, "vault.import", args)

		if result.OK {
			t.Fatalf("%s was accepted", name)
		}
	}

	if targetKV.replacements != 0 || targetKV.puts != 0 || targetKV.deletes != 0 {
		t.Fatalf("a refused import wrote: replace=%d put=%d delete=%d",
			targetKV.replacements, targetKV.puts, targetKV.deletes)
	}

	if !sameEntries(before, snapshot(t, targetKV)) {
		t.Fatal("a refused import changed the store")
	}

	// And the vault that was open is still open.
	if result := run(t, target, "vault.status", nil); !strings.Contains(result.Text, "unlocked: yes") {
		t.Fatalf("a refused import locked the vault: %+v", result)
	}

	// One successful import is one replace, and no put or delete.
	accepted := run(t, target, "vault.import", map[string]string{
		"confirm": "replace", "passphrase": "export passphrase", "json": view.JSON,
	})

	if !accepted.OK {
		t.Fatalf("import = %+v", accepted)
	}

	if targetKV.replacements != 1 || targetKV.puts != 0 || targetKV.deletes != 0 {
		t.Fatalf("the import wrote as replace=%d put=%d delete=%d, want 1/0/0",
			targetKV.replacements, targetKV.puts, targetKV.deletes)
	}
}

// TestVaultImportFailureLeavesTheVaultAlone: a store that cannot replace its contents must
// not leave the worker half-restored, and must not lock a vault that is still the old one.
func TestVaultImportFailureLeavesTheVaultAlone(t *testing.T) {
	source := populatedVault(t)
	view := exported(t, run(t, source, "vault.export", nil))

	targetKV := &callKV{
		KV:                 platform.NewMemoryKV(),
		replacementFailure: errReplacementRefused,
	}

	target := NewVaultDomain(targetKV, nil)

	if result := run(t, target, "vault.create", map[string]string{"passphrase": "old"}); !result.OK {
		t.Fatalf("create = %+v", result)
	}

	targetKV.puts = 0
	targetKV.deletes = 0
	targetKV.replacements = 0

	before := snapshot(t, targetKV)

	result := run(t, target, "vault.import", map[string]string{
		"confirm": "replace", "passphrase": "export passphrase", "json": view.JSON,
	})

	if result.OK {
		t.Fatal("an import that could not write reported success")
	}

	if !strings.Contains(result.Error, "nothing was changed") {
		t.Fatalf("refusal = %q", result.Error)
	}

	if targetKV.replacements != 1 || targetKV.puts != 0 || targetKV.deletes != 0 {
		t.Fatalf("the failed import wrote as replace=%d put=%d delete=%d",
			targetKV.replacements, targetKV.puts, targetKV.deletes)
	}

	if !sameEntries(before, snapshot(t, targetKV)) {
		t.Fatal("a failed import changed the store")
	}

	if result := run(t, target, "vault.status", nil); !strings.Contains(result.Text, "unlocked: yes") {
		t.Fatalf("a failed import locked the vault: %+v", result)
	}

	// The old passphrase still opens it, which is the fact that matters.
	if result := run(t, target, "vault.lock", nil); !result.OK {
		t.Fatalf("lock = %+v", result)
	}

	if result := run(t, target, "vault.unlock", map[string]string{"passphrase": "old"}); !result.OK {
		t.Fatalf("the old passphrase no longer opens the vault: %+v", result)
	}
}

// errReplacementRefused is a storage failure that is neither the caller's nor the file's.
var errReplacementRefused = &replacementError{}

type replacementError struct{}

func (*replacementError) Error() string { return "the store refused the replacement" }

// TestVaultExportRequiresAnOpenVault: a locked vault has nothing to read through the key,
// and a backup of ciphertext nobody can open is not a backup.
func TestVaultExportRequiresAnOpenVault(t *testing.T) {
	d := populatedVault(t)

	if result := run(t, d, "vault.lock", nil); !result.OK {
		t.Fatalf("lock = %+v", result)
	}

	result := run(t, d, "vault.export", nil)

	if result.OK || !strings.Contains(result.Error, "the vault is locked") {
		t.Fatalf("export of a locked vault = %+v", result)
	}
}

// TestVaultExportCarriesNoPlaintext: the file is the sealed store, so what it must not
// contain is any of the plaintext the store protects.
func TestVaultExportCarriesNoPlaintext(t *testing.T) {
	d := populatedVault(t)

	view := exported(t, run(t, d, "vault.export", nil))

	secretHash := ""

	record, err := d.store.Load(context.Background(), recordID(t, d))

	if err != nil {
		t.Fatal(err)
	}

	secretHash = record.SecretHash

	//
	// What must not be in the file is a value: the passphrase, the identity secret, a
	// secret hash, a sealed setting's plaintext. Key *names* are the store's layout rather
	// than its contents, and a backup a person cannot read the shape of is a backup they
	// cannot check; the header key below is the one that has to be there.
	//
	for name, needle := range map[string]string{
		"the passphrase":   "export passphrase",
		"the identity":     strings.Repeat("11", 32),
		"a secret hash":    secretHash,
		"a sealed setting": "a sealed preference",
	} {
		if strings.Contains(view.JSON, needle) {
			t.Fatalf("the export contains %s", name)
		}
	}

	if record.SwapID != "" && strings.Contains(view.JSON, `"value": "`+record.SwapID) {
		t.Fatal("the export carries a record id as a value")
	}

	// The KDF header is readable on purpose: a backup nobody can derive a key from is not
	// a backup.
	if !strings.Contains(view.JSON, vaultbackup.HeaderKey) {
		t.Fatal("the export has no KDF header key")
	}

	// And the summary line quotes nothing.
	if strings.Contains(run(t, d, "vault.export", nil).Text, strings.Repeat("11", 32)) {
		t.Fatal("the export summary quotes the identity")
	}
}

// TestVaultImportFromAnotherVault: the imported passphrase is the one that matters, which
// is what makes a backup a recovery rather than a copy of the current configuration.
func TestVaultImportFromAnotherVault(t *testing.T) {
	source := populatedVault(t)
	view := exported(t, run(t, source, "vault.export", nil))

	// A target with no vault at all: the store a person has when they have lost theirs.
	target := NewVaultDomain(platform.NewMemoryKV(), nil)

	if result := run(t, target, "vault.export", nil); result.OK {
		t.Fatal("a store with no vault offered an export")
	}

	if result := run(t, target, "vault.import", map[string]string{
		"confirm": "replace", "passphrase": "export passphrase", "json": view.JSON,
	}); !result.OK {
		t.Fatalf("import into an empty store = %+v", result)
	}

	if result := run(t, target, "vault.unlock", map[string]string{
		"passphrase": "export passphrase",
	}); !result.OK {
		t.Fatalf("unlock = %+v", result)
	}

	if result := run(t, target, "vault.export", nil); !result.OK {
		t.Fatalf("the restored vault could not be exported again: %+v", result)
	}
}
