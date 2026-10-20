package worker

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/client"
	"github.com/gjbrr/blake-swap/pkg/platform"
	"github.com/gjbrr/blake-swap/pkg/proto"
	"github.com/gjbrr/blake-swap/pkg/vaultbackup"
)

// snapshotVault reads every stored key and value.
func (d *vaultDomain) snapshotVault(ctx context.Context) (map[string][]byte, error) {
	keys, err := d.kv.Keys(ctx, "")

	if err != nil {
		return nil, err
	}

	if len(keys) > vaultbackup.MaxEntries {
		return nil, fmt.Errorf(
			"the vault has %d entries, more than the %d a backup holds",
			len(keys), vaultbackup.MaxEntries,
		)
	}

	entries := make(map[string][]byte, len(keys))

	total := 0

	for _, key := range keys {
		value, found, err := d.kv.Get(ctx, key)

		if err != nil {
			return nil, err
		}

		if !found {
			// A key that disappeared between the listing and the read is a concurrent
			// writer, not an error: a backup is a snapshot, and this snapshot does not
			// include it.
			continue
		}

		total += len(value)

		if total > vaultbackup.MaxBytes {
			return nil, fmt.Errorf(
				"the vault holds more than the %d bytes a backup holds", vaultbackup.MaxBytes,
			)
		}

		entries[key] = value
	}

	return entries, nil
}

// exportVault writes the whole sealed store as one checked document.
//
// The document is returned as data for the page to save, not written anywhere by the
// worker: a wasm instance has no filesystem, and a backup that a domain wrote to its own
// storage would be recoverable only while the thing it backs up still worked.
func (d *vaultDomain) exportVault(ctx context.Context, request proto.Request) proto.Result {
	if d.store == nil || !d.unlocked {
		return proto.Result{Command: request.Command, Error: "the vault is locked"}
	}

	entries, err := d.snapshotVault(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	now := time.Now()

	encoded, document, err := vaultbackup.Encode(now, entries)

	if err != nil {
		return failed(request.Command, err)
	}

	name := vaultbackup.FileName(now)

	// The text is counts only. The envelope itself is sensitive, and a summary line that
	// quoted any of it would put a sealed value into the page's log and into a
	// screenshot.
	text := "vault exported: " + strconv.Itoa(len(document.Entries)) + " entries, " +
		strconv.Itoa(len(encoded)) + " bytes"

	return withView(proto.Result{Command: request.Command, OK: true, Text: text},
		proto.VaultExportView{
			Name: name, JSON: string(encoded),
			Entries: len(document.Entries), Bytes: len(encoded),
		})
}

// importVault replaces this store with a backup, after proving the backup opens.
//
// The order is the whole safety property: nothing is written until the imported vault has
// been validated, and the passphrase that matters is the imported one, because a backup is
// how a vault that will not open is recovered. The current store may be locked, absent, or
// somebody else's.
func (d *vaultDomain) importVault(ctx context.Context, request proto.Request) proto.Result {
	if !strings.EqualFold(strings.TrimSpace(request.Args["confirm"]), "replace") {
		return proto.Result{
			Command: request.Command,
			Error:   "importing a vault replaces the one in this browser; pass confirm=replace",
		}
	}

	passphrase := request.Args["passphrase"]

	if passphrase == "" {
		return proto.Result{
			Command: request.Command,
			Error:   "the imported vault needs its own passphrase",
		}
	}

	_, entries, err := vaultbackup.Parse(request.Args["json"])

	if err != nil {
		return failed(request.Command, err)
	}

	// The proof that the file is a vault this passphrase opens. It runs against a memory
	// copy so a wrong passphrase or a tampered verifier fails before the first write, and
	// it reuses the store's own open path rather than a second derivation.
	probe := platform.NewMemoryKV()

	if err := probe.ReplaceAll(ctx, entries); err != nil {
		return failed(request.Command, err)
	}

	if _, err := client.OpenWith(ctx, probe, passphrase, d.random); err != nil {
		return proto.Result{
			Command: request.Command,
			Error:   "the backup did not open: " + err.Error(),
		}
	}

	if err := d.kv.ReplaceAll(ctx, entries); err != nil {
		return proto.Result{
			Command: request.Command,
			Error:   "the backup could not be written, so nothing was changed: " + err.Error(),
		}
	}

	// The store that was open described the vault that no longer exists. Dropping it is
	// what makes the next command ask for the imported passphrase instead of using a key
	// derived from the old one.
	d.store = nil
	d.unlocked = false

	bytes := 0

	for _, value := range entries {
		bytes += len(value)
	}

	return withView(proto.Result{
		Command: request.Command,
		OK:      true,
		Text:    "vault imported; unlock to continue",
	}, proto.VaultImportView{Entries: len(entries), Bytes: bytes})
}
