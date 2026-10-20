package settle

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/chain"
	"github.com/gjbrr/blake-swap/pkg/decimal"
	"github.com/gjbrr/blake-swap/pkg/psbt"
)

// minimalPSBT is an empty but structurally valid PSBT: the magic, a global map
// terminator, an input map terminator and an output map terminator. The claim
// path injects a preimage into input 0, which only a real PSBT can accept, so
// the fake node hands out this value rather than an opaque string.
func minimalPSBT() string {
	return base64.StdEncoding.EncodeToString(
		[]byte{0x70, 0x73, 0x62, 0x74, 0xff, 0x00, 0x00, 0x00},
	)
}

// fakeNode is a node that builds transactions whose vsize is a function of the
// fee, which is the whole difficulty of settlement.
type fakeNode struct {
	inputSats int64
	vsizeFor  func(feeSats int64) int64

	feeRate  string
	lockTime int64

	currentFee int64

	// lastFinalizeInput is the PSBT handed to finalizepsbt, so a test can prove
	// what was (and was not) attached to it.
	lastFinalizeInput string
	sent              []string
	finalizeComplete  bool
}

func newFakeNode(inputSats int64) *fakeNode {
	return &fakeNode{
		inputSats:        inputSats,
		vsizeFor:         func(int64) int64 { return 131 },
		feeRate:          "0.00010000", // 10 sat/vB
		finalizeComplete: true,
	}
}

func (f *fakeNode) Bind(wallet string) chain.Wallet { return &fakeWallet{name: wallet, node: f} }

func (f *fakeNode) CreateRawTransaction(
	_ context.Context, inputs []chain.Outpoint, outputs map[string]decimal.Dec,
) (string, error) {
	if len(inputs) != 1 || len(outputs) != 1 {
		return "", fmt.Errorf("settlement is single-input, single-output")
	}

	for _, value := range outputs {
		sats, err := amount.ParseSats(value.String())

		if err != nil {
			return "", err
		}

		f.currentFee = f.inputSats - int64(sats)
	}

	return fmt.Sprintf("raw:%d", f.currentFee), nil
}

func (f *fakeNode) CreateRawTransactionAt(
	ctx context.Context, inputs []chain.Outpoint, outputs map[string]decimal.Dec, lockTime int64,
) (string, error) {
	f.lockTime = lockTime

	return f.CreateRawTransaction(ctx, inputs, outputs)
}

func (f *fakeNode) ConvertToPSBT(_ context.Context, hexTx string) (string, error) {
	return "psbt:" + hexTx, nil
}

func (f *fakeNode) UtxoUpdatePSBT(_ context.Context, psbtHex string, descriptors []string) (string, error) {
	if len(descriptors) != 1 || descriptors[0] == "" {
		return "", fmt.Errorf("utxoupdatepsbt needs the HTLC descriptor")
	}

	return "upd:" + psbtHex, nil
}

func (f *fakeNode) FinalizePSBT(_ context.Context, psbtHex string) (*chain.FinalizeResult, error) {
	f.lastFinalizeInput = psbtHex

	if !f.finalizeComplete {
		return &chain.FinalizeResult{Complete: false}, nil
	}

	return &chain.FinalizeResult{
		Complete: true,
		Hex:      fmt.Sprintf("fin:%d", f.currentFee),
	}, nil
}

func (f *fakeNode) SendRaw(_ context.Context, hexTx string) (string, error) {
	f.sent = append(f.sent, hexTx)

	return "settlementtxid", nil
}

func (f *fakeNode) DecodeRaw(_ context.Context, hexTx string) (*chain.Tx, error) {
	var fee int64

	if _, err := fmt.Sscanf(strings.TrimPrefix(hexTx, "fin:"), "%d", &fee); err != nil {
		// The vsize of a provisional transaction is measured the same way.
		if _, err := fmt.Sscanf(strings.TrimPrefix(hexTx, "raw:"), "%d", &fee); err != nil {
			return nil, fmt.Errorf("cannot decode %q", hexTx)
		}
	}

	return &chain.Tx{
		VSize: f.vsizeFor(fee),
		Vout: []chain.Vout{{
			N:     0,
			Value: amount.Coin(amount.Sats(f.inputSats - fee).Coin().String()),
		}},
	}, nil
}

