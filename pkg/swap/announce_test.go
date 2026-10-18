package swap

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/contract"
	"github.com/gjbrr/blake-swap/pkg/coord"
	"github.com/gjbrr/blake-swap/pkg/fund"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/state"
	"github.com/gjbrr/blake-swap/pkg/store"
)

// recordingTransport is a coordination route that remembers what it carried.
type recordingTransport struct {
	sent     []sentMessage
	incoming map[string]*coord.Message
}

type sentMessage struct {
	swapID string
	kind   string
	body   map[string]any
	opts   coord.SendOptions
}

func newRecordingTransport() *recordingTransport {
	return &recordingTransport{incoming: map[string]*coord.Message{}}
}

func (r *recordingTransport) Send(
	_ context.Context, swapID, kind string, body map[string]any, opts coord.SendOptions,
) (string, error) {
	r.sent = append(r.sent, sentMessage{swapID: swapID, kind: kind, body: body, opts: opts})

	return "sent/" + kind, nil
}

func (r *recordingTransport) Receive(
	_ context.Context, swapID, kind string, opts coord.ReceiveOptions,
) (*coord.Message, error) {
	message, ok := r.incoming[kind]

	if !ok {
		return nil, coordNoMessage(kind)
	}

	if opts.ExpectedPubkey != "" && message.Sender != opts.ExpectedPubkey {
		return nil, coordWrongSender(kind)
	}

	return message, nil
}

func coordNoMessage(kind string) error { return &missingMessage{kind} }

type missingMessage struct{ kind string }

func (m *missingMessage) Error() string { return "no " + m.kind + " message" }

func coordWrongSender(kind string) error { return &wrongSender{kind} }

type wrongSender struct{ kind string }

func (w *wrongSender) Error() string {
	return "SECURITY FAILURE: " + w.kind + " was signed by the wrong Nostr identity"
}

// fundContract puts a confirmed HTLC output on a fake chain.
func fundContract(t *testing.T, node *mempoolNode, swap *model.Swap, ch chain.Chain) (string, int64) {
	t.Helper()

	ctx := context.Background()

	derived, err := contract.Derive(ctx, node, swap, ch)

	if err != nil {
		t.Fatal(err)
	}

	txid := strings.Repeat("11", 32)
	vout := int64(0)

	if ch == chain.BTC {
		txid = strings.Repeat("12", 32)
	}

	blockHash := fmtHash(node.height)

	tx := &chain.Tx{
		TxID:          txid,
		BlockHash:     blockHash,
		Confirmations: 1,
		Vout:          []chain.Vout{{N: vout, Value: amount.Coin(amount.Sats(derived.Amount).Coin().String())}},
	}
	tx.Vout[0].ScriptPubKey.Address = derived.Address

	node.txs[txid] = tx
	node.blocks[blockHash] = &chain.VerboseBlock{Hash: blockHash, Height: node.height, Tx: []chain.Tx{*tx}}

	out := &chain.TxOut{Confirmations: 1}
	out.Value = tx.Vout[0].Value
	out.ScriptPubKey.Address = derived.Address
	node.utxos[fmt.Sprintf("%s:%d", txid, vout)] = out

	return txid, vout
}

func fmtHash(height int64) string {
	return fmt.Sprintf("%064x", height)
}

