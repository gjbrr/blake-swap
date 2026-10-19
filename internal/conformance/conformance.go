package conformance

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// Schema is the manifest schema this checker understands. A manifest written
// for any other schema is refused rather than guessed at.
const Schema = "blake-swap-security-conformance-v1"

// InvariantCount is the number of portable invariants every manifest must
// declare, and the number of go_conformance rows that must map them.
const InvariantCount = 47

// Manifest is the part of tests/security_conformance.json the checker reads.
//
// It is a subset on purpose: the manifest also carries commentary such as the
// source document and the implementation policy, and a checker that modelled
// those would refuse a manifest for adding a note.
type Manifest struct {
	// Schema is the manifest's self-description, checked against Schema.
	Schema string `json:"schema"`
	// Invariants are the portable security invariants, in C-number order.
	Invariants []Invariant `json:"invariants"`
	// GoConformance maps each invariant to the Go tests that prove it, or to
	// the reference scenario that still proves it alone.
	GoConformance []GoRow `json:"go_conformance"`
}

// Invariant is one portable security invariant.
type Invariant struct {
	// ID is the invariant's stable name, C1 through C47.
	ID string `json:"id"`
	// Requirement is the contract in literal text.
	Requirement string `json:"requirement"`
	// ProvingSuites are file names under tests/ that prove the invariant in
	// the Python reference.
	ProvingSuites []string `json:"proving_suites"`
	// ComparisonSurface is what two implementations may be compared on.
	ComparisonSurface []string `json:"comparison_surface"`
}

// GoRow says who proves one invariant in this checkout: either the Go tests it
// names, or the reference scenario named in ReferenceOnly, never both.
type GoRow struct {
	// ID is the invariant this row belongs to, C1 through C47.
	ID string `json:"id"`
	// GoTests are "<package-directory>:<TestFunction>" mappings that must
	// resolve to a real test function in this checkout.
	GoTests []string `json:"go_tests"`
	// ReferenceOnly names the reference scenario that proves the invariant
	// while no Go test does. It is empty when GoTests is not.
	ReferenceOnly string `json:"reference_only"`
}

// Result reports what a successful check saw.
type Result struct {
	// Invariants is the number of portable invariants validated.
	Invariants int
	// GoMapped is the number of invariants proved by at least one Go test.
	GoMapped int
	// ReferenceOnly is the number of invariants proved only by the Python
	// reference.
	ReferenceOnly int
}

// ManifestPath returns the manifest's location in the repository rooted at root.
func ManifestPath(root string) string {
	return filepath.Join(root, "tests", "security_conformance.json")
}

// Check validates the security conformance manifest in the repository rooted at
// root.
//
// It reports the first violation rather than collecting them all, which is what
// the reference suite does: a manifest with one broken row is not more useful
// with five pages of downstream noise.
func Check(root string) (Result, error) {
	manifest, err := load(root)

	if err != nil {
		return Result{}, err
	}

	if manifest.Schema != Schema {
		return Result{}, fmt.Errorf("wrong manifest schema: %q", manifest.Schema)
	}

	if len(manifest.Invariants) != InvariantCount {
		return Result{}, fmt.Errorf(
			"expected %d invariants, got %d", InvariantCount, len(manifest.Invariants),
		)
	}

	want := invariantIDs()

	for index, record := range manifest.Invariants {
		if record.ID != want[index] {
			return Result{}, fmt.Errorf(
				"invariant %d is %q, want %q: C1-C%d are missing, duplicated, or out of order",
				index+1, record.ID, want[index], InvariantCount,
			)
		}

		if err := checkInvariant(root, record); err != nil {
			return Result{}, err
		}
	}

	result := Result{Invariants: len(manifest.Invariants)}

	if err := checkGoConformance(root, manifest.GoConformance, want, &result); err != nil {
		return Result{}, err
	}

	return result, nil
}

// load reads and parses the manifest without judging its contents.
func load(root string) (*Manifest, error) {
	path := ManifestPath(root)

	raw, err := os.ReadFile(path)

	if err != nil {
		return nil, fmt.Errorf("read the security conformance manifest: %w", err)
	}

	var manifest Manifest

	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	return &manifest, nil
}

