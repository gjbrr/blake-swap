// Command blake-swap-adapter serves the portable participant interface.
//
// It is how this implementation takes part in a mixed pair: a harness sends one JSON
// request on stdin and reads one JSON response on stdout, per process. The protocol is
// `blake-swap-adapter-v1`, and the contract is in docs/IMPLEMENTATION_ADAPTER_PROTOCOL.md.
//
// The adapter does not implement the protocol itself. It answers `command` by running this
// project's own CLI as the requested role, so an adapter and a person drive exactly the same
// code — an adapter with its own private paths would be a second implementation to trust.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gjbrr/blake-swap/pkg/adapter"
)

func main() {
	raw, err := io.ReadAll(bufio.NewReader(os.Stdin))

	if err != nil {
		emit(adapter.Response{OK: false, Error: "reading the request: " + err.Error()})

		os.Exit(1)
	}

	request, err := adapter.Decode(raw)

	if err != nil {
		emit(adapter.Response{OK: false, Error: err.Error()})

		os.Exit(1)
	}

	response := adapter.Handle(context.Background(), &cliPort{}, request)

	emit(response)

	if !response.OK && request.Op != adapter.OpCommand {
		os.Exit(2)
	}
}

// emit writes one response as one JSON document.
func emit(response adapter.Response) {
	encoded, err := adapter.Encode(response)

	if err != nil {
		fmt.Fprintf(os.Stderr, "blake-swap-adapter: %v\n", err)

		return
	}

	fmt.Println(string(encoded))
}

// cliPort drives this project's CLI as one role.
type cliPort struct{}

// Command runs one CLI command as a role and returns what it printed.
func (cliPort) Command(
	ctx context.Context, role string, args []string, timeoutSeconds float64,
) (adapter.CommandResult, error) {
	binary, err := cliBinary()

	if err != nil {
		return adapter.CommandResult{}, err
	}

	if timeoutSeconds > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSeconds*float64(time.Second)))
		defer cancel()
	}

	command := exec.CommandContext(ctx, binary, args...)
	command.Env = append(os.Environ(), "BLAKE_SWAP_ROLE="+role)

	var stdout, stderr strings.Builder

	command.Stdout = &stdout
	command.Stderr = &stderr

	err = command.Run()

	result := adapter.CommandResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}

	if err != nil {
		var exit *exec.ExitError

		if errors.As(err, &exit) {
			result.ReturnCode = exit.ExitCode()

			return result, nil
		}

		return adapter.CommandResult{}, err
	}

	return result, nil
}

// State returns one swap's portable state, read from this participant's state tree.
func (cliPort) State(_ context.Context, _ string, swapID string) (map[string]any, error) {
	directory := os.Getenv("BLAKE_SWAP_STATE_DIR")

	if directory == "" {
		home, err := os.UserHomeDir()

		if err != nil {
			return nil, err
		}

		directory = filepath.Join(home, ".blake-swaps-v2")
	}

	raw, err := os.ReadFile(filepath.Join(directory, swapID+".json"))

	if err != nil {
		return nil, fmt.Errorf("no record for swap %s: %w", swapID, err)
	}

	var record map[string]any

	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, fmt.Errorf("the record for swap %s is not readable", swapID)
	}

	return record, nil
}

// cliBinary finds this project's CLI.
//
// The environment variable exists for a harness that builds into a temporary directory; the
// sibling lookup is what makes an installed pair of binaries work without configuration.
func cliBinary() (string, error) {
	if configured := os.Getenv("BLAKE_SWAP_CLI"); configured != "" {
		return configured, nil
	}

	self, err := os.Executable()

	if err == nil {
		sibling := filepath.Join(filepath.Dir(self), "blake-swap-cli")

		if info, statErr := os.Stat(sibling); statErr == nil && !info.IsDir() {
			return sibling, nil
		}
	}

	if found, lookErr := exec.LookPath("blake-swap-cli"); lookErr == nil {
		return found, nil
	}

	return "", fmt.Errorf("cannot find blake-swap-cli: set BLAKE_SWAP_CLI")
}
