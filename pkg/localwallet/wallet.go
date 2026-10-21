// Package localwallet is the swap's node surface backed by the local wallet.
//
// The CLI and the rig run against a node that is also its own wallet; the browser has no such
// node. This package is the second implementation of that surface: everything a swap flow
// needs from a wallet is asked of tinywallet over its channel, everything that is a *chain*
// read stays on the node, and what has no local equivalent refuses by name through
// pkg/walletdep rather than failing as a connection error.
package localwallet

import (
	"context"

	"github.com/gjbrr/blake-swap/pkg/chain"
)

// Coin is one spendable output the wallet holds.
type Coin struct {
	// Chain is the wallet chain that owns it.
	Chain string
	// TxID is the transaction id as it is printed.
	TxID string
	// Vout is the output's position.
	Vout uint32
	// Value is the amount in satoshis.
	Value uint64
	// Height is the confirming block, or zero while unconfirmed.
	Height uint32
	// Script is the scriptPubKey that paid it.
	Script []byte
}

// HTLCTerms is a swap contract's terms. Nothing in it is secret.
type HTLCTerms struct {
	// Claimer and Refunder are SEC1 compressed public keys.
	Claimer  []byte
	Refunder []byte
	// SecretHash is the SHA-256 of the preimage.
	SecretHash [32]byte
	// Locktime is the refund branch's absolute locktime.
	Locktime uint32
}

// Contract is a derived HTLC.
type Contract struct {
	// Address is the P2WSH address on the chain.
	Address string
	// Script is the witness script.
	Script []byte
	// Program is the P2WSH program the address encodes.
	Program []byte
}

// Policy bounds what the wallet may do on one call.
type Policy struct {
	// MaxSpend is the largest total input value, in satoshis.
	MaxSpend uint64
	// MaxFee is the largest fee, in satoshis.
	MaxFee uint64
}

// Wallet is what the swap needs from tinywallet. It is an interface so this package can be
// tested without a browser or a wasm module, and so the channel client is the only thing that
// knows how the bytes travel.
type Wallet interface {
	// Height is the chain's verified tip.
	Height(ctx context.Context, chain string) (int64, error)
	// Coins lists the chain's own spendable coins, post-fork only.
	Coins(ctx context.Context, chain string) ([]Coin, error)
	// Address returns a fresh receive address and its public key.
	Address(ctx context.Context, chain string) (text string, pubkey []byte, err error)
	// Send pays an address from the wallet's coins within the policy.
	Send(ctx context.Context, chain, address string, sats uint64, fee uint64, policy Policy) (string, error)
	// DerivHTLC derives a contract from its terms.
	Derive(ctx context.Context, chain string, terms HTLCTerms) (Contract, error)
	// Watch adds a contract's scriptPubKey to the chain's scan set.
	Watch(ctx context.Context, chain string, script []byte) error
	// RawTx is the raw hex of a transaction this wallet sent, for recovery and for the
	// flows that expect a wallet transaction's bytes.
	RawTx(ctx context.Context, chain, txid string) (string, error)
	// SignPSBT signs a PSBT's wallet-owned inputs within the policy.
	SignPSBT(ctx context.Context, chain string, raw []byte, policy Policy) ([]byte, int, error)
}

// Compile-time proof that the adapter is the surface the engine is written against.
var _ chain.Wallet = (*boundWallet)(nil)
