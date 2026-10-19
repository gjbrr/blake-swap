//go:build !js

package installcheck

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// The CLI's package path and executable name, as the toolchain derives them.
const (
	// cliPackage is the main package installed by the suite.
	cliPackage = "./cmd/blake-swap-cli"
	// cliName is the executable `go install` writes into GOBIN.
	cliName = "blake-swap-cli"
	// ModulePath is the module the installed artifact must say it came from.
	ModulePath = "github.com/gjbrr/blake-swap"
)

// runTimeout bounds one installed-binary invocation. It exists so a binary that
// hangs on a missing node or relay fails the test instead of the whole go test
// run. It is a deadline, not a sleep: a successful command returns immediately.
const runTimeout = 90 * time.Second

// scrubbedPath is the PATH handed to the installed binary.
//
// The binary is self-contained and spawns no helper programs, so it needs no
// PATH at all. A fixed system PATH is supplied anyway because the process
// contract is a scrubbed environment: inheriting the operator's PATH could hand
// the child a directory that lives inside the checkout.
const scrubbedPath = "/usr/local/bin:/usr/bin:/bin"

// WithheldEnvKeys names the environment variables the installed binary is
// deliberately not given, because each one would let it reach a node, a key, a
// relay or state that belongs to this checkout or this machine.
//
// Withholding the two datadirs is the load-bearing part: a command that could
// find a Bitcoin or BLAKE node would prove nothing about an installation that
// has neither. The rest keep a developer's shell from turning the test into an
// accidental integration run.
var WithheldEnvKeys = []string{
	"BLAKE_SWAP_BTC_DATADIR",
	"BLAKE_SWAP_BLAKE_DATADIR",
	"BLAKE_SWAP_PARTICIPANT_KEY_FILE",
	"BLAKE_SWAP_NOSTR_KEY_FILE",
	"BLAKE_SWAP_NOSTR_KEY_DIR",
	"BLAKE_SWAP_MESSAGE_DIR",
	"BLAKE_SWAP_NOSTR_ENCRYPTION",
	"BLAKE_SWAP_RPC_TIMEOUT_SECONDS",
	"BLAKE_SWAP_TEST_MODE",
	"BLAKE_SWAP_ROLE",
	"BLAKE_SWAP_BTC_WALLET",
	"BLAKE_SWAP_BLAKE_WALLET",
	"BLAKE_SWAP_AUTO_MINE",
	"BLAKE_SWAP_BTC_SPEND_FEE_SATS",
	"BLAKE_SWAP_BLAKE_SPEND_FEE_SATS",
	"BLAKE_SWAP_BTC_SPEND_FEE_RATE",
	"BLAKE_SWAP_BLAKE_SPEND_FEE_RATE",
	"SWAP_V2",
	"SWAP_AGENT",
	"PYTHONPATH",
}

// Installation is the result of installing this checkout's CLI.
type Installation struct {
	// Repo is the absolute path of the source checkout the binary was built
	// from. It is only used to prove the binary is not executed from there.
	Repo string
	// GOBIN is the temporary directory `go install` wrote into.
	GOBIN string
	// Binary is the absolute path of the installed executable.
	Binary string
}

// RunOptions is one invocation of the installed binary.
type RunOptions struct {
	// Dir is the process working directory. The suite always points it at a
	// fresh temporary directory outside the repository.
	Dir string
	// Env is the complete child environment, normally built with ScrubbedEnv.
	Env []string
}

// Result is the captured output of a successful invocation.
type Result struct {
	// Stdout is everything the process wrote to standard output.
	Stdout string
	// Stderr is everything the process wrote to standard error.
	Stderr string
}

// RepoRoot finds the checkout root by walking up from the working directory
// until it finds the module's go.mod.
func RepoRoot(t testing.TB) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("locate working directory: %v", err)
	}

	for {
		path := filepath.Join(dir, "go.mod")

		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return dir
		}

		parent := filepath.Dir(dir)

		if parent == dir {
			t.Fatalf("no go.mod above %s; run this suite inside the module", dir)
		}

		dir = parent
	}
}

// GoTool locates the go command without assuming it is on PATH.
//
// The suite itself is started by `go test`, often with a GOROOT that was
// exported only for that one command. runtime.GOROOT records the toolchain the
// test binary was built with, so its bin/go is the most faithful choice; a PATH
// lookup is the fallback and a missing toolchain is a test failure, never a skip.
func GoTool(t testing.TB) string {
	t.Helper()

	if root := runtime.GOROOT(); root != "" {
		candidate := filepath.Join(root, "bin", "go")

		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}

	found, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("no go command: runtime.GOROOT()=%q has no bin/go and exec.LookPath(\"go\") failed: %v",
			runtime.GOROOT(), err)
	}

	return found
}

// Install builds this checkout's CLI into a fresh temporary GOBIN using
// `go install`.
//
// `go install` is used rather than `go build -o` because it is the real
// installation path: the toolchain resolves the package from the module, links
// it and places the executable in GOBIN exactly as an operator's install would.
// A build failure is reported as a test failure, never skipped.
func Install(t testing.TB) *Installation {
	t.Helper()

	repo := RepoRoot(t)
	gobin := filepath.Join(t.TempDir(), "gobin")

	if err := os.MkdirAll(gobin, 0o755); err != nil {
		t.Fatalf("create temporary GOBIN %s: %v", gobin, err)
	}

	goTool := GoTool(t)

	cmd := exec.Command(goTool, "install", cliPackage)
	cmd.Dir = repo
	cmd.Env = installEnv(gobin)

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go install %s failed: %v\ncommand: %s\ncwd: %s\nGOBIN: %s\noutput:\n%s",
			cliPackage, err, cmd.String(), repo, gobin, output)
	}

	binary := filepath.Join(gobin, cliName)

	info, err := os.Stat(binary)
	if err != nil {
		t.Fatalf("go install succeeded but %s is missing: %v\noutput:\n%s", binary, err, output)
	}

	if !info.Mode().IsRegular() {
		t.Fatalf("installed artifact %s is not a regular file (mode %s)", binary, info.Mode())
	}

	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("installed artifact %s is not executable (mode %s)", binary, info.Mode())
	}

	return &Installation{Repo: repo, GOBIN: gobin, Binary: binary}
}

