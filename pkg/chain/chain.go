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
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/decimal"
	"github.com/gjbrr/blake-swap/pkg/platform"
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
	// RetryAttempts is how many times a read-only method may be attempted. Zero
	// means the transport's default.
	RetryAttempts int
	// RetryBackoffMilliseconds is the delay before the second attempt, doubled
	// for each one after it. A negative value is refused rather than clamped.
	RetryBackoffMilliseconds int
	// RetryMaxBackoffMilliseconds bounds that doubling.
	RetryMaxBackoffMilliseconds int
}

// client builds the RPC client this configuration describes.
func (c Config) client(transport rpc.Transport) *rpc.Client {
	client := &rpc.Client{Transport: transport}

	if c.RetryAttempts > 0 {
		client.MaxAttempts = c.RetryAttempts
	}

	if c.RetryBackoffMilliseconds > 0 {
		client.Backoff = time.Duration(c.RetryBackoffMilliseconds) * time.Millisecond
	}

	if c.RetryMaxBackoffMilliseconds > 0 {
		client.MaxBackoff = time.Duration(c.RetryMaxBackoffMilliseconds) * time.Millisecond
	}

	return client
}

// timeout is the per-attempt deadline, zero meaning none.
func (c Config) timeout() time.Duration {
	if c.TimeoutMilliseconds <= 0 {
		return 0
	}

	return time.Duration(c.TimeoutMilliseconds) * time.Millisecond
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

	transport := platform.NewTransport(cfg.URL, cfg.User, cfg.Password, cfg.timeout())

	return &Node{
		Chain:  name,
		Client: cfg.client(transport),
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

		copied.Client = config.client(platform.NewTransport(
			config.URL, config.User, config.Password, config.timeout(),
		))
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
	if err := refuseNodeWallet(method); err != nil {
		return nil, err
	}

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

// VerboseBlock is a block with its transactions decoded.
//
// The recovery scans need each transaction's inputs, and asking for block
// verbosity 2 fetches that in one call instead of one getrawtransaction per
// transaction, which on a node without txindex would not even be possible.
type VerboseBlock struct {
	Hash          string `json:"hash"`
	Height        int64  `json:"height"`
	Confirmations int64  `json:"confirmations"`
	Tx            []Tx   `json:"tx"`
}

// BlockTxs returns a block with its transactions decoded.
func (n *Node) BlockTxs(ctx context.Context, hash string) (*VerboseBlock, error) {
	raw, err := n.call(ctx, "getblock", hash, 2)

	if err != nil {
		return nil, err
	}

	var block VerboseBlock

	if err := decode(raw, &block); err != nil {
		return nil, fmt.Errorf("%s: getblock: %w", n.Chain, err)
	}

	return &block, nil
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
	InActiveChain *bool  `json:"in_active_chain"`
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

// InMempool reports whether the node holds a transaction in its mempool.
//
// The distinction it preserves matters for recovery: "the node says it is not
// there" is a fact a caller may act on, while a transport failure is not, and a
// caller that branched on error text could not tell them apart.
func (n *Node) InMempool(ctx context.Context, txid string) (bool, error) {
	entry, err := n.MempoolEntry(ctx, txid)

	if err != nil {
		return false, err
	}

	return entry != nil, nil
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

// ChainInfo is the part of getblockchaininfo the client decides with.
type ChainInfo struct {
	// Chain is main, test, signet or regtest, as the node spells it.
	Chain string `json:"chain"`
	// Blocks is the current tip height, in blocks.
	Blocks int64 `json:"blocks"`
	// BestBlockHash is the tip hash.
	BestBlockHash string `json:"bestblockhash"`
}

// ChainInfo returns the node's own description of the chain it follows.
func (n *Node) ChainInfo(ctx context.Context) (*ChainInfo, error) {
	raw, err := n.call(ctx, "getblockchaininfo")

	if err != nil {
		return nil, err
	}

	var info ChainInfo

	if err := decode(raw, &info); err != nil {
		return nil, fmt.Errorf("%s: getblockchaininfo: %w", n.Chain, err)
	}

	return &info, nil
}

// IsRegtest reports whether the node follows a regtest chain.
//
// It is asked rather than configured because the answer belongs to the node: the same
// datadir can be started against a different network, and a client that assumed one would
// choose the wrong rescan start silently.
func (n *Node) IsRegtest(ctx context.Context) (bool, error) {
	info, err := n.ChainInfo(ctx)

	if err != nil {
		return false, err
	}

	return info.Chain == "regtest", nil
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

// WalletInfo is what the node says about one loaded wallet.
//
// The one field the client acts on is PrivateKeysEnabled: a signing slot must hold a wallet
// that can sign and a watch-only slot must hold one that cannot, and a name typed into the
// wrong row is a mistake this is how the client catches.
type WalletInfo struct {
	WalletName         string `json:"walletname"`
	WalletVersion      int64  `json:"walletversion"`
	PrivateKeysEnabled bool   `json:"private_keys_enabled"`
	Descriptors        bool   `json:"descriptors"`
	// WatchOnly is what some nodes call a wallet with private keys disabled. It is read
	// because two spellings of the same fact is how a type check silently stops working.
	WatchOnly bool `json:"private_keys_disabled"`
}

// Signing reports whether the wallet can sign for itself.
func (w WalletInfo) Signing() bool { return w.PrivateKeysEnabled && !w.WatchOnly }

// GetWalletInfo asks the node about one loaded wallet.
//
// The call is wallet-scoped, so it is the node bound to that wallet that answers: a node
// with no wallet selected answers this method with its own help text as an error.
func (n *Node) GetWalletInfo(ctx context.Context, wallet string) (*WalletInfo, error) {
	raw, err := n.WithWallet(wallet).callWithWallet(ctx, "getwalletinfo")

	if err != nil {
		return nil, err
	}

	var info WalletInfo

	if err := decode(raw, &info); err != nil {
		return nil, fmt.Errorf("%s: getwalletinfo: %w", n.Chain, err)
	}

	if info.WalletName == "" {
		info.WalletName = wallet
	}

	return &info, nil
}

// LoadWallet loads an existing wallet.
func (n *Node) LoadWallet(ctx context.Context, wallet string) error {
	_, err := n.call(ctx, "loadwallet", wallet)

	return err
}

// CreateWallet creates and loads a wallet, optionally watch-only.
//
// Both flags matter, and one of them is not the one it looks like. A watch-only
// wallet needs disable_private_keys (so it can hold a public descriptor at all),
// and *every* wallet the engine creates needs descriptors=true: a legacy wallet
// cannot import a descriptor, and the HTLC is a descriptor, so a wallet created
// without it would fail at the first watch-only import.
func (n *Node) CreateWallet(ctx context.Context, wallet string, watchOnly bool) error {
	_, err := n.call(ctx, "createwallet", wallet, watchOnly, true, "", false, true)

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

// BalancesFromTrusted builds a Balances from an exact decimal string.
//
// It exists so a caller that already has the figure — a test double, or a
// cached wallet snapshot — can construct one without going through RPC, and
// without a float ever standing in for the amount.
func BalancesFromTrusted(trusted string) *Balances {
	return &Balances{Trusted: coinAmount(trusted)}
}

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
	Hex           string      `json:"hex"`
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

// WalletTxIn reads a wallet transaction from a named wallet.
//
// The recovery paths hold a wallet *name*, not a node bound to a wallet, because
// the participant wallet is a configuration fact rather than a connection.
func (n *Node) WalletTxIn(ctx context.Context, wallet, txid string) (*WalletTx, error) {
	return n.WithWallet(wallet).WalletTx(ctx, txid)
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

// ImportWhen is the history start an imported descriptor rescans from.
//
// It is the one argument of importdescriptors that decides whether an import sees money
// that already moved, and getting it wrong is silent: the descriptor is stored, the address
// derives, and the wallet then reports an output it holds as somebody else's. Three cases
// cover the two callers:
//
//   - a descriptor imported *before* funding describes outputs that do not exist yet, so
//     there is nothing to rescan and "now" is both correct and cheapest;
//   - a descriptor restored into a fresh watch-only wallet must find what already exists,
//     so it rescans from the beginning (free on regtest, and the reason a mainnet restore
//     should pass a creation time instead);
//   - a recovery import on a chain with real history rescans from the moment the swap
//     was created, which is the earliest block that can hold its funding.
type ImportWhen struct {
	// Now means this describes a new address set and there is nothing in the past.
	Now bool
	// Unix is the rescan start when Now is false. Zero means the beginning of the chain.
	Unix int64
}

// ImportNow is the history start for a descriptor that describes only new outputs.
func ImportNow() ImportWhen { return ImportWhen{Now: true} }

// ImportEverything is the history start of a wallet that must find what already exists.
//
// It is cheap on regtest and expensive on a chain with a long history, so the recovery
// path prefers ImportFrom with the swap's creation time when it has one.
func ImportEverything() ImportWhen { return ImportWhen{} }

// ImportFrom is the history start at a unix time.
func ImportFrom(unix int64) ImportWhen {
	if unix <= 0 {
		return ImportEverything()
	}

	return ImportWhen{Unix: unix}
}

// render is the value importdescriptors takes: "now" or an epoch second.
func (w ImportWhen) render() any {
	if w.Now {
		return "now"
	}

	return w.Unix
}

// String describes the choice, for an error or an operator's report.
func (w ImportWhen) String() string {
	if w.Now {
		return "now"
	}

	if w.Unix == 0 {
		return "the beginning of the chain"
	}

	return fmt.Sprintf("unix %d", w.Unix)
}

// ImportDescriptor imports a descriptor into the bound wallet.
func (n *Node) ImportDescriptor(
	ctx context.Context, descriptor string, watchOnly bool, when ImportWhen,
) error {
	request := []map[string]any{{
		"desc":      descriptor,
		"timestamp": when.render(),
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

// Wallet is the wallet-scoped surface the engine's funding and settlement
// layers use.
//
// It is an interface, and the binding is by wallet name rather than by
// constructing a node with a wallet attached, so the same code can address the
// signing wallet and the watch-only wallet on one node — which is exactly what
// funding does — and so a fake can stand in for the node in tests.
type Wallet interface {
	// Name is the wallet as the node knows it.
	Name() string
	// Balances returns the wallet's trusted balance.
	Balances(ctx context.Context) (*Balances, error)
	// Tx returns a wallet transaction.
	Tx(ctx context.Context, txid string) (*WalletTx, error)
	// ListTransactions returns the wallet's recent entries, newest first.
	ListTransactions(ctx context.Context, count, skip int64) ([]WalletEntry, error)
	// AddressInfo reports what the wallet knows about an address, including
	// whether it can currently solve it.
	AddressInfo(ctx context.Context, address string) (*AddressInfo, error)
	// ImportDescriptor imports the descriptor and reports the node's verdict
	// per descriptor rather than as one opaque error. when is the rescan start,
	// and it is the argument that decides whether an import sees money that
	// already moved.
	ImportDescriptor(ctx context.Context, descriptor string, when ImportWhen) (*ImportResult, error)
	// SendToAddress sends funds at an explicit feerate, or at the node's own
	// estimate when the rate is nil.
	SendToAddress(ctx context.Context, address string, sats amount.Sats, feeRate *decimal.Dec) (string, error)
	// NewAddress returns a fresh receiving address.
	NewAddress(ctx context.Context) (string, error)
	// Decode decodes raw transaction hex. It is node-wide rather than
	// wallet-scoped, and it lives here so recovery code that holds a wallet can
	// verify what that wallet claims without also holding the node.
	Decode(ctx context.Context, hexTx string) (*Tx, error)
	// ProcessPSBT signs a PSBT with this wallet's keys.
	ProcessPSBT(ctx context.Context, psbt, sighash string) (*ProcessPSBTResult, error)
}

// Bind returns this node's wallet-scoped surface for one wallet name.
func (n *Node) Bind(name string) Wallet {
	return &walletNode{node: n.WithWallet(name), name: name}
}

// walletNode is the concrete Wallet: a node whose transport carries the
// wallet in the URL path.
type walletNode struct {
	node *Node
	name string
}

func (w *walletNode) Name() string { return w.name }

func (w *walletNode) Balances(ctx context.Context) (*Balances, error) {
	return w.node.GetBalances(ctx)
}

func (w *walletNode) Tx(ctx context.Context, txid string) (*WalletTx, error) {
	return w.node.WalletTx(ctx, txid)
}

func (w *walletNode) ListTransactions(ctx context.Context, count, skip int64) ([]WalletEntry, error) {
	return w.node.ListTransactions(ctx, count, skip)
}

func (w *walletNode) AddressInfo(ctx context.Context, address string) (*AddressInfo, error) {
	return w.node.AddressInfo(ctx, address)
}

func (w *walletNode) ImportDescriptor(
	ctx context.Context, descriptor string, when ImportWhen,
) (*ImportResult, error) {
	return w.node.ImportDescriptorResult(ctx, descriptor, when)
}

func (w *walletNode) SendToAddress(
	ctx context.Context, address string, sats amount.Sats, feeRate *decimal.Dec,
) (string, error) {
	return w.node.sendToAddressAt(ctx, address, sats, feeRate)
}

func (w *walletNode) NewAddress(ctx context.Context) (string, error) {
	return w.node.NewAddress(ctx)
}

func (w *walletNode) Decode(ctx context.Context, hexTx string) (*Tx, error) {
	return w.node.DecodeRaw(ctx, hexTx)
}

func (w *walletNode) ProcessPSBT(
	ctx context.Context, psbt, sighash string,
) (*ProcessPSBTResult, error) {
	return w.node.ProcessPSBT(ctx, psbt, sighash)
}

// WalletEntry is one listtransactions entry, reduced to what the engine reads.
type WalletEntry struct {
	Address       string      `json:"address"`
	Category      string      `json:"category"`
	Amount        amount.Coin `json:"amount"`
	TxID          string      `json:"txid"`
	Vout          int64       `json:"vout"`
	Confirmations int64       `json:"confirmations"`
	BlockHash     string      `json:"blockhash"`
}

// ListTransactions returns recent wallet entries.
//
// The "send" entries are how funding recovery finds a payment this wallet
// already made, so the list includes unconfirmed and abandoned entries: a
// funding transaction that has not confirmed is exactly the case recovery is
// for.
func (n *Node) ListTransactions(ctx context.Context, count, skip int64) ([]WalletEntry, error) {
	raw, err := n.callWithWallet(ctx, "listtransactions", "*", count, skip, true)

	if err != nil {
		return nil, err
	}

	var entries []WalletEntry

	if err := decode(raw, &entries); err != nil {
		return nil, fmt.Errorf("%s: listtransactions: %w", n.Chain, err)
	}

	return entries, nil
}

// AddressInfo is getaddressinfo's answer, reduced to the facts the engine uses.
type AddressInfo struct {
	Address string `json:"address"`
	// Pubkey is the address's public key, when the wallet knows it. It is how the
	// engine names a participant's contract key without a second key store: the
	// wallet is what will sign the settlement, so the HTLC must name its key.
	Pubkey       string `json:"pubkey"`
	ScriptPubKey string `json:"scriptPubKey"`
	IsMine       bool   `json:"ismine"`
	IsWatchOnly  bool   `json:"iswatchonly"`
	Solvable     bool   `json:"solvable"`
	Desc         string `json:"desc"`
}

// AddressInfo asks the bound wallet what it knows about an address.
func (n *Node) AddressInfo(ctx context.Context, address string) (*AddressInfo, error) {
	raw, err := n.callWithWallet(ctx, "getaddressinfo", address)

	if err != nil {
		return nil, err
	}

	var info AddressInfo

	if err := decode(raw, &info); err != nil {
		return nil, fmt.Errorf("%s: getaddressinfo: %w", n.Chain, err)
	}

	return &info, nil
}

// ImportResult is importdescriptors' verdict for one descriptor.
type ImportResult struct {
	Success  bool     `json:"success"`
	Warnings []string `json:"warnings"`
	Error    *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Reason describes a failed import.
func (r ImportResult) Reason() string {
	if r.Error != nil && r.Error.Message != "" {
		return r.Error.Message
	}

	return "the node reported the import as unsuccessful"
}

// ImportDescriptorResult imports a descriptor and reports the node's verdict.
//
// funding needs the verdict rather than a bool: a node that refuses the import
// has to be reported to the operator, but it must not stop the funding that is
// already in progress — see pkg/fund.
func (n *Node) ImportDescriptorResult(
	ctx context.Context, descriptor string, when ImportWhen,
) (*ImportResult, error) {
	request := []map[string]any{{
		"desc":      descriptor,
		"timestamp": when.render(),
		"active":    false,
		"internal":  false,
	}}

	payload, err := json.Marshal(request)

	if err != nil {
		return nil, err
	}

	raw, err := n.callWithWallet(ctx, "importdescriptors", json.RawMessage(payload))

	if err != nil {
		return nil, err
	}

	var results []ImportResult

	if err := decode(raw, &results); err != nil {
		return nil, fmt.Errorf("%s: importdescriptors: %w", n.Chain, err)
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("%s: importdescriptors: the node returned no result", n.Chain)
	}

	return &results[0], nil
}

// sendToAddressAt broadcasts a wallet payment at an explicit feerate.
//
// The rate is rendered exactly, and conf_target is omitted so the node cannot
// silently prefer its own estimate over the rate the engine chose.
func (n *Node) sendToAddressAt(
	ctx context.Context, address string, sats amount.Sats, feeRate *decimal.Dec,
) (string, error) {
	rendered := sats.Coin().String()

	params := []any{address, rendered, "", "", false, true, nil, "unset", false}

	if feeRate != nil {
		params = append(params, feeRate.String())
	}

	raw, err := n.callWithWallet(ctx, "sendtoaddress", params...)

	if err != nil {
		return "", err
	}

	var txid string

	if err := decode(raw, &txid); err != nil {
		return "", fmt.Errorf("%s: sendtoaddress: %w", n.Chain, err)
	}

	return txid, nil
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

// Outpoint names one transaction output.
type Outpoint struct {
	TxID string `json:"txid"`
	Vout int64  `json:"vout"`
}

// CreateRawTransaction builds an unsigned transaction with no locktime.
func (n *Node) CreateRawTransaction(
	ctx context.Context, inputs []Outpoint, outputs map[string]decimal.Dec,
) (string, error) {
	return n.CreateRawTransactionAt(ctx, inputs, outputs, 0)
}

// CreateRawTransactionAt builds an unsigned transaction with an explicit nLockTime.
//
// The locktime is not decoration: a timelock (CLTV) branch is only spendable by a
// transaction whose nLockTime has reached the contract's height, and the node's
// finalizer refuses to complete one that has not. A refund built without it looks
// perfectly correct and then fails to finalize.
//
// Amounts are rendered from exact decimals, never from floats: the node would
// happily accept 0.1 as the nearest double, and the resulting transaction would
// pay a different amount than the swap's record says it should.
func (n *Node) CreateRawTransactionAt(
	ctx context.Context, inputs []Outpoint, outputs map[string]decimal.Dec, lockTime int64,
) (string, error) {
	encodedOutputs := make(map[string]json.RawMessage, len(outputs))

	if len(inputs) == 0 {
		return "", fmt.Errorf("createrawtransaction: no inputs")
	}

	if len(outputs) == 0 {
		return "", fmt.Errorf("createrawtransaction: no outputs")
	}

	for address, amount := range outputs {
		if amount.Sign() <= 0 {
			return "", fmt.Errorf("createrawtransaction: amount must be positive")
		}

		encodedOutputs[address] = json.RawMessage(amount.String())
	}

	params := []any{inputs, encodedOutputs}

	if lockTime > 0 {
		params = append(params, lockTime)
	}

	raw, err := n.call(ctx, "createrawtransaction", params...)

	if err != nil {
		return "", err
	}

	var hexTx string

	if err := decode(raw, &hexTx); err != nil {
		return "", fmt.Errorf("%s: createrawtransaction: %w", n.Chain, err)
	}

	return hexTx, nil
}

// ConvertToPSBT turns an unsigned transaction into a PSBT.
//
// The node is asked rather than the engine because the PSBT is the node's
// serialization, and a hand-built one that disagrees with the finalizer would
// fail at the worst possible moment.
func (n *Node) ConvertToPSBT(ctx context.Context, hexTx string) (string, error) {
	raw, err := n.call(ctx, "converttopsbt", hexTx)

	if err != nil {
		return "", err
	}

	var psbt string

	if err := decode(raw, &psbt); err != nil {
		return "", fmt.Errorf("%s: converttopsbt: %w", n.Chain, err)
	}

	return psbt, nil
}

// UtxoUpdatePSBT fills in the UTXO and descriptor facts a witness needs.
//
// Without this the wallet can sign the input but the finalizer cannot build the
// witness script for an HTLC the wallet only knows from a descriptor.
func (n *Node) UtxoUpdatePSBT(
	ctx context.Context, psbt string, descriptors []string,
) (string, error) {
	raw, err := n.call(ctx, "utxoupdatepsbt", psbt, descriptors)

	if err != nil {
		return "", err
	}

	var updated string

	if err := decode(raw, &updated); err != nil {
		return "", fmt.Errorf("%s: utxoupdatepsbt: %w", n.Chain, err)
	}

	return updated, nil
}

// FinalizeResult is finalizepsbt's answer.
type FinalizeResult struct {
	Hex      string `json:"hex"`
	Complete bool   `json:"complete"`
}

// FinalizePSBT assembles the witnesses and returns the raw transaction.
func (n *Node) FinalizePSBT(ctx context.Context, psbt string) (*FinalizeResult, error) {
	raw, err := n.call(ctx, "finalizepsbt", psbt)

	if err != nil {
		return nil, err
	}

	var result FinalizeResult

	if err := decode(raw, &result); err != nil {
		return nil, fmt.Errorf("%s: finalizepsbt: %w", n.Chain, err)
	}

	return &result, nil
}

// ProcessPSBTResult is walletprocesspsbt's answer.
type ProcessPSBTResult struct {
	PSBT     string `json:"psbt"`
	Complete bool   `json:"complete"`
}

// ProcessPSBT asks the bound wallet to sign a PSBT.
//
// Finalization is left to FinalizePSBT: the claim branch needs a preimage
// injected between signing and finalizing, and a wallet that finalized early
// would refuse the injected field.
func (n *Node) ProcessPSBT(
	ctx context.Context, psbt, sighash string,
) (*ProcessPSBTResult, error) {
	raw, err := n.callWithWallet(ctx, "walletprocesspsbt", psbt, true, sighash, true, false)

	if err != nil {
		return nil, err
	}

	var result ProcessPSBTResult

	if err := decode(raw, &result); err != nil {
		return nil, fmt.Errorf("%s: walletprocesspsbt: %w", n.Chain, err)
	}

	return &result, nil
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

// DecodeRaw decodes a raw transaction.
//
// The answer is the same Tx shape every other read returns, so a transaction
// recovered through the wallet (gettransaction's hex, decoded here) and one
// fetched by txid are interchangeable to the caller. decoderawtransaction does
// not report confirmations or a block hash; a caller that knows them sets them.
func (n *Node) DecodeRaw(ctx context.Context, hexTx string) (*Tx, error) {
	raw, err := n.call(ctx, "decoderawtransaction", hexTx)

	if err != nil {
		return nil, err
	}

	var decoded Tx

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

// TimeoutFromEnv reads the chain RPC timeout.
//
// The default is the reference's, and it exists for a specific failure: a node that accepts
// the connection and then never answers. Without a deadline that is an engine that hangs
// forever with no error and nothing to retry, which is worse than a timeout because there is
// nothing to look at. A configured value is validated rather than coerced, so a typo in a
// unit file fails at the call that reads it.
//
// Override with BLAKE_SWAP_RPC_TIMEOUT_SECONDS.
func TimeoutFromEnv() (time.Duration, error) {
	raw := env("BLAKE_SWAP_RPC_TIMEOUT_SECONDS")

	if raw == "" {
		return defaultRPCTimeout, nil
	}

	seconds, err := strconv.ParseFloat(raw, 64)

	if err != nil {
		return 0, fmt.Errorf("BLAKE_SWAP_RPC_TIMEOUT_SECONDS must be a number")
	}

	if seconds <= 0 {
		return 0, fmt.Errorf("BLAKE_SWAP_RPC_TIMEOUT_SECONDS must be positive")
	}

	return time.Duration(seconds * float64(time.Second)), nil
}

// defaultRPCTimeout is the reference's RPC timeout.
const defaultRPCTimeout = 120 * time.Second

// env reads one environment variable through os, isolated so the timing helpers above are
// testable and so a future platform has one place to change.
func env(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

// RetryPolicy is how many times a read-only RPC may be attempted, and how long
// to wait between attempts.
//
// The allowlist itself is not configurable: which methods may be retried is a
// safety property, not a preference. Only the count and the delays are knobs.
type RetryPolicy struct {
	// Attempts is the first attempt plus its retries.
	Attempts int
	// Backoff is the delay before the second attempt, doubled after that.
	Backoff time.Duration
	// MaxBackoff bounds the doubling.
	MaxBackoff time.Duration
}

// RetryFromEnv reads the retry policy from the environment.
//
// The defaults and the validation are the reference's: three attempts, a one
// second backoff capped at thirty, a non-integer or out-of-range value refused
// rather than clamped. Clamping would silently give an operator a policy they did
// not ask for, which on a read that is being retried is how a slow node becomes a
// hung one.
func RetryFromEnv() (RetryPolicy, error) {
	policy := RetryPolicy{Attempts: 3, Backoff: time.Second, MaxBackoff: 30 * time.Second}

	if raw := env("BLAKE_SWAP_RPC_MAX_ATTEMPTS"); raw != "" {
		attempts, err := strconv.Atoi(raw)

		if err != nil {
			return policy, fmt.Errorf("BLAKE_SWAP_RPC_MAX_ATTEMPTS must be an integer")
		}

		if attempts < 1 {
			return policy, fmt.Errorf("BLAKE_SWAP_RPC_MAX_ATTEMPTS must be at least 1")
		}

		policy.Attempts = attempts
	}

	cap, err := secondsFromEnv("BLAKE_SWAP_RPC_MAX_RETRY_BACKOFF_SECONDS", 30)

	if err != nil {
		return policy, err
	}

	policy.MaxBackoff = cap

	backoff, err := secondsFromEnv("BLAKE_SWAP_RPC_RETRY_BACKOFF_SECONDS", 1)

	if err != nil {
		return policy, err
	}

	// The reference returns the smaller of the two, so a large base cannot
	// outgrow its own cap.
	policy.Backoff = min(backoff, cap)

	return policy, nil
}

// secondsFromEnv reads a non-negative duration in seconds.
func secondsFromEnv(name string, fallback float64) (time.Duration, error) {
	raw := env(name)

	if raw == "" {
		return time.Duration(fallback * float64(time.Second)), nil
	}

	value, err := strconv.ParseFloat(raw, 64)

	if err != nil {
		return 0, fmt.Errorf("%s must be a number", name)
	}

	if value < 0 {
		return 0, fmt.Errorf("%s must not be negative", name)
	}

	return time.Duration(value * float64(time.Second)), nil
}
