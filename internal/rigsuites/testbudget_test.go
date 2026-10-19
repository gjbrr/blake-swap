//go:build !js

package rigsuites

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain gives this package a longer budget than the go tool's default.
//
// Every test in this package drives the real two-chain regtest rig as a
// subprocess, several participants at a time, with real blocks and real
// transactions. That is the point of them: a settlement crash, a reorg or a race
// cannot be proved any other way. The cost is wall-clock time, and the ten
// minutes the go tool allows a package by default is less than the sum of the
// suites that live here.
//
// Raising the budget is not hiding a hang: a test that hangs still hits this
// deadline, and it still reports which test was running when it did.
const testBudget = 45 * time.Minute

func TestMain(m *testing.M) {
	//
	// The budget has to be changed in os.Args, not through flag.Set: the go tool
	// passes -test.timeout on the command line, and the flag package parses that
	// argument after TestMain returns, overwriting anything set here.
	//
	replaced := false

	for index, argument := range os.Args {
		if strings.HasPrefix(argument, "-test.timeout=") {
			os.Args[index] = "-test.timeout=" + testBudget.String()

			replaced = true
		}
	}

	if !replaced {
		os.Args = append(os.Args, "-test.timeout="+testBudget.String())
	}

	os.Exit(m.Run())
}
