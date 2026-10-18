// Package chain is the typed chain-node surface the swap engine uses.
//
// It sits on pkg/rpc and gives the engine named methods with parsed results
// instead of stringly-typed JSON-RPC calls. The method set is the port of the
// thirty-ish calls btc_cli/blake_cli make, and the parsing matches what the
// Python engine reads from them: heights, blocks, transactions, UTXOs, wallet
// balances, descriptor checksums and addresses.
//
// Two conventions are carried over deliberately:
//
//   - the node is the authority on the HTLC script. The engine builds a
//     descriptor body and asks the node to compile and derive from it, so a Go
//     reimplementation of Miniscript is not needed and cannot disagree; and
//   - amounts cross the boundary as exact decimal strings. A node value is
//     parsed into satoshis exactly, and a fee is rendered exactly, because a
//     rounded amount is a different trade.
package chain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/decimal"
	"github.com/gjbrr/blake-swap/pkg/rpc"
)

// Chain identifies which node is being addressed.
type Chain string

// The two chains. The names are the ones the rest of the engine uses.
const (
	BTC   Chain = "btc"
	Blake Chain = "blake"
)

// Config is how to reach one node.
type Config struct {
	// URL is the JSON-RPC endpoint, for example http://127.0.0.1:18443/.
	URL string
	// User and Password are basic-auth credentials. A browser-backed client
	// leaves them empty and goes through the app server's proxy, which holds
	// them.
	User     string
	Password string
	// Timeout is the per-attempt RPC timeout.
	TimeoutMilliseconds int
}

// Node is one chain's node, optionally bound to a wallet.
//
// Bitcoin Core selects a wallet by URL path (/wallet/<name>), not by an RPC
// argument, so binding a wallet rebuilds the transport rather than appending a
// parameter. Appending it is a mistake that looks plausible: the node answers
// with its own help text as an error.
type Node struct {
	Chain Chain
	// Client performs the RPC calls.
	Client *rpc.Client
	// Wallet is the wallet every wallet-scoped call uses. Empty means the
	// node's default wallet, or none.
	Wallet string

	config Config
}

// New builds a node from a config.
func New(name Chain, cfg Config) (*Node, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("%s: no node URL configured", name)
	}

	transport := &rpc.HTTPTransport{
		URL:      cfg.URL,
		User:     cfg.User,
		Password: cfg.Password,
	}

	return &Node{
		Chain:  name,
		Client: &rpc.Client{Transport: transport},
		config: cfg,
	}, nil
}

// WithWallet returns a copy of the node bound to a wallet.
//
// The wallet becomes part of the endpoint URL, which is how Core's wallet RPCs
// are addressed over HTTP.
func (n *Node) WithWallet(wallet string) *Node {
	copied := *n
	copied.Wallet = wallet

	if wallet != "" {
		config := n.config
		config.URL = walletURL(n.config.URL, wallet)
		copied.config = config

		copied.Client = &rpc.Client{Transport: &rpc.HTTPTransport{
			URL:      config.URL,
			User:     config.User,
			Password: config.Password,
		}}
	}

	return &copied
}

// walletURL appends the wallet path segment to a node URL.
func walletURL(base, wallet string) string {
	trimmed := strings.TrimRight(base, "/")

	return trimmed + "/wallet/" + wallet
}

// call performs one RPC call, adding the wallet argument when bound.
//
// Bitcoin Core's wallet methods take the wallet name as the final positional
// parameter, which is why every wrapper appends it rather than sending it in a
// named field.
func (n *Node) call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	return n.Client.Call(ctx, method, params...)
}

func (n *Node) callWithWallet(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	return n.Client.Call(ctx, method, params...)
}

// decode unmarshals a result into target.
func decode(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()

	return decoder.Decode(target)
}

// Height returns the current block count.
func (n *Node) Height(ctx context.Context) (int64, error) {
	raw, err := n.call(ctx, "getblockcount")

	if err != nil {
		return 0, err
	}

	var height int64

	if err := decode(raw, &height); err != nil {
		return 0, fmt.Errorf("%s: getblockcount: %w", n.Chain, err)
	}

	return height, nil
}

