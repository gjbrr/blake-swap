// Package proto is the message vocabulary shared by the page domain and the
// service-worker domain.
//
// The two run as separate wasm instances, so every value that crosses the
// link is serialized. Keeping the vocabulary in one stateless package means
// the two instances cannot drift, and it is the only thing they share: the
// moxie rule is that domains share types, never state.
//
// The encoding is JSON because the skeleton is meant to be readable in a
// browser devtools network/console view. When the relay proxy and signature
// checks land, the payloads those carry want a compact binary codec; the
// envelope is the seam where that change belongs.
package proto

import (
	"encoding/json"
	"fmt"
)

// Kind identifies a message. Kinds are declared here, never written as raw
// strings at call sites.
type Kind string

const (
	// Hello asks the worker to prove it is alive. Sent by the page domain
	// when it starts and again on demand.
	Hello Kind = "hello"

	// HelloAck is the worker's answer. Its Body carries the greeting the
	// page displays, which is the whole point of the skeleton.
	HelloAck Kind = "hello-ack"

	// WorkerReady is sent by the worker domain as soon as its loop is
	// running, before any request arrives. It lets the page distinguish
	// "the worker module is up" from "the worker answered", which are two
	// different facts and two different boot states.
	WorkerReady Kind = "worker-ready"

	// Command asks the worker to run one command. Its Body is a Request.
	//
	// The page does not get an API of its own: it speaks the same command
	// vocabulary as the CLI, so a browser and a terminal cannot drift into
	// two different ideas of what "dashboard" means.
	Command Kind = "command"

	// CommandResult is the worker's answer to a Command. Its Body is a
	// Result. A failed command is a Result with OK false, not a different
	// kind: the page renders errors and successes the same way.
	CommandResult Kind = "command-result"
)

// Request is the body of a Command.
type Request struct {
	// Command is one of the engine's command names, such as "status".
	Command string `json:"command"`
	// Args are the command's arguments, in the same shape the CLI accepts.
	Args map[string]string `json:"args,omitempty"`
}

// Result is the body of a CommandResult.
type Result struct {
	// Command echoes the request, so a reply cannot be attributed to the
	// wrong command when several are in flight.
	Command string `json:"command"`
	// OK reports whether the command did what was asked.
	OK bool `json:"ok"`
	// Text is the command's human-readable output, exactly as the CLI would
	// print it.
	Text string `json:"text,omitempty"`
	// Error names the failure when OK is false.
	Error string `json:"error,omitempty"`
	// Data is the structured view of the command's result, when it has one.
	//
	// Text stays because the CLI's output is a contract of its own and because a
	// terminal, a log and a bug report all want one line; Data exists because the page
	// must not parse that line. A command with a view returns both, and the view is the
	// authority for the UI.
	Data json.RawMessage `json:"data,omitempty"`
}

// WithData attaches a view to a result.
//
// A view that cannot be encoded is a programming error, not an operator's problem: the
// result keeps its text and loses its data rather than failing a command that already
// succeeded. The error is reported so a test can assert it never happens in practice.
func WithData(r Result, v any) (Result, error) {
	encoded, err := json.Marshal(v)

	if err != nil {
		return r, fmt.Errorf("encode %s view: %w", r.Command, err)
	}

	r.Data = encoded

	return r, nil
}

// DecodeData parses a result's view into v.
func DecodeData(r Result, v any) error {
	if len(r.Data) == 0 {
		return fmt.Errorf("%s result has no data", r.Command)
	}

	if err := json.Unmarshal(r.Data, v); err != nil {
		return fmt.Errorf("decode %s view: %w", r.Command, err)
	}

	return nil
}

// EncodeRequest renders a request as a command body.
func EncodeRequest(r Request) (string, error) {
	encoded, err := json.Marshal(r)

	if err != nil {
		return "", fmt.Errorf("encode request: %w", err)
	}

	return string(encoded), nil
}

// DecodeRequest parses a command body.
func DecodeRequest(body string) (Request, error) {
	var r Request

	if err := json.Unmarshal([]byte(body), &r); err != nil {
		return Request{}, fmt.Errorf("decode request: %w", err)
	}

	if r.Command == "" {
		return Request{}, fmt.Errorf("request has no command")
	}

	return r, nil
}

// EncodeResult renders a result as a command-result body.
func EncodeResult(r Result) (string, error) {
	encoded, err := json.Marshal(r)

	if err != nil {
		return "", fmt.Errorf("encode result: %w", err)
	}

	return string(encoded), nil
}

// DecodeResult parses a command-result body.
func DecodeResult(body string) (Result, error) {
	var r Result

	if err := json.Unmarshal([]byte(body), &r); err != nil {
		return Result{}, fmt.Errorf("decode result: %w", err)
	}

	return r, nil
}

// Envelope is the single wire shape of the link.
//
// One shape for every message keeps the shim type-agnostic: it moves bytes
// and does not need to know the protocol. AtMS is an echo field, not a
// cross-instance clock: the page stamps it, the worker copies it back, and
// the page measures the round trip against its own clock only.
type Envelope struct {
	Kind Kind   `json:"kind"`
	Seq  uint32 `json:"seq,omitempty"`
	Body string `json:"body,omitempty"`
	AtMS int64  `json:"at_ms,omitempty"`
}

// Encode renders an envelope for postMessage.
func Encode(e Envelope) (string, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return "", fmt.Errorf("encode envelope: %w", err)
	}
	return string(b), nil
}

// Decode parses an envelope received from the peer.
func Decode(text string) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal([]byte(text), &e); err != nil {
		return Envelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	if e.Kind == "" {
		return Envelope{}, fmt.Errorf("envelope has no kind")
	}
	return e, nil
}
