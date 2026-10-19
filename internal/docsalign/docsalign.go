//go:build !js

// Package docsalign checks that the repository's documentation still describes
// the implementation it ships with.
//
// The documentation is a specification of the protocol and of the command
// surface, so a change that leaves it wrong is a change that leaves the
// specification wrong. Every check here recomputes a fact from the Go code and
// requires the documentation to still state it.
//
// Parity: tests/docs_alignment_tests.py
package docsalign
