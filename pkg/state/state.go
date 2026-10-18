// Package state is the swap protocol's state machine.
//
// It is the port of the State enum and the two transition tables in the Python
// engine. The tables are data, not code, because the interesting property is
// negative: every edge that is *not* in them must be refused. Two tables exist
// rather than one because forward protocol progress and reorganisation
// recovery are different authorities — a recovery path may move durable state
// backward only along an edge someone wrote down.
package state

import "fmt"

// State is one durable protocol checkpoint.
type State string

// The nine checkpoints. The order here is the order they are declared in the
// Python engine, not an ordering of progress.
const (
	Offered        State = "OFFERED"
	BlakeFunded    State = "BLAKE_FUNDED"
	BTCFunded      State = "BTC_FUNDED"
	SecretRevealed State = "SECRET_REVEALED"
	BTCClaimed     State = "BTC_CLAIMED"
	BlakeClaimed   State = "BLAKE_CLAIMED"
	Complete       State = "COMPLETE"
	BTCRefunded    State = "BTC_REFUNDED"
	BlakeRefunded  State = "BLAKE_REFUNDED"
)

// All is every state, in declaration order.
var All = []State{
	Offered,
	BlakeFunded,
	BTCFunded,
	SecretRevealed,
	BTCClaimed,
	BlakeClaimed,
	Complete,
	BTCRefunded,
	BlakeRefunded,
}

// Parse validates a state string from durable storage or the wire.
func Parse(s string) (State, error) {
	for _, candidate := range All {
		if string(candidate) == s {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("unknown swap state %q", s)
}

// String implements fmt.Stringer.
func (s State) String() string { return string(s) }

// Terminal reports whether the state has no forward edge.
func (s State) Terminal() bool {
	return len(forward[s]) == 0
}

// forward is ALLOWED_TRANSITIONS.
var forward = map[State][]State{
	Offered:        {BlakeFunded},
	BlakeFunded:    {BTCFunded, BlakeRefunded},
	BTCFunded:      {SecretRevealed, BTCRefunded},
	SecretRevealed: {BTCClaimed},
	BTCClaimed:     {BlakeClaimed},
	BlakeClaimed:   {Complete},
	BTCRefunded:    {BlakeRefunded},
	Complete:       {},
	BlakeRefunded:  {},
}

// reorg is REORG_TRANSITIONS: backward corrections only a reorganisation
// recovery may perform.
var reorg = map[State][]State{
	BlakeClaimed:  {BTCClaimed},
	BlakeRefunded: {BTCRefunded},
}

func contains(states []State, want State) bool {
	for _, s := range states {
		if s == want {
			return true
		}
	}

	return false
}

// CanTransitionTo reports whether normal forward progress may move from s to
// next. It is deliberately not symmetric and deliberately does not include the
// reorganisation edges: a forward path that could jump backward would let a
// bug roll a swap back to a cheaper-to-attack checkpoint.
func (s State) CanTransitionTo(next State) bool {
	return contains(forward[s], next)
}

// ReorgAllowedTo reports whether recovery may move from s to target.
func (s State) ReorgAllowedTo(target State) bool {
	return contains(reorg[s], target)
}

// Transition applies a forward transition, returning the new state.
func Transition(from, to State) (State, error) {
	if !from.CanTransitionTo(to) {
		return from, fmt.Errorf(
			"illegal state transition: %s -> %s", from, to,
		)
	}

	return to, nil
}

// ApplyReorg applies a reorganisation correction, returning the new state.
//
// Any edge not in the recovery table is refused with the same SECURITY FAILURE
// wording the Python engine uses, because this is the one place a durable
// checkpoint may move backward.
func ApplyReorg(from, target State) (State, error) {
	if !from.ReorgAllowedTo(target) {
		return from, fmt.Errorf(
			"SECURITY FAILURE: reorganization transition %s -> %s is not permitted",
			from, target,
		)
	}

	return target, nil
}

// NextStates returns a copy of the forward edges from s, for reporting and
// tests.
func (s State) NextStates() []State {
	out := make([]State, len(forward[s]))
	copy(out, forward[s])

	return out
}

// ReorgTargets returns a copy of the recovery edges from s.
func (s State) ReorgTargets() []State {
	out := make([]State, len(reorg[s]))
	copy(out, reorg[s])

	return out
}
