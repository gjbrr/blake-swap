package localwallet

import (
	"context"
	"fmt"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/decimal"
	"github.com/gjbrr/blake-swap/pkg/walletdep"
)

// Node is the swap's node surface with its wallet half backed by tinywallet.
//
// Everything that is a chain read — headers, blocks, transactions, fee estimates, raw
// broadcast — stays on the node, because the light client is the chain read. Everything that
// is a wallet operation is answered by Wallet. What has no local equivalent refuses with
// walletdep.Refuse, naming the operation, instead of looking like a broken node.
type Node struct {
	// Chain is the node the chain reads go to.
	Chain *chain.Node
	// Wallet is the local wallet.
	Wallet Wallet
}

// New builds the adapter.
func New(node *chain.Node, wallet Wallet) *Node { return &Node{Chain: node, Wallet: wallet} }

// Compile-time proof that the adapter is the engine's node surface.
var _ interface {
	Bind(string) chain.Wallet
	Height(context.Context) (int64, error)
} = (*Node)(nil)

// Bind names the wallet a flow asked for. The local wallet is one wallet, so the name is only
// carried for messages; a flow that asks for two different wallets is asking for the node's
// wallet service, which this build does not have.
func (n *Node) Bind(wallet string) chain.Wallet {
	return &boundWallet{node: n, name: wallet}
}

// Height is the wallet chain's verified tip.
func (n *Node) Height(ctx context.Context) (int64, error) {
	return n.Wallet.Height(ctx, string(n.Chain.Chain))
}

// ListWallets has no local meaning: there is one wallet and it is not a node wallet.
func (n *Node) ListWallets(context.Context) ([]string, error) {
	return nil, walletdep.Refuse("listwallets")
}

// EnsureWallet would create a node wallet; the wallet already exists locally.
func (n *Node) EnsureWallet(context.Context, string, bool) error {
	return walletdep.Refuse("ensurewallet")
}

// GetWalletInfo describes a node wallet; there is none.
func (n *Node) GetWalletInfo(context.Context, string) (*chain.WalletInfo, error) {
	return nil, walletdep.Refuse("getwalletinfo")
}

// ChainInfo is the node's own chain description and is only used to decide a rescan start for
// a descriptor import, which this build does not do.
func (n *Node) ChainInfo(context.Context) (*chain.ChainInfo, error) {
	return nil, walletdep.Refuse("getblockchaininfo-for-import")
}

// DescriptorInfo is the node's descriptor compiler. The local contract is derived from its
// terms (claim key, refund key, secret hash, locktime), not from a descriptor.
func (n *Node) DescriptorInfo(context.Context, string) (*chain.DescriptorInfo, error) {
	return nil, walletdep.Refuse("getdescriptorinfo")
}

// DeriveAddresses is the node's descriptor-to-address step, for the same reason.
func (n *Node) DeriveAddresses(context.Context, string) ([]string, error) {
	return nil, walletdep.Refuse("deriveaddresses")
}

// DeriveHTLC builds a contract from its terms, which is what the engine asks a node's
// descriptor compiler for. It is the local half of contract.Derive.
func (n *Node) DeriveHTLC(ctx context.Context, ch chain.Chain, terms contract.Terms) (contract.LocalContract, error) {
	derived, err := n.Wallet.Derive(ctx, string(ch), HTLCTerms{
		Claimer:    terms.Claimer,
		Refunder:   terms.Refunder,
		SecretHash: terms.SecretHash,
		Locktime:   terms.Locktime,
	})

	if err != nil {
		return contract.LocalContract{}, err
	}

	return contract.LocalContract{Address: derived.Address, Script: derived.Script}, nil
}

// TxOut is a chain read.
func (n *Node) TxOut(ctx context.Context, txid string, vout int64) (*chain.TxOut, error) {
	return n.Chain.TxOut(ctx, txid, vout)
}

// RawTx is a chain read.
func (n *Node) RawTx(ctx context.Context, txid, blockHash string) (*chain.Tx, error) {
	return n.Chain.RawTx(ctx, txid, blockHash)
}

// DecodeRaw is a chain read.
func (n *Node) DecodeRaw(ctx context.Context, hexTx string) (*chain.Tx, error) {
	return n.Chain.DecodeRaw(ctx, hexTx)
}