func (f *fakeNode) EstimateSmartFee(context.Context, int64) (*chain.FeeEstimate, error) {
	return &chain.FeeEstimate{Feerate: json.Number(f.feeRate)}, nil
}

func (f *fakeNode) MempoolInfo(context.Context) (*chain.MempoolInfo, error) {
	info := &chain.MempoolInfo{}
	info.MinRelayTxFee = json.Number("0.00001000")
	info.MempoolMinFee = json.Number("0.00001000")

	return info, nil
}

// fakeWallet signs nothing and says so: the interesting part is which PSBT it
// was asked to sign.
type fakeWallet struct {
	name string
	node *fakeNode
}

func (w *fakeWallet) Name() string { return w.name }

func (w *fakeWallet) ProcessPSBT(context.Context, string, string) (*chain.ProcessPSBTResult, error) {
	return &chain.ProcessPSBTResult{PSBT: minimalPSBT(), Complete: false}, nil
}

// The rest of the wallet surface is unused by settlement.
func (w *fakeWallet) Balances(context.Context) (*chain.Balances, error) {
	return chain.BalancesFromTrusted("0"), nil
}

func (w *fakeWallet) Tx(context.Context, string) (*chain.WalletTx, error) {
	return nil, fmt.Errorf("unused")
}

func (w *fakeWallet) ListTransactions(context.Context, int64, int64) ([]chain.WalletEntry, error) {
	return nil, nil
}

func (w *fakeWallet) AddressInfo(context.Context, string) (*chain.AddressInfo, error) {
	return nil, fmt.Errorf("unused")
}

func (w *fakeWallet) ImportDescriptor(context.Context, string, chain.ImportWhen) (*chain.ImportResult, error) {
	return nil, fmt.Errorf("unused")
}

func (w *fakeWallet) SendToAddress(context.Context, string, amount.Sats, *decimal.Dec) (string, error) {
	return "", fmt.Errorf("unused")
}

func (w *fakeWallet) NewAddress(context.Context) (string, error) { return "", fmt.Errorf("unused") }

func (w *fakeWallet) Decode(context.Context, string) (*chain.Tx, error) {
	return nil, fmt.Errorf("unused")
}

// TestBuildConvergesAcrossADustCycle is the case the Python builder was fixed
// for: paying more removes an output, which lowers the requirement, which puts
// it back. Equality is never reached, and demanding it turned a good settlement
// into a happy-path failure.
// Parity: tests/dynamic_settlement_builder_tests.py
func TestBuildConvergesAcrossADustCycle(t *testing.T) {
	node := newFakeNode(100_000)

	// A genuine two-cycle: 1310 sats needs 140 vB, which needs 1400 sats, which
	// no longer needs 140 vB. The fee oscillates between the two requirements
	// and never equals either, which is why the builder accepts a fee that
	// covers the requirement instead of insisting on equality.
	node.vsizeFor = func(fee int64) int64 {
		if fee < 1350 {
			return 140
		}

		return 131
	}

	plan, err := Build(context.Background(), node, chain.Blake, node.inputSats, func(fee int64) (string, error) {
		return fmt.Sprintf("fin:%d", fee), nil
	}, Options{})

	if err != nil {
		t.Fatalf("the dust two-cycle was reported as non-convergence: %v", err)
	}

	if plan.FeeSats != 1400 {
		t.Fatalf("fee = %d; the covering fee for a 131 vB settlement at 10 sat/vB is 1310, and the measured one is 1400", plan.FeeSats)
	}

	if plan.Vsize != node.vsizeFor(plan.FeeSats) {
		t.Fatalf(
			"the plan reports vsize %d but the transaction it returns measures %d",
			plan.Vsize, node.vsizeFor(plan.FeeSats),
		)
	}
}

