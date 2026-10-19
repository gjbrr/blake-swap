//go:build !js

// The market accept surface: what the worker refuses before it touches a relay.
package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/gjbrr/blake-swap/pkg/proto"
)

// TestMarketAcceptRefusesBadIDs: a malformed id is refused as what it is, not looked up.
func TestMarketAcceptRefusesBadIDs(t *testing.T) {
	proxy := newFakeProxy(t)
	_, vault := engineDomainFor(t, proxy.server.URL)
	domain := newMarketDomain(vault, nil)

	cases := []struct {
		command string
		args    map[string]string
		want    string
	}{
		{"market.takes", map[string]string{}, "64-character"},
		{"market.takes", map[string]string{"offer_id": "short"}, "64-character"},
		{"market.accept-take", map[string]string{"take_id": "abc"}, "64-character"},
		{"market.accept-match", map[string]string{"match_id": ""}, "64-character"},
		{"market.cancel", map[string]string{"offer_id": "nope"}, "64-character"},
	}

	for _, test := range cases {
		result := domain.Handle(context.Background(), proto.Request{
			Command: test.command, Args: test.args,
		})

		if result.OK {
			t.Fatalf("%s %v was accepted", test.command, test.args)
		}

		if !strings.Contains(result.Error, test.want) {
			t.Fatalf("%s %v: error = %q, want %q", test.command, test.args, result.Error, test.want)
		}
	}
}

// TestMarketAcceptNeedsAnEngine: the accept path creates a swap, so a worker without an
// engine must say so rather than half-accept a take.
func TestMarketAcceptNeedsAnEngine(t *testing.T) {
	proxy := newFakeProxy(t)
	_, vault := engineDomainFor(t, proxy.server.URL)
	domain := newMarketDomain(vault, nil)

	id := strings.Repeat("11", 32)

	for _, command := range []string{"market.accept-take", "market.accept-match"} {
		argument := map[string]string{"take_id": id}

		if command == "market.accept-match" {
			argument = map[string]string{"match_id": id}
		}

		result := domain.Handle(context.Background(), proto.Request{
			Command: command, Args: argument,
		})

		if result.OK || !strings.Contains(result.Error, "no engine") {
			t.Fatalf("%s = %+v", command, result)
		}
	}
}
