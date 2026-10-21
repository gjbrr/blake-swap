// Package localwallet proves the adapter on the regtest rig.
//
// It needs the rig, so it skips unless NOSTRINO_RIG=1: two nodes with compact filters, the
// "rig" wallet loaded on each, clients at the paths below. The wallet is a real tinywallet
// domain driving both chains over nostrino; the swap side is this package's adapter. Nothing
// in the flow calls a node wallet RPC.
package localwallet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"

	chaincfg "git.smesh.lol/nostrino/pkg/chain"
	"git.smesh.lol/nostrino/pkg/chaindata"
	"git.smesh.lol/nostrino/pkg/neutrino"
	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/decimal"
	"github.com/gjbrr/blake-swap/pkg/fund"
	"github.com/gjbrr/blake-swap/pkg/model"

	"git.smesh.lol/tinywallet/pkg/domain"
	"git.smesh.lol/tinywallet/pkg/psbt"
	"git.smesh.lol/tinywallet/pkg/store"
	"git.smesh.lol/tinywallet/pkg/tx"
	"git.smesh.lol/tinywallet/pkg/utxo"
	"git.smesh.lol/tinywallet/pkg/wallet"
	swapchain "github.com/gjbrr/blake-swap/pkg/chain"
)

const (
	phrase     = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	passphrase = "rigproof"
)

// node is one regtest node: where it listens and how to talk to it.
type node struct {
	chainName string
	p2p       string
	rpc       string
	cli       string
	datadir   string
}

var nodes = map[string]node{
	"btc": {chainName: "btc", p2p: "127.0.0.1:18444", rpc: "http://127.0.0.1:18443/",
		cli: "/home/mleku/s/bitcoin-core/bin/bitcoin-cli", datadir: "/home/mleku/.blake-swap-test/btc"},
	"blake": {chainName: "blake", p2p: "127.0.0.1:19444", rpc: "http://127.0.0.1:19443/",
		cli: "/home/mleku/s/bitcoin-blake2b/bin/bitcoin-cli", datadir: "/home/mleku/.blake-swap-test/blake"},
}

// rpcNode is the node handle the adapter's chain reads use. The cookie is the rig's own auth.
func rpcNode(t *testing.T, name string) *swapchain.Node {
	t.Helper()

	n := nodes[name]

	cookie, err := os.ReadFile(n.datadir + "/regtest/.cookie")

	if err != nil {
		t.Fatalf("%s cookie: %v", name, err)
	}

	user, pass, ok := strings.Cut(strings.TrimSpace(string(cookie)), ":")

	if !ok {
		t.Fatalf("%s cookie is not user:pass", name)
	}

	node, err := swapchain.New(swapchain.Chain(name), swapchain.Config{
		URL: n.rpc, User: user, Password: pass,
	})

	if err != nil {
		t.Fatal(err)
	}

	return node
}

// seam adapts a light client to the wallet's chain interface.
type seam struct{ client *neutrino.Client }

func (s seam) Position() (chaindata.Position, error) {
	tip, _, ok := s.client.Store().HeaderTip()

	if !ok {
		return chaindata.Position{}, nil
	}

	filters, _, ok := s.client.Store().FilterHeaderTip()

	if !ok {
		filters = tip
	}

	return chaindata.Position{Tip: tip, Filters: filters}, nil
}

func (s seam) Sync(from uint32) (chaindata.Position, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	if _, err := s.client.SyncFrom(ctx, from, 0); err != nil {
		return chaindata.Position{}, err
	}

	return s.Position()
}

func (s seam) Match(scripts [][]byte) ([]uint32, error) {
	position, err := s.Position()

	if err != nil {
		return nil, err
	}

	return s.client.MatchScripts(0, position.Filters, scripts)
}

func (s seam) Block(height uint32) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	return s.client.Block(ctx, height)
}

func (s seam) Broadcast(raw []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	return s.client.Broadcast(ctx, raw)
}

