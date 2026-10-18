package swap

import (
	"context"
	"fmt"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/coord"
	"github.com/gjbrr/blake-swap/pkg/fund"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/state"
)

// MessageResult is what a coordination command did.
type MessageResult struct {
	Kind string
	// Location is the spool path or event id the message went to or came from.
	Location string
	// Sender is the authenticated author of a received message.
	Sender string
	// Duplicate means the message announced a funding this side already had.
	Duplicate bool
	State     state.State
}

// AnnounceBlakeFunding publishes Alice's funding outpoint.
//
// The message carries the contract facts and the outpoint. It deliberately does
// not carry the secret: Bob receives the preimage only from Alice's confirmed
// Bitcoin claim, which is why the swap is atomic at all.
func (e *Engine) AnnounceBlakeFunding(ctx context.Context, swapID string) (*MessageResult, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	if _, err := e.requireRole(swap, "alice", "BLAKE funding announcement"); err != nil {
		return nil, err
	}

	if swap.State != state.BlakeFunded {
		return nil, fmt.Errorf("publish-blake-funded requires Alice state BLAKE_FUNDED")
	}

	if swap.BlakeFundingTxID == nil || *swap.BlakeFundingTxID == "" {
		return nil, fmt.Errorf("Alice has no BLAKE funding txid")
	}

	if swap.BlakeFundingVout == nil {
		return nil, fmt.Errorf("Alice has no BLAKE funding vout")
	}

	body := map[string]any{
		"type":               "blake-funded",
		"swap_id":            swap.SwapID,
		"secret_hash":        swap.SecretHash,
		"blake_amount":       amount.Blake(swap.BlakeAmountBaseUnits).Format(),
		"blake_timeout":      swap.BlakeTimeout,
		"blake_funding_txid": *swap.BlakeFundingTxID,
		"blake_funding_vout": int64(*swap.BlakeFundingVout),
	}

	location, err := e.send(ctx, swap, chain.Blake, "blake-funded", body)

	if err != nil {
		return nil, err
	}

	return &MessageResult{Kind: "blake-funded", Location: location, State: swap.State}, nil
}

// ReceiveBlakeFunding consumes Alice's funding announcement as Bob.
//
// Nothing here trusts the announcement. The outpoint is bound in memory, verified
// against this participant's own node, anchored to the block that holds it, and
// only then written to the record — because a message can say anything, and the
// chain is the thing that decides.
func (e *Engine) ReceiveBlakeFunding(ctx context.Context, swapID string) (*MessageResult, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	if _, err := e.requireRole(swap, "bob", "BLAKE funding receipt"); err != nil {
		return nil, err
	}

	return e.receiveFunding(ctx, swap, chain.Blake, "blake-funded", state.BlakeFunded, func(message map[string]any) (string, int64, error) {
		if err := checkReceived(message, swap, "blake-funded"); err != nil {
			return "", 0, err
		}

		announced, err := blakeBaseUnits(message["blake_amount"])

		if err != nil {
			return "", 0, err
		}

		if announced != swap.BlakeAmountBaseUnits {
			return "", 0, fmt.Errorf("BLAKE amount mismatch")
		}

		if timeoutOf(message["blake_timeout"]) != swap.BlakeTimeout {
			return "", 0, fmt.Errorf("BLAKE timeout mismatch")
		}

		txid, err := stringField(message, "blake_funding_txid")

		if err != nil {
			return "", 0, err
		}

		vout, err := intField(message, "blake_funding_vout")

		if err != nil {
			return "", 0, err
		}

		return txid, vout, nil
	})
}

// AnnounceBTCFunding publishes Bob's Bitcoin funding outpoint.
func (e *Engine) AnnounceBTCFunding(ctx context.Context, swapID string) (*MessageResult, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	if _, err := e.requireRole(swap, "bob", "BTC funding announcement"); err != nil {
		return nil, err
	}

	if swap.State != state.BTCFunded {
		return nil, fmt.Errorf("publish-btc-funded requires Bob state BTC_FUNDED")
	}

	if swap.BTCFundingTxID == nil || *swap.BTCFundingTxID == "" {
		return nil, fmt.Errorf("Bob has no BTC funding txid")
	}

	if swap.BTCFundingVout == nil {
		return nil, fmt.Errorf("Bob has no BTC funding vout")
	}

	body := map[string]any{
		"type":             "btc-funded",
		"swap_id":          swap.SwapID,
		"secret_hash":      swap.SecretHash,
		"btc_sats":         swap.BTCSats,
		"btc_timeout":      swap.BTCTimeout,
		"btc_funding_txid": *swap.BTCFundingTxID,
		"btc_funding_vout": int64(*swap.BTCFundingVout),
	}

	location, err := e.send(ctx, swap, chain.BTC, "btc-funded", body)

	if err != nil {
		return nil, err
	}

	return &MessageResult{Kind: "btc-funded", Location: location, State: swap.State}, nil
}

