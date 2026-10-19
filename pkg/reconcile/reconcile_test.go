package reconcile

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/decimal"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/state"
)

// baseSwap returns a validated record in OFFERED with nothing negotiated.
func baseSwap(t *testing.T) *model.Swap {
	t.Helper()

	raw := make([]byte, 128)

	for i := range raw {
		raw[i] = byte(i + 1)
	}

	swap, err := model.New(bytes.NewReader(raw), model.NewParams{
		BlakeAmount:    amount.MustParseBlake("1"),
		BTCSats:        25_000,
		BlakeTimeout:   1_000,
		BTCTimeout:     900,
		BlakeScanStart: 100,
		BTCScanStart:   90,
	})

	if err != nil {
		t.Fatal(err)
	}

	return swap
}

func strPtr(s string) *string { return &s }
func intPtr(n int64) *int64   { return &n }

func hex64(b byte) string { return strings.Repeat(string([]byte{"0123456789abcdef"[b%16]}), 64) }

// pinned negotiates the counterparty keys so the fund-blake rule can fire.
func pinned(swap *model.Swap) {
	swap.NostrAlicePub = strPtr(hex64(1))
	swap.NostrBobPub = strPtr(hex64(2))
	swap.BlakeAlicePub = strPtr(hex64(3))
	swap.BlakeBobPub = strPtr(hex64(4))
	swap.BTCAlicePub = strPtr(hex64(5))
	swap.BTCBobPub = strPtr(hex64(6))
}

type observation = Observation

func funded() observation { return Observation{Status: Funded} }
func absent() observation { return Observation{Status: Absent} }

func spent(branch Branch) observation {
	return Observation{Status: Spent, Branch: branch}
}

