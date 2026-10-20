package worker

import (
	"context"
	"fmt"
	"strings"

	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/proto"
)

//
// Creating the four participant wallets from the page.
//
// A browser participant needs one signing wallet and one watch-only wallet per chain, and
// until now only `scripts/setup-participant` could make them: a person who installed the
// client had to drop to a shell before a funding command would work. This is that
// capability, in the worker, behind one confirmation per wallet.
//

// walletSlot is one of the four slots, and the facts that follow from it.
type walletSlot struct {
	// argument is the slot's name in the request.
	argument string
	// secret is the vault key the chosen name is stored under.
	secret string
	// chain is which node the wallet lives on.
	chain string
	// watch marks a watch-only slot, which decides the createwallet flags.
	watch bool
	// label is what the dialog calls it.
	label string
}

// walletSlotFor resolves one slot name.
//
// The slot decides the vault key and whether the wallet is watch-only, so there is no
// separate `watch` argument to disagree with it: one value, one meaning.
func walletSlotFor(argument string) (walletSlot, error) {
	switch strings.TrimSpace(argument) {
	case "blake":
		return walletSlot{"blake", SecretWalletBlake, "blake", false, "BLAKE signing wallet"}, nil
	case "btc":
		return walletSlot{"btc", SecretWalletBTC, "btc", false, "BTC signing wallet"}, nil
	case "blake_watch":
		return walletSlot{
			"blake_watch", SecretWalletBlakeWatch, "blake", true, "BLAKE watch-only wallet",
		}, nil
	case "btc_watch":
		return walletSlot{
			"btc_watch", SecretWalletBTCWatch, "btc", true, "BTC watch-only wallet",
		}, nil
	}

	return walletSlot{}, fmt.Errorf("slot must be one of blake, btc, blake_watch, btc_watch")
}

// validateCreatedWalletName refuses a name a node would not accept.
//
// The rule is the node's own, spelled out: a letter or digit, then up to 63 more of letters,
// digits, dot, underscore and dash. It is checked before any RPC, because a name that
// becomes a URL path or a filename is refused by the node in its own words and the operator
// is left guessing which part was wrong.
func validateCreatedWalletName(name string) error {
	if name == "" {
		return fmt.Errorf("a wallet name is required")
	}

	if len(name) > 64 {
		return fmt.Errorf("name is longer than 64 bytes")
	}

	for index, r := range name {
		first := index == 0

		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9':
			continue
		case r == '.' || r == '_' || r == '-':
			if first {
				return fmt.Errorf("name may not start with %q", string(r))
			}

			continue
		}

		return fmt.Errorf("name may contain only letters, digits, '.', '_' and '-'")
	}

	return nil
}

// createWallet loads or creates one slot's wallet and stores its name.
//
// The order is the point: the node first, the vault second. A name saved for a wallet the
// node does not have is worse than no name at all, because every later command reports a
// funded swap's wallet as missing and the operator has no way to tell that from a typo.
func (d *engineDomain) createWallet(ctx context.Context, request proto.Request) proto.Result {
	slot, err := walletSlotFor(request.Args["slot"])

	if err != nil {
		return proto.Result{Command: request.Command, Error: err.Error()}
	}

	name := strings.TrimSpace(request.Args["name"])

	if err := validateCreatedWalletName(name); err != nil {
		return proto.Result{
			Command: request.Command,
			Error:   slot.label + ": " + err.Error(),
		}
	}

	if err := nodeWalletRefusal("createwallet"); err != nil {
		return proto.Result{Command: request.Command, Error: err.Error()}
	}

	if err := d.connect(ctx); err != nil {
		return failed(request.Command, err)
	}

	node := d.chainNode(slot.chain)

	loaded, err := node.ListWallets(ctx)

	if err != nil {
		return failed(request.Command, err)
	}

	view := proto.WalletCreateView{
		Slot: slot.argument, Name: name, WatchOnly: slot.watch, Chain: slot.chain,
	}

	if containsWallet(loaded, name) {
		info, err := node.GetWalletInfo(ctx, name)

		if err != nil {
			return failed(request.Command, err)
		}

		if err := walletTypeMatches(slot, info); err != nil {
			return proto.Result{Command: request.Command, Error: err.Error()}
		}

		if err := d.storeWalletName(ctx, slot, name); err != nil {
			return failed(request.Command, err)
		}

		view.Loaded = true
		view.Signing = info.Signing()

		return withView(proto.Result{
			Command: request.Command,
			OK:      true,
			Text: "wallet " + name + " already exists on " + slot.chain +
				" and is loaded; the name was saved for " + slot.argument,
		}, view)
	}

	// Not loaded. `loadwallet` first, then `createwallet`: that is EnsureWallet's sequence,
	// done here so the answer can say which of the two happened, because "created" and
	// "loaded" are different facts and only one of them is news.
	created := false

	if err := node.LoadWallet(ctx, name); err != nil {
		if err := node.CreateWallet(ctx, name, slot.watch); err != nil {
			return failed(request.Command, err)
		}

		created = true
	}

	info, err := node.GetWalletInfo(ctx, name)

	if err != nil {
		return failed(request.Command, err)
	}

	if err := walletTypeMatches(slot, info); err != nil {
		return proto.Result{Command: request.Command, Error: err.Error()}
	}

	view.Loaded = true
	view.Created = created
	view.Signing = info.Signing()

	if err := d.storeWalletName(ctx, slot, name); err != nil {
		return proto.Result{
			Command: request.Command,
			Error: "wallet " + name + " exists on " + slot.chain +
				" but its name was not saved: " + err.Error() +
				"; run the same command again to save it",
		}
	}

	text := "wallet " + name + " is loaded on " + slot.chain +
		" and was saved for " + slot.argument

	if created {
		text = "wallet " + name + " was created on " + slot.chain +
			" and saved for " + slot.argument
	}

	return withView(proto.Result{Command: request.Command, OK: true, Text: text}, view)
}

// walletTypeMatches refuses a wallet whose keys do not suit the slot.
//
// A signing name in a watch-only slot looks fine until an HTLC import needs a descriptor a
// signing wallet will not take, and a watch-only name in a signing slot looks fine until a
// funding needs a signature. Both are cheap to catch here and expensive to discover later.
func walletTypeMatches(slot walletSlot, info *chain.WalletInfo) error {
	if slot.watch && info.Signing() {
		return fmt.Errorf(
			"wallet %s exists as a signing wallet, not a watch-only wallet", info.WalletName,
		)
	}

	if !slot.watch && !info.Signing() {
		return fmt.Errorf(
			"wallet %s exists as a watch-only wallet, not a signing wallet", info.WalletName,
		)
	}

	return nil
}

// storeWalletName seals the chosen name under the slot's key.
func (d *engineDomain) storeWalletName(ctx context.Context, slot walletSlot, name string) error {
	return d.vault.writeSecret(ctx, slot.secret, name)
}

// containsWallet reports whether a node lists a wallet name.
func containsWallet(loaded []string, name string) bool {
	for _, candidate := range loaded {
		if candidate == name {
			return true
		}
	}

	return false
}
