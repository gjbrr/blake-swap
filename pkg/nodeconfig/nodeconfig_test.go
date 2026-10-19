//go:build !js

package nodeconfig_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gjbrr/blake-swap/pkg/nodeconfig"
)

// Parity: tests/node_config_tests.py
//
// The reference suite pins the resolution rules in a child interpreter, because
// its constants resolve at import time. These are the same rules against the same
// shape of host, described rather than probed: a fake filesystem and a fake
// environment make every case exact, and no case needs a node to exist.

// fakeHost is a described host: which paths are directories, which are
// executables, what PATH resolves to, and what the environment says.
type fakeHost struct {
	dirs   map[string]bool
	files  map[string]bool
	path   map[string]string
	env    map[string]string
	home   string
	root   string
}

// newHost is a host with nothing installed and no rig.
func newHost(home string) *fakeHost {
	return &fakeHost{
		dirs:  map[string]bool{},
		files: map[string]bool{},
		path:  map[string]string{},
		env:   map[string]string{},
		home:  home,
		root:  filepath.Join(home, ".blake-swap-test"),
	}
}

// dir records a directory that exists.
func (h *fakeHost) dir(paths ...string) *fakeHost {
	for _, path := range paths {
		h.dirs[path] = true
	}

	return h
}

// exe records an executable file that exists.
func (h *fakeHost) exe(path string) *fakeHost {
	h.files[path] = true
	h.dirs[filepath.Dir(path)] = true

	return h
}

// onPath records a bare name that resolves through PATH.
func (h *fakeHost) onPath(name, resolved string) *fakeHost {
	h.path[name] = resolved

	return h
}

// set records one environment variable.
func (h *fakeHost) set(name, value string) *fakeHost {
	h.env[name] = value

	return h
}

// resolve runs the resolver against this described host.
func (h *fakeHost) resolve() nodeconfig.Config {
	return nodeconfig.Resolve(nodeconfig.Options{
		Home:     h.home,
		Root:     h.root,
		Getenv:   func(name string) string { return h.env[name] },
		LookPath: func(name string) (string, error) { return h.path[name], nil },
		Stat: func(path string) (os.FileInfo, error) {
			if h.files[path] {
				// The rig reader opens the file for real, so a rig state file has to
				// exist on disk. That is arranged by writeRigFile.
				return nodeInfo{name: filepath.Base(path)}, nil
			}

			if h.dirs[path] {
				return nodeInfo{name: filepath.Base(path), dir: true}, nil
			}

			return nil, os.ErrNotExist
		},
	})
}

// nodeInfo is the minimal os.FileInfo the resolver consults.
type nodeInfo struct {
	name string
	dir  bool
}

func (n nodeInfo) Name() string       { return n.name }
func (n nodeInfo) Size() int64        { return 0 }
func (n nodeInfo) Mode() os.FileMode  { return 0o755 }
func (n nodeInfo) ModTime() time.Time { return time.Time{} }
func (n nodeInfo) IsDir() bool        { return n.dir }
func (n nodeInfo) Sys() any           { return nil }

