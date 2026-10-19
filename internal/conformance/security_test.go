//go:build !js

package conformance_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gjbrr/blake-swap/internal/conformance"
)

// TestSecurityConformanceManifest validates the machine-readable security
// conformance manifest: the schema string, exactly C1-C47 in order, a literal
// requirement on every invariant, proving suites that exist under tests/, the
// portable comparison surface, and the Go-side mapping that must resolve to real
// test functions rather than aspirational names.
//
// There is no skip: this suite reads repository files, so a checkout without the
// manifest, or with a broken one, is a failure.
//
// Parity: tests/security_conformance_tests.py
func TestSecurityConformanceManifest(t *testing.T) {
	result, err := conformance.Check(repoRoot(t))

	if err != nil {
		t.Fatalf("tests/security_conformance.json: %v", err)
	}

	if result.Invariants != conformance.InvariantCount {
		t.Fatalf("validated %d invariants, want %d", result.Invariants, conformance.InvariantCount)
	}

	// Every invariant is proved by exactly one side, so the two counts must
	// cover the whole manifest.
	if proved := result.GoMapped + result.ReferenceOnly; proved != result.Invariants {
		t.Fatalf(
			"go_conformance accounts for %d of %d invariants: %d mapped, %d reference-only",
			proved, result.Invariants, result.GoMapped, result.ReferenceOnly,
		)
	}
}

// repoRoot finds the checkout root by walking up from the working directory
// until it finds the module's go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()

	if err != nil {
		t.Fatalf("locate working directory: %v", err)
	}

	for {
		path := filepath.Join(dir, "go.mod")

		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return dir
		}

		parent := filepath.Dir(dir)

		if parent == dir {
			t.Fatalf("no go.mod above %s; run this suite inside the module", dir)
		}

		dir = parent
	}
}
