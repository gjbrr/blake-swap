package adapter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture is the reference's adapter contract document.
type fixture struct {
	Protocol         string   `json:"protocol"`
	Transport        string   `json:"transport"`
	RequiredOps      []string `json:"required_operations"`
	RequiredRoles    []string `json:"required_roles"`
	PortableFields   []string `json:"portable_state_fields_used_by_seeded_corpus"`
	CapabilitiesReq  Request  `json:"capabilities_request"`
	CapabilitiesResp Response `json:"capabilities_response"`
	CommandRequest   Request  `json:"command_request_example"`
	CommandSuccess   Response `json:"command_success_response_example"`
	CommandRefusal   Response `json:"command_refusal_response_example"`
	StateRequest     Request  `json:"state_request_example"`
	StateMinimum     struct {
		OK    bool           `json:"ok"`
		State map[string]any `json:"state"`
	} `json:"state_response_minimum_example"`
}

// loadFixture reads the reference's own contract document.
//
// Testing against the document rather than against a copy of it is the point: if the
// reference changes the protocol, this test fails, which is the only way a shared contract
// stays shared.
func loadFixture(t *testing.T) fixture {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "tests", "moxie_adapter_fixture.json"))

	if err != nil {
		t.Fatalf("the reference adapter fixture is missing: %v", err)
	}

	var parsed fixture

	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("the reference adapter fixture is not JSON: %v", err)
	}

	return parsed
}

// TestContractMatchesTheReferenceFixture: the protocol string, the operations, the roles and
// the portable field set are all read from the reference's document.
func TestContractMatchesTheReferenceFixture(t *testing.T) {
	document := loadFixture(t)

	if document.Protocol != Protocol {
		t.Fatalf("protocol = %q, want %q", Protocol, document.Protocol)
	}

	if !sameSet(Operations, document.RequiredOps) {
		t.Fatalf("operations = %v, want %v", Operations, document.RequiredOps)
	}

	if !sameSet(Roles, document.RequiredRoles) {
		t.Fatalf("roles = %v, want %v", Roles, document.RequiredRoles)
	}

	// Every field the corpus reads must be one this adapter reports; reporting a
	// superset is fine, reporting less is a broken contract.
	for _, field := range document.PortableFields {
		if !contains(PortableFields, field) {
			t.Errorf("portable field %q is not reported", field)
		}
	}
}

// TestCapabilitiesMatchesTheReferenceResponse: the capabilities answer is the fixture's,
// field for field.
func TestCapabilitiesMatchesTheReferenceResponse(t *testing.T) {
	document := loadFixture(t)

	got := Handle(context.Background(), nil, document.CapabilitiesReq)

	if !got.OK || got.Protocol != document.CapabilitiesResp.Protocol {
		t.Fatalf("capabilities = %+v", got)
	}

	if !sameSet(got.Operations, document.CapabilitiesResp.Operations) {
		t.Fatalf("operations = %v, want %v", got.Operations, document.CapabilitiesResp.Operations)
	}

	if !sameSet(got.Roles, document.CapabilitiesResp.Roles) {
		t.Fatalf("roles = %v", got.Roles)
	}
}

// TestCommandSuccessAndRefusalShapes: a command answers with a return code and the two
// streams, and a non-zero return code is `ok: false` rather than a transport error.
func TestCommandSuccessAndRefusalShapes(t *testing.T) {
	document := loadFixture(t)

	port := &fakePort{}

	if got := Handle(context.Background(), port, document.CommandRequest); !got.OK ||
		got.ReturnCode != 0 {
		t.Fatalf("success = %+v", got)
	}

	// The role the request names is the role the command runs as, which is the whole
	// point of pinning it at the adapter boundary.
	if port.role != "bob" || strings.Join(port.args, " ") != "accept-offer abcdef123456" {
		t.Fatalf("ran as %q: %v", port.role, port.args)
	}

	refusing := &fakePort{result: CommandResult{ReturnCode: 1, Stderr: "implementation-defined refusal text"}}

	got := Handle(context.Background(), refusing, document.CommandRequest)

	if got.OK || got.ReturnCode != 1 || got.Stderr == "" {
		t.Fatalf("refusal = %+v", got)
	}
}

