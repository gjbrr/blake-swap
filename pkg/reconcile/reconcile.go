// Package reconcile turns observations into one recommendation.
//
// It is the port of the reconcile half of the Python engine: the observation
// vocabulary, the twelve-action decision table, the execution-readiness check,
// and the machine-readable report.
//
// The rule the table encodes is conservative: confirmed chain truth outranks a
// stale durable checkpoint, so the engine prefers a `sync-*` step over any
// action whose command would require newer local state than has been persisted.
// Nothing here executes anything.
package reconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/gjbrr/blake-swap/pkg/amount"
	"github.com/gjbrr/blake-swap/pkg/decimal"
	"github.com/gjbrr/blake-swap/pkg/model"
	"github.com/gjbrr/blake-swap/pkg/state"
)

// Role is a protocol role. Protocol Alice always funds BLAKE and holds the
// secret; protocol Bob always funds BTC and learns the secret from a claim.
type Role string

// The two protocol roles.
const (
	Alice Role = "alice"
	Bob   Role = "bob"
)

// Status is what one chain says about one HTLC outpoint.
type Status string

// The observation vocabulary.
const (
	// Absent means no funding output exists.
	Absent Status = "absent"
	// Pending means funding is broadcast and unconfirmed.
	Pending Status = "pending"
	// Funded means the HTLC output is confirmed and unspent.
	Funded Status = "funded"
	// Spent means the HTLC output is spent; Branch says how.
	Spent Status = "spent"
	// PendingSpend means a settlement transaction is broadcast and awaiting
	// confirmation.
	PendingSpend Status = "pending-spend"
	// Unknown means the observation failed; that is never actionable.
	Unknown Status = "unknown"
	// Invalid means the chain data did not make sense for this swap.
	Invalid Status = "invalid"
	// Ambiguous means a spend exists but cannot be classified safely.
	Ambiguous Status = "pending-or-ambiguous"
)

// Branch is which HTLC path a spend took.
type Branch string

// The two HTLC branches. BranchNone means "not applicable yet".
const (
	BranchNone   Branch = ""
	BranchClaim  Branch = "claim"
	BranchRefund Branch = "refund"
)

// Observation is one chain's answer about one HTLC.
type Observation struct {
	Status        Status
	Branch        Branch
	TxID          string
	Vout          int64
	BlockHash     string
	Confirmations int64
	// Reason explains a non-actionable status.
	Reason string
}

// Heights are the current chain heights.
type Heights struct {
	BTC   int64
	Blake int64
}

// Action is the one recommendation the table produces.
type Action string

// The twelve actions.
const (
	// ActionNone means the swap is settled.
	ActionNone Action = "none"
	// ActionWait means something is in flight; do nothing yet.
	ActionWait Action = "wait"
	// ActionManualAttention means no conservative rule applies.
	ActionManualAttention Action = "manual-attention"

	ActionSyncFunding    Action = "sync-funding"
	ActionSyncSettlement Action = "sync-settlement"

	ActionFundBlake   Action = "fund-blake"
	ActionFundBTC     Action = "fund-btc"
	ActionClaimBTC    Action = "claim-btc"
	ActionClaimBlake  Action = "claim-blake"
	ActionRefundBTC   Action = "refund-btc"
	ActionRefundBlake Action = "refund-blake"
	ActionComplete    Action = "complete"
)

// Class is the coarse category of an action, as the report exposes it.
type Class string

// The five decision classes.
const (
	ClassSettled         Class = "settled"
	ClassWaiting         Class = "waiting"
	ClassRecoveryNeeded  Class = "recovery-needed"
	ClassManualAttention Class = "manual-attention"
	ClassActionable      Class = "actionable"
)

// ClassOf maps an action to its class.
func ClassOf(a Action) Class {
	switch a {
	case ActionNone:
		return ClassSettled
	case ActionWait:
		return ClassWaiting
	case ActionSyncFunding, ActionSyncSettlement:
		return ClassRecoveryNeeded
	case ActionManualAttention:
		return ClassManualAttention
	default:
		return ClassActionable
	}
}

// Terminal reports whether an action ends this participant's work.
func Terminal(a Action) bool {
	return a == ActionNone
}