func TestDecideTable(t *testing.T) {
	const (
		blakeHeight = 500
		btcHeight   = 400
	)

	heights := Heights{BTC: btcHeight, Blake: blakeHeight}

	cases := []struct {
		name  string
		setup func(*model.Swap)
		role  Role
		blake observation
		btc   observation
		want  Action
	}{
		{
			name:  "complete with both claims is settled",
			setup: func(s *model.Swap) { s.State = state.Complete },
			role:  Alice,
			blake: spent(BranchClaim),
			btc:   spent(BranchClaim),
			want:  ActionNone,
		},
		{
			name:  "complete without both claims needs attention",
			setup: func(s *model.Swap) { s.State = state.Complete },
			role:  Alice,
			blake: spent(BranchClaim),
			btc:   funded(),
			want:  ActionManualAttention,
		},
		{
			name:  "a pending BLAKE settlement is waited on",
			setup: func(s *model.Swap) {},
			role:  Alice,
			blake: Observation{Status: PendingSpend},
			btc:   funded(),
			want:  ActionWait,
		},
		{
			name:  "a pending Bitcoin settlement is waited on",
			setup: func(s *model.Swap) {},
			role:  Alice,
			blake: funded(),
			btc:   Observation{Status: PendingSpend},
			want:  ActionWait,
		},
		{
			name:  "an unknown BLAKE observation is never actionable",
			setup: func(s *model.Swap) {},
			role:  Bob,
			blake: Observation{Status: NotEvaluated, Reason: "rpc down"},
			btc:   absent(),
			want:  ActionManualAttention,
		},
		{
			name:  "an ambiguous Bitcoin observation is never actionable",
			setup: func(s *model.Swap) {},
			role:  Bob,
			blake: funded(),
			btc:   Observation{Status: Ambiguous},
			want:  ActionManualAttention,
		},
		{
			name:  "pending BLAKE funding is waited on",
			setup: func(s *model.Swap) {},
			role:  Alice,
			blake: Observation{Status: Pending},
			btc:   absent(),
			want:  ActionWait,
		},
		{
			name:  "pending Bitcoin funding is waited on",
			setup: func(s *model.Swap) {},
			role:  Bob,
			blake: funded(),
			btc:   Observation{Status: Pending},
			want:  ActionWait,
		},
		{
			name:  "confirmed BLAKE funding ahead of checkpoint synchronises",
			setup: func(s *model.Swap) {},
			role:  Alice,
			blake: funded(),
			btc:   absent(),
			want:  ActionSyncFunding,
		},
		{
			name:  "confirmed Bitcoin funding ahead of checkpoint synchronises",
			setup: func(s *model.Swap) {},
			role:  Bob,
			blake: funded(),
			btc:   funded(),
			want:  ActionSyncFunding,
		},
		{
			//
			// The state must be BLAKE_FUNDED, not OFFERED: at OFFERED the
			// earlier "confirmed BLAKE funding" rule already returns
			// sync-funding, which would mask whether this rule exists.
			//
			name:  "confirmed Bitcoin spend ahead of funding synchronises funding first",
			setup: func(s *model.Swap) { s.State = state.BlakeFunded },
			role:  Alice,
			blake: funded(),
			btc:   spent(BranchClaim),
			want:  ActionSyncFunding,
		},
		{
			name:  "a confirmed BLAKE claim completes",
			setup: func(s *model.Swap) { s.State = state.BlakeClaimed },
			role:  Bob,
			blake: spent(BranchClaim),
			btc:   spent(BranchClaim),
			want:  ActionComplete,
		},
		{
			name:  "a BLAKE claim ahead of checkpoint synchronises settlement",
			setup: func(s *model.Swap) { s.State = state.BTCClaimed },
			role:  Bob,
			blake: spent(BranchClaim),
			btc:   spent(BranchClaim),
			want:  ActionSyncSettlement,
		},
		{
			name:  "a BLAKE refund settles",
			setup: func(s *model.Swap) { s.State = state.BlakeRefunded },
			role:  Alice,
			blake: spent(BranchRefund),
			btc:   Observation{Status: Absent},
			want:  ActionNone,
		},
		{
			name:  "a BLAKE refund ahead of checkpoint synchronises settlement",
			setup: func(s *model.Swap) { s.State = state.BTCRefunded },
			role:  Alice,
			blake: spent(BranchRefund),
			btc:   Observation{Status: Absent},
			want:  ActionSyncSettlement,
		},
		{
			name: "Bob claims BLAKE once he has S and BLAKE is funded",
			setup: func(s *model.Swap) {
				s.State = state.BTCClaimed
				s.RevealedSecretHex = s.SecretHex
				s.SecretHex = nil
			},
			role:  Bob,
			blake: funded(),
			btc:   spent(BranchClaim),
			want:  ActionClaimBlake,
		},
		{
			name: "Bob without S synchronises to recover it",
			setup: func(s *model.Swap) {
				s.State = state.BTCClaimed
				s.SecretHex = nil
			},
			role:  Bob,
			blake: funded(),
			btc:   spent(BranchClaim),
			want:  ActionSyncSettlement,
		},
		{
			name: "Bob with S but no BLAKE funding waits",
			setup: func(s *model.Swap) {
				s.State = state.BTCClaimed
				s.RevealedSecretHex = s.SecretHex
				s.SecretHex = nil
			},
			role:  Bob,
			blake: absent(),
			btc:   spent(BranchClaim),
			want:  ActionWait,
		},
		{
			name:  "protocol Alice never claims BLAKE",
			setup: func(s *model.Swap) { s.State = state.BTCClaimed },
			role:  Alice,
			blake: funded(),
			btc:   spent(BranchClaim),
			want:  ActionWait,
		},
		{
			name:  "an unimported Bitcoin refund synchronises settlement",
			setup: func(s *model.Swap) { s.State = state.BTCFunded },
			role:  Alice,
			blake: funded(),
			btc:   spent(BranchRefund),
			want:  ActionSyncSettlement,
		},
		{
			name: "Alice refunds BLAKE after a mature Bitcoin refund",
			setup: func(s *model.Swap) {
				s.State = state.BTCRefunded
				s.BlakeTimeout = blakeHeight
			},
			role:  Alice,
			blake: funded(),
			btc:   spent(BranchRefund),
			want:  ActionRefundBlake,
		},
		{
			name: "Alice waits for BLAKE refund maturity",
			setup: func(s *model.Swap) {
				s.State = state.BTCRefunded
				s.BlakeTimeout = blakeHeight + 10
			},
			role:  Alice,
			blake: funded(),
			btc:   spent(BranchRefund),
			want:  ActionWait,
		},
		{
			name:  "Bob waits after a Bitcoin refund",
			setup: func(s *model.Swap) { s.State = state.BTCRefunded },
			role:  Bob,
			blake: funded(),
			btc:   spent(BranchRefund),
			want:  ActionWait,
		},
		{
			name:  "Alice waits until Bob's keys are pinned",
			setup: func(s *model.Swap) { pinned(s); s.BlakeBobPub = nil },
			role:  Alice,
			blake: absent(),
			btc:   absent(),
			want:  ActionWait,
		},
		{
			name: "Alice refuses to fund past the BLAKE timeout",
			setup: func(s *model.Swap) {
				pinned(s)
				s.BlakeTimeout = blakeHeight
			},
			role:  Alice,
			blake: absent(),
			btc:   absent(),
			want:  ActionManualAttention,
		},
		{
			name:  "Alice funds BLAKE when the counterparty is pinned",
			setup: func(s *model.Swap) { pinned(s) },
			role:  Alice,
			blake: absent(),
			btc:   absent(),
			want:  ActionFundBlake,
		},
		{
			name: "Alice refunds BLAKE when Bitcoin was never funded and the timeout passed",
			setup: func(s *model.Swap) {
				pinned(s)
				s.State = state.BlakeFunded
				s.BlakeTimeout = blakeHeight
			},
			role:  Alice,
			blake: funded(),
			btc:   absent(),
			want:  ActionRefundBlake,
		},
		{
			name: "Alice waits for BLAKE refund maturity after the Bitcoin window closed",
			setup: func(s *model.Swap) {
				pinned(s)
				s.State = state.BlakeFunded
				s.BlakeTimeout = blakeHeight + 100
				s.BTCTimeout = btcHeight
			},
			role:  Alice,
			blake: funded(),
			btc:   absent(),
			want:  ActionWait,
		},
		{
			name: "Alice waits while Bob may still fund Bitcoin",
			setup: func(s *model.Swap) {
				pinned(s)
				s.State = state.BlakeFunded
			},
			role:  Alice,
			blake: funded(),
			btc:   absent(),
			want:  ActionWait,
		},
		{
			name: "Alice cannot claim without S",
			setup: func(s *model.Swap) {
				pinned(s)
				s.State = state.BTCFunded
				s.SecretHex = nil
			},
			role:  Alice,
			blake: funded(),
			btc:   funded(),
			want:  ActionManualAttention,
		},
		{
			name: "Alice will not race a mature Bitcoin refund",
			setup: func(s *model.Swap) {
				pinned(s)
				s.State = state.BTCFunded
				s.BTCTimeout = btcHeight
			},
			role:  Alice,
			blake: funded(),
			btc:   funded(),
			want:  ActionManualAttention,
		},
		{
			name: "Alice claims Bitcoin when funded and in time",
			setup: func(s *model.Swap) {
				pinned(s)
				s.State = state.BTCFunded
			},
			role:  Alice,
			blake: funded(),
			btc:   funded(),
			want:  ActionClaimBTC,
		},
		{
			name:  "Bob waits for BLAKE funding",
			setup: func(s *model.Swap) {},
			role:  Bob,
			blake: absent(),
			btc:   absent(),
			want:  ActionWait,
		},
		{
			name: "Bob refuses to fund past the Bitcoin timeout",
			setup: func(s *model.Swap) {
				s.State = state.BlakeFunded
				s.BTCTimeout = btcHeight
			},
			role:  Bob,
			blake: funded(),
			btc:   absent(),
			want:  ActionManualAttention,
		},
		{
			name: "Bob funds Bitcoin when BLAKE is funded and time remains",
			setup: func(s *model.Swap) {
				s.State = state.BlakeFunded
			},
			role:  Bob,
			blake: funded(),
			btc:   absent(),
			want:  ActionFundBTC,
		},
		{
			name: "Bob refunds Bitcoin when the timelock is mature",
			setup: func(s *model.Swap) {
				s.State = state.BTCFunded
				s.BTCTimeout = btcHeight
			},
			role:  Bob,
			blake: funded(),
			btc:   funded(),
			want:  ActionRefundBTC,
		},
		{
			name: "Bob waits for Alice's claim while the timelock is open",
			setup: func(s *model.Swap) {
				s.State = state.BTCFunded
			},
			role:  Bob,
			blake: funded(),
			btc:   funded(),
			want:  ActionWait,
		},
		{
			name:  "an unclassifiable spend needs attention",
			setup: func(s *model.Swap) { s.State = state.BlakeClaimed },
			role:  Alice,
			blake: Observation{Status: Spent, Branch: BranchNone},
			btc:   absent(),
			want:  ActionManualAttention,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			swap := baseSwap(t)
			c.setup(swap)

			decision := Decide(swap, c.role, c.blake, c.btc, heights)

			if decision.Action != c.want {
				t.Fatalf("action = %s (%s), want %s", decision.Action, decision.Reason, c.want)
			}

			if decision.Class != ClassOf(c.want) {
				t.Fatalf("class = %s, want %s", decision.Class, ClassOf(c.want))
			}

			if decision.Reason == "" {
				t.Error("decision has no reason")
			}
		})
	}
}