// EstimateSmartFee is a chain read.
func (n *Node) EstimateSmartFee(ctx context.Context, confTarget int64) (*chain.FeeEstimate, error) {
	return n.Chain.EstimateSmartFee(ctx, confTarget)
}

// MempoolInfo is a chain read.
func (n *Node) MempoolInfo(ctx context.Context) (*chain.MempoolInfo, error) {
	return n.Chain.MempoolInfo(ctx)
}

// MineToAddress is the regtest rig's command, and it is the node's, not a wallet's.
func (n *Node) MineToAddress(ctx context.Context, blocks int64, address string) ([]string, error) {
	return n.Chain.MineToAddress(ctx, blocks, address)
}

// BlockHash is a chain read.
func (n *Node) BlockHash(ctx context.Context, height int64) (string, error) {
	return n.Chain.BlockHash(ctx, height)
}

// BlockTxs is a chain read.
func (n *Node) BlockTxs(ctx context.Context, hash string) (*chain.VerboseBlock, error) {
	return n.Chain.BlockTxs(ctx, hash)
}

// WalletTx reconstructs what a wallet transaction must say from the chain and the wallet's own
// coins: the raw hex comes from the node, the block hash from the transaction's confirmation.
func (n *Node) WalletTx(ctx context.Context, txid string) (*chain.WalletTx, error) {
	hexTx, err := n.Wallet.RawTx(ctx, string(n.Chain.Chain), txid)

	if err != nil {
		return nil, err
	}

	return &chain.WalletTx{TxID: txid, Hex: hexTx}, nil
}

// WalletTxIn reads a named node wallet, which this build does not have.
func (n *Node) WalletTxIn(context.Context, string, string) (*chain.WalletTx, error) {
	return nil, walletdep.Refuse("gettransaction")
}

// InMempool is a chain read.
func (n *Node) InMempool(ctx context.Context, txid string) (bool, error) {
	return n.Chain.InMempool(ctx, txid)
}

// CreateRawTransaction is a chain-level builder: it needs no key.
func (n *Node) CreateRawTransaction(ctx context.Context, inputs []chain.Outpoint, outputs map[string]decimal.Dec) (string, error) {
	return n.Chain.CreateRawTransaction(ctx, inputs, outputs)
}

// CreateRawTransactionAt is the same builder with a locktime.
func (n *Node) CreateRawTransactionAt(ctx context.Context, inputs []chain.Outpoint, outputs map[string]decimal.Dec, lockTime int64) (string, error) {
	return n.Chain.CreateRawTransactionAt(ctx, inputs, outputs, lockTime)
}

// ConvertToPSBT is a chain-level conversion.
func (n *Node) ConvertToPSBT(ctx context.Context, hexTx string) (string, error) {
	return n.Chain.ConvertToPSBT(ctx, hexTx)
}

// UtxoUpdatePSBT fills in witness UTXOs and scripts; it needs no key.
func (n *Node) UtxoUpdatePSBT(ctx context.Context, psbt string, descriptors []string) (string, error) {
	return n.Chain.UtxoUpdatePSBT(ctx, psbt, descriptors)
}

// FinalizePSBT is a chain-level finaliser; it needs no key.
func (n *Node) FinalizePSBT(ctx context.Context, psbt string) (*chain.FinalizeResult, error) {
	return n.Chain.FinalizePSBT(ctx, psbt)
}

// SendRaw is a chain-level broadcast.
func (n *Node) SendRaw(ctx context.Context, hexTx string) (string, error) {
	return n.Chain.SendRaw(ctx, hexTx)
}

// boundWallet is one name bound to the local wallet.
type boundWallet struct {
	node *Node
	name string
}

// Name is the name the flow asked for.
func (w *boundWallet) Name() string { return w.name }

// Balances is the sum of the chain's own coins, as an exact decimal.
func (w *boundWallet) Balances(ctx context.Context) (*chain.Balances, error) {
	coins, err := w.node.Wallet.Coins(ctx, string(w.node.Chain.Chain))

	if err != nil {
		return nil, err
	}

	var total uint64

	for _, coin := range coins {
		total += coin.Value
	}

	return chain.BalancesFromTrusted(fmt.Sprintf("%d.%08d", total/100_000_000, total%100_000_000)), nil
}

