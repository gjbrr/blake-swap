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
)

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
