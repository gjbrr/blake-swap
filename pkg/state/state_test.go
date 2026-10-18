package state

import (
	"strings"
	"testing"
)

// TestForwardMatrix pins every pair of states, not just the legal ones.
//
// The interesting property of a state machine is the negative space: an edge
// that is not written down must be refused, so a table-driven test over all
// 81 pairs is the only way to know none was added by accident.
func TestForwardMatrix(t *testing.T) {
	legal := map[State][]State{
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

	for _, from := range All {
		for _, to := range All {
			want := false

			for _, allowed := range legal[from] {
				if allowed == to {
					want = true
				}
			}

			if got := from.CanTransitionTo(to); got != want {
				t.Errorf("%s -> %s: allowed=%v, want %v", from, to, got, want)
			}

			if want {
				if next, err := Transition(from, to); err != nil || next != to {
					t.Errorf("Transition(%s, %s) = %v, %v", from, to, next, err)
				}
			} else if _, err := Transition(from, to); err == nil {
				t.Errorf("Transition(%s, %s) was accepted", from, to)
			}
		}
	}
}

func TestReorgMatrix(t *testing.T) {
	// Only two backward edges exist, and only recovery may take them.
	legal := map[State]State{
		BlakeClaimed:  BTCClaimed,
		BlakeRefunded: BTCRefunded,
	}

	for _, from := range All {
		for _, to := range All {
			want, present := legal[from]
			isLegal := present && want == to

			if got := from.ReorgAllowedTo(to); got != isLegal {
				t.Errorf("reorg %s -> %s: allowed=%v, want %v", from, to, got, isLegal)
			}

			if isLegal {
				if next, err := ApplyReorg(from, to); err != nil || next != to {
					t.Errorf("ApplyReorg(%s, %s) = %v, %v", from, to, next, err)
				}

				// The same edge must NOT be available to forward progress.
				if from.CanTransitionTo(to) {
					t.Errorf("reorg edge %s -> %s is also a forward edge", from, to)
				}
			} else if _, err := ApplyReorg(from, to); err == nil {
				t.Errorf("ApplyReorg(%s, %s) was accepted", from, to)
			}
		}
	}

	// The refusal names the edge and is recognisable as a security refusal.
	_, err := ApplyReorg(Complete, Offered)

	if err == nil {
		t.Fatal("reorg from COMPLETE was accepted")
	}

	if !strings.Contains(err.Error(), "SECURITY FAILURE") ||
		!strings.Contains(err.Error(), "COMPLETE -> OFFERED") {
		t.Fatalf("refusal is not actionable: %v", err)
	}
}

func TestForwardCannotMoveBackward(t *testing.T) {
	// Every reorg edge must be refused by the forward path; this is the
	// property that stops a bug from rolling a swap backward.
	for from, targets := range map[State][]State{
		BlakeClaimed:  {BTCClaimed},
		BlakeRefunded: {BTCRefunded},
	} {
		for _, target := range targets {
			if from.CanTransitionTo(target) {
				t.Errorf("forward path allows %s -> %s", from, target)
			}
		}
	}
}

func TestTerminalStates(t *testing.T) {
	for _, s := range []State{Complete, BlakeRefunded} {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}

		if len(s.NextStates()) != 0 {
			t.Errorf("%s has forward edges", s)
		}
	}

	for _, s := range All {
		if s == Complete || s == BlakeRefunded {
			continue
		}

		if s.Terminal() {
			t.Errorf("%s should not be terminal", s)
		}

		if len(s.NextStates()) == 0 {
			t.Errorf("%s has no forward edges", s)
		}
	}
}

func TestParse(t *testing.T) {
	for _, s := range All {
		got, err := Parse(string(s))

		if err != nil || got != s {
			t.Errorf("Parse(%q) = %v, %v", s, got, err)
		}
	}

	for _, bad := range []string{"", "offered", "OFFER", "BLAKE_FUNDED ", "UNKNOWN"} {
		if got, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted it as %v", bad, got)
		}
	}
}

func TestNamesAreStable(t *testing.T) {
	// These strings are durable state and wire-adjacent: renaming one is a
	// migration, not a refactor.
	want := []string{
		"OFFERED", "BLAKE_FUNDED", "BTC_FUNDED", "SECRET_REVEALED",
		"BTC_CLAIMED", "BLAKE_CLAIMED", "COMPLETE", "BTC_REFUNDED",
		"BLAKE_REFUNDED",
	}

	if len(All) != len(want) {
		t.Fatalf("got %d states, want %d", len(All), len(want))
	}

	for i, s := range All {
		if string(s) != want[i] {
			t.Errorf("state %d = %q, want %q", i, s, want[i])
		}
	}
}