// TestBuildRefusesPathologicalGrowth: a transaction whose vsize grows with its
// own fee can never converge, and the bound has to say so instead of looping.
func TestBuildRefusesPathologicalGrowth(t *testing.T) {
	node := newFakeNode(100_000)
	// One satoshi per vB, and each vB adds a unit of vsize: the requirement
	// always exceeds the fee that produced it, so no attempt can converge.
	node.feeRate = "0.00001000"
	node.vsizeFor = func(fee int64) int64 { return 100 + fee }

	_, err := Build(context.Background(), node, chain.Blake, node.inputSats, func(fee int64) (string, error) {
		return fmt.Sprintf("fin:%d", fee), nil
	}, Options{})

	if err == nil || !strings.Contains(err.Error(), "did not converge") {
		t.Fatalf("err = %v", err)
	}
}

// TestBuildAppliesTheFeeCap: a construction that would pay an absurd share of
// the HTLC fails closed, and the error names the cap rather than the symptom.
func TestBuildAppliesTheFeeCap(t *testing.T) {
	node := newFakeNode(1000)
	node.vsizeFor = func(int64) int64 { return 500 }

	_, err := Build(context.Background(), node, chain.Blake, node.inputSats, func(fee int64) (string, error) {
		return fmt.Sprintf("fin:%d", fee), nil
	}, Options{MaxPercent: decimal.MustParse("1")})

	if err == nil {
		t.Fatal("a settlement paying 50% of the HTLC was accepted under a 1% cap")
	}

	// The refusal should be actionable: naming the vsize is what turns "fee
	// above cap" into "this HTLC is too small for a spend this size".
	if !strings.Contains(err.Error(), "vsize") {
		t.Fatalf("the refusal does not name the transaction size: %v", err)
	}
}

// TestExactFeeOverrideIsUsed: the environment's exact-satoshis override is a
// standing default, and when nothing outranks it the fee is exactly that.
func TestExactFeeOverrideIsUsed(t *testing.T) {
	node := newFakeNode(100_000)

	fee := int64(210)

	plan, err := Build(context.Background(), node, chain.Blake, node.inputSats, func(fee int64) (string, error) {
		return fmt.Sprintf("fin:%d", fee), nil
	}, Options{FeeSats: &fee})

	if err != nil {
		t.Fatal(err)
	}

	if plan.FeeSats != 210 || plan.Source != "override" {
		t.Fatalf("plan = %+v", plan)
	}

	if plan.OutputSats != node.inputSats-210 {
		t.Fatalf("output = %d", plan.OutputSats)
	}
}

// TestExplicitRateOutranksTheExactFee: a parameter is this invocation's
// instruction. The override is not consulted, and the plan says it was passed
// over rather than ignoring it silently.
func TestExplicitRateOutranksTheExactFee(t *testing.T) {
	node := newFakeNode(100_000)
	node.vsizeFor = func(int64) int64 { return 140 }

	fee := int64(9_000)
	rate := decimal.MustParse("2")

	plan, err := Build(context.Background(), node, chain.Blake, node.inputSats, func(fee int64) (string, error) {
		return fmt.Sprintf("fin:%d", fee), nil
	}, Options{FeeSats: &fee, FeeRate: &rate})

	if err != nil {
		t.Fatal(err)
	}

	if plan.Source != "--fee-rate" || plan.FeeSats != 280 {
		t.Fatalf("plan = %+v", plan)
	}

	if !strings.Contains(plan.Note, "9000") {
		t.Fatalf("the plan did not say the override was passed over: %q", plan.Note)
	}
}

// TestExplicitRateIsHonouredExactly: 2.5 sat/vB on a 140 vB transaction is 350
// sats, and no estimator gets a vote.
func TestExplicitRateIsHonouredExactly(t *testing.T) {
	node := newFakeNode(100_000)
	node.vsizeFor = func(int64) int64 { return 140 }

	rate := decimal.MustParse("2.5")

	plan, err := Build(context.Background(), node, chain.BTC, node.inputSats, func(fee int64) (string, error) {
		return fmt.Sprintf("fin:%d", fee), nil
	}, Options{FeeRate: &rate})

	if err != nil {
		t.Fatal(err)
	}

	if plan.FeeSats != 350 {
		t.Fatalf("fee = %d, want 350", plan.FeeSats)
	}
}