// BlockHash returns the hash at a height.
func (n *Node) BlockHash(ctx context.Context, height int64) (string, error) {
	raw, err := n.call(ctx, "getblockhash", height)

	if err != nil {
		return "", err
	}

	var hash string

	if err := decode(raw, &hash); err != nil {
		return "", fmt.Errorf("%s: getblockhash: %w", n.Chain, err)
	}

	return hash, nil
}

// Block is the part of a block the engine reads.
type Block struct {
	Hash          string   `json:"hash"`
	Height        int64    `json:"height"`
	Confirmations int64    `json:"confirmations"`
	Time          int64    `json:"time"`
	Tx            []string `json:"tx"`
}

// Block returns a block by hash.
func (n *Node) Block(ctx context.Context, hash string, verbosity int) (*Block, error) {
	raw, err := n.call(ctx, "getblock", hash, verbosity)

	if err != nil {
		return nil, err
	}

	var block Block

	if err := decode(raw, &block); err != nil {
		return nil, fmt.Errorf("%s: getblock: %w", n.Chain, err)
	}

	return &block, nil
}

// Vin is one transaction input.
type Vin struct {
	TxID     string   `json:"txid"`
	Vout     int64    `json:"vout"`
	Coinbase string   `json:"coinbase"`
	Witness  []string `json:"txinwitness"`
	Sequence int64    `json:"sequence"`
}

// Vout is one transaction output.
//
// Value is an amount.Coin because getrawtransaction writes output values as
// bare JSON numbers while the wallet calls write the same quantity as quoted
// strings; see amount.Coin for why neither may pass through a float.
type Vout struct {
	Value        amount.Coin `json:"value"`
	N            int64       `json:"n"`
	ScriptPubKey struct {
		Address string `json:"address"`
		Hex     string `json:"hex"`
		Type    string `json:"type"`
	} `json:"scriptPubKey"`
}

// Tx is a decoded transaction.
type Tx struct {
	TxID          string `json:"txid"`
	Hash          string `json:"hash"`
	VSize         int64  `json:"vsize"`
	Weight        int64  `json:"weight"`
	Confirmations int64  `json:"confirmations"`
	BlockHash     string `json:"blockhash"`
	BlockTime     int64  `json:"blocktime"`
	Time          int64  `json:"time"`
	Vin           []Vin  `json:"vin"`
	Vout          []Vout `json:"vout"`
}

// RawTx returns a transaction, optionally anchored to an explicit block hash.
//
// The block hash matters for the engine's recovery paths: a transaction may be
// confirmed in a block that is no longer on the active chain, and asking for it
// by block hash is how the engine reads the exact transaction it recorded
// rather than whatever is at that txid now.
func (n *Node) RawTx(ctx context.Context, txid, blockHash string) (*Tx, error) {
	var (
		raw json.RawMessage
		err error
	)

	if blockHash != "" {
		raw, err = n.call(ctx, "getrawtransaction", txid, true, blockHash)
	} else {
		raw, err = n.call(ctx, "getrawtransaction", txid, true)
	}

	if err != nil {
		return nil, err
	}

	var tx Tx

	if err := decode(raw, &tx); err != nil {
		return nil, fmt.Errorf("%s: getrawtransaction: %w", n.Chain, err)
	}

	return &tx, nil
}

// TxOut is one unspent output as gettxout reports it.
type TxOut struct {
	Value         amount.Coin `json:"value"`
	Confirmations int64       `json:"confirmations"`
	BestBlock     string      `json:"bestblock"`
	ScriptPubKey  struct {
		Address string `json:"address"`
		Hex     string `json:"hex"`
		Type    string `json:"type"`
	} `json:"scriptPubKey"`
}

// TxOut returns an unspent output, or nil when it is spent or unknown.
func (n *Node) TxOut(ctx context.Context, txid string, vout int64) (*TxOut, error) {
	raw, err := n.call(ctx, "gettxout", txid, vout, true)

	if err != nil {
		return nil, err
	}

	trimmed := strings.TrimSpace(string(raw))

	if trimmed == "null" {
		return nil, nil
	}

	var out TxOut

	if err := decode(raw, &out); err != nil {
		return nil, fmt.Errorf("%s: gettxout: %w", n.Chain, err)
	}

	return &out, nil
}

