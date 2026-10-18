package swap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/coord"
	"github.com/gjbrr/blake-swap/pkg/htlc"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/state"
)

// announcement is one settlement a peer announced.
type announcement struct {
	Kind      string
	TxID      string
	BlockHash string
	Branch    htlc.Branch
	Sender    string
	Location  string
	Duplicate bool
	State     state.State
}

// ExtractSecret reads the preimage out of Alice's Bitcoin claim.
//
// The secret never travels as a message: it is published on Bitcoin only in the
// form of a witness item, and Bob recovers it from the chain. Reading it here is
// also the only way Bob can be sure the preimage is the one that actually
// unlocked the hashlock — a message claiming a secret could say anything.
func (e *Engine) ExtractSecret(ctx context.Context, swapID string) (string, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return "", err
	}

	if _, err := e.requireRole(swap, "bob", "secret extraction"); err != nil {
		return "", err
	}

	if swap.State != state.BTCClaimed {
		return "", fmt.Errorf("extract-secret requires state BTC_CLAIMED")
	}

	if swap.BTCCLaimTxID == nil || *swap.BTCCLaimTxID == "" {
		return "", fmt.Errorf("swap has no Bitcoin claim txid")
	}

	if swap.BTCCLaimBlockHash == nil || *swap.BTCCLaimBlockHash == "" {
		return "", fmt.Errorf("swap has no Bitcoin claim blockhash")
	}

	fundingTxID, fundingVout, err := contract.Outpoint(swap, chain.BTC)

	if err != nil {
		return "", err
	}

	tx, err := e.BTC.RawTx(ctx, *swap.BTCCLaimTxID, *swap.BTCCLaimBlockHash)

	if err != nil {
		return "", err
	}

	var witness []string

	for _, in := range tx.Vin {
		if in.TxID == fundingTxID && in.Vout == fundingVout {
			witness = in.Witness

			break
		}
	}

	if witness == nil {
		return "", fmt.Errorf("Bitcoin claim does not spend the expected HTLC output")
	}

	for _, item := range witness {
		// Our secret is exactly 32 bytes, which is 64 hex characters.
		if len(item) != 64 {
			continue
		}

		candidate, err := hex.DecodeString(item)

		if err != nil {
			continue
		}

		sum := sha256.Sum256(candidate)

		if hex.EncodeToString(sum[:]) == swap.SecretHash {
			revealed := item
			swap.RevealedSecretHex = &revealed

			if err := e.save(ctx, swap); err != nil {
				return "", err
			}

			return revealed, nil
		}
	}

	return "", fmt.Errorf("No witness item matched the swap SHA256 hash")
}

// AnnounceBTCCLaimed publishes Alice's claim, which is the settlement that
// reveals the preimage on Bitcoin.
func (e *Engine) AnnounceBTCCLaimed(ctx context.Context, swapID string) (*announcement, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	if _, err := e.requireRole(swap, "alice", "BTC claim announcement"); err != nil {
		return nil, err
	}

	if swap.State != state.BTCClaimed {
		return nil, fmt.Errorf("publish-btc-claimed requires Alice state BTC_CLAIMED")
	}

	if swap.BTCCLaimTxID == nil || *swap.BTCCLaimTxID == "" {
		return nil, fmt.Errorf("Alice has no BTC claim txid")
	}

	if swap.BTCCLaimBlockHash == nil || *swap.BTCCLaimBlockHash == "" {
		return nil, fmt.Errorf("Alice has no BTC claim blockhash")
	}

	return e.announceSettlement(ctx, swap, "btc-claimed", map[string]any{
		"type":                "btc-claimed",
		"swap_id":             swap.SwapID,
		"secret_hash":         swap.SecretHash,
		"btc_claim_txid":      *swap.BTCCLaimTxID,
		"btc_claim_blockhash": *swap.BTCCLaimBlockHash,
	}, htlc.Claim)
}

// ReceiveBTCCLaimed consumes Alice's claim as Bob.
//
// The claim is verified on Bitcoin before it is recorded: the announced
// transaction must still be on the active chain, spend this swap's HTLC, and use
// the claim branch. Only then does Bob's own record advance.
func (e *Engine) ReceiveBTCCLaimed(ctx context.Context, swapID string) (*announcement, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	if _, err := e.requireRole(swap, "bob", "BTC claim receipt"); err != nil {
		return nil, err
	}

	return e.receiveSettlement(ctx, swap, chain.BTC, "btc-claimed", htlc.Claim, func(swap *model.Swap) error {
		if err := swap.Transition(state.SecretRevealed); err != nil {
			return err
		}

		return swap.Transition(state.BTCClaimed)
	})
}

