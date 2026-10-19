//go:build !js

// Package packaging checks the Go artefacts that describe how this repository
// is built, packaged and verified.
//
// It is the Go-artefact analogue of the reference packaging suite,
// tests/packaging_tests.py. The reference suite compares a Python
// distribution's metadata, dependency pins, module list and console entry
// points against the sources, and compares .github/workflows/ci.yml against
// the commands and runner it claims to use. This repository does not build a
// Python distribution, so the same kind of check is made against what it does
// build:
//
//   - go.mod declares the module path and a Go directive;
//   - go.sum is complete enough to be usable, not truncated or corrupted;
//   - every make target the documentation tells a reader to run exists in the
//     Makefile;
//   - .github/workflows/ci.yml parses as YAML, pins its runner image,
//     provisions two distinct node builds, runs the unit and wasm gates, and
//     never makes a merge-blocking job depend on a public Nostr relay;
//   - LICENSE is present and non-empty;
//   - the packages under pkg/ and cmd/ belong to this module.
//
// The Python assertions that are structural to a Python distribution (the
// pyproject.toml contents, the src/ module list, the console entry points, the
// wheel contents) have no Go artefact to inspect. They are not faked here:
// packaging_test.go records them in a documented waiver table, and a test
// asserts that every waiver names the Python assertion it replaces and gives a
// concrete reason.
//
// Every check is an ordinary function returning error, so a caller can decide
// whether a missing or malformed artefact is fatal. The checks never skip: a
// missing artefact is always reported as an error, because a packaging suite
// that passes when it cannot find the thing it checks is the failure mode it
// exists to prevent.
//
// The package reads the host filesystem and is therefore host-only; it is
// built with the !js constraint and is never part of a browser bundle. It does
// not run `go build`, `go mod`, `go vet` or any other subprocess: this package
// inspects the artefacts, and the toolchain's own gates run in CI and from the
// Makefile.
package packaging
