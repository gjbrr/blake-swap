//go:build !js

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFileStore runs the shared behaviour suite against the on-disk store.
func TestFileStore(t *testing.T) {
	testStore(t, NewFile(t.TempDir()))
}

// TestFileStoreLayout pins the layout, because a port that cannot read the
// Python engine's state tree is not a drop-in replacement.
func TestFileStoreLayout(t *testing.T) {
	base := t.TempDir()
	st := NewFile(base)
	ctx := context.Background()

	swap := newSwap(t, "aaaa00000301")

	if err := st.Save(ctx, Alice, swap); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(base, swap.SwapID+".json")); err != nil {
		t.Fatalf("the record is not at <base>/<swap-id>.json: %v", err)
	}

	sender := strings.Repeat("cd", 32)

	if err := st.SendMessage(ctx, swap.SwapID, "offer", sender, map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(base, "messages", swap.SwapID+".offer.json")); err != nil {
		t.Fatalf("the message is not at <base>/messages/<id>.<kind>.json: %v", err)
	}

	if err := st.Archive(ctx, Alice, swap.SwapID); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(base, "archive", swap.SwapID+".json")); err != nil {
		t.Fatalf("the archive is not at <base>/archive/<id>.json: %v", err)
	}
}

// TestReplaceIsAtomic is the crash-safety property made observable.
//
// A reader that already has the record open keeps seeing the *previous* bytes
// after a save publishes the new ones, which is only true when the write went to a
// new file and was renamed into place. An in-place truncate-and-write would make
// the same handle see the new contents, and a crash during it would leave a
// prefix — which is exactly what the protocol's authoritative record must never
// do.
func TestReplaceIsAtomic(t *testing.T) {
	base := t.TempDir()
	st := NewFile(base)
	ctx := context.Background()

	swap := newSwap(t, "aaaa00000302")

	if err := st.Save(ctx, Alice, swap); err != nil {
		t.Fatal(err)
	}

	watcher, err := os.Open(filepath.Join(base, swap.SwapID+".json"))

	if err != nil {
		t.Fatal(err)
	}

	defer watcher.Close()

	before, err := os.ReadFile(filepath.Join(base, swap.SwapID+".json"))

	if err != nil {
		t.Fatal(err)
	}

	swap.BTCSats = 26_000

	if err := st.Save(ctx, Alice, swap); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(filepath.Join(base, swap.SwapID+".json"))

	if err != nil {
		t.Fatal(err)
	}

	if string(before) == string(after) {
		t.Fatal("the second save did not change the record")
	}

	held := make([]byte, len(before))

	if _, err := watcher.ReadAt(held, 0); err != nil {
		t.Fatal(err)
	}

	if string(held) != string(before) {
		t.Fatal("an open reader saw the replacement, so the write was not published by rename")
	}

	entries, err := os.ReadDir(base)

	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("a temporary file survived the write: %s", entry.Name())
		}
	}
}

// TestFileStoreRefusesToOverwriteAnArchive: an archived record may be the only
// copy of a swap that was already retired, so a second archive must not replace
// it.
func TestFileStoreRefusesToOverwriteAnArchive(t *testing.T) {
	base := t.TempDir()
	st := NewFile(base)
	ctx := context.Background()

	swap := newSwap(t, "aaaa00000303")

	if err := st.Save(ctx, Alice, swap); err != nil {
		t.Fatal(err)
	}

	if err := st.Archive(ctx, Alice, swap.SwapID); err != nil {
		t.Fatal(err)
	}

	// A record with the same ID reappears (a restore, or a re-created swap).
	if err := st.Save(ctx, Alice, swap); err != nil {
		t.Fatal(err)
	}

	if err := st.Archive(ctx, Alice, swap.SwapID); err == nil {
		t.Fatal("an existing archive was overwritten")
	}
}

// TestFileStoreIgnoresNonRecords: the state root is shared with other files, and
// a scan that tried to load them would report them as corrupt swaps.
func TestFileStoreIgnoresNonRecords(t *testing.T) {
	base := t.TempDir()
	st := NewFile(base)

	if err := os.WriteFile(filepath.Join(base, "notes.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := st.Save(context.Background(), Alice, newSwap(t, "aaaa00000304")); err != nil {
		t.Fatal(err)
	}

	ids, err := st.List(context.Background())

	if err != nil {
		t.Fatal(err)
	}

	if len(ids) != 1 || ids[0] != "aaaa00000304" {
		t.Fatalf("ids = %v", ids)
	}
}

// TestFileStoreReportsACorruptRecord: a record that cannot be decoded is an
// error, not an empty swap.
func TestFileStoreReportsACorruptRecord(t *testing.T) {
	base := t.TempDir()
	st := NewFile(base)

	path := filepath.Join(base, "aaaa00000305.json")

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := st.Load(context.Background(), "aaaa00000305")

	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("a corrupt record was reported as %v", err)
	}
}