// Decision is one recommendation and why.
type Decision struct {
	Action Action
	Class  Class
	Reason string
}

func decide(a Action, reason string) Decision {
	return Decision{Action: a, Class: ClassOf(a), Reason: reason}
}

// Decide returns one conservative recommendation. It never executes it.
func Decide(swap *model.Swap, role Role, blake, btc Observation, h Heights) Decision {
	current := swap.State

	if current == state.Complete {
		if blake.Status == Spent && blake.Branch == BranchClaim &&
			btc.Status == Spent && btc.Branch == BranchClaim {
			return decide(ActionNone,
				"swap is COMPLETE and both confirmed claim branches remain active")
		}

		return decide(ActionManualAttention,
			"local checkpoint is COMPLETE but current chain reality does not prove both claim branches")
	}

	if blake.Status == PendingSpend {
		return decide(ActionWait,
			"BLAKE settlement transaction is broadcast and awaiting confirmation")
	}

	if btc.Status == PendingSpend {
		return decide(ActionWait,
			"Bitcoin settlement transaction is broadcast and awaiting confirmation")
	}

	switch blake.Status {
	case Unknown, Invalid, Ambiguous:
		return decide(ActionManualAttention,
			"BLAKE observation is "+string(blake.Status))
	}

	switch btc.Status {
	case Unknown, Invalid, Ambiguous:
		return decide(ActionManualAttention,
			"Bitcoin observation is "+string(btc.Status))
	}

	if blake.Status == Pending {
		return decide(ActionWait,
			"BLAKE funding is broadcast and awaiting confirmation")
	}

	if btc.Status == Pending {
		return decide(ActionWait,
			"Bitcoin funding is broadcast and awaiting confirmation")
	}

	//
	// Confirmed funding may be ahead of the durable checkpoint. Never
	// recommend an action whose command requires a newer local state than has
	// been persisted: synchronise first, then reconcile again.
	//
	if blake.Status == Funded && current == state.Offered {
		return decide(ActionSyncFunding,
			"confirmed BLAKE funding is ahead of local checkpoint")
	}

	if btc.Status == Funded &&
		(current == state.Offered || current == state.BlakeFunded) {
		return decide(ActionSyncFunding,
			"confirmed Bitcoin funding is ahead of local checkpoint")
	}

	//
	// A confirmed Bitcoin spend proves Bitcoin funding happened first. If
	// durable state has not imported it, restore that checkpoint before
	// attempting settlement reconciliation.
	//
	if btc.Status == Spent &&
		(current == state.Offered || current == state.BlakeFunded) {
		return decide(ActionSyncFunding,
			"confirmed Bitcoin settlement exists but Bitcoin funding is ahead of local checkpoint")
	}

	if blake.Status == Spent {
		if blake.Branch == BranchClaim {
			if current == state.BlakeClaimed {
				return decide(ActionComplete, "final BLAKE claim is confirmed")
			}

			return decide(ActionSyncSettlement,
				"confirmed BLAKE claim is ahead of local checkpoint")
		}

		if blake.Branch == BranchRefund {
			if current == state.BlakeRefunded {
				return decide(ActionNone, "final BLAKE refund is confirmed")
			}

			return decide(ActionSyncSettlement,
				"confirmed BLAKE refund is ahead of local checkpoint")
		}
	}

	if btc.Status == Spent {
		if btc.Branch == BranchClaim {
			if current != state.BTCClaimed && current != state.BlakeClaimed {
				return decide(ActionSyncSettlement,
					"confirmed Bitcoin claim is ahead of local checkpoint")
			}

			if role == Bob {
				if swap.RevealedSecretHex == nil {
					return decide(ActionSyncSettlement,
						"Bitcoin claim reveals S but local Bob state has not recovered it")
				}

				if blake.Status == Funded {
					return decide(ActionClaimBlake,
						"S is known and the BLAKE HTLC remains unspent")
				}
			}

			return decide(ActionWait,
				"Bitcoin claim is confirmed; protocol Bob must claim BLAKE")
		}

		if btc.Branch == BranchRefund {
			if current != state.BTCRefunded {
				return decide(ActionSyncSettlement,
					"confirmed Bitcoin refund is ahead of local checkpoint")
			}

			if role == Alice && blake.Status == Funded {
				if h.Blake >= swap.BlakeTimeout {
					return decide(ActionRefundBlake,
						"Bitcoin was refunded and BLAKE refund is mature")
				}

				return decide(ActionWait,
					"Bitcoin was refunded; BLAKE refund is not mature")
			}

			return decide(ActionWait,
				"Bitcoin refund is confirmed; protocol Alice controls BLAKE refund")
		}
	}

	if role == Alice {
		if blake.Status == Absent {
			if swap.BlakeBobPub == nil || swap.BTCBobPub == nil || swap.NostrBobPub == nil {
				return decide(ActionWait,
					"protocol Bob acceptance/public keys are not fully pinned")
			}

			if h.Blake >= swap.BlakeTimeout {
				return decide(ActionManualAttention,
					"BLAKE timeout has already been reached")
			}

			return decide(ActionFundBlake,
				"counterparty is pinned and no BLAKE funding exists")
		}

		if blake.Status == Funded && btc.Status == Absent {
			if h.Blake >= swap.BlakeTimeout {
				return decide(ActionRefundBlake,
					"Bitcoin was never funded and BLAKE refund is mature")
			}

			if h.BTC >= swap.BTCTimeout {
				return decide(ActionWait,
					"Bitcoin funding window has closed; waiting for BLAKE refund maturity")
			}

			return decide(ActionWait,
				"BLAKE is funded; protocol Bob must fund Bitcoin")
		}

		if btc.Status == Funded {
			if swap.SecretHex == nil {
				return decide(ActionManualAttention,
					"protocol Alice has no local secret S")
			}

			if h.BTC >= swap.BTCTimeout {
				return decide(ActionManualAttention,
					"Bitcoin refund timelock is mature; claim may race a refund")
			}

			return decide(ActionClaimBTC,
				"Bitcoin HTLC is confirmed/unspent and protocol Alice possesses S")
		}
	}

	if role == Bob {
		if blake.Status == Absent {
			return decide(ActionWait, "protocol Alice must fund BLAKE first")
		}

		if blake.Status == Funded && btc.Status == Absent {
			if h.BTC >= swap.BTCTimeout {
				return decide(ActionManualAttention,
					"Bitcoin timeout has already been reached")
			}

			return decide(ActionFundBTC,
				"BLAKE HTLC is confirmed/unspent and Bitcoin is not yet funded")
		}

		if btc.Status == Funded {
			if h.BTC >= swap.BTCTimeout {
				return decide(ActionRefundBTC,
					"Bitcoin HTLC is unspent and its refund timelock is mature")
			}

			return decide(ActionWait,
				"Bitcoin is funded; protocol Alice must claim and reveal S")
		}
	}

	return decide(ActionManualAttention,
		fmt.Sprintf("no conservative rule matched checkpoint %s", current))
}