// memPersistence is the store's durable side, in memory: this test is about the swap and the
// wallet, not about IndexedDB.
type memPersistence struct {
	mu    sync.Mutex
	items map[string][]byte
}

func (m *memPersistence) Load() (map[string][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string][]byte, len(m.items))

	for key, value := range m.items {
		out[key] = append([]byte{}, value...)
	}

	return out, nil
}

func (m *memPersistence) Write(batch []store.Op) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, op := range batch {
		if op.Delete {
			delete(m.items, op.Key)

			continue
		}

		m.items[op.Key] = append([]byte{}, op.Value...)
	}

	return nil
}

// walletRig is a wallet driving both rig chains.
type walletRig struct {
	dom     *domain.Domain
	clients map[string]*neutrino.Client
}

func (r *walletRig) Close() {
	for _, client := range r.clients {
		client.Close()
	}
}

// rpc runs the regtest client for one chain.
func rpc(t *testing.T, name string, args ...string) string {
	t.Helper()

	n := nodes[name]
	full := append([]string{"-datadir=" + n.datadir, "-rpcwallet=rig"}, args...)
	out, err := exec.Command(n.cli, full...).CombinedOutput()

	if err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}

	return strings.TrimSpace(string(out))
}

// newRig opens both chains and restores the wallet.
func newRig(t *testing.T) *walletRig {
	t.Helper()

	cache, err := store.OpenManual(&memPersistence{items: map[string][]byte{}})

	if err != nil {
		t.Fatal(err)
	}

	dom, err := domain.New(cache)

	if err != nil {
		t.Fatal(err)
	}

	rig := &walletRig{dom: dom, clients: map[string]*neutrino.Client{}}

	t.Cleanup(func() {
		rig.Close()
		_ = cache.Close()
	})

	for _, name := range []string{"btc", "blake"} {
		n := nodes[name]
		profile, err := chaincfg.ByName("regtest")

		if err != nil {
			t.Fatal(err)
		}

		client, err := neutrino.New(neutrino.Config{
			Profile: profile,
			Store:   neutrino.NewMemory(),
			Address: n.p2p,
		})

		if err != nil {
			t.Fatal(err)
		}

		rig.clients[name] = client

		walletProfile, err := wallet.ProfileFor(name, "regtest")

		if err != nil {
			t.Fatal(err)
		}

		if err := dom.Add(name, domain.Link{Profile: walletProfile, Client: seam{client: client}}); err != nil {
			t.Fatal(err)
		}

		if _, err := dom.Restore(name, phrase, passphrase); err != nil {
			t.Fatal(err)
		}

		if _, err := dom.Sync(name, 0); err != nil {
			t.Fatalf("%s sync: %v", name, err)
		}
	}

	return rig
}

