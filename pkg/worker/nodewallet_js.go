//go:build js && wasm

package worker

import "github.com/gjbrr/blake-swap/pkg/walletdep"

// nodeWalletRefusal is the browser's answer to a node-wallet command.
//
// The browser has no wallet service to call: the page reaches a light client over a relay, and
// the keys are the page's own. Saying so is better than letting the request fail at the proxy
// with a connection error, which reads as a broken node rather than as a build that does not
// have one.
func nodeWalletRefusal(operation string) error {
	return walletdep.Refuse(operation)
}
