// Package nodeconfig resolves which chain nodes a command should talk to.
//
// The reference suites decide whether they can run at all from
// `tests/node_config.py`; the Go CLI has to make the same decision, and for the
// same reason. Getting it wrong is silent in the worst possible way: a command
// either claims to have done something while talking to nothing, or ignores a
// perfectly good regtest rig because discovery did not find it.
//
// The rules are the reference's, in the reference's order:
//
//  1. an explicit `BLAKE_SWAP_*` environment variable always wins, even when it
//     does not resolve, so a typo is a clear diagnostic rather than a silent
//     fallback to a different node;
//  2. a `scripts/test-nodes` rig that reports itself as *running* is the next
//     target;
//  3. conventional locations last.
//
// A rig whose state file says it is stopped is never treated as usable. It is
// reported as a problem instead: falling through to a unrelated datadir would
// run a suite against whatever happened to be there, which is exactly the failure
// this package exists to prevent.
package nodeconfig

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Environment variable names this package reads. They are the engine's own names.
const (
	BTCEnvCLI       = "BLAKE_SWAP_BTC_CLI"
	BTCEnvDatadir   = "BLAKE_SWAP_BTC_DATADIR"
	BTCEnvRPCArgs   = "BLAKE_SWAP_BTC_RPC_ARGS"
	BlakeEnvCLI     = "BLAKE_SWAP_BLAKE_CLI"
	BlakeEnvDatadir = "BLAKE_SWAP_BLAKE_DATADIR"
	BlakeEnvRPCArgs = "BLAKE_SWAP_BLAKE_RPC_ARGS"
	// TestRootEnv moves the rig root, which is how an isolated suite runs
	// without touching the developer's real rig.
	TestRootEnv = "BLAKE_SWAP_TEST_ROOT"
)

// Chain is one chain's resolved node configuration.
type Chain struct {
	// CLI is the node's command-line tool, or empty when nothing resolved.
	CLI string
	// CLISource says where CLI came from: the environment variable that set it,
	// the candidate path that matched, or an explanation when it did not resolve.
	CLISource string
	// Datadir is the node's data directory, or empty when nothing resolved.
	Datadir string
	// DatadirSource says where Datadir came from, on the same terms.
	DatadirSource string
	// RPCArgs is the remote-node argument list. The Go engine speaks JSON-RPC and
	// reads the port and cookie out of the datadir, so it does not consume these;
	// they are resolved because a harness that exports this configuration to a
	// child (including the Python engine) needs them.
	RPCArgs []string
}

// Config is the resolution for both chains.
type Config struct {
	// Root is the rig root, and RigFile the state file inside it.
	Root    string
	RigFile string
	// RigRunning is true only when the state file says so.
	RigRunning bool
	// RigPresent is true when a state file exists at all, running or not. It is
	// what distinguishes "this checkout never started a rig" from "this checkout's
	// rig is stopped": only the second is a problem worth reporting.
	RigPresent bool
	BTC        Chain
	Blake      Chain

	getenv func(string) string
}

// Options are the seams a test needs to resolve against a description of a host
// rather than the host itself.
type Options struct {
	// Home is the user's home directory. Empty means ask the OS.
	Home string
	// Root is the rig root. Empty means $BLAKE_SWAP_TEST_ROOT, then the
	// conventional location.
	Root string
	// Getenv reads one variable. Nil means os.Getenv.
	Getenv func(string) string
	// Stat reports whether a path exists and is a directory. Nil means os.Stat.
	Stat func(string) (os.FileInfo, error)
	// LookPath finds an executable on PATH. Nil means exec.LookPath.
	LookPath func(string) (string, error)
}