// checkInvariant validates one invariant's contract text, proving suites and
// comparison surface.
func checkInvariant(root string, record Invariant) error {
	if record.Requirement == "" {
		return fmt.Errorf("%s: empty requirement", record.ID)
	}

	if len(record.ProvingSuites) == 0 {
		return fmt.Errorf("%s: has no proving suite", record.ID)
	}

	for _, suite := range record.ProvingSuites {
		path := filepath.Join(root, "tests", suite)

		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("%s: proving suite does not exist: %s", record.ID, suite)
		}
	}

	surface := map[string]bool{}

	for _, name := range record.ComparisonSurface {
		surface[name] = true
	}

	if !surface["durable_state"] {
		return fmt.Errorf("%s: missing durable-state comparison surface", record.ID)
	}

	if !surface["accepted_or_rejected"] {
		return fmt.Errorf("%s: missing accept/reject comparison surface", record.ID)
	}

	return nil
}

// checkGoConformance validates the Go-side mapping: one row per invariant in
// order, exactly one proof per row, and every named test actually present.
func checkGoConformance(root string, rows []GoRow, want []string, result *Result) error {
	if len(rows) != InvariantCount {
		return fmt.Errorf(
			"expected %d go_conformance rows, got %d", InvariantCount, len(rows),
		)
	}

	for index, row := range rows {
		if row.ID != want[index] {
			return fmt.Errorf(
				"go_conformance row %d is %q, want %q: C1-C%d are missing, duplicated, or out of order",
				index+1, row.ID, want[index], InvariantCount,
			)
		}

		hasTests := len(row.GoTests) > 0
		hasReference := strings.TrimSpace(row.ReferenceOnly) != ""

		switch {
		case hasTests && hasReference:
			return fmt.Errorf(
				"%s: go_conformance row names both Go tests and a reference-only scenario",
				row.ID,
			)
		case !hasTests && !hasReference:
			return fmt.Errorf(
				"%s: go_conformance row names neither a Go test nor a reference-only scenario",
				row.ID,
			)
		}

		if hasReference {
			result.ReferenceOnly++

			continue
		}

		result.GoMapped++

		for _, mapping := range row.GoTests {
			if err := checkGoTest(root, mapping); err != nil {
				return fmt.Errorf("%s: %w", row.ID, err)
			}
		}
	}

	return nil
}

// checkGoTest proves that one "<package-directory>:<TestFunction>" mapping names
// a test function that exists in this checkout.
//
// The directory is resolved inside root and the function is read from the
// package's _test.go sources, so a mapping cannot point outside the repository
// and cannot name a test that is not there.
func checkGoTest(root, mapping string) error {
	dir, name, found := strings.Cut(mapping, ":")

	if !found || dir == "" || name == "" {
		return fmt.Errorf("%q is not <package-directory>:<TestFunction>", mapping)
	}

	if strings.Contains(name, ":") {
		return fmt.Errorf("%q is not <package-directory>:<TestFunction>", mapping)
	}

	if filepath.IsAbs(dir) {
		return fmt.Errorf("%q names an invalid package directory", mapping)
	}

	for _, part := range strings.Split(dir, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("%q names an invalid package directory", mapping)
		}
	}

	tests, err := testFunctions(filepath.Join(root, filepath.FromSlash(dir)))

	if err != nil {
		return fmt.Errorf("%q: %w", mapping, err)
	}

	if !tests[name] {
		return fmt.Errorf(
			"%q points at a test that does not exist: no func %s in %s",
			mapping, name, dir,
		)
	}

	return nil
}

// testFunctions lists the Go test functions declared by the _test.go files of
// one package directory.
//
// The sources are parsed rather than the package built, because checking a
// manifest must not depend on every package it names still compiling; a test
// that does not compile is already a `go test ./...` failure, and reporting it
// here would only hide the mapping problem the manifest is checked for.
func testFunctions(dir string) (map[string]bool, error) {
	entries, err := os.ReadDir(dir)

	if err != nil {
		return nil, fmt.Errorf("no such Go package directory: %w", err)
	}

	names := map[string]bool{}
	files := token.NewFileSet()

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}

		path := filepath.Join(dir, entry.Name())

		file, err := parser.ParseFile(files, path, nil, parser.SkipObjectResolution)

		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}

		for _, decl := range file.Decls {
			function, ok := decl.(*ast.FuncDecl)

			// A test is a package-level function whose name the testing
			// package accepts; a method named TestSomething is not a test.
			if !ok || function.Recv != nil || !strings.HasPrefix(function.Name.Name, "Test") {
				continue
			}

			names[function.Name.Name] = true
		}
	}

	return names, nil
}

// invariantIDs returns the expected invariant names in manifest order.
func invariantIDs() []string {
	ids := make([]string, InvariantCount)

	for index := range ids {
		ids[index] = fmt.Sprintf("C%d", index+1)
	}

	return ids
}
