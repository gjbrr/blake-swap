//go:build !js

package packaging

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Parity: tests/packaging_tests.py
//
// The reference suite checks how a Python distribution is declared, built and
// verified: pyproject.toml against the sources, requirements.txt against the
// extras, LICENSE, the modules in src/, the console entry points, and the CI
// workflow against the commands it claims to run. This file checks the same
// invariants against this repository's Go artefacts: go.mod, go.sum, the
// Makefile, .github/workflows/ci.yml, LICENSE and the package tree under pkg/
// and cmd/.
//
// The assertions that are structural to a Python distribution have no Go
// artefact to inspect. They are not faked: waived records each one with the
// Python assertion it replaces and a concrete reason, and
// TestWaivedPythonAssertionsAreDocumented enforces that the record stays
// specific.

// waivedCheck is one Python packaging assertion that this Go analogue cannot
// check against a Go artefact.
type waivedCheck struct {
	name   string
	reason string
}

// waived is the documented waiver table. Each reason names the Python
// assertion it replaces and says what the Go analogue is instead.
var waived = []waivedCheck{
	{
		name:   "pyproject.toml project metadata",
		reason: "tests/packaging_tests.py::test_pyproject_declares_the_project asserts name, version, readme, licence and requires-python in a Python distribution that this repository does not build; the Go declaration of the project is go.mod, whose module path and go directive TestGoModDeclaresTheModule checks, so there is no second metadata file to keep in step.",
	},
	{
		name:   "runtime and crypto dependency pins",
		reason: "tests/packaging_tests.py::test_dependencies_match_requirements keeps pyproject.toml's runtime dependencies and crypto extra equal to the pins in requirements.txt; this repository installs neither, and its dependency graph is declared by the require blocks of go.mod and pinned by the h1 hashes in go.sum, which TestGoModDeclaresTheModule and TestGoSumIsIntact check.",
	},
	{
		name:   "console entry points",
		reason: "tests/packaging_tests.py::test_console_scripts_are_declared pins the installed blake-swap-engine and blake-swap-agent entry points to swap_v2:main and swap_agent:main; the Go port ships executables built from the main packages under cmd/, not installed console-script declarations, so there is no entry-point table to compare and TestEveryGoPackageDirectoryBelongsToTheModule checks the source tree those binaries are built from.",
	},
	{
		name:   "README Python version range",
		reason: "tests/packaging_tests.py::test_readme_python_range_matches_pyproject requires the README to advertise the supported Python 3.12-3.13 range and to drop the obsolete 3.10+ claim; this README documents the Go and TinyGo toolchains, and asserting Python version wording against it would test the reference implementation rather than this repository's Go artefacts.",
	},
	{
		name:   "src/ module manifest",
		reason: "tests/packaging_tests.py::test_declared_modules_match_src compares pyproject.toml's py-modules list with the modules present in src/; the Go build has no declared module list to compare against, because a package exists exactly when a directory of .go files is inside the module, which is the property TestEveryGoPackageDirectoryBelongsToTheModule checks.",
	},
	{
		name:   "installed wheel contents",
		reason: "tests/installed_wheel_tests.py::test_wheel_install builds a Python wheel and asserts its contents, while tests/packaging_tests.py::test_declared_modules_match_src fixes the module list that wheel would carry; this repository builds wasm modules and a TinyGo server binary rather than a Python wheel, so there is no wheel to unpack, and the installable set is the Go package tree checked by TestEveryGoPackageDirectoryBelongsToTheModule.",
	},
}

// repoRoot finds the checkout by walking up from the test's working directory
// until go.mod appears. No path is hardcoded, so the suite works from any
// checkout and from a worktree.
func repoRoot(t *testing.T) string {
	t.Helper()

	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}

	root, err := FindRoot(workingDirectory)
	if err != nil {
		t.Fatalf("find repository root from %s: %v", workingDirectory, err)
	}

	return root
}

// writeTree creates a temporary directory containing the named files, keyed by
// slash-separated relative path, and returns its root. It is how the negative
// cases describe a malformed repository without touching the real one.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()

	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))

		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create parent of %s: %v", name, err)
		}

		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	return root
}

// mustReadFile reads a repository file relative to root.
func mustReadFile(t *testing.T, root, name string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}

	return string(data)
}