// MempoolEntry returns the mempool entry for a transaction, or nil when it is
// not in the mempool.
func (n *Node) MempoolEntry(ctx context.Context, txid string) (json.RawMessage, error) {
	raw, err := n.call(ctx, "getmempoolentry", txid)

	if err != nil {
		// The node reports "not in mempool" as an error code, not null.
		var nodeErr *rpc.Error

		if asNodeError(err, &nodeErr) {
			return nil, nil
		}

		return nil, err
	}

	return raw, nil
}

// MempoolInfo is the part of getmempoolinfo the fee policy reads.
type MempoolInfo struct {
	Size                int64       `json:"size"`
	Bytes               int64       `json:"bytes"`
	Usage               int64       `json:"usage"`
	MempoolMinFee       json.Number `json:"mempoolminfee"`
	MinRelayTxFee       json.Number `json:"minrelaytxfee"`
	IncrementalRelayFee json.Number `json:"incrementalrelayfee"`
}

// MempoolInfo returns the node's mempool state.
func (n *Node) MempoolInfo(ctx context.Context) (*MempoolInfo, error) {
	raw, err := n.call(ctx, "getmempoolinfo")

	if err != nil {
		return nil, err
	}

	var info MempoolInfo

	if err := decode(raw, &info); err != nil {
		return nil, fmt.Errorf("%s: getmempoolinfo: %w", n.Chain, err)
	}

	return &info, nil
}

// FeeEstimate is estimatesmartfee's answer.
type FeeEstimate struct {
	Feerate json.Number `json:"feerate"`
	Blocks  int64       `json:"blocks"`
	Errors  []string    `json:"errors"`
}

// EstimateSmartFee asks the node for a feerate.
func (n *Node) EstimateSmartFee(ctx context.Context, confTarget int64) (*FeeEstimate, error) {
	raw, err := n.call(ctx, "estimatesmartfee", confTarget)

	if err != nil {
		return nil, err
	}

	var estimate FeeEstimate

	if err := decode(raw, &estimate); err != nil {
		return nil, fmt.Errorf("%s: estimatesmartfee: %w", n.Chain, err)
	}

	return &estimate, nil
}

// ListWallets returns the loaded wallet names.
func (n *Node) ListWallets(ctx context.Context) ([]string, error) {
	raw, err := n.call(ctx, "listwallets")

	if err != nil {
		return nil, err
	}

	var wallets []string

	if err := decode(raw, &wallets); err != nil {
		return nil, fmt.Errorf("%s: listwallets: %w", n.Chain, err)
	}

	return wallets, nil
}

// WalletLoaded reports whether a wallet is loaded.
func (n *Node) WalletLoaded(ctx context.Context, wallet string) (bool, error) {
	wallets, err := n.ListWallets(ctx)

	if err != nil {
		return false, err
	}

	for _, name := range wallets {
		if name == wallet {
			return true, nil
		}
	}

	return false, nil
}

// LoadWallet loads an existing wallet.
func (n *Node) LoadWallet(ctx context.Context, wallet string) error {
	_, err := n.call(ctx, "loadwallet", wallet)

	return err
}

// CreateWallet creates and loads a wallet, optionally watch-only.
func (n *Node) CreateWallet(ctx context.Context, wallet string, watchOnly bool) error {
	_, err := n.call(ctx, "createwallet", wallet, true, true, "", false, watchOnly)

	return err
}

// EnsureWallet loads the wallet, creating it when it does not exist.
//
// The Python engine does the same, and the reason is worth keeping: a fresh
// regtest node is enough to run the engine, with no manual wallet setup.
func (n *Node) EnsureWallet(ctx context.Context, wallet string, watchOnly bool) error {
	loaded, err := n.ListWallets(ctx)

	if err != nil {
		return err
	}

	for _, name := range loaded {
		if name == wallet {
			return nil
		}
	}

	err = n.LoadWallet(ctx, wallet)

	if err == nil {
		return nil
	}

	return n.CreateWallet(ctx, wallet, watchOnly)
}

