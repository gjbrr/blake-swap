//go:build js && wasm

package chain

import "github.com/gjbrr/blake-swap/pkg/walletdep"

// refuseNodeWallet is the browser's answer to every node wallet call.
//
// A wallet RPC is not one method: it is createwallet, loadwallet, importdescriptors,
// walletprocesspsbt, finalizepsbt, sendtoaddress, listunspent, listtransactions, and the rest.
// They all pass through callWithWallet, so that is where the refusal goes — one place, every
// method, and a message that says the wallet is local rather than a proxy error that reads as
// a dead node.
func refuseNodeWallet(method string) error {
	return walletdep.Refuse(method)
}