// Tx reconstructs a transaction from the chain, which is where its bytes are.
func (w *boundWallet) Tx(ctx context.Context, txid string) (*chain.WalletTx, error) {
	return w.node.WalletTx(ctx, txid)
}

// ListTransactions reports the coins the wallet holds. A send this wallet made is not a coin,
// so recovery of a send needs the transaction id from the swap record or the intent, which is
// where the engine already keeps it; there is no node wallet history to search.
func (w *boundWallet) ListTransactions(ctx context.Context, count, skip int64) ([]chain.WalletEntry, error) {
	coins, err := w.node.Wallet.Coins(ctx, string(w.node.Chain.Chain))

	if err != nil {
		return nil, err
	}

	entries := make([]chain.WalletEntry, 0, len(coins))

	for _, coin := range coins {
		amountText, err := amount.Sats(coin.Value).Coin().String(), error(nil)

		if err != nil {
			return nil, err
		}

		entries = append(entries, chain.WalletEntry{
			Category: "receive",
			Amount:   amount.Coin(amountText),
			TxID:     coin.TxID,
			Vout:     int64(coin.Vout),
		})
	}

	return entries, nil
}

// AddressInfo reports one of the wallet's own addresses.
func (w *boundWallet) AddressInfo(ctx context.Context, address string) (*chain.AddressInfo, error) {
	text, pubkey, err := w.node.Wallet.Address(ctx, string(w.node.Chain.Chain))

	if err != nil {
		return nil, err
	}

	return &chain.AddressInfo{Address: text, Pubkey: fmt.Sprintf("%x", pubkey), IsMine: true}, nil
}

// ImportDescriptor has no local equivalent: the wallet watches a contract's script through its
// own watch set, not through a node wallet's descriptor import.
func (w *boundWallet) ImportDescriptor(context.Context, string, chain.ImportWhen) (*chain.ImportResult, error) {
	return nil, walletdep.Refuse("importdescriptors")
}

// SendToAddress pays from the wallet's own coins.
func (w *boundWallet) SendToAddress(ctx context.Context, address string, sats amount.Sats, feeRate *decimal.Dec) (string, error) {
	// The local wallet takes an explicit fee: a one-input, two-output P2WPKH spend is about
	// 140 vbytes, so a rate in sat/vB times 140 is the fee. Without a rate the fee is a flat
	// 1000 sat; the policy carries the ceiling either way.
	fee := uint64(1_000)

	if feeRate != nil {
		perKB, err := feeRate.Fixed(0)

		if err != nil {
			return "", err
		}

		var rate uint64

		if _, err := fmt.Sscanf(perKB, "%d", &rate); err == nil && rate > 0 {
			fee = rate * 140
		}
	}

	coins, err := w.node.Wallet.Coins(ctx, string(w.node.Chain.Chain))

	if err != nil {
		return "", err
	}

	var balance uint64

	for _, coin := range coins {
		balance += coin.Value
	}

	return w.node.Wallet.Send(ctx, string(w.node.Chain.Chain), address, uint64(sats), fee, Policy{
		MaxSpend: balance,
		MaxFee:   fee,
	})
}

// NewAddress returns the wallet's next address.
func (w *boundWallet) NewAddress(ctx context.Context) (string, error) {
	text, _, err := w.node.Wallet.Address(ctx, string(w.node.Chain.Chain))

	return text, err
}

// Decode is a chain read.
func (w *boundWallet) Decode(ctx context.Context, hexTx string) (*chain.Tx, error) {
	return w.node.Chain.DecodeRaw(ctx, hexTx)
}

// ProcessPSBT signs with the wallet's own keys.
func (w *boundWallet) ProcessPSBT(ctx context.Context, psbt, sighash string) (*chain.ProcessPSBTResult, error) {
	signed, count, err := w.node.Wallet.SignPSBT(ctx, string(w.node.Chain.Chain), []byte(psbt), Policy{})

	if err != nil {
		return nil, err
	}

	if count == 0 {
		return nil, fmt.Errorf("localwallet: the wallet signed none of this psbt")
	}

	return &chain.ProcessPSBTResult{PSBT: string(signed), Complete: false}, nil
}
