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

// WatchStatus is the outcome of a watch-only import.
type WatchStatus struct {
	Chain      chain.Chain
	Wallet     string
	Address    string
	Descriptor string
	// When is the rescan start the import used, which is what makes a silent miss
	// explainable.
	When     chain.ImportWhen
	Imported bool
	Reason   string
}

// ImportWatchOnly records the HTLC in a watch-only wallet and reports what the
// node said about it.
//
// Idempotent: the descriptor is a pure function of the swap terms, so a second
// call imports an identical string and the node reports that as a warning
// rather than an error.
//
// when is the rescan start, and it is not a detail. An import with "now" that
// happens after the funding block has been mined stores the descriptor and
// never sees the output, and the wallet then reports a funded HTLC as somebody
// else's; the caller decides between ImportNow (nothing exists yet),
// ImportEverything on regtest and for a restore, and ImportFrom a creation time
// on a chain with real history. See chain.ImportWhen.
func ImportWatchOnly(
	ctx context.Context,
	node Node,
	swap *model.Swap,
	ch chain.Chain,
	derived *contract.Contract,
	wallet string,
	when chain.ImportWhen,
) WatchStatus {
	status := WatchStatus{
		Chain:      ch,
		Wallet:     wallet,
		Address:    derived.Address,
		Descriptor: derived.Descriptor,
		When:       when,
	}

	if err := ensureWatchOnly(ctx, node, wallet); err != nil {
		status.Reason = err.Error()

		return status
	}

	result, err := node.Bind(wallet).ImportDescriptor(ctx, derived.Descriptor, when)

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

// ensureWatchOnly loads the wallet, creating it without keys when absent, and
// refuses one that has keys.
//
// disable_private_keys and blank are the arguments that matter: the wallet holds
// no key of its own and generates none, so it can never be confused with the
// signing wallet, and importing the public HTLC descriptor is the only way it
// learns about an output.
//
// The type check is not paranoia. A name that already exists as a signing wallet
// keeps working for every other command, and the node refuses the import with
// "Cannot import descriptor without private keys to a wallet with private keys
// enabled" — a refusal that used to be swallowed, leaving a funded HTLC that the
// watch-only wallet could not see. Naming the problem here is what turns that
// into an operator's answer.
func ensureWatchOnly(ctx context.Context, node Node, wallet string) error {
	if err := node.EnsureWallet(ctx, wallet, true); err != nil {
		return err
	}

	info, err := node.GetWalletInfo(ctx, wallet)

	if err != nil {
		return fmt.Errorf("wallet %s: %w", wallet, err)
	}

	if info.Signing() {
		return fmt.Errorf(
			"wallet %s has private keys enabled, so it cannot hold a watch-only descriptor; "+
				"set a watch-only wallet name for this chain", wallet,
		)
	}

	return nil
}

// recoveryRescanMarginSeconds is how far before a swap's creation its recovery import
// starts.
//
// A block's timestamp is set by the miner and can be earlier than the moment the block was
// found, so a rescan from exactly the creation second can step over the funding block. A
// day of blocks is a cheap margin on a chain with history and free on regtest.
const recoveryRescanMarginSeconds = 24 * 60 * 60

// importWhenForFunding is the rescan start a pre-funding import uses.
//
// Nothing exists yet, so "now" is right and cheapest. On regtest a rescan from the
// beginning costs nothing and survives the one race that matters: a block mined between the
// import and the funding transaction, which is exactly what a fast regtest does. A node
// that cannot say what chain it follows is treated as a chain with history, where "now" is
// the harmless answer for an import that expects no history.
func importWhenForFunding(ctx context.Context, node Node) chain.ImportWhen {
	if regtest, err := isRegtest(ctx, node); err == nil && regtest {
		return chain.ImportEverything()
	}

	return chain.ImportNow()
}

// ImportWhenForRecovery is the rescan start a recovery import uses.
//
// The opposite trade: money may already have moved, so an import that misses it is worse
// than a slow one. On regtest the whole chain. On a chain with history, the swap's creation
// time less a margin. A record with no creation time, or a node that cannot say what chain
// it follows, rescans from the beginning: correct and possibly slow, and the status carries
// the choice so an operator can see it.
func ImportWhenForRecovery(
	ctx context.Context, node Node, swap *model.Swap,
) chain.ImportWhen {
	if regtest, err := isRegtest(ctx, node); err == nil && regtest {
		return chain.ImportEverything()
	}

	if swap != nil && swap.CreatedAt > 0 {
		return chain.ImportFrom(swap.CreatedAt - recoveryRescanMarginSeconds)
	}

	return chain.ImportEverything()
}

// isRegtest asks the node whether it follows a regtest chain.
func isRegtest(ctx context.Context, node Node) (bool, error) {
	info, err := node.ChainInfo(ctx)

	if err != nil {
		return false, err
	}

	return info.Chain == "regtest", nil
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
