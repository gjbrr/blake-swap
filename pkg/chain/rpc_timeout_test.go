package chain

import (
	"strings"
	"testing"
	"time"
)

// This file mirrors the part of the reference's `tests/command_surface_tests.py` that pins
// BLAKE_SWAP_RPC_TIMEOUT_SECONDS. The value matters because of one failure mode: a node that
// accepts the connection and then never answers. Without a deadline the engine hangs with no
// error and nothing to retry, which is worse than a timeout — there is nothing to look at.

// TestRPCTimeoutDefaultsToTheReference: two minutes, the reference's default.
func TestRPCTimeoutDefaultsToTheReference(t *testing.T) {
	t.Setenv("BLAKE_SWAP_RPC_TIMEOUT_SECONDS", "")

	timeout, err := TimeoutFromEnv()

	if err != nil {
		t.Fatal(err)
	}

	if timeout != 120*time.Second {
		t.Fatalf("default = %s, want 120s", timeout)
	}
}

// TestRPCTimeoutAcceptsFractionalSeconds: a sub-second timeout is a legitimate test knob, and
// 7.5 seconds is the reference's own example.
func TestRPCTimeoutAcceptsFractionalSeconds(t *testing.T) {
	t.Setenv("BLAKE_SWAP_RPC_TIMEOUT_SECONDS", "7.5")

	timeout, err := TimeoutFromEnv()

	if err != nil {
		t.Fatal(err)
	}

	if timeout != 7500*time.Millisecond {
		t.Fatalf("timeout = %s, want 7.5s", timeout)
	}
}

// TestRPCTimeoutRefusesWhatCannotBeADeadline: a typo in a unit file fails where it is read
// rather than becoming a zero timeout nobody notices.
func TestRPCTimeoutRefusesWhatCannotBeADeadline(t *testing.T) {
	for name, value := range map[string]string{
		"a word":    "soon",
		"zero":      "0",
		"negative":  "-30",
		"empty-ish": "   ",
	} {
		t.Setenv("BLAKE_SWAP_RPC_TIMEOUT_SECONDS", value)

		timeout, err := TimeoutFromEnv()

		if value == "   " {
			// Whitespace is the unset case, not a malformed one.
			if err != nil || timeout != 120*time.Second {
				t.Errorf("%s: %s, %v", name, timeout, err)
			}

			continue
		}

		if err == nil {
			t.Errorf("%s: accepted, giving %s", name, timeout)

			continue
		}

		if !strings.Contains(err.Error(), "BLAKE_SWAP_RPC_TIMEOUT_SECONDS") {
			t.Errorf("%s: refusal does not name the variable: %v", name, err)
		}
	}
}