// TestGoModDeclaresTheModule checks the first invariant: go.mod parses,
// declares github.com/gjbrr/blake-swap, and carries a go directive.
func TestGoModDeclaresTheModule(t *testing.T) {
	root := repoRoot(t)

	if err := CheckGoMod(root); err != nil {
		t.Fatalf("the repository's go.mod is not a valid module declaration: %v", err)
	}

	module, goVersion, err := parseGoMod(mustReadFile(t, root, goModName))
	if err != nil {
		t.Fatalf("parse the repository's go.mod: %v", err)
	}

	t.Logf("found module %q with go directive %q", module, goVersion)

	const valid = "module github.com/gjbrr/blake-swap\n\ngo 1.25.3\n"

	tests := []struct {
		name        string
		goMod       string
		wantInError string
	}{
		{name: "valid", goMod: valid},
		{
			name:        "no module directive",
			goMod:       "go 1.25.3\n",
			wantInError: "no module directive",
		},
		{
			name:        "no go directive",
			goMod:       "module github.com/gjbrr/blake-swap\n",
			wantInError: "no go directive",
		},
		{
			name:        "wrong module path",
			goMod:       "module example.com/other\n\ngo 1.25.3\n",
			wantInError: "example.com/other",
		},
		{
			name:        "duplicate module directive",
			goMod:       "module github.com/gjbrr/blake-swap\nmodule github.com/gjbrr/blake-swap\n\ngo 1.25.3\n",
			wantInError: "duplicate module directive",
		},
		{
			name:        "module directive without a path",
			goMod:       "module\n\ngo 1.25.3\n",
			wantInError: "module directive must be a single path",
		},
		{
			name:        "go directive is not a version",
			goMod:       "module github.com/gjbrr/blake-swap\n\ngo banana\n",
			wantInError: "not a Go version",
		},
		{
			name:        "duplicate go directive",
			goMod:       "module github.com/gjbrr/blake-swap\n\ngo 1.25.3\ngo 1.26\n",
			wantInError: "duplicate go directive",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := writeTree(t, map[string]string{"go.mod": test.goMod})

			err := CheckGoMod(fixture)
			if test.wantInError == "" {
				if err != nil {
					t.Fatalf("valid go.mod rejected: %v", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("malformed go.mod accepted; want an error mentioning %q", test.wantInError)
			}

			if !strings.Contains(err.Error(), test.wantInError) {
				t.Fatalf("error %q does not mention %q", err, test.wantInError)
			}
		})
	}

	t.Run("missing go.mod", func(t *testing.T) {
		if err := CheckGoMod(t.TempDir()); err == nil {
			t.Fatal("a root without go.mod was accepted")
		}
	})
}

// TestGoSumIsIntact checks the second invariant: go.sum exists, is non-empty,
// and every non-comment line has the documented three-field shape with an h1:
// hash.
func TestGoSumIsIntact(t *testing.T) {
	root := repoRoot(t)

	if err := CheckGoSum(root); err != nil {
		t.Fatalf("the repository's go.sum is not intact: %v", err)
	}

	entries := 0

	for _, line := range strings.Split(mustReadFile(t, root, goSumName), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			entries++
		}
	}

	t.Logf("go.sum has %d entries", entries)

	const hash = "h1:BFJXAMqAt4Z/PJJTP+6xHxboIy5mSrYm+US5VGD1NKY="
	const valid = "example.com/mod v1.2.3 " + hash + "\n" +
		"example.com/mod v1.2.3/go.mod " + hash + "\n"

	tests := []struct {
		name        string
		goSum       string
		wantInError string
	}{
		{name: "valid", goSum: valid},
		{
			name:        "comments are allowed",
			goSum:       "# a comment\n" + valid,
			wantInError: "",
		},
		{
			name:        "empty file",
			goSum:       "",
			wantInError: "has no entries",
		},
		{
			name:        "truncated line",
			goSum:       "example.com/mod v1.2.3\n",
			wantInError: "want 3 fields",
		},
		{
			name:        "missing hash",
			goSum:       "example.com/mod v1.2.3 " + hash + " extra\n",
			wantInError: "want 3 fields",
		},
		{
			name:        "unsupported hash algorithm",
			goSum:       "example.com/mod v1.2.3 h2:abcdef\n",
			wantInError: "unsupported algorithm",
		},
		{
			name:        "hash without an algorithm prefix",
			goSum:       "example.com/mod v1.2.3 " + strings.TrimPrefix(hash, "h1:") + "\n",
			wantInError: "no algorithm prefix",
		},
		{
			name:        "hash is not base64",
			goSum:       "example.com/mod v1.2.3 h1:not-base64!\n",
			wantInError: "not valid base64",
		},
		{
			name:        "hash is not a SHA-256 digest",
			goSum:       "example.com/mod v1.2.3 h1:YWJj\n",
			wantInError: "32-byte SHA-256",
		},
		{
			name:        "version is not v-prefixed",
			goSum:       "example.com/mod 1.2.3 " + hash + "\n",
			wantInError: "v-prefixed",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := writeTree(t, map[string]string{"go.sum": test.goSum})

			err := CheckGoSum(fixture)
			if test.wantInError == "" {
				if err != nil {
					t.Fatalf("valid go.sum rejected: %v", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("malformed go.sum accepted; want an error mentioning %q", test.wantInError)
			}

			if !strings.Contains(err.Error(), test.wantInError) {
				t.Fatalf("error %q does not mention %q", err, test.wantInError)
			}
		})
	}

	t.Run("missing go.sum", func(t *testing.T) {
		if err := CheckGoSum(t.TempDir()); err == nil {
			t.Fatal("a root without go.sum was accepted")
		}
	})
}

// TestDocumentedMakeTargetsExist checks the third invariant: every
// `make <target>` written in README.md or under docs/ is a target the Makefile
// defines.
func TestDocumentedMakeTargetsExist(t *testing.T) {
	root := repoRoot(t)

	if err := CheckDocumentedMakeTargets(root); err != nil {
		t.Fatalf("the documentation names make targets the Makefile lacks: %v", err)
	}

	documents, err := documentationFiles(root)
	if err != nil {
		t.Fatalf("list documentation: %v", err)
	}

	documented := map[string]string{}

	for _, document := range documents {
		for _, mention := range documentedMakeTargets(document.text) {
			if _, ok := documented[mention.target]; !ok {
				documented[mention.target] = document.name + ":" + strconv.Itoa(mention.line)
			}
		}
	}

	t.Logf("documented make targets: %v", sortedKeys(documented))
	t.Logf("Makefile targets: %v", sortedKeysStringSet(makefileTargets(mustReadFile(t, root, makefileName))))

	tests := []struct {
		name        string
		files       map[string]string
		wantInError string
	}{
		{
			name: "all documented targets exist",
			files: map[string]string{
				"Makefile":     ".PHONY: all wasm\n\nall: wasm\n\nwasm:\n\ttrue\n",
				"README.md":    "```bash\nmake\nmake all\nmake wasm\n```\n",
				"docs/use.md":  "Run `make wasm` to build.\n",
				"docs/note.md": "This would make the event invalid, and make a mess.\n",
			},
		},
		{
			name: "missing target in a fenced block",
			files: map[string]string{
				"Makefile":  "all:\n\ttrue\n",
				"README.md": "```bash\nmake missing\n```\n",
				"docs/x.md": "nothing here\n",
			},
			wantInError: "missing",
		},
		{
			name: "missing target in an inline code span",
			files: map[string]string{
				"Makefile":  "all:\n\ttrue\n",
				"README.md": "Run `make verify` first.\n",
				"docs/x.md": "nothing here\n",
			},
			wantInError: "verify",
		},
		{
			name: "prose is not documentation",
			files: map[string]string{
				"Makefile":  "all:\n\ttrue\n",
				"README.md": "This does not make the historical event invalid.\n",
				"docs/x.md": "A merge claim can make refunds impossible.\n",
			},
		},
		{
			name: "makefile without targets",
			files: map[string]string{
				"Makefile":  "# only a comment\n",
				"README.md": "No targets.\n",
				"docs/x.md": "No targets.\n",
			},
			wantInError: "defines no targets",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := CheckDocumentedMakeTargets(writeTree(t, test.files))

			if test.wantInError == "" {
				if err != nil {
					t.Fatalf("valid documentation rejected: %v", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("missing target accepted; want an error mentioning %q", test.wantInError)
			}

			if !strings.Contains(err.Error(), test.wantInError) {
				t.Fatalf("error %q does not mention %q", err, test.wantInError)
			}
		})
	}
}

// TestCIWorkflowParses checks the first part of the fourth invariant: the CI
// workflow is valid YAML and declares a test job.
func TestCIWorkflowParses(t *testing.T) {
	root := repoRoot(t)

	if err := CheckWorkflowParses(root); err != nil {
		t.Fatalf("the repository's CI workflow does not parse: %v", err)
	}

	document, _, err := loadWorkflow(root)
	if err != nil {
		t.Fatalf("load the repository's CI workflow: %v", err)
	}

	jobs, _ := document.field("jobs")
	t.Logf("workflow jobs: %v", jobs.keys)

	valid := "name: fixture\n" +
		"on:\n" +
		"  push:\n" +
		"jobs:\n" +
		"  test:\n" +
		"    runs-on: ubuntu-24.04\n" +
		"    steps:\n" +
		"      - run: |\n" +
		"          make wasm\n" +
		"          scripts/test.sh --unit-only\n"

	parsed, err := parseYAML(valid)
	if err != nil {
		t.Fatalf("valid fixture rejected: %v", err)
	}

	block, ok := parsed.field("jobs")
	if !ok {
		t.Fatal("valid fixture lost its jobs key")
	}

	testJob, ok := block.field("test")
	if !ok {
		t.Fatal("valid fixture lost its test job")
	}

	steps, ok := testJob.field("steps")
	if !ok || steps.kind != yamlSequence || len(steps.items) != 1 {
		t.Fatalf("valid fixture did not parse its step list: %+v", steps)
	}

	scripts := steps.items[0].fields["run"]
	if scripts == nil || !strings.Contains(scripts.scalar, "make wasm") {
		t.Fatalf("block scalar content was not preserved: %+v", scripts)
	}

	tests := []struct {
		name        string
		workflow    string
		wantInError string
	}{
		{
			name:        "empty document",
			workflow:    "",
			wantInError: "no content",
		},
		{
			name:        "tab indentation",
			workflow:    "jobs:\n\ttest:\n",
			wantInError: "tab used for indentation",
		},
		{
			name:        "missing colon",
			workflow:    "jobs:\n  test\n",
			wantInError: "expected 'key: value'",
		},
		{
			name:        "duplicate key",
			workflow:    "jobs:\n  test:\n  test:\n",
			wantInError: "duplicate mapping key",
		},
		{
			name:        "inconsistent indentation",
			workflow:    "jobs:\n  test:\n     runs-on: ubuntu-24.04\n    steps: []\n",
			wantInError: "unexpected indentation",
		},
		{
			name:        "unterminated quoted scalar",
			workflow:    "name: \"unterminated\n",
			wantInError: "unterminated quoted scalar",
		},
		{
			name:        "sequence item at mapping indentation",
			workflow:    "jobs:\n  test: yes\n  - item\n",
			wantInError: "sequence item at mapping indentation",
		},
		{
			name:        "no jobs",
			workflow:    "name: fixture\n",
			wantInError: "declares no jobs",
		},
		{
			name:        "no test job",
			workflow:    "jobs:\n  build:\n    runs-on: ubuntu-24.04\n",
			wantInError: "no test job",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := writeTree(t, map[string]string{
				".github/workflows/ci.yml": test.workflow,
			})

			err := CheckWorkflowParses(fixture)
			if err == nil {
				t.Fatalf("malformed workflow accepted; want an error mentioning %q", test.wantInError)
			}

			if !strings.Contains(err.Error(), test.wantInError) {
				t.Fatalf("error %q does not mention %q", err, test.wantInError)
			}
		})
	}
}

// TestCIRunnerPins checks the runner-pin part of the fourth invariant: every
// runs-on value is a versioned image, not a bare distro name.
func TestCIRunnerPins(t *testing.T) {
	root := repoRoot(t)

	if err := CheckCIRunnerPins(root); err != nil {
		t.Fatalf("the repository's CI workflow does not pin its runner: %v", err)
	}

	document, _, err := loadWorkflow(root)
	if err != nil {
		t.Fatalf("load the repository's CI workflow: %v", err)
	}

	t.Logf("runs-on values in use: %v", runnerImages(document))

	tests := []struct {
		name        string
		runsOn      string
		wantInError string
	}{
		{name: "ubuntu-24.04", runsOn: "ubuntu-24.04"},
		{name: "ubuntu-latest", runsOn: "ubuntu-latest"},
		{name: "macos-14", runsOn: "macos-14"},
		{
			name:        "bare ubuntu",
			runsOn:      "ubuntu",
			wantInError: "unpinned runner",
		},
		{
			name:        "self-hosted",
			runsOn:      "self-hosted",
			wantInError: "unpinned runner",
		},
		{
			name:        "matrix expression",
			runsOn:      "${{ matrix.os }}",
			wantInError: "unpinned runner",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := writeTree(t, map[string]string{
				".github/workflows/ci.yml": "jobs:\n  test:\n    runs-on: " + test.runsOn + "\n",
			})

			err := CheckCIRunnerPins(fixture)

			if test.wantInError == "" {
				if err != nil {
					t.Fatalf("pinned runner rejected: %v", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("unpinned runner accepted; want an error mentioning %q", test.wantInError)
			}

			if !strings.Contains(err.Error(), test.wantInError) {
				t.Fatalf("error %q does not mention %q", err, test.wantInError)
			}
		})
	}

	t.Run("one unpinned job among pinned jobs", func(t *testing.T) {
		fixture := writeTree(t, map[string]string{
			".github/workflows/ci.yml": "jobs:\n" +
				"  test:\n    runs-on: ubuntu-24.04\n" +
				"  lint:\n    runs-on: ubuntu\n",
		})

		err := CheckCIRunnerPins(fixture)
		if err == nil || !strings.Contains(err.Error(), "ubuntu") {
			t.Fatalf("an unpinned second job was not reported: %v", err)
		}
	})
}

// TestCIProvisionsBothNodeBinaries checks the node-provisioning part of the
// fourth invariant: the workflow downloads a Bitcoin Core build and a
// BLAKE2b-chain build, from distinct hosts, with their checksums verified.
func TestCIProvisionsBothNodeBinaries(t *testing.T) {
	root := repoRoot(t)

	if err := CheckCIProvisionsBothNodeBinaries(root); err != nil {
		t.Fatalf("the repository's CI workflow does not provision both node builds: %v", err)
	}

	text := mustReadFile(t, root, ciWorkflow)
	active := strings.Join(activeLines(text), "\n")

	t.Logf("Bitcoin Core marker: %q", firstMarker(active, coreNodeMarkers))
	t.Logf("BLAKE2b node marker: %q", firstMarker(active, blakeNodeMarkers))
	t.Logf("node download hosts: %v", nodeDownloadHosts(active))
	t.Logf("node pins: %v", workflowPins(text))

	if !strings.Contains(strings.ToLower(active), "bitcoin-blake2b") {
		t.Logf("note: the workflow provisions the BLAKE2b chain with the repository's Bitcoin Knots build; scripts/test-nodes prefers $HOME/s/bitcoin-blake2b when it is installed")
	}

	const (
		coreSHA  = "4753cd70416c629a2090503e8545a3c05e94a0f5037f8fb97b4c35c6e1afee0e"
		blakeSHA = "b59d0445a317e21a03dc29425db3aba79b27d5125230b1a2b1dce62e120827c5"
	)

	baseEnv := "env:\n" +
		"  CORE_VERSION: \"30.3\"\n" +
		"  BLAKE_VERSION: \"29.4.2.knots20260508\"\n"

	baseRun := "    steps:\n" +
		"      - run: |\n" +
		"          curl -fsSLO \"https://bitcoincore.org/bin/bitcoin-core-${CORE_VERSION}/x.tar.gz\"\n" +
		"          curl -fsSLO \"https://bitcoinknots.org/files/29.x/${BLAKE_VERSION}/bitcoin-${BLAKE_VERSION}.tar.gz\"\n"

	tests := []struct {
		name        string
		workflow    string
		wantInError string
	}{
		{
			name: "both chains from distinct hosts",
			workflow: baseEnv +
				"  CORE_SHA256: \"" + coreSHA + "\"\n" +
				"  BLAKE_SHA256: \"" + blakeSHA + "\"\n" +
				"jobs:\n  test:\n    runs-on: ubuntu-24.04\n" + baseRun +
				"          echo \"${CORE_SHA256}\" | sha256sum -c -\n" +
				"          echo \"${BLAKE_SHA256}\" | sha256sum -c -\n",
		},
		{
			name: "only a Bitcoin Core build",
			workflow: baseEnv +
				"jobs:\n  test:\n    runs-on: ubuntu-24.04\n" +
				"    steps:\n" +
				"      - run: |\n" +
				"          curl -fsSLO \"https://bitcoincore.org/bin/bitcoin-core-${CORE_VERSION}/x.tar.gz\"\n" +
				"          echo \"${CORE_SHA256}\" | sha256sum -c -\n",
			wantInError: "BLAKE2b node build",
		},
		{
			name: "both chains from one host",
			workflow: baseEnv +
				"jobs:\n  test:\n    runs-on: ubuntu-24.04\n" +
				"    steps:\n" +
				"      - run: |\n" +
				"          curl -fsSLO \"https://example.org/bitcoin-core-${CORE_VERSION}/x.tar.gz\"\n" +
				"          curl -fsSLO \"https://example.org/bitcoin-knots-${BLAKE_VERSION}/x.tar.gz\"\n" +
				"          echo \"${CORE_SHA256}\" | sha256sum -c -\n",
			wantInError: "distinct hosts",
		},
		{
			name: "no checksum verification",
			workflow: baseEnv +
				"jobs:\n  test:\n    runs-on: ubuntu-24.04\n" + baseRun,
			wantInError: "sha256sum -c -",
		},
		{
			name: "same checksum for both binaries",
			workflow: baseEnv +
				"  CORE_SHA256: \"" + coreSHA + "\"\n" +
				"  BLAKE_SHA256: \"" + coreSHA + "\"\n" +
				"jobs:\n  test:\n    runs-on: ubuntu-24.04\n" + baseRun +
				"          echo \"${CORE_SHA256}\" | sha256sum -c -\n" +
				"          echo \"${BLAKE_SHA256}\" | sha256sum -c -\n",
			wantInError: "same checksum",
		},
		{
			name: "same version for both binaries",
			workflow: "env:\n" +
				"  CORE_VERSION: \"30.3\"\n" +
				"  BLAKE_VERSION: \"30.3\"\n" +
				"jobs:\n  test:\n    runs-on: ubuntu-24.04\n" + baseRun +
				"          echo \"${CORE_SHA256}\" | sha256sum -c -\n",
			wantInError: "same version",
		},
		{
			name: "version pin is not used",
			workflow: "env:\n" +
				"  CORE_VERSION: \"30.3\"\n" +
				"  BLAKE_VERSION: \"29.4.2.knots20260508\"\n" +
				"jobs:\n  test:\n    runs-on: ubuntu-24.04\n" +
				"    steps:\n" +
				"      - run: |\n" +
				"          curl -fsSLO \"https://bitcoincore.org/bin/bitcoin-core/x.tar.gz\"\n" +
				"          curl -fsSLO \"https://bitcoinknots.org/files/x/bitcoin-x.tar.gz\"\n" +
				"          echo x | sha256sum -c -\n",
			wantInError: "no download URL uses",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := writeTree(t, map[string]string{
				".github/workflows/ci.yml": test.workflow,
			})

			err := CheckCIProvisionsBothNodeBinaries(fixture)

			if test.wantInError == "" {
				if err != nil {
					t.Fatalf("valid node provisioning rejected: %v", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("incomplete node provisioning accepted; want an error mentioning %q", test.wantInError)
			}

			if !strings.Contains(err.Error(), test.wantInError) {
				t.Fatalf("error %q does not mention %q", err, test.wantInError)
			}
		})
	}
}

// TestCIRunsUnitAndWasmGates checks the gate part of the fourth invariant: the
// workflow runs the unit-only suite gate and `make wasm`.
func TestCIRunsUnitAndWasmGates(t *testing.T) {
	root := repoRoot(t)

	if err := CheckCIRunsUnitAndWasmGates(root); err != nil {
		t.Fatalf("the repository's CI workflow does not run both gates: %v", err)
	}

	for _, line := range activeLines(mustReadFile(t, root, ciWorkflow)) {
		if strings.Contains(line, "scripts/test.sh") || makeWasmRE.MatchString(line) {
			t.Logf("gate command: %s", line)
		}
	}

	tests := []struct {
		name        string
		workflow    string
		wantInError string
	}{
		{
			name: "both gates",
			workflow: "jobs:\n  test:\n    runs-on: ubuntu-24.04\n" +
				"    steps:\n" +
				"      - run: scripts/test.sh --unit-only --strict\n" +
				"      - run: make wasm\n",
		},
		{
			name: "unit gate only",
			workflow: "jobs:\n  test:\n    runs-on: ubuntu-24.04\n" +
				"    steps:\n" +
				"      - run: scripts/test.sh --unit-only\n",
			wantInError: "wasm gate",
		},
		{
			name: "wasm gate only",
			workflow: "jobs:\n  test:\n    runs-on: ubuntu-24.04\n" +
				"    steps:\n" +
				"      - run: make wasm\n",
			wantInError: "unit gate",
		},
		{
			name: "wasm mentioned only in a comment",
			workflow: "jobs:\n  test:\n    runs-on: ubuntu-24.04\n" +
				"    steps:\n" +
				"      # make wasm\n" +
				"      - run: scripts/test.sh --unit-only\n",
			wantInError: "wasm gate",
		},
		{
			name: "wasmtest does not satisfy the wasm gate",
			workflow: "jobs:\n  test:\n    runs-on: ubuntu-24.04\n" +
				"    steps:\n" +
				"      - run: scripts/test.sh --unit-only\n" +
				"      - run: make wasmtest\n",
			wantInError: "wasm gate",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := writeTree(t, map[string]string{
				".github/workflows/ci.yml": test.workflow,
			})

			err := CheckCIRunsUnitAndWasmGates(fixture)

			if test.wantInError == "" {
				if err != nil {
					t.Fatalf("valid gate set rejected: %v", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("missing gate accepted; want an error mentioning %q", test.wantInError)
			}

			if !strings.Contains(err.Error(), test.wantInError) {
				t.Fatalf("error %q does not mention %q", err, test.wantInError)
			}
		})
	}
}

// TestCINoMergeBlockingJobDependsOnPublicRelay checks the relay part of the
// fourth invariant: the workflow never invokes the public-relay suites, names
// a known public relay, or uses a non-local relay URL in a command.
func TestCINoMergeBlockingJobDependsOnPublicRelay(t *testing.T) {
	root := repoRoot(t)

	if err := CheckCINoPublicRelayGate(root); err != nil {
		t.Fatalf("the repository's CI workflow depends on a public relay: %v", err)
	}

	tests := []struct {
		name        string
		workflow    string
		wantInError string
	}{
		{
			name: "local relay only",
			workflow: "jobs:\n  test:\n    runs-on: ubuntu-24.04\n" +
				"    steps:\n" +
				"      - run: scripts/test.sh --unit-only BLAKE_SWAP_RELAY=ws://127.0.0.1:7777\n",
		},
		{
			name: "documenting the flag in a comment",
			workflow: "jobs:\n  test:\n    runs-on: ubuntu-24.04\n" +
				"    steps:\n" +
				"      # run scripts/test.sh --with-nostr on a schedule if at all\n" +
				"      - run: scripts/test.sh --unit-only\n",
		},
		{
			name: "invoking the public-relay suites",
			workflow: "jobs:\n  test:\n    runs-on: ubuntu-24.04\n" +
				"    steps:\n" +
				"      - run: scripts/test.sh --with-nostr\n",
			wantInError: "--with-nostr",
		},
		{
			name: "naming a public relay",
			workflow: "jobs:\n  test:\n    runs-on: ubuntu-24.04\n" +
				"    env:\n" +
				"      NOSTR_RELAY: wss://nos.lol\n" +
				"    steps:\n" +
				"      - run: scripts/test.sh --unit-only\n",
			wantInError: "nos.lol",
		},
		{
			name: "external relay URL under another name",
			workflow: "jobs:\n  test:\n    runs-on: ubuntu-24.04\n" +
				"    steps:\n" +
				"      - run: scripts/test.sh --unit-only --relay wss://relay.example.com\n",
			wantInError: "external relay URL",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := writeTree(t, map[string]string{
				".github/workflows/ci.yml": test.workflow,
			})

			err := CheckCINoPublicRelayGate(fixture)

			if test.wantInError == "" {
				if err != nil {
					t.Fatalf("a local-relay workflow was rejected: %v", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("public-relay dependency accepted; want an error mentioning %q", test.wantInError)
			}

			if !strings.Contains(err.Error(), test.wantInError) {
				t.Fatalf("error %q does not mention %q", err, test.wantInError)
			}
		})
	}
}

// TestLicenseIsPresentAndNonEmpty checks the fifth invariant.
func TestLicenseIsPresentAndNonEmpty(t *testing.T) {
	root := repoRoot(t)

	if err := CheckLicense(root); err != nil {
		t.Fatalf("the repository has no usable LICENSE: %v", err)
	}

	t.Logf("LICENSE has %d bytes", len(mustReadFile(t, root, licenseName)))

	tests := []struct {
		name        string
		files       map[string]string
		wantInError string
	}{
		{
			name:  "present and non-empty",
			files: map[string]string{"LICENSE": "MIT License\n\nPermission is hereby granted, free of charge.\n"},
		},
		{
			name:        "empty",
			files:       map[string]string{"LICENSE": ""},
			wantInError: "is empty",
		},
		{
			name:        "whitespace only",
			files:       map[string]string{"LICENSE": " \n\t\n"},
			wantInError: "is empty",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := CheckLicense(writeTree(t, test.files))

			if test.wantInError == "" {
				if err != nil {
					t.Fatalf("valid LICENSE rejected: %v", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("unusable LICENSE accepted; want an error mentioning %q", test.wantInError)
			}

			if !strings.Contains(err.Error(), test.wantInError) {
				t.Fatalf("error %q does not mention %q", err, test.wantInError)
			}
		})
	}

	t.Run("missing", func(t *testing.T) {
		if err := CheckLicense(t.TempDir()); err == nil {
			t.Fatal("a root without LICENSE was accepted")
		}
	})
}

// TestEveryGoPackageDirectoryBelongsToTheModule checks the sixth invariant:
// every directory under pkg/ and cmd/ that contains Go files has no own
// go.mod, so it is part of this module.
func TestEveryGoPackageDirectoryBelongsToTheModule(t *testing.T) {
	root := repoRoot(t)

	if err := CheckPackageLayout(root); err != nil {
		t.Fatalf("the package layout is not part of one module: %v", err)
	}

	t.Logf("checked the Go package directories under pkg/ and cmd/")

	tests := []struct {
		name        string
		files       map[string]string
		wantInError string
	}{
		{
			name: "one module",
			files: map[string]string{
				"pkg/a/a.go":      "package a\n",
				"pkg/a/a_test.go": "package a\n",
				"cmd/b/main.go":   "package main\n",
				"pkg/c/README.md": "not a Go package\n",
			},
		},
		{
			name: "nested module under pkg",
			files: map[string]string{
				"pkg/a/a.go":    "package a\n",
				"pkg/a/go.mod":  "module example.com/a\n",
				"cmd/b/main.go": "package main\n",
			},
			wantInError: "separate module",
		},
		{
			name: "nested module under cmd",
			files: map[string]string{
				"pkg/a/a.go":    "package a\n",
				"cmd/b/main.go": "package main\n",
				"cmd/b/go.mod":  "module example.com/b\n",
			},
			wantInError: "separate module",
		},
		{
			name: "no Go packages at all",
			files: map[string]string{
				"pkg/notes/notes.md": "nothing\n",
				"cmd/tool/README.md": "nothing\n",
			},
			wantInError: "no Go package directories",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := CheckPackageLayout(writeTree(t, test.files))

			if test.wantInError == "" {
				if err != nil {
					t.Fatalf("coherent layout rejected: %v", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("incoherent layout accepted; want an error mentioning %q", test.wantInError)
			}

			if !strings.Contains(err.Error(), test.wantInError) {
				t.Fatalf("error %q does not mention %q", err, test.wantInError)
			}
		})
	}

	t.Run("missing pkg directory", func(t *testing.T) {
		if err := CheckPackageLayout(t.TempDir()); err == nil {
			t.Fatal("a root without pkg/ was accepted")
		}
	})
}

// TestWaivedPythonAssertionsAreDocumented checks the waiver table: every entry
// names a concrete Python assertion it replaces, gives a specific reason, and
// the table covers every packaging_tests.py assertion that is structural to the
// Python distribution.
func TestWaivedPythonAssertionsAreDocumented(t *testing.T) {
	const minimumReasonLength = 120

	assertionRE := regexp.MustCompile(`tests/[A-Za-z0-9_./-]+\.py::[A-Za-z0-9_]+`)

	required := []string{
		"tests/packaging_tests.py::test_pyproject_declares_the_project",
		"tests/packaging_tests.py::test_dependencies_match_requirements",
		"tests/packaging_tests.py::test_console_scripts_are_declared",
		"tests/packaging_tests.py::test_readme_python_range_matches_pyproject",
		"tests/packaging_tests.py::test_declared_modules_match_src",
	}

	vague := []string{
		"not applicable",
		"n/a",
		"no go counterpart",
		"not ported",
		"none",
		"todo",
	}

	if len(waived) == 0 {
		t.Fatal("the waiver table is empty")
	}

	seenNames := map[string]bool{}
	seenAssertions := map[string]bool{}

	for _, waiver := range waived {
		waiver := waiver

		t.Run(waiver.name, func(t *testing.T) {
			if strings.TrimSpace(waiver.name) == "" {
				t.Fatal("waiver has no name")
			}

			if seenNames[waiver.name] {
				t.Fatalf("waiver name %q appears more than once", waiver.name)
			}

			seenNames[waiver.name] = true

			reason := strings.TrimSpace(waiver.reason)
			if reason == "" {
				t.Fatalf("waiver %q has no reason", waiver.name)
			}

			if len(reason) < minimumReasonLength {
				t.Fatalf("waiver %q has a %d-character reason; a concrete reason needs at least %d", waiver.name, len(reason), minimumReasonLength)
			}

			lower := strings.ToLower(reason)

			for _, phrase := range vague {
				if lower == phrase || strings.Contains(lower, phrase) {
					t.Fatalf("waiver %q uses the vague reason %q; say what the Python assertion checks and what the Go analogue is", waiver.name, phrase)
				}
			}

			assertions := assertionRE.FindAllString(reason, -1)
			if len(assertions) == 0 {
				t.Fatalf("waiver %q does not name the Python assertion it replaces (want tests/<file>.py::<assertion>)", waiver.name)
			}

			for _, assertion := range assertions {
				seenAssertions[assertion] = true
				t.Logf("%s replaces %s", waiver.name, assertion)
			}
		})
	}

	for _, assertion := range required {
		if !seenAssertions[assertion] {
			t.Errorf("the waiver table does not cover %s", assertion)
		}
	}
}

// sortedKeys returns the sorted keys of a map from name to location.
func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))

	for key := range values {
		keys = append(keys, key)
	}

	sortStrings(keys)

	return keys
}

// sortedKeysStringSet returns the sorted members of a string set.
func sortedKeysStringSet(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))

	for key := range values {
		keys = append(keys, key)
	}

	sortStrings(keys)

	return keys
}

// sortStrings sorts a small slice without importing sort into the test file's
// helper set twice.
func sortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for current := index; current > 0 && values[current] < values[current-1]; current-- {
			values[current], values[current-1] = values[current-1], values[current]
		}
	}
}
