package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderMatchPublishedMatchesTheReference pins the confirmation block.
func TestRenderMatchPublishedMatchesTheReference(t *testing.T) {
	offerID := strings.Repeat("ab", 32)
	takeID := strings.Repeat("cd", 32)
	matchID := strings.Repeat("ef", 32)

	want := `
MARKET MATCH PUBLISHED
======================
offer id: ` + offerID + `
take id:  ` + takeID + `
match id: ` + matchID + `
swap id:  aaaa00000001

PASS: match is signed by the offer's maker
PASS: the exact signed take is referenced
PASS: no secret was published
PASS: no funds moved
`

	if got := renderMatchPublished(offerID, takeID, matchID, "aaaa00000001"); got != want {
		t.Fatalf("renderMatchPublished mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestWriteAtomicallyPublishesByRename: the selection record binds a published match to a
// swap, so a half-written one is worse than a missing one.
func TestWriteAtomicallyPublishesByRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "selection.json")

	if err := writeAtomically(path, []byte("{\"swap_id\":\"aaaa00000001\"}\n")); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)

	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(raw), "aaaa00000001") {
		t.Fatalf("content = %s", raw)
	}

	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("directory holds %d files, want 1", len(entries))
	}
}