func TestClassMapping(t *testing.T) {
	cases := map[Action]Class{
		ActionNone:            ClassSettled,
		ActionWait:            ClassWaiting,
		ActionSyncFunding:     ClassRecoveryNeeded,
		ActionSyncSettlement:  ClassRecoveryNeeded,
		ActionManualAttention: ClassManualAttention,
		ActionFundBlake:       ClassActionable,
		ActionFundBTC:         ClassActionable,
		ActionClaimBTC:        ClassActionable,
		ActionClaimBlake:      ClassActionable,
		ActionRefundBTC:       ClassActionable,
		ActionRefundBlake:     ClassActionable,
		ActionComplete:        ClassActionable,
	}

	for action, want := range cases {
		if got := ClassOf(action); got != want {
			t.Errorf("ClassOf(%s) = %s, want %s", action, got, want)
		}
	}
}

func TestNegotiationStatus(t *testing.T) {
	swap := baseSwap(t)

	if status, _ := NegotiationStatus(swap); status != "identity-unbound" {
		t.Fatalf("unpinned status = %s", status)
	}

	swap.NostrAlicePub = strPtr(hex64(1))
	swap.NostrBobPub = strPtr(hex64(2))

	if status, _ := NegotiationStatus(swap); status != "negotiation-incomplete" {
		t.Fatalf("no settlement keys status = %s", status)
	}

	swap.BlakeAlicePub = strPtr(hex64(3))
	swap.BTCAlicePub = strPtr(hex64(5))

	if status, _ := NegotiationStatus(swap); status != "negotiation-incomplete" {
		t.Fatalf("no bob keys status = %s", status)
	}

	swap.BlakeBobPub = strPtr(hex64(4))
	swap.BTCBobPub = strPtr(hex64(6))

	if status, _ := NegotiationStatus(swap); status != "ready" {
		t.Fatalf("fully pinned status = %s", status)
	}
}

