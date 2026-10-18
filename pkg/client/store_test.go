package client

import (
	"context"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/store"
)

// newRecord builds a valid swap record.
func newRecord(t *testing.T, id string) *model.Swap {
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

	return record
}

// TestEncryptedRoundTrip: the engine's store interface works unchanged over an
// encrypted backend.
func TestEncryptedRoundTrip(t *testing.T) {
	ctx := context.Background()
	kv := NewMemory()

	st, err := Create(ctx, kv, "correct horse battery staple")

	if err != nil {
		t.Fatal(err)
	}

	record := newRecord(t, "aaaa00000001")

	if err := st.Save(ctx, store.Alice, record); err != nil {
		t.Fatal(err)
	}

	loaded, err := st.Load(ctx, record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if loaded.SwapID != record.SwapID || loaded.SecretHash != record.SecretHash {
		t.Fatalf("round trip changed the record: %+v", loaded)
	}

	if loaded.BlakeAmountBaseUnits != record.BlakeAmountBaseUnits {
		t.Fatalf("blake amount = %d", loaded.BlakeAmountBaseUnits)
	}

	// What is actually on the storage is ciphertext: the secret must not appear.
	raw, found := kv.Raw(recordPrefix + record.SwapID)

	if !found {
		t.Fatal("the record was not stored")
	}

	if strings.Contains(string(raw), *record.SecretHex) {
		t.Fatal("the swap secret is stored in the clear")
	}

	if strings.Contains(string(raw), record.SwapID) {
		t.Fatal("the record is stored in the clear")
	}
}

// TestWrongPassphraseIsDetectedAtUnlock: the failure must be named where it happened, not
// later when the first record fails to open.
func TestWrongPassphraseIsDetectedAtUnlock(t *testing.T) {
	ctx := context.Background()
	kv := NewMemory()

	st, err := Create(ctx, kv, "right")

	if err != nil {
		t.Fatal(err)
	}

	if err := st.Save(ctx, store.Alice, newRecord(t, "aaaa00000002")); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(ctx, kv, "wrong"); err == nil ||
		!strings.Contains(err.Error(), "wrong passphrase") {
		t.Fatalf("err = %v", err)
	}

	// And the right one still opens.
	reopened, err := Open(ctx, kv, "right")

	if err != nil {
		t.Fatal(err)
	}

	if _, err := reopened.Load(ctx, "aaaa00000002"); err != nil {
		t.Fatal(err)
	}
}

// TestRecordsCannotBeMovedBetweenSlots is the reason the record key is bound into the
// plaintext: NIP-44 authenticates the payload, not the storage key, so without the binding
// a sealed record could be copied to another record's slot and would still open.
func TestRecordsCannotBeMovedBetweenSlots(t *testing.T) {
	ctx := context.Background()
	kv := NewMemory()

	st, err := Create(ctx, kv, "passphrase")

	if err != nil {
		t.Fatal(err)
	}

	first := newRecord(t, "aaaa00000003")
	second := newRecord(t, "aaaa00000004")

	if err := st.Save(ctx, store.Alice, first); err != nil {
		t.Fatal(err)
	}

	if err := st.Save(ctx, store.Alice, second); err != nil {
		t.Fatal(err)
	}

	// Move the first record's ciphertext into the second record's slot.
	sealed, found, err := kv.Get(ctx, recordPrefix+first.SwapID)

	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}

	if err := kv.Put(ctx, recordPrefix+second.SwapID, sealed); err != nil {
		t.Fatal(err)
	}

	if _, err := st.Load(ctx, second.SwapID); err == nil {
		t.Fatal("a sealed record opened under another record's key")
	}
}