// TestFundAndSettleAnHTLC: for each chain, the adapter's SendToAddress funds a contract from
// the wallet's own coins, the contract becomes a coin in the wallet, and the wallet claims it
// with its own key and the preimage — no node wallet anywhere.
func TestFundAndSettleAnHTLC(t *testing.T) {
	if os.Getenv("NOSTRINO_RIG") != "1" {
		t.Skip("NOSTRINO_RIG is not set, and this test needs the regtest rig")
	}

	rig := newRig(t)
	ctx := context.Background()
	direct := NewDirect(rig.dom)

	preimage := []byte("the preimage of the rig contract")
	sum := sha256.Sum256(preimage)

	refundKey, _ := btcec.PrivKeyFromBytes([]byte{31: 0x42})

	for _, name := range []string{"btc", "blake"} {
		ensureCoins(t, rig, name)

		addresses, err := rig.dom.Addresses(name, false, 0, 1)

		if err != nil {
			t.Fatal(err)
		}

		status, err := rig.dom.Status(name)

		if err != nil {
			t.Fatal(err)
		}

		locktime := status.Position.Tip + 200

		terms := HTLCTerms{
			Claimer:    addresses[0].Pubkey,
			Refunder:   refundKey.PubKey().SerializeCompressed(),
			SecretHash: sum,
			Locktime:   locktime,
		}

		contract, err := direct.Derive(ctx, name, terms)

		if err != nil {
			t.Fatalf("%s derive: %v", name, err)
		}

		// Watch the contract's output script before funding: a scanned height is not refetched.
		if err := direct.Watch(ctx, name, p2wsh(contract.Script)); err != nil {
			t.Fatalf("%s watch: %v", name, err)
		}

		adapter := New(&swapchain.Node{Chain: swapchain.Chain(name)}, direct)

		const funding = 60_000

		fundTx, err := adapter.Bind(name).SendToAddress(ctx, contract.Address, funding, nil)

		if err != nil {
			t.Fatalf("%s fund: %v", name, err)
		}

		rpc(t, name, "generatetoaddress", "1", rpc(t, name, "getnewaddress"))

		if _, err := rig.dom.Sync(name, 0); err != nil {
			t.Fatalf("%s sync after funding: %v", name, err)
		}

		coin := contractCoin(t, rig.dom, name, p2wsh(contract.Script))

		if coin.Value != funding {
			t.Fatalf("%s: contract coin is %d sat", name, coin.Value)
		}

		say(t, "%s: funded %s with %s, coin at height %d", name, contract.Address, fundTx, coin.Height)

		// Claim with the wallet's own key: the contract names it, and the preimage is in the
		// PSBT. The signed PSBT carries a final witness, which is applied before broadcast.
		payout, err := rig.dom.Addresses(name, false, 1, 1)

		if err != nil {
			t.Fatal(err)
		}

		claim := tx.New()
		claim.AddInput(tx.Outpoint{Hash: coin.TxID, Index: coin.Vout}, 0xfffffffe)
		claim.AddOutput(coin.Value-2_000, payout[0].Script())

		unsigned, err := psbt.Write(claim, []psbt.SegwitInput{{
			Amount:        coin.Value,
			ScriptPubKey:  p2wsh(contract.Script),
			WitnessScript: contract.Script,
			Preimage:      preimage,
			SecretHash:    sum,
		}})

		if err != nil {
			t.Fatalf("%s: build claim psbt: %v", name, err)
		}

		signed, count, err := rig.dom.SignPSBT(name, unsigned, psbt.Policy{MaxSpend: coin.Value, MaxFee: 2_000})

		if err != nil {
			t.Fatalf("%s: sign claim: %v", name, err)
		}

		if count != 1 {
			t.Fatalf("%s: signed %d claims", name, count)
		}

		witness, err := finalWitness(signed)

		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		claim.Inputs[0].Witness = witness

		raw, err := claim.SerializeWitness()

		if err != nil {
			t.Fatalf("%s: serialise claim: %v", name, err)
		}

		if err := (seam{client: rig.clients[name]}).Broadcast(raw); err != nil {
			t.Fatalf("%s: broadcast claim: %v", name, err)
		}

		claimTxID, err := claim.TxID()

		if err != nil {
			t.Fatal(err)
		}

		// The node agrees the contract output is spent, and the wallet sees the payout.
		spent := rpc(t, name, "gettxout", displayTxID(coin.TxID), fmt.Sprintf("%d", coin.Vout))

		if strings.TrimSpace(spent) != "" {
			t.Fatalf("%s: the contract outpoint is still unspent: %s", name, spent)
		}

		rpc(t, name, "generatetoaddress", "1", rpc(t, name, "getnewaddress"))

		syncChain(t, rig, name)

		paid := contractCoin(t, rig.dom, name, payout[0].Script())

		if paid.Value != coin.Value-2_000 {
			t.Fatalf("%s: the payout is %d sat", name, paid.Value)
		}

		say(t, "%s: claimed %s to %s, payout coin at height %d", name, displayTxID(claimTxID), payout[0].Text, paid.Height)
	}
}