// AnnounceBlakeClaimed publishes Bob's BLAKE claim.
func (e *Engine) AnnounceBlakeClaimed(ctx context.Context, swapID string) (*announcement, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	if _, err := e.requireRole(swap, "bob", "BLAKE claim announcement"); err != nil {
		return nil, err
	}

	if swap.State != state.BlakeClaimed {
		return nil, fmt.Errorf("publish-blake-claimed requires Bob state BLAKE_CLAIMED")
	}

	if swap.BlakeClaimTxID == nil || *swap.BlakeClaimTxID == "" {
		return nil, fmt.Errorf("Bob has no BLAKE claim txid")
	}

	if swap.BlakeClaimBlockHash == nil || *swap.BlakeClaimBlockHash == "" {
		return nil, fmt.Errorf("Bob has no BLAKE claim blockhash")
	}

	return e.announceSettlement(ctx, swap, "blake-claimed", map[string]any{
		"type":                  "blake-claimed",
		"swap_id":               swap.SwapID,
		"secret_hash":           swap.SecretHash,
		"blake_claim_txid":      *swap.BlakeClaimTxID,
		"blake_claim_blockhash": *swap.BlakeClaimBlockHash,
	}, htlc.Claim)
}

// ReceiveBlakeClaimed consumes Bob's BLAKE claim as Alice.
func (e *Engine) ReceiveBlakeClaimed(ctx context.Context, swapID string) (*announcement, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	if _, err := e.requireRole(swap, "alice", "BLAKE claim receipt"); err != nil {
		return nil, err
	}

	return e.receiveSettlement(ctx, swap, chain.Blake, "blake-claimed", htlc.Claim, func(swap *model.Swap) error {
		return swap.Transition(state.BlakeClaimed)
	})
}

// AnnounceBTCRefunded publishes Bob's Bitcoin refund.
func (e *Engine) AnnounceBTCRefunded(ctx context.Context, swapID string) (*announcement, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	if _, err := e.requireRole(swap, "bob", "BTC refund announcement"); err != nil {
		return nil, err
	}

	if swap.State != state.BTCRefunded {
		return nil, fmt.Errorf("publish-btc-refunded requires Bob state BTC_REFUNDED")
	}

	if swap.BTCRefundTxID == nil || *swap.BTCRefundTxID == "" {
		return nil, fmt.Errorf("Bob has no BTC refund txid")
	}

	if swap.BTCRefundBlockHash == nil || *swap.BTCRefundBlockHash == "" {
		return nil, fmt.Errorf("Bob has no BTC refund blockhash")
	}

	return e.announceSettlement(ctx, swap, "btc-refunded", map[string]any{
		"type":                 "btc-refunded",
		"swap_id":              swap.SwapID,
		"secret_hash":          swap.SecretHash,
		"btc_refund_txid":      *swap.BTCRefundTxID,
		"btc_refund_blockhash": *swap.BTCRefundBlockHash,
	}, htlc.Refund)
}

// ReceiveBTCRefunded consumes Bob's refund as Alice.
func (e *Engine) ReceiveBTCRefunded(ctx context.Context, swapID string) (*announcement, error) {
	swap, err := e.load(ctx, swapID)

	if err != nil {
		return nil, err
	}

	if _, err := e.requireRole(swap, "alice", "BTC refund receipt"); err != nil {
		return nil, err
	}

	return e.receiveSettlement(ctx, swap, chain.BTC, "btc-refunded", htlc.Refund, func(swap *model.Swap) error {
		return swap.Transition(state.BTCRefunded)
	})
}

// announceSettlement sends one of the three settlement announcements.
func (e *Engine) announceSettlement(
	ctx context.Context, swap *model.Swap, kind string, body map[string]any, branch htlc.Branch,
) (*announcement, error) {
	if _, present := body["secret_hex"]; present {
		return nil, fmt.Errorf("SECURITY FAILURE: %s message contains secret_hex", kind)
	}

	location, err := e.send(ctx, swap, kind, body)

	if err != nil {
		return nil, err
	}

	return &announcement{
		Kind: kind, TxID: settlementTxID(swap, chain.BTC, branch),
		Branch: branch, Location: location, State: swap.State,
	}, nil
}

