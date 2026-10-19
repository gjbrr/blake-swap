// Package adapter is the portable participant interface: `blake-swap-adapter-v1`.
//
// The point of the interface is that a swap can be run with two *different*
// implementations, one per role, and the scenario logic does not need to know which. A
// harness speaks one JSON request per process on stdin/stdout, and the implementation
// answers it: what can you do, run this command as this role, and tell me this swap's
// state.
//
// The contract is deliberately smaller than the engine. A command is whatever the
// implementation's own command line offers, and the state is a fixed set of portable
// fields — the ones the adversarial corpus reads. Anything else is an implementation
// detail, and putting it on this wire would make two implementations agree only by
// accident.
package adapter

import (
	"context"
	"encoding/json"
	"fmt"
)

// Protocol is the version string every request and response carries.
const Protocol = "blake-swap-adapter-v1"

// The three operations, in the order the fixture lists them.
const (
	OpCapabilities = "capabilities"
	OpCommand      = "command"
	OpState        = "state"
)

// Operations is the operation set this implementation serves.
var Operations = []string{OpCapabilities, OpCommand, OpState}

// Roles are the two protocol roles a participant can be asked to be.
var Roles = []string{"alice", "bob"}

// PortableFields are the record fields the seeded corpus reads.
//
// This list is the contract. A record field outside it is not portable: another
// implementation may store the same fact under another name, or not at all.
var PortableFields = []string{
	"swap_id",
	"state",
	"secret_hex",
	"revealed_secret_hex",
	"blake_funding_txid",
	"blake_funding_vout",
	"btc_funding_txid",
	"btc_funding_vout",
	"btc_claim_txid",
	"btc_refund_txid",
	"blake_claim_txid",
	"blake_refund_txid",
}

// Request is one adapter request.
type Request struct {
	// Op is capabilities, command or state.
	Op string `json:"op"`
	// Role is the protocol role to act as, for command and state.
	Role string `json:"role,omitempty"`
	// Args is the command line, for command.
	Args []string `json:"args,omitempty"`
	// SwapID names the swap, for state.
	SwapID string `json:"swap_id,omitempty"`
	// TimeoutSeconds bounds a command. Zero means the implementation's default.
	TimeoutSeconds float64 `json:"timeout_seconds,omitempty"`
}

// Response is one adapter response.
//
// A refused command is a response, not an error: the harness needs the return code and the
// implementation's own refusal text, because in this protocol the refusal text is what a
// person reads when a scenario fails.
type Response struct {
	OK bool `json:"ok"`

	// Capability fields.
	Protocol   string   `json:"protocol,omitempty"`
	Operations []string `json:"operations,omitempty"`
	Roles      []string `json:"roles,omitempty"`

	// Command fields.
	ReturnCode int    `json:"returncode,omitempty"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`

	// State fields.
	State map[string]any `json:"state,omitempty"`

	// Error explains a refusal that is not a command result.
	Error string `json:"error,omitempty"`
}

// CommandResult is what one command run produced.
type CommandResult struct {
	ReturnCode int
	Stdout     string
	Stderr     string
}

// Port is where an adapter implementation reaches its participant.
//
// It exists so the protocol can be tested without a process, a node or a state directory:
// the response shapes are the contract, and they are worth testing on their own.
type Port interface {
	// Command runs one command as one role.
	Command(ctx context.Context, role string, args []string, timeoutSeconds float64) (CommandResult, error)
	// State returns one swap's portable state.
	State(ctx context.Context, role, swapID string) (map[string]any, error)
}

// Capabilities is the answer to the capabilities request.
func Capabilities() Response {
	return Response{
		OK:         true,
		Protocol:   Protocol,
		Operations: append([]string{}, Operations...),
		Roles:      append([]string{}, Roles...),
	}
}

// Handle answers one request.
//
// Every refusal is a response with `ok: false` and a reason, because a harness that has to
// tell "the implementation said no" from "the transport broke" needs both facts on the same
// channel.
func Handle(ctx context.Context, port Port, request Request) Response {
	switch request.Op {
	case OpCapabilities:
		return Capabilities()
	case "":
		return refused("no op was supplied")
	case OpCommand:
		return handleCommand(ctx, port, request)
	case OpState:
		return handleState(ctx, port, request)
	}

	return refused(fmt.Sprintf("unknown op %q", request.Op))
}

// handleCommand runs one command as one role.
func handleCommand(ctx context.Context, port Port, request Request) Response {
	if port == nil {
		return refused("the adapter has no participant to command")
	}

	if !validRole(request.Role) {
		return refused(fmt.Sprintf("unknown protocol role %q", request.Role))
	}

	if len(request.Args) == 0 {
		return refused("command needs at least one argument")
	}

	result, err := port.Command(ctx, request.Role, request.Args, request.TimeoutSeconds)

	if err != nil {
		return refused(err.Error())
	}

	return Response{
		OK:         result.ReturnCode == 0,
		ReturnCode: result.ReturnCode,
		Stdout:     result.Stdout,
		Stderr:     result.Stderr,
	}
}

// handleState returns one swap's portable state.
func handleState(ctx context.Context, port Port, request Request) Response {
	if port == nil {
		return refused("the adapter has no participant to ask")
	}

	if !validRole(request.Role) {
		return refused(fmt.Sprintf("unknown protocol role %q", request.Role))
	}

	if request.SwapID == "" {
		return refused("state needs a swap_id")
	}

	state, err := port.State(ctx, request.Role, request.SwapID)

	if err != nil {
		return refused(err.Error())
	}

	if state == nil {
		return refused(fmt.Sprintf("no record for swap %s", request.SwapID))
	}

	return Response{OK: true, State: PortableState(state)}
}

// PortableState projects a record onto the portable field set.
//
// Absent fields are kept, as JSON null, because "this implementation has no secret" and
// "this implementation does not report secrets" are different claims and the corpus reads
// the difference.
func PortableState(record map[string]any) map[string]any {
	projected := make(map[string]any, len(PortableFields))

	for _, field := range PortableFields {
		projected[field] = record[field]
	}

	return projected
}

// validRole reports whether a role is one of the two the protocol has.
func validRole(role string) bool {
	return role == "alice" || role == "bob"
}

// refused is a response that says no, with a reason.
func refused(reason string) Response {
	return Response{OK: false, Error: reason}
}

// Decode reads one request from JSON.
func Decode(raw []byte) (Request, error) {
	var request Request

	if err := json.Unmarshal(raw, &request); err != nil {
		return Request{}, fmt.Errorf("adapter request is not JSON: %w", err)
	}

	return request, nil
}

// Encode renders one response as JSON, with no trailing newline added: the caller decides
// how the response is framed on its transport.
func Encode(response Response) ([]byte, error) {
	return json.Marshal(response)
}