// ensureCoins gives the wallet a coin it can really spend: a fresh in-window address, funded
// from the rig wallet and confirmed. A restored wallet also lists coins from earlier runs, some
// already spent on-chain; the wallet has no spend reconciliation yet, so this test funds its own
// and spends the newest coin, which is this one.
func ensureCoins(t *testing.T, rig *walletRig, name string) {
	t.Helper()

	// Inside the scan window (0..99): a coin outside it is never seen.
	index := uint32(time.Now().UnixNano() % 90)

	addresses, err := rig.dom.Addresses(name, false, index, 1)

	if err != nil {
		t.Fatal(err)
	}

	rpc(t, name, "sendtoaddress", addresses[0].Text, "0.002")
	rpc(t, name, "generatetoaddress", "1", rpc(t, name, "getnewaddress"))
	syncChain(t, rig, name)

	coins, err := rig.dom.UnspentOn(name)

	if err != nil {
		t.Fatal(err)
	}

	for _, coin := range coins {
		if string(coin.Script) == string(addresses[0].Script()) {
			return
		}
	}

	t.Fatalf("%s: the fresh coin at index %d was not seen (%d coins)", name, index, len(coins))
}

// waitForMempool waits until the node knows a transaction before a block is mined. A P2P
// broadcast is fire-and-forget, so mining immediately can produce a block that does not
// include it.
func waitForMempool(t *testing.T, name, txid string) {
	t.Helper()

	n := nodes[name]

	for attempt := 0; attempt < 30; attempt++ {
		cmd := exec.Command(n.cli, "-datadir="+n.datadir, "-rpcwallet=rig", "getmempoolentry", txid)

		if err := cmd.Run(); err == nil {
			return
		}

		time.Sleep(200 * time.Millisecond)
	}

	t.Fatalf("%s: %s never reached the mempool", name, txid)
}

// syncChain advances the wallet, retrying a dropped rig connection a couple of times. A node
// closing an idle socket between two test steps is not what these tests are about.
func syncChain(t *testing.T, rig *walletRig, name string) {
	t.Helper()

	var err error

	for attempt := 0; attempt < 3; attempt++ {
		if _, err = rig.dom.Sync(name, 0); err == nil {
			return
		}

		time.Sleep(2 * time.Second)
	}

	t.Fatalf("%s sync: %v", name, err)
}

// contractCoin returns the wallet's coin paying script, failing if there is none.
func contractCoin(t *testing.T, dom interface {
	UnspentOn(string) ([]utxo.Coin, error)
}, name string, script []byte) utxo.Coin {
	t.Helper()

	coins, err := dom.UnspentOn(name)

	if err != nil {
		t.Fatal(err)
	}

	for _, coin := range coins {
		if string(coin.Script) == string(script) {
			return coin
		}
	}

	t.Fatalf("%s: no coin pays %x (%d held)", name, script, len(coins))

	return utxo.Coin{}
}

// p2wsh is the output script a witness script pays to.
func p2wsh(witness []byte) []byte {
	sum := sha256.Sum256(witness)

	out := make([]byte, 0, 34)
	out = append(out, 0x00, 0x20)

	return append(out, sum[:]...)
}

// finalWitness reads BIP174's final scriptWitness record out of a signed PSBT.
func finalWitness(raw []byte) ([][]byte, error) {
	for at := 0; at+2 < len(raw); at++ {
		if raw[at] != 0x01 || raw[at+1] != 0x08 {
			continue
		}

		length, next := int(raw[at+2]), at+3

		if next+length > len(raw) || length < 1 {
			return nil, fmt.Errorf("a short final witness record")
		}

		value := raw[next : next+length]
		count := int(value[0])

		if count == 0 || count > 4 {
			return nil, fmt.Errorf("a %d-item witness", count)
		}

		items := make([][]byte, 0, count)
		offset := 1

		for i := 0; i < count; i++ {
			if offset >= len(value) {
				return nil, fmt.Errorf("a witness that ends inside an item")
			}

			size := int(value[offset])
			offset++

			if offset+size > len(value) {
				return nil, fmt.Errorf("a witness item that runs past the record")
			}

			items = append(items, append([]byte(nil), value[offset:offset+size]...))
			offset += size
		}

		return items, nil
	}

	return nil, fmt.Errorf("no final witness record")
}