// Resolve applies the resolution rules and returns the configuration.
//
// It never returns an error: an unresolvable value is empty and carries the
// reason in its source field, so a caller can report every problem at once
// instead of one per attempt.
func Resolve(opts Options) Config {
	getenv := opts.Getenv

	if getenv == nil {
		getenv = os.Getenv
	}

	lookPath := opts.LookPath

	if lookPath == nil {
		lookPath = exec.LookPath
	}

	stat := opts.Stat

	if stat == nil {
		stat = os.Stat
	}

	home := opts.Home

	if home == "" {
		home, _ = os.UserHomeDir()
	}

	root := opts.Root

	if root == "" {
		root = getenv(TestRootEnv)
	}

	if root == "" {
		root = filepath.Join(home, ".blake-swap-test")
	}

	config := Config{
		Root:    root,
		RigFile: filepath.Join(root, "rig-env.json"),
		getenv:  getenv,
	}

	state, present, running := readRigState(config.RigFile, stat)

	config.RigPresent = present
	config.RigRunning = running

	// Only a running rig contributes candidates. A stopped one contributes none,
	// so the conventional locations are considered on their own merits rather than
	// silently borrowing the rig's stale paths.
	var rig map[string]string

	if running {
		rig = state
	}

	btcCLI, btcCLISource := resolveExecutable(getenv, lookPath, stat, BTCEnvCLI,
		rigCandidate(rig, "btc_cli", root, "btc"),
		[]string{
			filepath.Join(home, "s", "bitcoin-core", "bin", "bitcoin-cli"),
			filepath.Join(home, "s", "bitcoin", "bin", "bitcoin-cli"),
			filepath.Join(home, "bitcoin-core", "build", "bin", "bitcoin-cli"),
			filepath.Join(home, "bitcoin", "build", "bin", "bitcoin-cli"),
			"/usr/local/bin/bitcoin-cli",
			"/usr/bin/bitcoin-cli",
			"bitcoin-cli",
		},
	)

	btcDatadir, btcDatadirSource := resolveDatadir(getenv, stat, BTCEnvDatadir,
		rigCandidate(rig, "btc_datadir", root, "btc"),
		[]string{
			filepath.Join(home, "bitcoin-swap-regtest"),
			filepath.Join(home, ".bitcoin"),
		},
	)

	config.BTC = Chain{
		CLI:           btcCLI,
		CLISource:     btcCLISource,
		Datadir:       btcDatadir,
		DatadirSource: btcDatadirSource,
		RPCArgs:       splitArgs(getenv(BTCEnvRPCArgs)),
	}

	// The BLAKE CLI is deliberately not resolved through PATH: a generic
	// `bitcoin-cli` on PATH is almost certainly Bitcoin Core, and using it for the
	// BLAKE chain would silently test the wrong node.
	blakeCLI, blakeCLISource := resolveExecutable(getenv, lookPath, stat, BlakeEnvCLI,
		rigCandidate(rig, "blake_cli", root, "blake"),
		[]string{
			filepath.Join(home, "s", "bitcoin-blake2b", "bin", "bitcoin-cli"),
			filepath.Join(home, "s", "bitcoinknots", "bitcoin", "build", "bin", "bitcoin-cli"),
			filepath.Join(home, "s", "bitcoin-knots", "bin", "bitcoin-cli"),
			filepath.Join(home, "bitcoin-blake-rc2", "build-rc2", "bin", "bitcoin-cli"),
			filepath.Join(home, "bitcoin-blake-rc2", "build", "bin", "bitcoin-cli"),
			filepath.Join(home, "bitcoin-blake", "build", "bin", "bitcoin-cli"),
		},
	)

	blakeDatadir, blakeDatadirSource := resolveDatadir(getenv, stat, BlakeEnvDatadir,
		rigCandidate(rig, "blake_datadir", root, "blake"),
		[]string{
			filepath.Join(home, "blake-swap-regtest"),
			filepath.Join(home, ".blake-swap-regtest"),
		},
	)

	config.Blake = Chain{
		CLI:           blakeCLI,
		CLISource:     blakeCLISource,
		Datadir:       blakeDatadir,
		DatadirSource: blakeDatadirSource,
		RPCArgs:       splitArgs(getenv(BlakeEnvRPCArgs)),
	}

	return config
}

// rigCandidate is one rig path, or nothing when the rig is not running.
func rigCandidate(rig map[string]string, key, root, chain string) []string {
	if rig == nil {
		return nil
	}

	value := rig[key]

	if value == "" {
		value = filepath.Join(root, chain)
	}

	return []string{value}
}

// readRigState reads the file `scripts/test-nodes` writes.
//
// A corrupt or missing file degrades to "no rig", with no stop problem: a
// checkout that has never started a rig must not be told that its rig is
// stopped.
func readRigState(path string, stat func(string) (os.FileInfo, error)) (map[string]string, bool, bool) {
	if _, err := stat(path); err != nil {
		return nil, false, false
	}

	data, err := os.ReadFile(path)

	if err != nil {
		return nil, false, false
	}

	var state map[string]any

	if err := json.Unmarshal(data, &state); err != nil {
		// Unreadable degrades to "no rig", which is the reference's behaviour and
		// the only safe reading: a file that cannot be parsed cannot be trusted to
		// name a target, and calling it a stopped rig would report a problem the
		// operator cannot act on.
		return nil, false, false
	}

	flat := make(map[string]string, len(state))

	for key, value := range state {
		if text, ok := value.(string); ok {
			flat[key] = text
		}
	}

	running, _ := state["running"].(bool)

	return flat, true, running
}

// resolveExecutable resolves one chain CLI.
//
// An explicit environment variable wins even when it does not resolve: a typo
// must produce a clear diagnostic instead of silently falling back to a different
// node.
func resolveExecutable(
	getenv func(string) string,
	lookPath func(string) (string, error),
	stat func(string) (os.FileInfo, error),
	envVar string,
	groups ...[]string,
) (string, string) {
	explicit := getenv(envVar)

	if explicit != "" {
		if found := existingExecutable(explicit, lookPath, stat); found != "" {
			return found, envVar
		}

		return "", envVar + "=" + explicit + " (not executable)"
	}

	for _, group := range groups {
		for _, candidate := range group {
			if found := existingExecutable(candidate, lookPath, stat); found != "" {
				return found, candidate
			}
		}
	}

	return "", ""
}

