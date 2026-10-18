package fund

import (
	"context"
	"fmt"
	"strings"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/model"
)

// Payment is a wallet payment to a contract address, proven from the decoded
// transaction rather than from the wallet's own summary.
type Payment struct {
	TxID      string
	Vout      int64
	BlockHash string
}

// FindPayment looks in this wallet for a transaction that already pays the
// exact contract address and amount.
//
// This is recovery, not counterparty trust: every candidate is decoded locally
// and its actual output checked, so a wallet whose history was edited, or whose
// amounts were rounded on display, cannot make the engine adopt the wrong
// output. The comparison is in the chain's smallest unit, exactly.
func FindPayment(
	ctx context.Context, wallet chain.Wallet, address string, amount int64,
) (*Payment, error) {
	entries, err := wallet.ListTransactions(ctx, 1000, 0)

	if err != nil {
		return nil, err
	}

	var candidates []string

	seen := map[string]bool{}

	for _, entry := range entries {
		if entry.Category != "send" || entry.Address != address {
			continue
		}

		// Wallet entries report sends as negative amounts, so the magnitude is
		// what compares to the amount the swap expects.
		observed, err := parseCoinMagnitude(entry.Amount)

		if err != nil || observed != amount {
			continue
		}

		if entry.TxID == "" || seen[entry.TxID] {
			continue
		}

		seen[entry.TxID] = true
		candidates = append(candidates, entry.TxID)
	}

	for _, txid := range candidates {
		walletTx, err := wallet.Tx(ctx, txid)

		if err != nil || walletTx.Hex == "" {
			continue
		}

		decoded, err := wallet.Decode(ctx, walletTx.Hex)

		if err != nil {
			continue
		}

		for _, out := range decoded.Vout {
			if out.ScriptPubKey.Address != address {
				continue
			}

			value, err := out.Value.Sats()

			if err != nil || int64(value) != amount {
				continue
			}

			return &Payment{
				TxID: txid, Vout: out.N, BlockHash: walletTx.BlockHash,
			}, nil
		}
	}

	return nil, nil
}

// parseCoinMagnitude reads a wallet amount's absolute value in base units.
func parseCoinMagnitude(value amount.Coin) (int64, error) {
	text := strings.TrimPrefix(strings.TrimSpace(value.String()), "-")

	parsed, err := amount.ParseSats(text)

	if err != nil {
		return 0, err
	}

	return int64(parsed), nil
}

// LocatePaymentOutput finds the contract output of one transaction this wallet
// created, and reports whether it is confirmed yet.
func LocatePaymentOutput(
	ctx context.Context, wallet chain.Wallet, txid, address string, amount int64,
) (int64, string, error) {
	walletTx, err := wallet.Tx(ctx, txid)

	if err != nil {
		return 0, "", err
	}

	if walletTx.Hex == "" {
		return 0, "", fmt.Errorf("wallet transaction has no raw hex")
	}

	decoded, err := wallet.Decode(ctx, walletTx.Hex)

	if err != nil {
		return 0, "", err
	}

	for _, out := range decoded.Vout {
		if out.ScriptPubKey.Address != address {
			continue
		}

		value, err := out.Value.Sats()

		if err != nil || int64(value) != amount {
			continue
		}

		return out.N, walletTx.BlockHash, nil
	}

	return 0, "", fmt.Errorf(
		"Could not locate expected HTLC funding output in exact wallet transaction",
	)
}

// Anchor is a confirmed funding outpoint proven to be the contract.
type Anchor struct {
	TxID          string
	Vout          int64
	BlockHash     string
	Confirmations int64
}

// VerifyAnchor proves an exact funding outpoint is confirmed in a block that is
// still on the active chain, before durable protocol state advances.
//
// A wallet-reported block hash is only a recovery hint. The chain is
// authoritative, and this is the call that makes the difference: it asks for the
// transaction *by that block*, so a reorged transaction comes back inactive
// rather than looking fine.
func VerifyAnchor(
	ctx context.Context,
	node Node,
	swap *model.Swap,
	ch chain.Chain,
	txid string,
	vout int64,
	blockHash string,
) (*Anchor, error) {
	name := chainName(ch)

	if txid == "" {
		return nil, fmt.Errorf("SECURITY FAILURE: %s funding has no txid", name)
	}

	if blockHash == "" {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: %s funding has no confirmed block anchor", name,
		)
	}

	tx, err := node.RawTx(ctx, txid, blockHash)

	if err != nil {
		return nil, err
	}

	if tx.InActiveChain != nil && !*tx.InActiveChain {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: %s funding transaction is not on the active chain",
			name,
		)
	}

	if tx.Confirmations < 1 {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: %s funding transaction is not confirmed on the active chain",
			name,
		)
	}

	var output *chain.Vout

	for i := range tx.Vout {
		if tx.Vout[i].N == vout {
			output = &tx.Vout[i]

			break
		}
	}

	if output == nil {
		return nil, fmt.Errorf(
			"SECURITY FAILURE: %s funding transaction has no output %d",
			name, vout,
		)
	}

	derived, err := contract.Derive(ctx, node, swap, ch)

	if err != nil {
		return nil, err
	}

	if err := derived.CheckOutput(output.Value, output.ScriptPubKey.Address); err != nil {
		return nil, fmt.Errorf("SECURITY FAILURE: %w", err)
	}

	return &Anchor{
		TxID:          txid,
		Vout:          vout,
		BlockHash:     blockHash,
		Confirmations: tx.Confirmations,
	}, nil
}