func TestReadinessFundBlake(t *testing.T) {
	swap := baseSwap(t)
	pinned(swap)
	heights := Heights{BTC: 400, Blake: 500}
	cfg := DefaultReadinessConfig()

	// The wrong role is never ready, whatever the balance.
	readiness := EvaluateReadiness(swap, Bob, ActionFundBlake, heights,
		WalletFacts{Available: true, Loaded: true, Trusted: decimalPtr("100")}, cfg)

	if readiness.Ready == nil || *readiness.Ready {
		t.Fatalf("Bob was ready to fund BLAKE: %+v", readiness)
	}

	// The right role, a loaded wallet and enough balance is ready.
	readiness = EvaluateReadiness(swap, Alice, ActionFundBlake, heights,
		WalletFacts{Available: true, Loaded: true, Trusted: decimalPtr("1.5")}, cfg)

	if readiness.Ready == nil || !*readiness.Ready {
		t.Fatalf("Alice was not ready: %+v", readiness)
	}

	// The fee reserve is part of the requirement: 1.0 exactly is not enough.
	readiness = EvaluateReadiness(swap, Alice, ActionFundBlake, heights,
		WalletFacts{Available: true, Loaded: true, Trusted: decimalPtr("1")}, cfg)

	if readiness.Ready == nil || *readiness.Ready {
		t.Fatalf("balance without the reserve was accepted: %+v", readiness)
	}

	// A timeout margin below the configured minimum refuses funding.
	swap.BlakeTimeout = heights.Blake + 1

	readiness = EvaluateReadiness(swap, Alice, ActionFundBlake, heights,
		WalletFacts{Available: true, Loaded: true, Trusted: decimalPtr("1.5")}, cfg)

	if readiness.Ready == nil || *readiness.Ready {
		t.Fatalf("a thin timeout margin was accepted: %+v", readiness)
	}

	// An unloaded wallet is not ready even with a balance.
	readiness = EvaluateReadiness(swap, Alice, ActionFundBlake, heights,
		WalletFacts{Available: true, Loaded: false, Trusted: decimalPtr("5")}, cfg)

	if readiness.Ready == nil || *readiness.Ready {
		t.Fatalf("an unloaded wallet was accepted: %+v", readiness)
	}
}

