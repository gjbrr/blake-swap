// Package walletdep names what the swap lost when the full node stopped being its wallet.
//
// The CLI and the browser used to ask bitcoind to create wallets, import descriptors, build
// and sign PSBTs, and report balances. Those calls are still in pkg/chain, but there is no
// wallet service behind them any more: the wallet is tinywallet, which holds the seed, derives
// the addresses and signs locally. A caller that reaches a node-wallet path must be told that
// plainly, because "connection refused" or "method not found" reads as a broken node, and the
// operator then goes looking for the fault in the wrong place.
package walletdep

import "errors"

// ErrNoWalletService is the refusal. It is a sentinel so a caller can test for it and a test
// can assert on it without matching prose.
var ErrNoWalletService = errors.New(
	"the full node's wallet service is not part of this build: the wallet is local (seed, " +
		"addresses and signing live in tinywallet), so creating node wallets, importing " +
		"descriptors, and signing through the node are unavailable")

// Refuse names the operation that has no wallet service behind it.
func Refuse(operation string) error {
	return &refusal{operation: operation}
}

// refusal carries the operation so the message says which call failed.
type refusal struct {
	operation string
}

// Error renders the refusal.
func (r *refusal) Error() string {
	return r.operation + ": " + ErrNoWalletService.Error()
}

// Unwrap makes errors.Is(err, ErrNoWalletService) work through the operation.
func (r *refusal) Unwrap() error { return ErrNoWalletService }
