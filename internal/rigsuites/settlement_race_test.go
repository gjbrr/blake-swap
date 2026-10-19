//go:build !js

package rigsuites

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/gjbrr/blake-swap/internal/rigtest"
)

// raceCase is one economic command to race against itself.
type raceCase struct {
	name    string
	command string
	actor   string
	field   string
	which   string
}

// Parity: tests/swap_settlement_race_tests.py
//
// Two invocations of the same economic command, released at the same instant. The
// reference gates two processes on a file so that both have read the same durable
// record before either acts. Here the two processes are started together and the
// engine's own locking and durable record are what prevent a second spend.
//
// What the race is allowed to do: one of them fails, or both succeed having
// adopted the same transaction. What it must never do: produce two HTLC spends.
func TestSettlementRace(t *testing.T) {
	r := rigtest.Load(t)

	cases := []raceCase{
		{name: "btc claim", command: "claim-btc", actor: "alice", field: "btc_claim_txid", which: "btc"},
		{name: "blake claim", command: "claim-blake", actor: "bob", field: "blake_claim_txid", which: "blake"},
		{name: "btc refund", command: "refund-btc", actor: "bob", field: "btc_refund_txid", which: "btc"},
		{name: "blake refund", command: "refund-blake", actor: "alice", field: "blake_refund_txid", which: "blake"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			operation := settlementOperationFor(c)

			scene := prepare(t, r, operation)

			// Both invocations are built before either starts, so the race is over
			// the work and not over process creation.
			commands := []*exec.Cmd{
				r.Command(t, scene.actor, scene.messages, nil, c.command, scene.id),
				r.Command(t, scene.actor, scene.messages, nil, c.command, scene.id),
			}

			outputs := make([]bytes.Buffer, len(commands))
			exits := make([]int, len(commands))
			failures := make([]error, len(commands))

			for index := range commands {
				commands[index].Stdout = &outputs[index]
				commands[index].Stderr = &outputs[index]
			}

			var wait sync.WaitGroup

			release := make(chan struct{})

			for index := range commands {
				wait.Add(1)

				go func(index int) {
					defer wait.Done()

					<-release

					if err := commands[index].Start(); err != nil {
						failures[index] = err

						return
					}

					failures[index] = commands[index].Wait()
				}(index)
			}

			close(release)
			wait.Wait()

			succeeded := 0

			for index := range commands {
				if failures[index] == nil {
					succeeded++

					continue
				}

				var exitErr *exec.ExitError

				if !errors.As(failures[index], &exitErr) {
					t.Fatalf("%s: process %d could not be run: %v",
						c.command, index, failures[index])
				}

				exits[index] = exitErr.ExitCode()
			}

			if succeeded == 0 {
				t.Fatalf("%s: both concurrent invocations failed:\n%s\n---\n%s",
					c.command, outputs[0].String(), outputs[1].String())
			}

			// The durable record names one transaction, and the chain agrees.
			txid, _ := rigtest.RecordField(t, scene.actor, scene.id, c.field)

			if fmt.Sprint(txid) == "" || fmt.Sprint(txid) == "<nil>" {
				t.Fatalf("%s: the race left no %s", c.command, c.field)
			}

			state, _ := rigtest.RecordField(t, scene.actor, scene.id, "state")

			if fmt.Sprint(state) == scene.sourceState && !strings.Contains(c.command, "refund") {
				// A claim against an unconfirmed transaction is legitimately still
				// pending; only the spender count below is decisive then.
				t.Logf("%s: still %s after the race, which is correct for an "+
					"unconfirmed settlement", c.command, scene.sourceState)
			}

			blockHash := scene.confirmedBlock(t, operation, fmt.Sprint(txid))

			if blockHash == "" {
				t.Fatalf("%s: the settlement is not confirmed", c.command)
			}

			// Exactly one transaction spent the HTLC, and it is the recorded one.
			spenders := scene.confirmedSpenders(t, operation)

			if len(spenders) != 1 {
				t.Fatalf("%s: %d transactions spent the HTLC: %v",
					c.command, len(spenders), spenders)
			}

			if spenders[0] != fmt.Sprint(txid) {
				t.Fatalf("%s: the HTLC was spent by %s, not the recorded %v",
					c.command, spenders[0], txid)
			}
		})
	}
}

// settlementOperationFor maps a race case onto the settlement coordinates the
// shared harness uses.
func settlementOperationFor(c raceCase) settlementOperation {
	for _, operation := range operations() {
		if operation.command == c.command {
			return operation
		}
	}

	panic("unknown settlement command: " + c.command)
}
