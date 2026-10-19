//go:build !js

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Parity: tests/node_config_tests.py
//
// The reference suite pins the resolution rules of the *harness*. This is the
// other half of the same contract: the CLI itself must obey them, because a rule
// that only the harness follows is a rule the tool does not have.
//
// Each case runs the built binary with a scrubbed environment against a described
// rig root, so nothing depends on the developer's real nodes.

// cli builds the CLI once per package test binary.
func cliForNodeConfig(t *testing.T) string {
	t.Helper()

	repo := repoRootForTest(t)
	binary := filepath.Join(t.TempDir(), "blake-swap-cli")

	build := exec.Command("go", "build", "-o", binary, "./cmd/blake-swap-cli")
	build.Dir = repo

	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the CLI: %v\n%s", err, output)
	}

	return binary
}

// repoRootForTest walks up to the module root.
func repoRootForTest(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()

	if err != nil {
		t.Fatal(err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)

		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}

		dir = parent
	}
}

// runCLI runs the binary with only the variables a bare checkout would have.
func runCLI(t *testing.T, binary, root string, args ...string) (string, error) {
	t.Helper()

	state := t.TempDir()
	keyFile := filepath.Join(state, "participant.key")

	if err := os.WriteFile(keyFile, []byte(strings.Repeat("11", 32)), 0o600); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(binary, args...)
	command.Dir = state
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + state,
		"BLAKE_SWAP_TEST_ROOT=" + root,
		"BLAKE_SWAP_STATE_DIR=" + state,
		"BLAKE_SWAP_PARTICIPANT_KEY_FILE=" + keyFile,
	}

	output, err := command.CombinedOutput()

	return string(output), err
}

// writeRigState writes a rig state file and returns its root.
func writeRigState(t *testing.T, running bool, datadirs map[string]string) string {
	t.Helper()

	root := t.TempDir()

	state := map[string]any{"running": running, "root": root}

	for key, value := range datadirs {
		state[key] = value
	}

	data, err := json.Marshal(state)

	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "rig-env.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	return root
}

// TestCLIReportsAStoppedRig is the rule that matters most: a stopped rig is a
// reason, not a fallback. The command must say the rig is stopped instead of
// connecting to whichever datadir happens to be there.
func TestCLIReportsAStoppedRig(t *testing.T) {
	binary := cliForNodeConfig(t)

	btc := t.TempDir()
	blake := t.TempDir()

	root := writeRigState(t, false, map[string]string{
		"btc_datadir": btc, "blake_datadir": blake,
	})

	output, err := runCLI(t, binary, root, "list")

	if err == nil {
		t.Fatalf("a command with a stopped rig succeeded:\n%s", output)
	}

	if !strings.Contains(output, "rig is stopped") {
		t.Fatalf("the stopped rig is not reported:\n%s", output)
	}

	// And it must not have quietly used the rig's datadirs.
	if strings.Contains(output, btc) || strings.Contains(output, blake) {
		t.Fatalf("the stopped rig's datadirs were used anyway:\n%s", output)
	}
}

// TestCLIUsesARunningRig: a rig that says it is running is the target, which is
// proved by the failure naming that datadir when the node is not actually there.
func TestCLIUsesARunningRig(t *testing.T) {
	binary := cliForNodeConfig(t)

	btc := t.TempDir()
	blake := t.TempDir()

	root := writeRigState(t, true, map[string]string{
		"btc_datadir": btc, "blake_datadir": blake,
	})

	output, err := runCLI(t, binary, root, "list")

	if err == nil {
		t.Fatalf("a command against empty datadirs succeeded:\n%s", output)
	}

	// The datadirs have no cookie, so the engine cannot build a node from them.
	// That failure is the proof it went to the rig rather than somewhere else.
	if !strings.Contains(output, btc) && !strings.Contains(output, blake) {
		t.Fatalf("the running rig's datadirs were not used:\n%s", output)
	}
}

// TestCLINeverExportsAnUnresolvedPath: with no rig at all, the message must name
// the variable to set rather than an empty or literal path.
func TestCLINeverExportsAnUnresolvedPath(t *testing.T) {
	binary := cliForNodeConfig(t)

	root := t.TempDir() // no rig-env.json

	output, err := runCLI(t, binary, root, "list")

	if err == nil {
		t.Fatalf("a command with no node configuration succeeded:\n%s", output)
	}

	if strings.Contains(output, "None") {
		t.Fatalf("an unresolved value was rendered as None:\n%s", output)
	}

	if !strings.Contains(output, "BLAKE_SWAP_BTC_DATADIR") &&
		!strings.Contains(output, "BLAKE_SWAP_BLAKE_DATADIR") {
		t.Fatalf("the message does not name the variable to set:\n%s", output)
	}

	// A checkout with no rig is never told its rig is stopped.
	if strings.Contains(output, "rig is stopped") {
		t.Fatalf("a checkout with no rig was told its rig is stopped:\n%s", output)
	}
}