// NegotiationStatus classifies whether enough authenticated negotiation state
// exists to derive the HTLCs and safely inspect chain reality.
//
// This is deliberately separate from blockchain state.
func NegotiationStatus(swap *model.Swap) (status, reason string) {
	if swap.NostrAlicePub == nil || swap.NostrBobPub == nil {
		return "identity-unbound", "both protocol Nostr identities are not pinned"
	}

	aliceKeys := swap.BlakeAlicePub != nil && swap.BTCAlicePub != nil
	bobKeys := swap.BlakeBobPub != nil && swap.BTCBobPub != nil

	if !aliceKeys {
		return "negotiation-incomplete",
			"protocol Alice settlement pubkeys were never created or imported"
	}

	if !bobKeys {
		return "negotiation-incomplete",
			"protocol Bob settlement pubkeys were never created or imported"
	}

	return "ready", "all protocol identities and settlement pubkeys are pinned"
}

// WalletFacts is what the readiness check needs to know about a local wallet.
type WalletFacts struct {
	// Loaded reports whether the signing wallet is loaded.
	Loaded bool
	// Trusted is the spendable balance in coins.
	Trusted *decimal.Dec
	// Available reports whether the wallet service could be asked at all.
	Available bool
	// Error explains an unavailable wallet.
	Error string
}