// ReceiveBTCFunding consumes Bob's funding announcement as Alice.
func (e *Engine) ReceiveBTCFunding(ctx context.Context, swapID string) (*MessageResult, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	if _, err := e.requireRole(swap, "alice", "BTC funding receipt"); err != nil {
		return nil, err
	}

	return e.receiveFunding(ctx, swap, chain.BTC, "btc-funded", state.BTCFunded, func(message map[string]any) (string, int64, error) {
		if err := checkReceived(message, swap, "btc-funded"); err != nil {
			return "", 0, err
		}

		sats, err := intField(message, "btc_sats")

		if err != nil {
			return "", 0, err
		}

		if sats != swap.BTCSats {
			return "", 0, fmt.Errorf("BTC amount mismatch")
		}

		if timeoutOf(message["btc_timeout"]) != swap.BTCTimeout {
			return "", 0, fmt.Errorf("BTC timeout mismatch")
		}

		txid, err := stringField(message, "btc_funding_txid")

		if err != nil {
			return "", 0, err
		}

		vout, err := intField(message, "btc_funding_vout")

		if err != nil {
			return "", 0, err
		}

		return txid, vout, nil
	})
}

// receiveFunding applies one funding announcement.
//
// `read` validates the announcement's own claims and returns the outpoint; the
// rest of the verification is the same for both chains and is the part that
// matters: the outpoint must be this swap's contract, confirmed, and in the block
// the confirmations imply.
func (e *Engine) receiveFunding(
	ctx context.Context,
	swap *model.Swap,
	ch chain.Chain,
	kind string,
	target state.State,
	read func(map[string]any) (string, int64, error),
) (*MessageResult, error) {
	if e.Transport == nil {
		return nil, fmt.Errorf("the engine has no coordination transport")
	}

	node := e.nodeFor(ch)

	peer := e.peerFor(swap, ch)

	message, err := e.Transport.Receive(ctx, swap.SwapID, kind, coord.ReceiveOptions{
		ExpectedPubkey: peer,
		Mode:           e.Mode,
	})

	if err != nil {
		return nil, err
	}

	if _, present := message.Body["secret_hex"]; present {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: %s message contains secret_hex", chainLabel(ch),
		)
	}

	txid, vout, err := read(message.Body)

	if err != nil {
		return nil, err
	}

	existingTxID := fundingTxID(swap, ch)
	existingVout := fundingVout(swap, ch)

	if existingTxID != "" || existingVout != nil {
		//
		// The same authenticated announcement arriving twice is harmless: relays,
		// reconnects and retries produce it legitimately. A *different* outpoint
		// for the same swap is a conflict, and there is no rule that could pick
		// between them.
		//
		if existingTxID == txid && existingVout != nil && int64(*existingVout) == vout {
			return &MessageResult{
				Kind: kind, Location: message.Location, Sender: message.Sender,
				Duplicate: true, State: swap.State,
			}, nil
		}

		return nil, fmt.Errorf(
			"SECURITY FAILURE: conflicting %s funding message for an already-funded swap",
			chainLabel(ch),
		)
	}

	// Bind in memory only: nothing durable changes until the chain agrees.
	index := int32(vout)
	setFunding(swap, ch, &txid, &index, nil)

	//
	// Two checks prove the same property, deliberately. VerifyFunding reads the
	// live UTXO set, and verifyAnchor reads the transaction out of the block the
	// confirmations imply. Either one alone would refuse a wrong amount or
	// address; keeping both means neither a lagging UTXO view nor a cached block
	// lookup can nod the funding through on its own.
	//
	if err := contract.VerifyFunding(ctx, node, swap, ch); err != nil {
		return nil, err
	}

	utxo, err := node.TxOut(ctx, txid, vout)

	if err != nil {
		return nil, err
	}

	if utxo == nil {
		return nil, fmt.Errorf(
			"%s HTLC disappeared immediately after successful verification",
			chainLabel(ch),
		)
	}

	if utxo.Confirmations < 1 {
		return nil, fmt.Errorf("%s funding transaction is not confirmed", chainLabel(ch))
	}

	tip, err := node.Height(ctx)

	if err != nil {
		return nil, err
	}

	height := tip - utxo.Confirmations + 1

	if height < 0 {
		return nil, fmt.Errorf(
			"Invalid %s funding height derived from confirmations", chainLabel(ch),
		)
	}

	blockHash, err := node.BlockHash(ctx, height)

	if err != nil {
		return nil, err
	}

	block, err := node.BlockTxs(ctx, blockHash)

	if err != nil {
		return nil, err
	}

	found := false

	for _, transaction := range block.Tx {
		if transaction.TxID == txid {
			found = true

			break
		}
	}

	if !found {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: independently derived %s funding block does not "+
				"contain the announced funding transaction", chainLabel(ch),
		)
	}

	if err := e.verifyAnchor(ctx, swap, ch, txid, vout, blockHash); err != nil {
		return nil, err
	}

	setFunding(swap, ch, &txid, &index, &blockHash)

	if err := swap.Transition(target); err != nil {
		return nil, err
	}

	if err := e.save(ctx, swap); err != nil {
		return nil, err
	}

	return &MessageResult{
		Kind: kind, Location: message.Location, Sender: message.Sender,
		State: swap.State,
	}, nil
}