// Balances is the spendable balance the engine cares about.
type Balances struct {
	Trusted coinAmount
}

// coinAmount keeps the node's decimal string.
type coinAmount string

// Trusted returns the trusted balance as an exact decimal.
func (b Balances) TrustedAmount() (decimal.Dec, error) {
	return decimal.Parse(string(b.Trusted))
}

// GetBalances returns a wallet's balances.
func (n *Node) GetBalances(ctx context.Context) (*Balances, error) {
	raw, err := n.callWithWallet(ctx, "getbalances")

	if err != nil {
		return nil, err
	}

	var envelope struct {
		Mine struct {
			Trusted json.Number `json:"trusted"`
		} `json:"mine"`
	}

	if err := decode(raw, &envelope); err != nil {
		return nil, fmt.Errorf("%s: getbalances: %w", n.Chain, err)
	}

	return &Balances{Trusted: coinAmount(envelope.Mine.Trusted.String())}, nil
}

// ListUnspent returns a wallet's unspent outputs.
func (n *Node) ListUnspent(ctx context.Context) ([]Unspent, error) {
	raw, err := n.callWithWallet(ctx, "listunspent")

	if err != nil {
		return nil, err
	}

	var unspent []Unspent

	if err := decode(raw, &unspent); err != nil {
		return nil, fmt.Errorf("%s: listunspent: %w", n.Chain, err)
	}

	return unspent, nil
}

// Unspent is one listunspent entry.
type Unspent struct {
	TxID          string      `json:"txid"`
	Vout          int64       `json:"vout"`
	Address       string      `json:"address"`
	ScriptPubKey  string      `json:"scriptPubKey"`
	Amount        amount.Coin `json:"amount"`
	Confirmations int64       `json:"confirmations"`
	Spendable     bool        `json:"spendable"`
	Solvable      bool        `json:"solvable"`
}

// Sats returns the amount in satoshis, exactly.
func (u Unspent) Sats() (amount.Sats, error) { return u.Amount.Sats() }

// WalletTx is gettransaction's answer, reduced to what the engine reads.
type WalletTx struct {
	TxID          string      `json:"txid"`
	Confirmations int64       `json:"confirmations"`
	BlockHash     string      `json:"blockhash"`
	Amount        amount.Coin `json:"amount"`
	Time          int64       `json:"time"`
	Details       []struct {
		Address  string      `json:"address"`
		Category string      `json:"category"`
		Amount   amount.Coin `json:"amount"`
		Vout     int64       `json:"vout"`
	} `json:"details"`
}

// WalletTx returns a wallet transaction.
func (n *Node) WalletTx(ctx context.Context, txid string) (*WalletTx, error) {
	raw, err := n.callWithWallet(ctx, "gettransaction", txid, true)

	if err != nil {
		return nil, err
	}

	var tx WalletTx

	if err := decode(raw, &tx); err != nil {
		return nil, fmt.Errorf("%s: gettransaction: %w", n.Chain, err)
	}

	return &tx, nil
}

// NewAddress returns a fresh wallet address.
func (n *Node) NewAddress(ctx context.Context) (string, error) {
	raw, err := n.callWithWallet(ctx, "getnewaddress", "", "bech32")

	if err != nil {
		return "", err
	}

	var address string

	if err := decode(raw, &address); err != nil {
		return "", fmt.Errorf("%s: getnewaddress: %w", n.Chain, err)
	}

	return address, nil
}

// DescriptorInfo is getdescriptorinfo's answer.
type DescriptorInfo struct {
	Descriptor string `json:"descriptor"`
	Checksum   string `json:"checksum"`
	IsRange    bool   `json:"isrange"`
	IsSolvable bool   `json:"issolvable"`
	HasPrivate bool   `json:"hasprivatekeys"`
}

// DescriptorInfo compiles a descriptor body and returns its checksum.
func (n *Node) DescriptorInfo(ctx context.Context, body string) (*DescriptorInfo, error) {
	raw, err := n.call(ctx, "getdescriptorinfo", body)

	if err != nil {
		return nil, err
	}

	var info DescriptorInfo

	if err := decode(raw, &info); err != nil {
		return nil, fmt.Errorf("%s: getdescriptorinfo: %w", n.Chain, err)
	}

	return &info, nil
}