// ReadinessConfig is the funding-margin policy.
type ReadinessConfig struct {
	// BlakeFeeReserve is the extra BLAKE kept back for the settlement fee.
	BlakeFeeReserve decimal.Dec
	// BTCFeeReserveSats is the extra Bitcoin kept back, in satoshis.
	BTCFeeReserveSats int64
	// MinBlakeFundBlocks and MinBTCFundBlocks are the minimum timeout margins.
	MinBlakeFundBlocks int64
	MinBTCFundBlocks   int64
}

// DefaultReadinessConfig matches the Python defaults.
func DefaultReadinessConfig() ReadinessConfig {
	return ReadinessConfig{
		BlakeFeeReserve:    decimal.MustParse("0.001"),
		BTCFeeReserveSats:  5000,
		MinBlakeFundBlocks: 2,
		MinBTCFundBlocks:   2,
	}
}

// Readiness is the answer to "may the driver execute a funding action now".
type Readiness struct {
	Checked  bool
	Action   Action
	Ready    *bool
	Wallet   string
	Loaded   *bool
	Trusted  *decimal.Dec
	Required *decimal.Dec
	Reserve  *decimal.Dec
	Unit     string
	// TimeoutBlocksRemaining and MinimumTimeoutBlocks bound the funding
	// margin: funding too close to the timeout is not worth doing.
	TimeoutBlocksRemaining *int64
	MinimumTimeoutBlocks   *int64
	Reasons                []string
}

// EvaluateReadiness checks whether a funding action can proceed.
//
// Only funding actions are checked; everything else is ready by definition
// because the wallet is not the constraint.
func EvaluateReadiness(
	swap *model.Swap,
	role Role,
	action Action,
	h Heights,
	wallet WalletFacts,
	cfg ReadinessConfig,
) Readiness {
	out := Readiness{Action: action}

	switch action {
	case ActionFundBlake:
		out.Wallet = "blake"
	case ActionFundBTC:
		out.Wallet = "btc"
	default:
		return out
	}

	out.Checked = true

	var (
		required  decimal.Dec
		reserve   decimal.Dec
		unit      string
		remaining int64
		minimum   int64
	)

	if action == ActionFundBlake {
		if role != Alice {
			ready := false
			out.Ready = &ready
			out.Reasons = append(out.Reasons, "fund-blake requires protocol Alice")

			return out
		}

		if cfg.BlakeFeeReserve.Sign() < 0 {
			ready := false
			out.Ready = &ready
			out.Reasons = append(out.Reasons, "BLAKE fee reserve must be >= 0")

			return out
		}

		reserve = cfg.BlakeFeeReserve
		required = amount.Blake(swap.BlakeAmountBaseUnits).Decimal().Add(reserve)
		unit = "BLAKE"
		remaining = swap.BlakeTimeout - h.Blake
		minimum = cfg.MinBlakeFundBlocks
	} else {
		if role != Bob {
			ready := false
			out.Ready = &ready
			out.Reasons = append(out.Reasons, "fund-btc requires protocol Bob")

			return out
		}

		if cfg.BTCFeeReserveSats < 0 {
			ready := false
			out.Ready = &ready
			out.Reasons = append(out.Reasons, "Bitcoin fee reserve must be >= 0")

			return out
		}

		reserve = amount.Sats(cfg.BTCFeeReserveSats).Coin()
		required = amount.Sats(swap.BTCSats).Coin().Add(reserve)
		unit = "BTC"
		remaining = swap.BTCTimeout - h.BTC
		minimum = cfg.MinBTCFundBlocks
	}

	if minimum < 1 {
		ready := false
		out.Ready = &ready
		out.Reasons = append(out.Reasons, "funding minimum timeout blocks must be >= 1")

		return out
	}

	out.Required = &required
	out.Reserve = &reserve
	out.Unit = unit
	out.TimeoutBlocksRemaining = &remaining
	out.MinimumTimeoutBlocks = &minimum

	loaded := wallet.Loaded
	out.Loaded = &loaded

	if !wallet.Available {
		ready := false
		out.Ready = &ready

		detail := wallet.Error
		if detail == "" {
			detail = "wallet service unavailable"
		}

		out.Reasons = append(out.Reasons, detail)

		return out
	}

	if !loaded {
		out.Reasons = append(out.Reasons,
			fmt.Sprintf("local wallet %q is not loaded", out.Wallet))
	}

	var trustedValue decimal.Dec

	if loaded && wallet.Trusted != nil {
		trustedValue = *wallet.Trusted
		out.Trusted = &trustedValue

		if trustedValue.Cmp(required) < 0 {
			out.Reasons = append(out.Reasons,
				"trusted wallet balance is below amount plus configured fee reserve")
		}
	}

	if remaining < minimum {
		out.Reasons = append(out.Reasons,
			"timeout margin is below configured funding minimum")
	}

	ready := loaded &&
		wallet.Trusted != nil &&
		trustedValue.Cmp(required) >= 0 &&
		remaining >= minimum

	out.Ready = &ready

	if ready {
		out.Reasons = append(out.Reasons,
			"wallet, balance, fee reserve, and timeout margin are ready")
	}

	return out
}