func TestReadinessFundBTC(t *testing.T) {
	swap := baseSwap(t)
	heights := Heights{BTC: 400, Blake: 500}
	cfg := DefaultReadinessConfig()

	// 25,000 sats plus the 5,000 sat reserve is 30,000 sats = 0.0003 BTC.
	readiness := EvaluateReadiness(swap, Bob, ActionFundBTC, heights,
		WalletFacts{Available: true, Loaded: true, Trusted: decimalPtr("0.0003")}, cfg)

	if readiness.Ready == nil || !*readiness.Ready {
		t.Fatalf("exact requirement was not ready: %+v", readiness)
	}

	readiness = EvaluateReadiness(swap, Bob, ActionFundBTC, heights,
		WalletFacts{Available: true, Loaded: true, Trusted: decimalPtr("0.00029")}, cfg)

	if readiness.Ready == nil || *readiness.Ready {
		t.Fatalf("below the requirement was accepted: %+v", readiness)
	}

	// Alice does not fund Bitcoin.
	readiness = EvaluateReadiness(swap, Alice, ActionFundBTC, heights,
		WalletFacts{Available: true, Loaded: true, Trusted: decimalPtr("1")}, cfg)

	if readiness.Ready == nil || *readiness.Ready {
		t.Fatalf("Alice was ready to fund BTC: %+v", readiness)
	}
}

func TestReadinessOnlyForFunding(t *testing.T) {
	swap := baseSwap(t)
	readiness := EvaluateReadiness(swap, Alice, ActionClaimBTC,
		Heights{}, WalletFacts{}, DefaultReadinessConfig())

	if readiness.Checked {
		t.Fatal("a non-funding action was checked")
	}

	if readiness.Wallet != "" {
		t.Fatalf("non-funding action named a wallet: %q", readiness.Wallet)
	}
}

func TestBuildReport(t *testing.T) {
	swap := baseSwap(t)
	pinned(swap)

	report := Build(Input{
		Swap:         swap,
		Role:         Alice,
		Me:           hex64(9),
		Counterparty: hex64(2),
		Heights:      Heights{BTC: 400, Blake: 500},
		Blake:        absent(),
		BTC:          absent(),
		Wallet: WalletFacts{
			Available: true,
			Loaded:    true,
			Trusted:   decimalPtr("2"),
		},
		Config: DefaultReadinessConfig(),
	})

	if report.NextAction != ActionFundBlake {
		t.Fatalf("action = %s (%s)", report.NextAction, report.Reason)
	}

	if report.DecisionClass != ClassActionable {
		t.Fatalf("class = %s", report.DecisionClass)
	}

	if !report.Secret.Known || report.Secret.Source != "locally-generated" {
		t.Fatalf("secret = %+v", report.Secret)
	}

	if report.Secret.HashMatches == nil || !*report.Secret.HashMatches {
		t.Fatal("secret hash did not match")
	}

	if report.Readiness.Ready == nil || !*report.Readiness.Ready {
		t.Fatalf("readiness = %+v", report.Readiness)
	}

	if report.Timeouts.BlakeBlocksRemaining != swap.BlakeTimeout-500 {
		t.Fatalf("blake remaining = %d", report.Timeouts.BlakeBlocksRemaining)
	}

	// Unpinned negotiation short-circuits to manual attention, without
	// deciding anything about the chain.
	unpinned := baseSwap(t)

	report = Build(Input{
		Swap:    unpinned,
		Role:    Alice,
		Heights: Heights{BTC: 400, Blake: 500},
		Blake:   absent(),
		BTC:     absent(),
	})

	if report.NextAction != ActionManualAttention ||
		report.DecisionClass != ClassManualAttention {
		t.Fatalf("unpinned report = %s/%s", report.NextAction, report.DecisionClass)
	}
}

func decimalPtr(s string) *decimal.Dec {
	d := decimal.MustParse(s)

	return &d
}