// installEnv is the environment for `go install`.
//
// Unlike the environment handed to the installed binary, this one inherits the
// operator's environment on purpose: the toolchain needs HOME (for the build
// cache), GOPATH/GOMODCACHE (for the module cache) and GOPROXY, and the point of
// the build is that it succeeds against the same module inputs an operator has.
// Only GOBIN, GOROOT and PATH are corrected.
func installEnv(gobin string) []string {
	env := append([]string{}, os.Environ()...)
	env = setEnv(env, "GOBIN", gobin)

	if root := runtime.GOROOT(); root != "" {
		env = setEnv(env, "GOROOT", root)
		env = setEnv(env, "PATH", filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	}

	return env
}

// Run executes the installed binary and fails the test if it does not succeed.
//
// The child receives exactly opts.Env, not a copy of this process's environment.
// That is what makes "the installed artifact needs nothing from the checkout"
// testable: there is no inherited PYTHONPATH, GOROOT, GOFLAGS or BLAKE_SWAP_*
// value the program could lean on.
func (i *Installation) Run(t testing.TB, opts RunOptions, args ...string) Result {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, i.Binary, args...)
	cmd.Dir = opts.Dir
	cmd.Env = opts.Env
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("installed binary timed out after %s: %s %v\ncwd: %s\nstdout:\n%s\nstderr:\n%s",
			runTimeout, i.Binary, args, opts.Dir, stdout.String(), stderr.String())
	}

	if err != nil {
		t.Fatalf("installed binary failed: %v\ncommand: %s %v\ncwd: %s\nenv: %v\nstdout:\n%s\nstderr:\n%s",
			err, i.Binary, args, opts.Dir, opts.Env, stdout.String(), stderr.String())
	}

	return Result{Stdout: stdout.String(), Stderr: stderr.String()}
}

// BuildInfo returns `go version -m` for the installed binary.
//
// The output embeds the main package path and the module the linker recorded,
// which is how the suite proves the artifact came from this checkout's sources
// rather than from some other binary that happened to be on PATH.
func (i *Installation) BuildInfo(t testing.TB, goTool string) string {
	t.Helper()

	cmd := exec.Command(goTool, "version", "-m", i.Binary)
	cmd.Dir = i.Repo

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go version -m %s failed: %v\noutput:\n%s", i.Binary, err, output)
	}

	return string(output)
}

// ScrubbedEnv builds the complete environment for one installed-binary run.
//
// It starts from nothing: only PATH, HOME and, when stateDir is non-empty,
// BLAKE_SWAP_STATE_DIR are present, plus the explicitly allow-listed extra
// variables the chosen command needs (for the market test, the loopback relay
// URL). Because the environment is constructed rather than filtered, every key
// in WithheldEnvKeys is absent by construction and cannot be reintroduced by a
// developer's shell.
//
// The extra map is intended for a single allow-listed variable per test; the
// tests assert that none of the withheld chain or key variables appear.
func ScrubbedEnv(home, stateDir string, extra map[string]string) []string {
	env := []string{
		"PATH=" + scrubbedPath,
		"HOME=" + home,
	}

	if stateDir != "" {
		env = append(env, "BLAKE_SWAP_STATE_DIR="+stateDir)
	}

	keys := make([]string, 0, len(extra))

	for key := range extra {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	for _, key := range keys {
		env = append(env, key+"="+extra[key])
	}

	return env
}

// EnvValue returns the value of key in env, or "" when the key is absent.
func EnvValue(env []string, key string) string {
	prefix := key + "="

	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}

	return ""
}

// IsWithin reports whether path is root or lives underneath it.
func IsWithin(root, path string) bool {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}

	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return false
	}

	relative, err := filepath.Rel(absoluteRoot, absolutePath)
	if err != nil {
		return false
	}

	return relative == "." ||
		(relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)))
}

// RepoFiles lists every regular file in the checkout.
//
// Symlinks are not followed, VCS metadata is skipped, and any walk error is a
// test failure: the caller uses this list to prove the installed binary is not
// the same file as anything in the repository, and a partial list would make
// that proof hollow.
func RepoFiles(t testing.TB, root string) []string {
	t.Helper()

	var files []string

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}

			return nil
		}

		if entry.Type().IsRegular() {
			files = append(files, path)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk repository %s: %v", root, err)
	}

	sort.Strings(files)

	return files
}

// setEnv returns env with key replaced or appended, leaving other entries alone.
func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := env[:0]
	replaced := false

	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			if !replaced {
				out = append(out, prefix+value)
				replaced = true
			}

			continue
		}

		out = append(out, entry)
	}

	if !replaced {
		out = append(out, prefix+value)
	}

	return out
}

// String renders the installation for failure messages and test logs.
func (i *Installation) String() string {
	return fmt.Sprintf("Installation{repo=%s, GOBIN=%s, binary=%s}", i.Repo, i.GOBIN, i.Binary)
}
