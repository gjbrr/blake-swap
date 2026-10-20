//go:build !(js && wasm)

package worker

// nodeWalletRefusal is nil on native builds: the operator runs the node these commands talk
// to, and the rig suites are the evidence that those paths work. The refusal belongs to the
// browser surface, which has no node wallet to call.
func nodeWalletRefusal(string) error { return nil }