// say logs in the test's own words.
func say(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Logf(format, args...)
}

// TestFundThroughTheEngine runs fund.Fund itself against the local wallet: the contract comes
// from the wallet's own derivation (no node descriptor), the payment from its own coins, and
// the receipt is checked against the node.
func TestFundThroughTheEngine(t *testing.T) {
	if os.Getenv("NOSTRINO_RIG") != "1" {
		t.Skip("NOSTRINO_RIG is not set, and this test needs the regtest rig")
	}

	rig := newRig(t)
	ctx := context.Background()
	direct := NewDirect(rig.dom)

	preimage := []byte("the engine's funding preimage")
	sum := sha256.Sum256(preimage)

	rate, err := decimal.Parse("2")

	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"btc", "blake"} {
		ensureCoins(t, rig, name)

		addresses, err := rig.dom.Addresses(name, false, 0, 1)

		if err != nil {
			t.Fatal(err)
		}

		pub := hex.EncodeToString(addresses[0].Pubkey)

		status, err := rig.dom.Status(name)

		if err != nil {
			t.Fatal(err)
		}

		timeout := int64(status.Position.Tip) + 200

		ptr := func(value string) *string { return &value }

		swap := &model.Swap{
			SwapID:               "rigfund" + name,
			BlakeAmountBaseUnits: 20_000,
			BTCSats:              25_000,
			SecretHex:            ptr(hex.EncodeToString(preimage)),
			SecretHash:           hex.EncodeToString(sum[:]),
			BlakeTimeout:         timeout,
			BTCTimeout:           timeout,
			BlakeAlicePub:        ptr(pub),
			BlakeBobPub:          ptr(pub),
			BTCAlicePub:          ptr(pub),
			BTCBobPub:            ptr(pub),
		}

		ch := swapchain.Chain(name)

		adapter := New(rpcNode(t, name), direct)

		terms := contract.Terms{
			Claimer:    addresses[0].Pubkey,
			Refunder:   addresses[0].Pubkey,
			SecretHash: sum,
			Locktime:   uint32(timeout),
		}

		derived, err := adapter.DeriveHTLC(ctx, ch, terms)

		if err != nil {
			t.Fatalf("%s derive: %v", name, err)
		}

		if err := direct.Watch(ctx, name, p2wsh(derived.Script)); err != nil {
			t.Fatalf("%s watch: %v", name, err)
		}

		receipt, err := fund.Fund(ctx, adapter, swap, ch, fund.Options{
			Wallet:        name,
			SkipWatchOnly: true,
			FeeRate:       &rate,
			Intents:       fund.NewMemStore(),
		})

		if err != nil {
			t.Fatalf("%s fund.Fund: %v", name, err)
		}

		if receipt.Address != derived.Address {
			t.Fatalf("%s: funded %s, the contract is %s", name, receipt.Address, derived.Address)
		}

		want := swap.BTCSats

		if ch == swapchain.Blake {
			want = swap.BlakeAmountBaseUnits
		}

		if receipt.Amount != want {
			t.Fatalf("%s: amount %d, want %d", name, receipt.Amount, want)
		}

		sent, err := rig.dom.Sends(name)

		if err != nil {
			t.Fatal(err)
		}

		if len(sent) == 0 || sent[len(sent)-1].TxID != receipt.TxID {
			t.Fatalf("%s: the receipt's txid %s is not the wallet's send", name, receipt.TxID)
		}

		rpc(t, name, "generatetoaddress", "1", rpc(t, name, "getnewaddress"))

		syncChain(t, rig, name)

		coin := contractCoin(t, rig.dom, name, p2wsh(derived.Script))

		if coin.Value != uint64(want) {
			t.Fatalf("%s: the contract coin is %d, want %d", name, coin.Value, want)
		}

		say(t, "%s: fund.Fund paid %s (%d) in %s, coin at height %d",
			name, receipt.Address, receipt.Amount, receipt.TxID, coin.Height)
	}
}
