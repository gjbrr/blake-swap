// Package conformance validates the machine-readable security conformance
// manifest in tests/security_conformance.json.
//
// The manifest is the contract between the Python reference and this port: every
// invariant the reference proves is named there, with the reference suites that
// prove it and the surface a parallel implementation may be compared on. As the
// port takes over the proofs, each invariant also names the Go test that proves
// it now, or the one reference scenario that still proves it alone.
//
// The checker is deliberately strict. A conformance manifest that quietly
// accepts an incomplete row reports coverage that does not exist, which is worse
// than having no manifest at all. It also resolves every Go test the manifest
// names against the source in this checkout, because a mapping to a test that
// was renamed or never written is exactly the kind of drift the manifest exists
// to catch.
package conformance