// TestClaimCarriesTheRevealedPreimage is the claim's whole proof: the hashlock
// branch is only spendable with the preimage, and the engine must attach the one
// the durable record holds.
func TestClaimCarriesTheRevealedPreimage(t *testing.T) {
	node := newFakeNode(1_000_000)
	node.vsizeFor = func(int64) int64 { return 131 }

	preimage := strings.Repeat("11", 32)
	hash := sha256Hex(preimage)

	plan, err := Claim(context.Background(), node, Request{
		Chain:       chain.Blake,
		Funding:     chain.Outpoint{TxID: strings.Repeat("aa", 32), Vout: 0},
		Destination: "bcrt1qdestination",
		Descriptor:  "wsh(or_i(...))#deadbeef",
		InputSats:   node.inputSats,
		SecretHash:  hash,
		Preimage:    preimage,
		Wallet:      "bob",
	}, Options{})

	if err != nil {
		t.Fatal(err)
	}

	records, err := psbt.SHA256Records(node.lastFinalizeInput)

	if err != nil {
		t.Fatalf("the finalized PSBT is not parseable: %v", err)
	}

	if len(records) != 1 {
		t.Fatalf("the claim PSBT carries %d preimage records", len(records))
	}

	if plan.FeeSats < 1 {
		t.Fatalf("plan = %+v", plan)
	}
}

// TestClaimRefusesWithoutAPreimage: a claim without the secret is not a claim.
func TestClaimRefusesWithoutAPreimage(t *testing.T) {
	node := newFakeNode(1_000_000)

	_, err := Claim(context.Background(), node, Request{
		Chain: chain.Blake, InputSats: node.inputSats, Wallet: "bob",
	}, Options{})

	if err == nil || !strings.Contains(err.Error(), "revealed preimage") {
		t.Fatalf("err = %v", err)
	}
}

// TestRefundNeedsNoPreimage: the timelock branch is the fallback for when the
// secret never arrives, so it must not require one.
func TestRefundNeedsNoPreimage(t *testing.T) {
	node := newFakeNode(1_000_000)
	node.vsizeFor = func(int64) int64 { return 131 }

	plan, err := Refund(context.Background(), node, Request{
		Chain:       chain.BTC,
		Funding:     chain.Outpoint{TxID: strings.Repeat("bb", 32), Vout: 1},
		Destination: "bcrt1qrefund",
		Descriptor:  "wsh(or_i(...))#deadbeef",
		InputSats:   node.inputSats,
		Wallet:      "bob",
	}, Options{})

	if err != nil {
		t.Fatal(err)
	}

	records, err := psbt.SHA256Records(node.lastFinalizeInput)

	if err != nil {
		t.Fatalf("the finalized PSBT is not parseable: %v", err)
	}

	if len(records) != 0 {
		t.Fatalf("a refund PSBT carries %d preimage records", len(records))
	}

	if plan.FeeSats < 1 {
		t.Fatalf("plan = %+v", plan)
	}
}

// TestIncompleteFinalizationIsRefused: an incomplete PSBT has no broadcastable
// hex, and treating it as success would advance state on nothing.
func TestIncompleteFinalizationIsRefused(t *testing.T) {
	node := newFakeNode(1_000_000)
	node.finalizeComplete = false

	_, err := Refund(context.Background(), node, Request{
		Chain: chain.BTC, InputSats: node.inputSats, Wallet: "bob",
		Descriptor:  "wsh(or_i(...))#deadbeef",
		Destination: "bcrt1qrefund",
		Funding:     chain.Outpoint{TxID: strings.Repeat("bb", 32)},
	}, Options{})

	if err == nil || !strings.Contains(err.Error(), "did not finalize") {
		t.Fatalf("err = %v", err)
	}
}