// TestTamperingIsDetected: a ciphertext altered on storage must not open.
func TestTamperingIsDetected(t *testing.T) {
	ctx := context.Background()
	kv := NewMemory()

	st, err := Create(ctx, kv, "passphrase")

	if err != nil {
		t.Fatal(err)
	}

	record := newRecord(t, "aaaa00000005")

	if err := st.Save(ctx, store.Alice, record); err != nil {
		t.Fatal(err)
	}

	sealed, _, err := kv.Get(ctx, recordPrefix+record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	// Flip one byte of the payload.
	tampered := append([]byte{}, sealed...)

	if tampered[len(tampered)-1] == 'A' {
		tampered[len(tampered)-1] = 'B'
	} else {
		tampered[len(tampered)-1] = 'A'
	}

	if err := kv.Put(ctx, recordPrefix+record.SwapID, tampered); err != nil {
		t.Fatal(err)
	}

	if _, err := st.Load(ctx, record.SwapID); err == nil {
		t.Fatal("a tampered record opened")
	}
}

// TestListArchiveAndMessages covers the rest of the store interface.
func TestListArchiveAndMessages(t *testing.T) {
	ctx := context.Background()
	kv := NewMemory()

	st, err := Create(ctx, kv, "passphrase")

	if err != nil {
		t.Fatal(err)
	}

	first := newRecord(t, "aaaa00000006")
	second := newRecord(t, "aaaa00000007")

	for _, record := range []*model.Swap{second, first} {
		if err := st.Save(ctx, store.Alice, record); err != nil {
			t.Fatal(err)
		}
	}

	ids, err := st.List(ctx)

	if err != nil {
		t.Fatal(err)
	}

	if len(ids) != 2 || ids[0] != first.SwapID || ids[1] != second.SwapID {
		t.Fatalf("ids = %v", ids)
	}

	// The vault's own metadata must not look like a record.
	for _, id := range ids {
		if strings.Contains(id, "vault") {
			t.Fatalf("the vault metadata is listed as a record: %s", id)
		}
	}

	if err := st.Archive(ctx, store.Alice, first.SwapID); err != nil {
		t.Fatal(err)
	}

	retired, err := st.Retired(ctx, first.SwapID)

	if err != nil || !retired {
		t.Fatalf("retired=%v err=%v", retired, err)
	}

	if _, err := st.Load(ctx, first.SwapID); err == nil {
		t.Fatal("an archived record is still loadable as live")
	}

	sender := strings.Repeat("cd", 32)

	if err := st.SendMessage(ctx, second.SwapID, "offer", sender,
		map[string]any{"type": "offer"}); err != nil {
		t.Fatal(err)
	}

	message, err := st.ReceiveMessage(ctx, second.SwapID, "offer")

	if err != nil {
		t.Fatal(err)
	}

	if message.Sender != sender || message.Body["_nostr_pubkey"] != sender {
		t.Fatalf("message = %+v", message)
	}

	kinds, err := st.ListMessages(ctx, second.SwapID)

	if err != nil || len(kinds) != 1 || kinds[0] != "offer" {
		t.Fatalf("kinds = %v err = %v", kinds, err)
	}
}

// TestNoVaultAndNoPassphrase cover the two ways an unlock fails before any crypto.
func TestNoVaultAndNoPassphrase(t *testing.T) {
	ctx := context.Background()

	if _, err := Open(ctx, NewMemory(), "anything"); err == nil ||
		!strings.Contains(err.Error(), "no vault") {
		t.Fatalf("err = %v", err)
	}

	if _, err := Create(ctx, NewMemory(), ""); err == nil {
		t.Fatal("a vault was created with no passphrase")
	}

	kv := NewMemory()

	exists, err := Exists(ctx, kv)

	if err != nil || exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}

	if _, err := Create(ctx, kv, "passphrase"); err != nil {
		t.Fatal(err)
	}

	if exists, err = Exists(ctx, kv); err != nil || !exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
}

// TestSealedRecordsDiffer: sealing is randomised, so two saves of the same record produce
// different ciphertext — otherwise an observer could tell when nothing changed.
func TestSealedRecordsDiffer(t *testing.T) {
	ctx := context.Background()
	kv := NewMemory()

	st, err := Create(ctx, kv, "passphrase")

	if err != nil {
		t.Fatal(err)
	}

	record := newRecord(t, "aaaa00000008")

	if err := st.Save(ctx, store.Alice, record); err != nil {
		t.Fatal(err)
	}

	first, _, err := kv.Get(ctx, recordPrefix+record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if err := st.Save(ctx, store.Alice, record); err != nil {
		t.Fatal(err)
	}

	second, _, err := kv.Get(ctx, recordPrefix+record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if string(first) == string(second) {
		t.Fatal("two seals of the same record are identical")
	}
}
