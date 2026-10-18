//go:build js && wasm

package page

import (
	"fmt"
	"strings"
	"time"

	"git.smesh.lol/tiny-spawn"

	"github.com/gjbrr/blake-swap/pkg/proto"
)

// Chans is the page domain's fixed channel set. Per the tiny-spawn rules it is
// bound at spawn and never changes: the link carries worker messages and UI
// carries commands from the document's controls.
type Chans struct {
	Link tinyspawn.Duplex[proto.Envelope]
	UI   <-chan string
	Stop tinyspawn.Breaker
}

// UICmd is the command a DOM control sends. The string crosses from
// JavaScript, which is the realm boundary, so keep the vocabulary tiny and
// named rather than accepting arbitrary text.
type UICmd = string

// UICmdPing asks the domain to send another hello.
const UICmdPing UICmd = "ping"

// The element ids are part of the page contract: index.html renders them and
// the headless check in scripts/verify-hello.sh reads them. Keeping them in
// one place stops the three from drifting.
const (
	ElementPageStatus   = "page-status"
	ElementWorkerStatus = "worker-status"
	ElementHello        = "hello"
	ElementRoundTrip    = "round-trip"

	// VerifiedAttribute marks the document once the worker's greeting has
	// been rendered, so a test can assert the round trip without scraping
	// presentation text.
	VerifiedAttribute = "data-hello-verified"
)

// Domain is the page's event loop. It runs until Stop is closed.
//
// The state it owns is the outbound sequence number. That is deliberately the
// only mutable state on this side of the link.
func Domain(_ struct{}, ch Chans) {
	defer setPageStatus("stopped", "bad")

	seq := uint32(0)

	sendHello := func() {
		seq++

		ch.Link.Send(proto.Envelope{
			Kind: proto.Hello,
			Seq:  seq,
			Body: "hello from the page",
			AtMS: time.Now().UnixMilli(),
		})
	}

	setPageStatus("waiting for the worker", "wait")
	sendHello()

	for {
		select {
		case e := <-ch.Link.Recv():
			switch e.Kind {
			case proto.WorkerReady:
				//
				// The worker's loop is up. Say so without claiming it
				// has answered: that is what the ack is for.
				//
				markWorkerReady()
				setWorkerStatus("connected", "wait")

			case proto.HelloAck:
				renderAck(e)

			default:
				consoleLog("page: ignoring unexpected kind " + string(e.Kind))
			}

		case cmd := <-ch.UI:
			switch cmd {
			case UICmdPing:
				setPageStatus("pinging the worker", "wait")
				sendHello()

			default:
				consoleLog("page: ignoring unknown UI command " + cmd)
			}

		case <-ch.Stop.Done():
			return
		}
	}
}

// renderAck displays the worker's greeting. This is the visible proof the
// skeleton exists for: the text comes from a different wasm instance, over the
// channel shim, and lands in the document here.
func renderAck(e proto.Envelope) {
	if strings.TrimSpace(e.Body) == "" {
		setWorkerStatus("empty answer", "bad")
		return
	}

	setHello(e.Body)

	micros := time.Now().UnixMilli() - e.AtMS

	if e.AtMS > 0 {
		setRoundTrip(fmt.Sprintf("%d ms, seq %d", micros, e.Seq))
	} else {
		setRoundTrip(fmt.Sprintf("seq %d", e.Seq))
	}

	setWorkerStatus("verified", "ok")
	setPageStatus("running", "ok")
	markVerified()
}
