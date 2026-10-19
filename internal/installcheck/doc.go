//go:build !js

// Package installcheck proves that a freshly installed blake-swap binary works
// from outside the source checkout.
//
// It is the Go counterpart of the Python reference's installed-wheel suite
// (tests/installed_wheel_tests.py). That suite builds a wheel, installs it into
// a clean virtual environment, runs the generated console entry points from
// outside the repository and proves the installed modules resolve from the
// environment rather than from src/. This package does the same thing for the
// Go artifact that actually ships:
//
//   - it runs `go install ./cmd/blake-swap-cli` into a fresh temporary GOBIN;
//   - it executes the resulting binary from a temporary working directory and
//     an empty temporary HOME, outside the repository;
//   - it hands the process a from-scratch environment containing only PATH,
//     HOME and the state/relay variables the chosen command genuinely needs,
//     and withholds every BLAKE_SWAP_* chain discovery variable;
//   - it requires the embedded build information to name this module; and
//   - it exercises a node-free market read against a loopback relay, so the
//     installed artifact is shown doing real work (WebSocket, event
//     verification, state rendering, cache writing) rather than only printing
//     usage text.
//
// The wheel-specific assertions have no Go analogue and are recorded as
// explicit waivers in install_test.go, each naming the Python assertion it
// replaces and the Go evidence that covers the real risk.
package installcheck