// existingExecutable resolves one path or bare name to an executable.
func existingExecutable(value string, lookPath func(string) (string, error), stat func(string) (os.FileInfo, error)) string {
	if value == "" {
		return ""
	}

	if info, err := stat(value); err == nil && !info.IsDir() {
		return value
	}

	// A bare name (for example `bitcoin-cli`) is resolved through PATH. A path
	// that does not exist is not, because a caller who typed a path meant that
	// path.
	if strings.ContainsRune(value, os.PathSeparator) {
		return ""
	}

	if found, err := lookPath(value); err == nil {
		return found
	}

	return ""
}

// resolveDatadir resolves one chain data directory.
func resolveDatadir(
	getenv func(string) string, stat func(string) (os.FileInfo, error),
	envVar string, groups ...[]string,
) (string, string) {
	explicit := getenv(envVar)

	if explicit != "" {
		if info, err := stat(explicit); err == nil && info.IsDir() {
			return explicit, envVar
		}

		return "", envVar + "=" + explicit + " (not a directory)"
	}

	for _, group := range groups {
		for _, candidate := range group {
			if candidate == "" {
				continue
			}

			if info, err := stat(candidate); err == nil && info.IsDir() {
				return candidate, candidate
			}
		}
	}

	return "", ""
}

// splitArgs splits a remote-node argument list the way a shell would.
func splitArgs(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	return strings.Fields(raw)
}

// Problems lists what is unresolved, and why.
//
// A datadir is only required when no explicit RPC arguments are supplied, because
// a remote node supplies its connection arguments instead.
func (c Config) Problems() []string {
	var problems []string

	// A rig state file that says "stopped" must be reported rather than quietly
	// worked around: the rig is the intended target, and a generic datadir is not
	// a substitute for a regtest chain a command can spend on.
	if c.RigPresent && !c.RigRunning {
		problems = append(problems,
			"The scripts/test-nodes rig is stopped. Start it with "+
				"`scripts/test-nodes start`.",
		)
	}

	if c.BTC.CLI == "" {
		problems = append(problems,
			"Bitcoin CLI unresolved. Set "+BTCEnvCLI+" to your bitcoin-cli path.",
		)
	}

	if c.Blake.CLI == "" {
		problems = append(problems,
			"BLAKE CLI unresolved. Set "+BlakeEnvCLI+" to the BLAKE build's "+
				"bitcoin-cli (not Bitcoin Core).",
		)
	}

	if c.BTC.CLI != "" && c.BTC.Datadir == "" && len(c.BTC.RPCArgs) == 0 {
		problems = append(problems,
			"Bitcoin datadir unresolved. Set "+BTCEnvDatadir+", or set "+
				BTCEnvRPCArgs+" for a remote node.",
		)
	}

	if c.Blake.CLI != "" && c.Blake.Datadir == "" && len(c.Blake.RPCArgs) == 0 {
		problems = append(problems,
			"BLAKE datadir unresolved. Set "+BlakeEnvDatadir+", or set "+
				BlakeEnvRPCArgs+" for a remote node.",
		)
	}

	return problems
}

// Env is the environment that points a child process at the resolved nodes.
//
// Only resolved values are included. An unresolved value must never be passed on
// as an empty or literal `"None"` path: doing so would shadow the engine's own
// defaults with something meaningless.
func (c Config) Env() map[string]string {
	env := map[string]string{}

	for variable, value := range map[string]string{
		BTCEnvCLI:       c.BTC.CLI,
		BlakeEnvCLI:     c.Blake.CLI,
		BTCEnvDatadir:   c.BTC.Datadir,
		BlakeEnvDatadir: c.Blake.Datadir,
	} {
		if value != "" {
			env[variable] = value
		}
	}

	if len(c.BTC.RPCArgs) > 0 {
		env[BTCEnvRPCArgs] = strings.Join(c.BTC.RPCArgs, " ")
	}

	if len(c.Blake.RPCArgs) > 0 {
		env[BlakeEnvRPCArgs] = strings.Join(c.Blake.RPCArgs, " ")
	}

	return env
}

// Summary is the human-readable resolution, one line per value.
func (c Config) Summary() []string {
	shown := func(path, source string) string {
		if path != "" {
			return path + "  [" + source + "]"
		}

		if source == "" {
			source = "no candidate found"
		}

		return "<unresolved>  [" + source + "]"
	}

	return []string{
		"Bitcoin CLI:      " + shown(c.BTC.CLI, c.BTC.CLISource),
		"Bitcoin datadir:  " + shown(c.BTC.Datadir, c.BTC.DatadirSource),
		"Bitcoin RPC args: " + formatArgs(c.BTC.RPCArgs),
		"BLAKE CLI:        " + shown(c.Blake.CLI, c.Blake.CLISource),
		"BLAKE datadir:    " + shown(c.Blake.Datadir, c.Blake.DatadirSource),
		"BLAKE RPC args:   " + formatArgs(c.Blake.RPCArgs),
	}
}

// formatArgs renders an argument list the way the reference's summary does.
func formatArgs(args []string) string {
	quoted := make([]string, 0, len(args))

	for _, arg := range args {
		quoted = append(quoted, "'"+arg+"'")
	}

	return "[" + strings.Join(quoted, ", ") + "]"
}