// TestAnnounceBlakeFundingCarriesTheContractButNotTheSecret: Bob needs the
// outpoint and the terms; the preimage is what he must *not* receive.
func TestAnnounceBlakeFundingCarriesTheContractButNotTheSecret(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	transport := newRecordingTransport()

	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = aliceIdentity
	engine.Transport = transport

	record := pinnedSwap(t, st, "aaaa00000301")

	txid := strings.Repeat("21", 32)
	vout := int32(1)
	blockHash := strings.Repeat("22", 32)

	record.State = state.BlakeFunded
	record.BlakeFundingTxID = &txid
	record.BlakeFundingVout = &vout
	record.BlakeFundingBlockHash = &blockHash

	if err := st.Save(ctx, store.Alice, record); err != nil {
		t.Fatal(err)
	}

	result, err := engine.AnnounceBlakeFunding(ctx, record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if result.Location == "" {
		t.Fatal("no location was reported")
	}

	if len(transport.sent) != 1 {
		t.Fatalf("sent %d messages", len(transport.sent))
	}

	sent := transport.sent[0]

	if sent.kind != "blake-funded" {
		t.Fatalf("kind = %s", sent.kind)
	}

	if _, present := sent.body["secret_hex"]; present {
		t.Fatal("the announcement carries the secret")
	}

	if sent.body["secret_hash"] != record.SecretHash {
		t.Fatalf("secret_hash = %v", sent.body["secret_hash"])
	}

	if sent.body["blake_funding_txid"] != txid {
		t.Fatalf("txid = %v", sent.body["blake_funding_txid"])
	}

	if sent.body["blake_amount"] != amount.Blake(record.BlakeAmountBaseUnits).Format() {
		t.Fatalf("amount = %v", sent.body["blake_amount"])
	}

	if sent.opts.PeerPubkey != string(bobIdentity) {
		t.Fatalf("peer = %q", sent.opts.PeerPubkey)
	}
}

// TestReceiveBlakeFundingVerifiesTheChain: the announcement proposes an
// outpoint; the chain decides whether it is the contract.
func TestReceiveBlakeFundingVerifiesTheChain(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	transport := newRecordingTransport()

	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = bobIdentity
	engine.Transport = transport

	record := pinnedSwap(t, st, "aaaa00000302")

	txid, vout := fundContract(t, node, record, chain.Blake)

	transport.incoming["blake-funded"] = &coord.Message{
		Kind:   "blake-funded",
		Sender: string(aliceIdentity),
		Body: map[string]any{
			"type":               "blake-funded",
			"swap_id":            record.SwapID,
			"secret_hash":        record.SecretHash,
			"blake_amount":       amount.Blake(record.BlakeAmountBaseUnits).Format(),
			"blake_timeout":      record.BlakeTimeout,
			"blake_funding_txid": txid,
			"blake_funding_vout": float64(vout),
		},
		Location: "event-1",
	}

	result, err := engine.ReceiveBlakeFunding(ctx, record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if result.State != state.BlakeFunded {
		t.Fatalf("state = %s", result.State)
	}

	persisted, _ := st.Load(ctx, record.SwapID)

	if persisted.BlakeFundingTxID == nil || *persisted.BlakeFundingTxID != txid {
		t.Fatal("the funding outpoint was not recorded")
	}

	if persisted.BlakeFundingBlockHash == nil || *persisted.BlakeFundingBlockHash != fmtHash(node.height) {
		t.Fatalf("block hash = %v", persisted.BlakeFundingBlockHash)
	}

	// Bob still does not know the secret.
	if persisted.RevealedSecretHex != nil {
		t.Fatal("Bob learned the secret from a funding announcement")
	}
}

// TestReceiveRefusesAnAnnouncementThatIsNotTheContract: an authenticated message
// from the right peer is still not authority over the chain.
func TestReceiveRefusesAnAnnouncementThatIsNotTheContract(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	transport := newRecordingTransport()

	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = bobIdentity
	engine.Transport = transport

	record := pinnedSwap(t, st, "aaaa00000303")

	txid, vout := fundContract(t, node, record, chain.Blake)

	// The announcement claims a different amount than the swap.
	transport.incoming["blake-funded"] = &coord.Message{
		Kind:   "blake-funded",
		Sender: string(aliceIdentity),
		Body: map[string]any{
			"type":               "blake-funded",
			"swap_id":            record.SwapID,
			"secret_hash":        record.SecretHash,
			"blake_amount":       "9.00000000",
			"blake_timeout":      record.BlakeTimeout,
			"blake_funding_txid": txid,
			"blake_funding_vout": float64(vout),
		},
	}

	_, err := engine.ReceiveBlakeFunding(ctx, record.SwapID)

	if err == nil || !strings.Contains(err.Error(), "BLAKE amount mismatch") {
		t.Fatalf("err = %v", err)
	}

	persisted, _ := st.Load(ctx, record.SwapID)

	if persisted.State != state.Offered || persisted.BlakeFundingTxID != nil {
		t.Fatal("a refused announcement changed the record")
	}
}

// TestDuplicateAnnouncementIsANoOp: relays, reconnects and retries deliver the
// same authenticated message more than once, and that must be harmless.
func TestDuplicateAnnouncementIsANoOp(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	transport := newRecordingTransport()

	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = bobIdentity
	engine.Transport = transport

	record := pinnedSwap(t, st, "aaaa00000304")

	txid, vout := fundContract(t, node, record, chain.Blake)
	voutIndex := int32(vout)

	record.State = state.BlakeFunded
	record.BlakeFundingTxID = &txid
	record.BlakeFundingVout = &voutIndex

	if err := st.Save(ctx, store.Alice, record); err != nil {
		t.Fatal(err)
	}

	transport.incoming["blake-funded"] = &coord.Message{
		Kind:   "blake-funded",
		Sender: string(aliceIdentity),
		Body: map[string]any{
			"type":               "blake-funded",
			"swap_id":            record.SwapID,
			"secret_hash":        record.SecretHash,
			"blake_amount":       amount.Blake(record.BlakeAmountBaseUnits).Format(),
			"blake_timeout":      record.BlakeTimeout,
			"blake_funding_txid": txid,
			"blake_funding_vout": float64(vout),
		},
	}

	result, err := engine.ReceiveBlakeFunding(ctx, record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if !result.Duplicate {
		t.Fatal("the duplicate was not reported as one")
	}

	// A different outpoint for the same swap cannot be resolved by any rule.
	other := strings.Repeat("33", 32)
	transport.incoming["blake-funded"].Body["blake_funding_txid"] = other

	if _, err := engine.ReceiveBlakeFunding(ctx, record.SwapID); err == nil ||
		!strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("err = %v", err)
	}
}

// TestReceiveBTCFundingTransitionsToBTCFunded covers Alice's side of the second
// announcement.
func TestReceiveBTCFundingTransitionsToBTCFunded(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	transport := newRecordingTransport()

	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = aliceIdentity
	engine.Transport = transport

	record := pinnedSwap(t, st, "aaaa00000305")
	record.State = state.BlakeFunded

	if err := st.Save(ctx, store.Alice, record); err != nil {
		t.Fatal(err)
	}

	txid, vout := fundContract(t, node, record, chain.BTC)

	transport.incoming["btc-funded"] = &coord.Message{
		Kind:   "btc-funded",
		Sender: string(bobIdentity),
		Body: map[string]any{
			"type":             "btc-funded",
			"swap_id":          record.SwapID,
			"secret_hash":      record.SecretHash,
			"btc_sats":         record.BTCSats,
			"btc_timeout":      record.BTCTimeout,
			"btc_funding_txid": txid,
			"btc_funding_vout": float64(vout),
		},
		Location: "event-2",
	}

	result, err := engine.ReceiveBTCFunding(ctx, record.SwapID)

	if err != nil {
		t.Fatal(err)
	}

	if result.State != state.BTCFunded {
		t.Fatalf("state = %s", result.State)
	}

	persisted, _ := st.Load(ctx, record.SwapID)

	if persisted.BTCFundingBlockHash == nil {
		t.Fatal("the BTC funding block was not anchored")
	}
}

// TestReceiveRefusesWhenTheChainDisagreesWithTheTerms is the case that matters
// most: the announcement is internally consistent and authenticated, and the chain
// output is still not this swap's contract.
func TestReceiveRefusesWhenTheChainDisagreesWithTheTerms(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	transport := newRecordingTransport()

	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = bobIdentity
	engine.Transport = transport

	record := pinnedSwap(t, st, "aaaa00000306")

	txid, vout := fundContract(t, node, record, chain.Blake)

	// The output pays a different amount than the swap's terms, while the
	// announcement repeats the terms correctly.
	out := node.utxos[fmt.Sprintf("%s:%d", txid, vout)]
	out.Value = amount.Coin("0.02000000")

	node.txs[txid].Vout[0].Value = out.Value

	transport.incoming["blake-funded"] = &coord.Message{
		Kind:   "blake-funded",
		Sender: string(aliceIdentity),
		Body: map[string]any{
			"type":               "blake-funded",
			"swap_id":            record.SwapID,
			"secret_hash":        record.SecretHash,
			"blake_amount":       amount.Blake(record.BlakeAmountBaseUnits).Format(),
			"blake_timeout":      record.BlakeTimeout,
			"blake_funding_txid": txid,
			"blake_funding_vout": float64(vout),
		},
	}

	_, err := engine.ReceiveBlakeFunding(ctx, record.SwapID)

	if err == nil || !strings.Contains(err.Error(), "Wrong BLAKE amount") {
		t.Fatalf("err = %v", err)
	}
}

// TestReceiveRefusesABlockThatDoesNotHoldTheTransaction: the anchor is derived
// from confirmations, and the block it points at must contain the announced
// transaction.
func TestReceiveRefusesABlockThatDoesNotHoldTheTransaction(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	node := newMempoolNode()
	transport := newRecordingTransport()

	engine := NewEngine(st, fund.NewMemStore(), node, node)
	engine.Identity = bobIdentity
	engine.Transport = transport

	record := pinnedSwap(t, st, "aaaa00000307")

	txid, vout := fundContract(t, node, record, chain.Blake)

	// The block the confirmations imply does not list the transaction.
	delete(node.blocks, fmtHash(node.height))

	transport.incoming["blake-funded"] = &coord.Message{
		Kind:   "blake-funded",
		Sender: string(aliceIdentity),
		Body: map[string]any{
			"type":               "blake-funded",
			"swap_id":            record.SwapID,
			"secret_hash":        record.SecretHash,
			"blake_amount":       amount.Blake(record.BlakeAmountBaseUnits).Format(),
			"blake_timeout":      record.BlakeTimeout,
			"blake_funding_txid": txid,
			"blake_funding_vout": float64(vout),
		},
	}

	_, err := engine.ReceiveBlakeFunding(ctx, record.SwapID)

	if err == nil || !strings.Contains(err.Error(), "does not contain") {
		t.Fatalf("err = %v", err)
	}
}