// verifyAnchor proves a confirmed funding outpoint is still the contract, using
// the block the confirmations imply rather than the announcement.
func (e *Engine) verifyAnchor(
	ctx context.Context, swap *model.Swap, ch chain.Chain, txid string, vout int64, blockHash string,
) error {
	_, err := fund.VerifyAnchor(ctx, e.nodeFor(ch), swap, ch, txid, vout, blockHash)

	return err
}

// send delivers one coordination message as the sender's role.
func (e *Engine) send(
	ctx context.Context, swap *model.Swap, ch chain.Chain, kind string, body map[string]any,
) (string, error) {
	if e.Transport == nil {
		return "", fmt.Errorf("the engine has no coordination transport")
	}

	if e.Identity == nil {
		return "", fmt.Errorf("no local Nostr identity is configured")
	}

	role, peer, err := LocalRole(swap, e.Identity.PublicKey())

	if err != nil {
		return "", err
	}

	return e.Transport.Send(ctx, swap.SwapID, kind, body, coord.SendOptions{
		LocalRole:    role,
		ExpectedRole: role,
		PeerPubkey:   peer,
		Mode:         e.Mode,
		CreatedAt:    e.now().Unix(),
	})
}

// peerFor is the pinned counterparty identity for one chain's announcement.
func (e *Engine) peerFor(swap *model.Swap, ch chain.Chain) string {
	if ch == chain.Blake {
		if swap.NostrAlicePub != nil {
			return *swap.NostrAlicePub
		}

		return ""
	}

	if swap.NostrBobPub != nil {
		return *swap.NostrBobPub
	}

	return ""
}

// checkReceived is the part of a received announcement every type shares.
func checkReceived(message map[string]any, swap *model.Swap, kind string) error {
	if message["type"] != kind {
		return fmt.Errorf("message is not %s", kind)
	}

	if message["swap_id"] != swap.SwapID {
		return fmt.Errorf("swap_id mismatch")
	}

	if message["secret_hash"] != swap.SecretHash {
		return fmt.Errorf("secret_hash mismatch")
	}

	return nil
}

// fundingTxID reads one chain's recorded funding txid.
func fundingTxID(swap *model.Swap, ch chain.Chain) string {
	if ch == chain.Blake {
		if swap.BlakeFundingTxID == nil {
			return ""
		}

		return *swap.BlakeFundingTxID
	}

	if swap.BTCFundingTxID == nil {
		return ""
	}

	return *swap.BTCFundingTxID
}

// fundingVout reads one chain's recorded funding vout.
func fundingVout(swap *model.Swap, ch chain.Chain) *int32 {
	if ch == chain.Blake {
		return swap.BlakeFundingVout
	}

	return swap.BTCFundingVout
}

// setFunding records one chain's funding outpoint.
func setFunding(swap *model.Swap, ch chain.Chain, txid *string, vout *int32, blockHash *string) {
	if ch == chain.Blake {
		swap.BlakeFundingTxID = txid
		swap.BlakeFundingVout = vout
		swap.BlakeFundingBlockHash = blockHash

		return
	}

	swap.BTCFundingTxID = txid
	swap.BTCFundingVout = vout
	swap.BTCFundingBlockHash = blockHash
}

// stringField reads a string from a decoded message body.
func stringField(message map[string]any, name string) (string, error) {
	value, ok := message[name].(string)

	if !ok || value == "" {
		return "", fmt.Errorf("message has no %s", name)
	}

	return value, nil
}

// intField reads an integer from a decoded message body.
//
// JSON decoding produces float64 for numbers, so a value that is not integral is
// refused rather than truncated: a vout of 1.5 is not a vout.
func intField(message map[string]any, name string) (int64, error) {
	switch value := message[name].(type) {
	case int64:
		return value, nil
	case int:
		return int64(value), nil
	case float64:
		if value != float64(int64(value)) {
			return 0, fmt.Errorf("message field %s is not an integer", name)
		}

		return int64(value), nil
	}

	return 0, fmt.Errorf("message has no %s", name)
}

// timeoutOf reads a timeout height from a message.
func timeoutOf(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case float64:
		return int64(typed)
	}

	return -1
}

// blakeBaseUnits parses a wire amount into base units.
func blakeBaseUnits(value any) (int64, error) {
	units, err := amount.ParseBlake(value)

	if err != nil {
		return 0, err
	}

	return int64(units), nil
}
