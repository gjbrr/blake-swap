//go:build !(js && wasm)

package chain

// refuseNodeWallet is nil natively: the CLI and the rig suites drive a node they run, and that
// is the evidence those paths work.
func refuseNodeWallet(string) error { return nil }