// receiveSettlement applies one settlement announcement.
func (e *Engine) receiveSettlement(
	ctx context.Context,
	swap *model.Swap,
	ch chain.Chain,
	kind string,
	branch htlc.Branch,
	advance func(*model.Swap) error,
) (*announcement, error) {
	if e.Transport == nil {
		return nil, fmt.Errorf("the engine has no coordination transport")
	}

	peer, err := e.peerFor(swap)

	if err != nil {
		return nil, err
	}

	message, err := e.Transport.Receive(ctx, swap.SwapID, kind, coord.ReceiveOptions{
		ExpectedPubkey: peer,
		Mode:           e.Mode,
	})

	if err != nil {
		return nil, err
	}

	if err := checkReceived(message.Body, swap, kind); err != nil {
		return nil, err
	}

	if _, present := message.Body["secret_hex"]; present {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: %s message contains secret_hex", kind,
		)
	}

	prefix := "blake"

	if ch == chain.BTC {
		prefix = "btc"
	}

	suffix := "claim"

	if branch == htlc.Refund {
		suffix = "refund"
	}

	txid, err := stringField(message.Body, prefix+"_"+suffix+"_txid")

	if err != nil {
		return nil, err
	}

	blockHash, err := stringField(message.Body, prefix+"_"+suffix+"_blockhash")

	if err != nil {
		return nil, err
	}

	recordedTxID := settlementTxID(swap, ch, branch)
	recordedBlockHash := settlementBlockHash(swap, ch, branch)

	duplicate := false

	if recordedTxID != "" || recordedBlockHash != "" {
		if recordedTxID == txid && recordedBlockHash == blockHash {
			duplicate = true
		} else {
			return nil, fmt.Errorf(
				"SECURITY FAILURE: conflicting %s message for an already-recorded settlement",
				kind,
			)
		}
	}

	//
	// The announcement is verified against the chain before anything is
	// persisted, and it is verified even when it is a duplicate: a claim that
	// was reorged out between the two messages is not the settlement it was.
	//
	if _, err := e.verifyAnnouncedSettlement(ctx, swap, ch, txid, blockHash, branch); err != nil {
		return nil, err
	}

	result := &announcement{
		Kind: kind, TxID: txid, BlockHash: blockHash, Branch: branch,
		Sender: message.Sender, Location: message.Location, Duplicate: duplicate,
		State: swap.State,
	}

	if duplicate {
		return result, nil
	}

	setSettlement(swap, ch, branch, txid, blockHash, settlementHex(swap, ch, branch))

	if err := advance(swap); err != nil {
		return nil, err
	}

	if err := e.save(ctx, swap); err != nil {
		return nil, err
	}

	result.State = swap.State

	return result, nil
}

// verifyAnnouncedSettlement proves an announced settlement against the chain.
//
// The announced transaction must exist in the announced block, still be on the
// active chain, spend this swap's exact HTLC outpoint, and use the announced
// branch. Nothing from the peer's message is persisted by this check.
func (e *Engine) verifyAnnouncedSettlement(
	ctx context.Context,
	swap *model.Swap,
	ch chain.Chain,
	txid, blockHash string,
	branch htlc.Branch,
) (*chain.Tx, error) {
	fundingTxID, fundingVout, err := contract.Outpoint(swap, ch)

	if err != nil {
		return nil, fmt.Errorf("SECURITY FAILURE: %w", err)
	}

	tx, err := e.nodeFor(ch).RawTx(ctx, txid, blockHash)

	if err != nil {
		return nil, err
	}

	if tx.InActiveChain != nil && !*tx.InActiveChain {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: announced %s %s transaction is not on the active chain",
			chainLabel(ch), branch,
		)
	}

	if tx.Confirmations < 1 {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: announced %s %s transaction is not confirmed on the "+
				"active chain", chainLabel(ch), branch,
		)
	}

	if !spendsOutpoint(tx, fundingTxID, fundingVout) {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: announced %s %s transaction does not spend this "+
				"swap's HTLC", chainLabel(ch), branch,
		)
	}

	actual, err := classifyTx(tx, fundingTxID, fundingVout, swap.SecretHash)

	if err != nil {
		return nil, err
	}

	if actual != branch {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: announced %s settlement branch mismatch: expected "+
				"%s, observed %s", chainLabel(ch), branch, actual,
		)
	}

	return tx, nil
}

// spendsOutpoint reports whether a transaction spends an exact outpoint.
func spendsOutpoint(tx *chain.Tx, txid string, vout int64) bool {
	for _, in := range tx.Vin {
		if in.TxID == txid && in.Vout == vout {
			return true
		}
	}

	return false
}

// settlementBlockHash reads the recorded block hash for one branch.
func settlementBlockHash(swap *model.Swap, ch chain.Chain, branch htlc.Branch) string {
	field := settlementFields(swap, ch, branch)

	if field.blockHash == nil {
		return ""
	}

	return *field.blockHash
}
