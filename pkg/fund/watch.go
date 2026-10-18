package fund

import (
	"context"
	"fmt"

	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/model"
)

// WatchWalletName is the dedicated wallet that holds this participant's
// watch-only HTLCs.
//
// A wallet of its own rather than the participant's, for two reasons the node
// enforces: a wallet with private keys enabled refuses to import a watch-only
// descriptor at all, and the participant wallet's keys are exactly what a key
// loss destroys — a watch-only copy kept inside it would be lost with them.
//
// The name is per chain, but the wallet is per node, because the two chains are
// separate datadirs: `<wallet>-watch` on each node is two wallets, not one.
func WatchWalletName(ch chain.Chain, signingWallet, override string) string {
	if override != "" {
		return override
	}

	if signingWallet == "" {
		signingWallet = DefaultWallet(ch)
	}

	return signingWallet + "-watch"
}

// watchWallet resolves the option's watch-only wallet name.
func watchWallet(opts Options, ch chain.Chain) string {
	return WatchWalletName(ch, opts.Wallet, opts.WatchWallet)
}

// WatchStatus is the outcome of the pre-funding watch-only import.
type WatchStatus struct {
	Chain      chain.Chain
	Wallet     string
	Address    string
	Descriptor string
	Imported   bool
	Reason     string
}

// ImportWatchOnly records the HTLC in a watch-only wallet before funds move.
//
// Idempotent: the descriptor is a pure function of the swap terms, so a second
// call imports an identical string and the node reports that as a warning
// rather than an error. The timestamp is "now" on purpose — the descriptor is
// imported before the HTLC is funded, and rescanning the chain to find an
// output that does not exist yet would be wasted work. What the import buys is
// that the output is *solvable* the moment it appears.
func ImportWatchOnly(
	ctx context.Context,
	node Node,
	swap *model.Swap,
	ch chain.Chain,
	derived *contract.Contract,
	wallet string,
) WatchStatus {
	status := WatchStatus{
		Chain:      ch,
		Wallet:     wallet,
		Address:    derived.Address,
		Descriptor: derived.Descriptor,
	}

	if err := ensureWatchOnly(ctx, node, wallet); err != nil {
		status.Reason = err.Error()

		return status
	}

	result, err := node.Bind(wallet).ImportDescriptor(ctx, derived.Descriptor)

	if err != nil {
		status.Reason = err.Error()

		return status
	}

	if !result.Success {
		status.Reason = result.Reason()

		return status
	}

	status.Imported = true

	return status
}

// ensureWatchOnly loads the wallet, creating it without keys when absent.
//
// disable_private_keys and blank are the arguments that matter: the wallet holds
// no key of its own and generates none, so it can never be confused with the
// signing wallet, and importing the public HTLC descriptor is the only way it
// learns about an output.
func ensureWatchOnly(ctx context.Context, node Node, wallet string) error {
	return node.EnsureWallet(ctx, wallet, true)
}

// Solvability says whether any loaded wallet can currently solve this swap's
// HTLC.
type Solvability struct {
	Chain    chain.Chain
	Address  string
	Wallet   string
	Solvable *bool
	Reason   string
}

// CheckSolvability asks each wallet that might be able to solve the HTLC: the
// signing wallet, which can spend it if it still holds the keys, and the
// watch-only wallet, which imported the public descriptor and can at least see
// the output.
//
// It returns a status rather than an error because it feeds a read-only
// dashboard: a chain that cannot be reached is reported, not raised.
func CheckSolvability(
	ctx context.Context, node Node, swap *model.Swap, ch chain.Chain, opts Options,
) Solvability {
	status := Solvability{Chain: ch}

	derived, err := contract.Derive(ctx, node, swap, ch)

	if err != nil {
		status.Reason = err.Error()

		return status
	}

	status.Address = derived.Address

	candidates := []string{opts.Wallet, watchWallet(opts, ch)}

	for _, wallet := range candidates {
		if wallet == "" {
			continue
		}

		loaded, err := node.ListWallets(ctx)

		if err != nil {
			status.Reason = "wallet service unavailable: " + err.Error()

			return status
		}

		if !contains(loaded, wallet) {
			continue
		}

		info, err := node.Bind(wallet).AddressInfo(ctx, derived.Address)

		if err != nil {
			continue
		}

		if info.Solvable {
			solvable := true
			status.Wallet = wallet
			status.Solvable = &solvable

			return status
		}
	}

	unsolvable := false
	status.Solvable = &unsolvable
	status.Reason = fmt.Sprintf(
		"no loaded wallet can solve this HTLC; import the descriptor with " +
			"`watch-htlc` (see docs/KEY_RECOVERY.md)",
	)

	return status
}

// contains reports whether the list holds the name.
func contains(list []string, name string) bool {
	for _, candidate := range list {
		if candidate == name {
			return true
		}
	}

	return false
}