// DeriveAddresses returns the addresses a descriptor derives.
func (n *Node) DeriveAddresses(ctx context.Context, descriptor string) ([]string, error) {
	raw, err := n.call(ctx, "deriveaddresses", descriptor)

	if err != nil {
		return nil, err
	}

	var addresses []string

	if err := decode(raw, &addresses); err != nil {
		return nil, fmt.Errorf("%s: deriveaddresses: %w", n.Chain, err)
	}

	return addresses, nil
}

// ImportDescriptor imports a descriptor into the bound wallet.
func (n *Node) ImportDescriptor(ctx context.Context, descriptor string, watchOnly bool) error {
	request := []map[string]any{{
		"desc":      descriptor,
		"timestamp": "now",
		"active":    false,
		"watchonly": watchOnly,
	}}

	payload, err := json.Marshal(request)

	if err != nil {
		return err
	}

	_, err = n.callWithWallet(ctx, "importdescriptors", json.RawMessage(payload))

	return err
}

// SendToAddress sends funds from the bound wallet.
//
// The amount is rendered exactly, and the confirmation target is passed so the
// node's own estimator prices the funding. Funding fees are the node wallet's
// choice by design; the settlement fee policy belongs to the engine.
func (n *Node) SendToAddress(ctx context.Context, address string, sats amount.Sats, confTarget int64) (string, error) {
	rendered := sats.Coin().String()

	raw, err := n.callWithWallet(ctx, "sendtoaddress",
		address, rendered, "", "", false, true, confTarget, "conservative")

	if err != nil {
		return "", err
	}

	var txid string

	if err := decode(raw, &txid); err != nil {
		return "", fmt.Errorf("%s: sendtoaddress: %w", n.Chain, err)
	}

	return txid, nil
}

// SendRaw broadcasts a finalized transaction.
func (n *Node) SendRaw(ctx context.Context, hexTx string) (string, error) {
	raw, err := n.call(ctx, "sendrawtransaction", hexTx)

	if err != nil {
		return "", err
	}

	var txid string

	if err := decode(raw, &txid); err != nil {
		return "", fmt.Errorf("%s: sendrawtransaction: %w", n.Chain, err)
	}

	return txid, nil
}

// DecodedTx is decoderawtransaction's answer, reduced to the fields used.
type DecodedTx struct {
	TxID   string `json:"txid"`
	VSize  int64  `json:"vsize"`
	Weight int64  `json:"weight"`
	Vin    []Vin  `json:"vin"`
	Vout   []Vout `json:"vout"`
}

// DecodeRaw decodes a raw transaction.
func (n *Node) DecodeRaw(ctx context.Context, hexTx string) (*DecodedTx, error) {
	raw, err := n.call(ctx, "decoderawtransaction", hexTx)

	if err != nil {
		return nil, err
	}

	var decoded DecodedTx

	if err := decode(raw, &decoded); err != nil {
		return nil, fmt.Errorf("%s: decoderawtransaction: %w", n.Chain, err)
	}

	return &decoded, nil
}

// MineToAddress mines blocks to an address. It exists for the test rig.
func (n *Node) MineToAddress(ctx context.Context, blocks int64, address string) ([]string, error) {
	raw, err := n.call(ctx, "generatetoaddress", blocks, address)

	if err != nil {
		return nil, err
	}

	var hashes []string

	if err := decode(raw, &hashes); err != nil {
		return nil, fmt.Errorf("%s: generatetoaddress: %w", n.Chain, err)
	}

	return hashes, nil
}

func asNodeError(err error, target **rpc.Error) bool {
	for err != nil {
		if nodeErr, ok := err.(*rpc.Error); ok {
			*target = nodeErr
			return true
		}

		type unwrapper interface{ Unwrap() error }

		unwrapped, ok := err.(unwrapper)

		if !ok {
			return false
		}

		err = unwrapped.Unwrap()
	}

	return false
}
