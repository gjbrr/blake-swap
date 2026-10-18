package main

import (
	"strings"
	"testing"
)

// TestRenderRebroadcastMatchesTheReference pins both blocks: the promise made before the
// publish, and what was established afterwards.
func TestRenderRebroadcastMatchesTheReference(t *testing.T) {
	id := strings.Repeat("ab", 32)

	before := `
REBROADCASTING CACHED NOSTR EVENT
=================================
event id: ` + id + `

PASS: cached event re-verified
PASS: existing signature will be reused
PASS: event will NOT be rebuilt or re-signed
`

	if got := renderRebroadcasting(id); got != before {
		t.Fatalf("before mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, before)
	}

	after := `
NOSTR EVENT REBROADCAST
=======================
event id: ` + id + `

PASS: exact signed event was rebroadcast
PASS: event ID remained unchanged
`

	if got := renderRebroadcast(id); got != after {
		t.Fatalf("after mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, after)
	}
}