// rig writes a real rig state file and returns the host describing it.
func rig(t *testing.T, running bool, withDatadirs bool) (*fakeHost, string, string) {
	t.Helper()

	root := filepath.Join(t.TempDir(), "rig")
	home := filepath.Join(t.TempDir(), "home")

	btc := filepath.Join(root, "btc")
	blake := filepath.Join(root, "blake")

	for _, dir := range []string{root, btc, blake} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	state := `{"running": ` + boolText(running) + `, "root": "` + root + `"`

	if withDatadirs {
		state += `, "btc_datadir": "` + btc + `", "blake_datadir": "` + blake + `"`
	}

	state += `}`

	if err := os.WriteFile(filepath.Join(root, "rig-env.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}

	host := newHost(home)
	host.root = root
	host.files[filepath.Join(root, "rig-env.json")] = true
	host.dir(root, btc, blake)

	return host, btc, blake
}

// boolText renders a JSON boolean.
func boolText(value bool) string {
	if value {
		return "true"
	}

	return "false"
}

// TestNodeConfigDiscoversARunningRig: with no environment at all, a rig that
// says it is running is the target, and it is exported to children.
func TestNodeConfigDiscoversARunningRig(t *testing.T) {
	host, btc, blake := rig(t, true, true)

	btcCLI := filepath.Join(t.TempDir(), "bitcoin-cli")
	blakeCLI := filepath.Join(t.TempDir(), "blake-cli")
	host.exe(btcCLI).exe(blakeCLI)
	host.set(nodeconfig.BTCEnvCLI, btcCLI)
	host.set(nodeconfig.BlakeEnvCLI, blakeCLI)

	config := host.resolve()

	if config.BTC.Datadir != btc || config.Blake.Datadir != blake {
		t.Fatalf("resolved %q / %q, want the rig's %q / %q",
			config.BTC.Datadir, config.Blake.Datadir, btc, blake)
	}

	if !config.RigRunning {
		t.Fatal("the rig is not reported as running")
	}

	if problems := config.Problems(); len(problems) != 0 {
		t.Fatalf("a running rig reports problems: %v", problems)
	}

	env := config.Env()

	if env[nodeconfig.BTCEnvDatadir] != btc || env[nodeconfig.BlakeEnvDatadir] != blake {
		t.Fatalf("the rig was not exported to children: %v", env)
	}
}

// TestNodeConfigStoppedRigIsNotATarget: a stopped rig contributes no candidate
// and is reported, rather than falling through to whatever datadir exists.
func TestNodeConfigStoppedRigIsNotATarget(t *testing.T) {
	host, btc, blake := rig(t, false, true)

	config := host.resolve()

	if config.BTC.Datadir == btc || config.Blake.Datadir == blake {
		t.Fatalf("a stopped rig was used as the target: %q / %q",
			config.BTC.Datadir, config.Blake.Datadir)
	}

	if config.RigRunning {
		t.Fatal("a stopped rig is reported as running")
	}

	if !hasFragment(config.Problems(), "rig is stopped") {
		t.Fatalf("a stopped rig does not explain itself: %v", config.Problems())
	}
}

// TestNodeConfigNoRigFileIsNotAStoppedRig: a checkout that never started a rig
// must not be told that its rig is stopped.
func TestNodeConfigNoRigFileIsNotAStoppedRig(t *testing.T) {
	host := newHost(t.TempDir())

	config := host.resolve()

	if config.RigPresent {
		t.Fatal("a rig is reported present in a checkout that has none")
	}

	if hasFragment(config.Problems(), "rig is stopped") {
		t.Fatalf("a checkout with no rig is told its rig is stopped: %v", config.Problems())
	}

	// A host with no CLI to find must say which one is missing rather than
	// silently resolving nothing.
	if config.BTC.CLI != "" || config.Blake.CLI != "" {
		t.Fatalf("a bare host resolved a CLI: %q / %q", config.BTC.CLI, config.Blake.CLI)
	}

	if !hasFragment(config.Problems(), "Bitcoin CLI unresolved") {
		t.Fatalf("an unresolved Bitcoin CLI is not reported: %v", config.Problems())
	}

	if !hasFragment(config.Problems(), "BLAKE CLI unresolved") {
		t.Fatalf("an unresolved BLAKE CLI is not reported: %v", config.Problems())
	}
}

// TestNodeConfigExplicitEnvironmentWins: one overridden chain comes from the
// environment, the other still comes from the rig.
func TestNodeConfigExplicitEnvironmentWins(t *testing.T) {
	host, _, blake := rig(t, true, true)

	override := filepath.Join(t.TempDir(), "explicit-datadir")
	if err := os.MkdirAll(override, 0o755); err != nil {
		t.Fatal(err)
	}

	host.dir(override)
	host.set(nodeconfig.BTCEnvDatadir, override)

	config := host.resolve()

	if config.BTC.Datadir != override {
		t.Fatalf("btc datadir = %q, want the override %q", config.BTC.Datadir, override)
	}

	if config.BTC.DatadirSource != nodeconfig.BTCEnvDatadir {
		t.Fatalf("btc datadir source = %q, want %q",
			config.BTC.DatadirSource, nodeconfig.BTCEnvDatadir)
	}

	if config.Blake.Datadir != blake {
		t.Fatalf("blake datadir = %q, want the rig's %q", config.Blake.Datadir, blake)
	}
}

// TestNodeConfigTypoIsADiagnostic: an explicit variable that does not resolve is
// reported as such instead of falling back to a different node.
func TestNodeConfigTypoIsADiagnostic(t *testing.T) {
	host := newHost(t.TempDir())

	host.set(nodeconfig.BTCEnvDatadir, "/nonexistent/typo")

	config := host.resolve()

	if config.BTC.Datadir != "" {
		t.Fatalf("a non-existent override resolved to %q", config.BTC.Datadir)
	}

	if !strings.Contains(config.BTC.DatadirSource, "not a directory") {
		t.Fatalf("the typo is not explained: %q", config.BTC.DatadirSource)
	}
}

// TestNodeConfigNeverRendersAnUnresolvedValue: nothing exports an empty or
// invented path, and the summary says <unresolved> rather than an empty string.
func TestNodeConfigNeverRendersAnUnresolvedValue(t *testing.T) {
	host := newHost(t.TempDir())

	config := host.resolve()
	env := config.Env()

	if len(env) != 0 {
		t.Fatalf("an unresolved host exported %v", env)
	}

	for name, value := range env {
		if value == "" || value == "None" || strings.Contains(value, "None") {
			t.Fatalf("%s exported a meaningless value %q", name, value)
		}
	}

	summary := strings.Join(config.Summary(), "\n")

	if !strings.Contains(summary, "<unresolved>") {
		t.Fatalf("the summary does not mark unresolved values:\n%s", summary)
	}

	if strings.Contains(summary, "None") {
		t.Fatalf("the summary renders a value as None:\n%s", summary)
	}
}

// TestNodeConfigMalformedStateDegrades: a corrupt rig file is not a crash and not
// a stop problem; it is simply no rig.
func TestNodeConfigMalformedStateDegrades(t *testing.T) {
	root := t.TempDir()

	if err := os.WriteFile(filepath.Join(root, "rig-env.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	host := newHost(t.TempDir())
	host.root = root
	host.files[filepath.Join(root, "rig-env.json")] = true

	config := host.resolve()

	if config.RigRunning {
		t.Fatal("a corrupt rig file is reported as running")
	}

	if hasFragment(config.Problems(), "rig is stopped") {
		t.Fatalf("a corrupt rig file is reported as stopped: %v", config.Problems())
	}
}

// TestNodeConfigPathResolution: a bare name resolves through PATH, and a path
// that does not exist does not.
func TestNodeConfigPathResolution(t *testing.T) {
	host := newHost(t.TempDir())

	resolved := filepath.Join(t.TempDir(), "real", "bitcoin-cli")
	host.onPath("bitcoin-cli", resolved)

	config := host.resolve()

	if config.BTC.CLI != resolved {
		t.Fatalf("btc CLI = %q, want the PATH hit %q", config.BTC.CLI, resolved)
	}

	// The BLAKE chain never consults PATH: a generic bitcoin-cli is almost
	// certainly Bitcoin Core.
	if config.Blake.CLI != "" {
		t.Fatalf("the BLAKE CLI resolved through PATH to %q", config.Blake.CLI)
	}
}

// TestNodeConfigRemoteNodeNeedsNoDatadir: RPC arguments stand in for a datadir.
func TestNodeConfigRemoteNodeNeedsNoDatadir(t *testing.T) {
	host := newHost(t.TempDir())

	btcCLI := filepath.Join(t.TempDir(), "bitcoin-cli")
	blakeCLI := filepath.Join(t.TempDir(), "blake-cli")

	host.exe(btcCLI).exe(blakeCLI)
	host.set(nodeconfig.BTCEnvCLI, btcCLI)
	host.set(nodeconfig.BlakeEnvCLI, blakeCLI)
	host.set(nodeconfig.BTCEnvRPCArgs, "-regtest -rpcconnect=host -rpcport=18443")
	host.set(nodeconfig.BlakeEnvRPCArgs, "-regtest -rpcconnect=host -rpcport=19443")

	config := host.resolve()

	if problems := config.Problems(); len(problems) != 0 {
		t.Fatalf("a remote node reports problems: %v", problems)
	}

	if len(config.BTC.RPCArgs) != 3 || config.BTC.RPCArgs[2] != "-rpcport=18443" {
		t.Fatalf("btc rpc args = %v", config.BTC.RPCArgs)
	}

	if config.Env()[nodeconfig.BTCEnvRPCArgs] == "" {
		t.Fatal("the rpc args were not exported")
	}
}

// hasFragment reports whether any problem contains a fragment.
func hasFragment(problems []string, fragment string) bool {
	for _, problem := range problems {
		if strings.Contains(problem, fragment) {
			return true
		}
	}

	return false
}