// Input is everything needed to build a report.
type Input struct {
	Swap         *model.Swap
	Role         Role
	Me           string
	Counterparty string
	Heights      Heights
	Blake        Observation
	BTC          Observation
	Wallet       WalletFacts
	Config       ReadinessConfig
}

// Report is the machine-readable, read-only protocol report.
type Report struct {
	SwapID            string
	ParticipantPubkey string
	ProtocolRole      Role
	Counterparty      string
	CheckpointState   state.State
	DecisionClass     Class
	NegotiationStatus string
	NegotiationReason string
	Secret            Secret
	Heights           Heights
	Timeouts          Timeouts
	Blake             Observation
	BTC               Observation
	NextAction        Action
	Reason            string
	Readiness         Readiness
}

// Secret is what this participant knows about S.
type Secret struct {
	Known       bool
	Source      string
	HashMatches *bool
}

// Timeouts is the durable timeout shape plus the remaining margins.
type Timeouts struct {
	BlakeTimeout         int64
	BTCTimeout           int64
	BlakeBlocksRemaining int64
	BTCBlocksRemaining   int64
}

// Build assembles a report. It performs no I/O: every fact comes from Input, so
// the report is reproducible from recorded observations.
func Build(in Input) Report {
	status, reason := NegotiationStatus(in.Swap)

	var secret Secret

	secretValue := ""
	secret.Source = "none"

	switch {
	case in.Role == Alice && in.Swap.SecretHex != nil:
		secretValue = *in.Swap.SecretHex
		secret.Source = "locally-generated"
	case in.Role == Bob && in.Swap.RevealedSecretHex != nil:
		secretValue = *in.Swap.RevealedSecretHex
		secret.Source = "recovered-from-bitcoin"
	}

	if secretValue != "" {
		secret.Known = true

		raw, err := hex.DecodeString(secretValue)

		if err == nil && len(raw) == 32 {
			sum := sha256.Sum256(raw)
			matches := hex.EncodeToString(sum[:]) == in.Swap.SecretHash
			secret.HashMatches = &matches
		} else {
			matches := false
			secret.HashMatches = &matches
		}
	}

	report := Report{
		SwapID:            in.Swap.SwapID,
		ParticipantPubkey: in.Me,
		ProtocolRole:      in.Role,
		Counterparty:      in.Counterparty,
		CheckpointState:   in.Swap.State,
		NegotiationStatus: status,
		NegotiationReason: reason,
		Secret:            secret,
		Heights:           in.Heights,
		Timeouts: Timeouts{
			BlakeTimeout:         in.Swap.BlakeTimeout,
			BTCTimeout:           in.Swap.BTCTimeout,
			BlakeBlocksRemaining: in.Swap.BlakeTimeout - in.Heights.Blake,
			BTCBlocksRemaining:   in.Swap.BTCTimeout - in.Heights.BTC,
		},
		Blake: in.Blake,
		BTC:   in.BTC,
	}

	if status != "ready" {
		report.DecisionClass = ClassManualAttention
		report.NextAction = ActionManualAttention
		report.Reason = reason

		return report
	}

	decision := Decide(in.Swap, in.Role, in.Blake, in.BTC, in.Heights)

	report.DecisionClass = decision.Class
	report.NextAction = decision.Action
	report.Reason = decision.Reason
	report.Readiness = EvaluateReadiness(
		in.Swap, in.Role, decision.Action, in.Heights, in.Wallet, in.Config,
	)

	return report
}