// TestStateReportsTheMinimumShape: the portable projection keeps every contract field,
// including the ones this participant has no value for.
func TestStateReportsTheMinimumShape(t *testing.T) {
	document := loadFixture(t)

	port := &fakePort{state: map[string]any{
		"swap_id":     "abcdef123456",
		"state":       "OFFERED",
		"secret_hex":  nil,
		"blake_vout":  3,
		"extra_field": "not portable",
	}}

	got := Handle(context.Background(), port, document.StateRequest)

	if !got.OK {
		t.Fatalf("state = %+v", got)
	}

	for _, field := range document.PortableFields {
		if _, present := got.State[field]; !present {
			t.Errorf("portable state is missing %q: %v", field, got.State)
		}
	}

	if _, present := got.State["extra_field"]; present {
		t.Errorf("a non-portable field was reported: %v", got.State)
	}

	if _, present := got.State["blake_vout"]; present {
		t.Errorf("an implementation-specific field was reported: %v", got.State)
	}

	// The absent secret stays in the projection as null.
	if got.State["secret_hex"] != nil {
		t.Errorf("secret_hex = %v, want null", got.State["secret_hex"])
	}
}

// TestHandleRefusesMalformedRequests: every refusal is a response with a reason, so a
// harness can tell it from a broken transport.
func TestHandleRefusesMalformedRequests(t *testing.T) {
	cases := map[string]Request{
		"no op":           {},
		"unknown op":      {Op: "frobnicate"},
		"bad role":        {Op: OpCommand, Role: "carol", Args: []string{"show"}},
		"no args":         {Op: OpCommand, Role: "alice"},
		"no swap id":      {Op: OpState, Role: "alice"},
		"state no port":   {Op: OpState, Role: "alice", SwapID: "abcdef123456"},
		"command no port": {Op: OpCommand, Role: "alice", Args: []string{"show"}},
	}

	for name, request := range cases {
		// A port is supplied wherever the refusal must come from the request rather
		// than from the adapter having nowhere to send it, so each check is the one
		// under test.
		var port Port

		switch name {
		case "bad role", "no args", "no swap id":
			// These must be refused by the request check, not by having no participant.
			port = &fakePort{state: map[string]any{"state": "OFFERED"}}
		}

		got := Handle(context.Background(), port, request)

		if got.OK {
			t.Errorf("%s: accepted", name)

			continue
		}

		if got.Error == "" {
			t.Errorf("%s: refused without a reason", name)
		}
	}
}

// TestDecodeRefusesGarbage: a request that is not JSON is an error, not a silent default.
func TestDecodeRefusesGarbage(t *testing.T) {
	if _, err := Decode([]byte("{not json")); err == nil {
		t.Fatal("garbage was accepted")
	}

	request, err := Decode([]byte(`{"op":"capabilities"}`))

	if err != nil || request.Op != OpCapabilities {
		t.Fatalf("request = %+v, %v", request, err)
	}
}

// TestEncodeRoundTrip: a response survives the wire and carries its fields.
func TestEncodeRoundTrip(t *testing.T) {
	encoded, err := Encode(Response{
		OK:         true,
		ReturnCode: 2,
		Stdout:     "OUT",
		Stderr:     "ERR",
		State:      map[string]any{"state": "OFFERED"},
	})

	if err != nil {
		t.Fatal(err)
	}

	var decoded map[string]any

	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}

	for key, want := range map[string]any{
		"ok": true, "returncode": float64(2), "stdout": "OUT", "stderr": "ERR",
	} {
		if decoded[key] != want {
			t.Errorf("%s = %v, want %v", key, decoded[key], want)
		}
	}

	if _, present := decoded["protocol"]; present {
		t.Errorf("an empty capability field was serialized: %s", encoded)
	}
}

// fakePort is a participant that answers from the test.
type fakePort struct {
	role   string
	args   []string
	result CommandResult
	state  map[string]any
}

func (f *fakePort) Command(
	_ context.Context, role string, args []string, _ float64,
) (CommandResult, error) {
	f.role = role
	f.args = args

	return f.result, nil
}

func (f *fakePort) State(_ context.Context, _, _ string) (map[string]any, error) {
	return f.state, nil
}

// sameSet compares two string lists as sets.
func sameSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}

	seen := map[string]bool{}

	for _, value := range left {
		seen[value] = true
	}

	for _, value := range right {
		if !seen[value] {
			return false
		}
	}

	return true
}

// contains reports whether a list has a value.
func contains(list []string, value string) bool {
	for _, candidate := range list {
		if candidate == value {
			return true
		}
	}

	return false
}