// TestBroadcastMeasuresTheRealFee: the fee reported is the one the transaction
// actually pays, measured from the transaction rather than trusted from the
// builder that produced it.
func TestBroadcastMeasuresTheRealFee(t *testing.T) {
	node := newFakeNode(100_000)
	node.vsizeFor = func(int64) int64 { return 131 }

	plan, err := Build(context.Background(), node, chain.Blake, node.inputSats, func(fee int64) (string, error) {
		return fmt.Sprintf("fin:%d", fee), nil
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}

	txid, fee, err := Broadcast(context.Background(), node, chain.Blake, plan, node.inputSats)

	if err != nil {
		t.Fatal(err)
	}

	if txid != "settlementtxid" {
		t.Fatalf("txid = %q", txid)
	}

	if fee != plan.FeeSats {
		t.Fatalf("measured fee = %d, plan = %d", fee, plan.FeeSats)
	}
}

// TestSpendFeeRefusesANonPositiveFee: a transaction that pays its input back in
// full is not a settlement, and the measurement must not return a negative fee.
func TestSpendFeeRefusesANonPositiveFee(t *testing.T) {
	node := newFakeNode(100_000)

	// fin:0 pays the whole input to the output.
	if _, err := SpendFee(context.Background(), node, "fin:0", node.inputSats); err == nil {
		t.Fatal("a zero-fee settlement was accepted")
	}
}

func sha256Hex(value string) string {
	sum := sha256.Sum256(mustHex(value))

	return hex.EncodeToString(sum[:])
}

func mustHex(value string) []byte {
	decoded, err := hex.DecodeString(value)

	if err != nil {
		panic(err)
	}

	return decoded
}

// TestSettlementRateHonoursTheEnvironmentOverride: the settlement path reads the same
// per-chain knob the funding path does, so a client that sets one rate pays it on
// every leg it is responsible for.
func TestSettlementRateHonoursTheEnvironmentOverride(t *testing.T) {
	t.Setenv("BLAKE_SWAP_BTC_SPEND_FEE_RATE", "6")

	rate, source, err := settlementRate(context.Background(), &fakeNode{feeRate: "0.00002000"}, chain.BTC, Options{})

	if err != nil {
		t.Fatal(err)
	}

	if rate.String() != "6" || source != "rate-override" {
		t.Fatalf("rate = %s from %q, want 6 from the override", rate, source)
	}
}

// TestSettlementRateExplicitBeatsTheEnvironment: the parameter outranks the host
// default, which is the precedence the client asked for.
func TestSettlementRateExplicitBeatsTheEnvironment(t *testing.T) {
	t.Setenv("BLAKE_SWAP_BTC_SPEND_FEE_RATE", "6")

	explicit := decimal.MustParse("8")

	rate, source, err := settlementRate(context.Background(), &fakeNode{feeRate: "0.00002000"}, chain.BTC, Options{FeeRate: &explicit})

	if err != nil {
		t.Fatal(err)
	}

	if rate.String() != "8" || source != "--fee-rate" {
		t.Fatalf("rate = %s from %q, want the explicit 8", rate, source)
	}
}

// TestSettlementRateOverrideBelowTheFloorIsRefused: a rate the node will not accept is
// refused before a transaction is built, naming the knob and the floor.
func TestSettlementRateOverrideBelowTheFloorIsRefused(t *testing.T) {
	t.Setenv("BLAKE_SWAP_BTC_SPEND_FEE_RATE", "0.5")

	_, _, err := settlementRate(context.Background(), &fakeNode{feeRate: "0.00002000"}, chain.BTC, Options{})

	if err == nil {
		t.Fatal("a rate below the node floor was accepted")
	}

	if !strings.Contains(err.Error(), "BLAKE_SWAP_BTC_SPEND_FEE_RATE") {
		t.Fatalf("error does not name the knob: %v", err)
	}
}
