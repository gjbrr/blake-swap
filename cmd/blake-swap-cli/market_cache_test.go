package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/event"
	"github.com/gjbrr/blake-swap/pkg/marketflow"
	"github.com/gjbrr/blake-swap/pkg/nostr"
)

// TestCacheDirectoryMatchesTheReferenceStateTree: the event cache is shared state.
//
// A participant who ran the Python engine yesterday and this one today must find their
// evidence where they left it — a prefix that resolved yesterday cannot become
// unresolvable because the client changed — so the directory name is part of the
// contract, not a detail.
func TestCacheDirectoryMatchesTheReferenceStateTree(t *testing.T) {
	t.Setenv("BLAKE_SWAP_STATE_DIR", "/tmp/state")

	if got := cacheDirectory(); got != filepath.Join("/tmp/state", "nostr-event-cache") {
		t.Fatalf("cacheDirectory = %q, want the reference's nostr-event-cache", got)
	}
}

// TestDirCacheReadsAReferenceWrittenFile: the cache files are the reference's format,
// not merely JSON of our own shape.
func TestDirCacheReadsAReferenceWrittenFile(t *testing.T) {
	dir := t.TempDir()

	id := strings.Repeat("ab", 32)

	// The reference writes the relay's event dict, compactly, keyed by these names.
	body := `{"id":"` + id + `","pubkey":"` + strings.Repeat("11", 32) +
		`","created_at":1700000000,"kind":1,"tags":[["t","blake-swap-market"]],"content":"{}","sig":"` +
		strings.Repeat("22", 64) + `"}`

	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cache := dirCache{dir: dir}

	loaded, ok, err := cache.Get(context.Background(), id)

	if err != nil || !ok {
		t.Fatalf("Get = %v, %v", ok, err)
	}

	if loaded.ID != id || loaded.Kind != event.KindTextNote || loaded.CreatedAt != 1700000000 {
		t.Fatalf("decoded event disagrees with the file: %+v", loaded)
	}

	if !loaded.HasTag("t", "blake-swap-market") {
		t.Fatalf("tags were not preserved: %v", loaded.Tags)
	}

	// And what we write back is the same shape, so the reference can read it.
	if err := cache.Put(context.Background(), loaded); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, id+".json"))

	if err != nil {
		t.Fatal(err)
	}

	var roundTripped map[string]any

	if err := json.Unmarshal(raw, &roundTripped); err != nil {
		t.Fatalf("written cache file is not JSON: %v", err)
	}

	for _, key := range []string{"id", "pubkey", "created_at", "kind", "tags", "content", "sig"} {
		if _, ok := roundTripped[key]; !ok {
			t.Fatalf("written cache file is missing %q:\n%s", key, raw)
		}
	}
}

// TestCacheMarketTakesRetainsOnlyAuthenticTakes: the cache keeps what a signature proved
// and a take tag identified, and nothing else — matching the point in the reference's take
// validator where retention happens, which is before protocol validity.
func TestCacheMarketTakesRetainsOnlyAuthenticTakes(t *testing.T) {
	dir := t.TempDir()

	client := &nostr.Client{Cache: dirCache{dir: dir}}

	take := event.Event{
		ID:      strings.Repeat("aa", 32),
		Kind:    event.KindTextNote,
		Tags:    [][]string{{"t", marketflow.TakeTag}},
		Content: `{"type":"market-take"}`,
	}
	offer := event.Event{
		ID:      strings.Repeat("bb", 32),
		Kind:    event.KindTextNote,
		Tags:    [][]string{{"t", marketflow.MarketTag}},
		Content: `{"type":"market-offer"}`,
	}
	notANote := event.Event{
		ID:   strings.Repeat("cc", 32),
		Kind: 7,
		Tags: [][]string{{"t", marketflow.TakeTag}},
	}

	cacheMarketTakes(context.Background(), client, []event.Event{take, offer, notANote})

	ids, err := client.Cache.IDs(context.Background())

	if err != nil {
		t.Fatal(err)
	}

	if len(ids) != 1 || ids[0] != take.ID {
		t.Fatalf("cached ids = %v, want only the take", ids)
	}
}
