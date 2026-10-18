//go:build js && wasm

package link

import (
	"fmt"
	"syscall/js"

	"git.smesh.lol/tiny-spawn"

	"github.com/gjbrr/blake-swap/pkg/proto"
)

// DeliverName is the global the JavaScript side must define before it calls
// into Go. PostName is the global Go calls to send. They are constants so the
// bootstrap scripts and this file cannot disagree silently: web/page.js and
// web/worker.js define exactly these names.
const (
	PostName    = "__tinylink_post"
	DeliverName = "__tinylink_deliver"
)

// Bridge is the transport end of the link: it receives from JavaScript and
// posts to it. The app never touches a Bridge; it gets the Duplex.
type Bridge struct {
	in   chan proto.Envelope
	out  chan proto.Envelope
	done chan struct{}
}

// New creates the bridge and the duplex the app will use.
//
// buffer sizes both directions. A full inbound buffer means the app is not
// draining its loop; the delivery is dropped and reported, because blocking
// inside a JavaScript event handler would stall the whole realm — the same
// way a stalled domain in moxie must not silently wedge the parent.
func New(buffer int) (*Bridge, tinyspawn.Duplex[proto.Envelope]) {
	if buffer < 1 {
		buffer = 1
	}

	b := &Bridge{
		in:   make(chan proto.Envelope, buffer),
		out:  make(chan proto.Envelope, buffer),
		done: make(chan struct{}),
	}

	//
	// The only goroutine the shim owns: it drains the app's Tx side and
	// hands each message to JavaScript. Keeping it here means the app code
	// sees channels only, exactly as it would in one instance.
	//
	go b.pump()

	return b, tinyspawn.Duplex[proto.Envelope]{Tx: b.out, Rx: b.in}
}

// pump posts every outbound envelope in order.
func (b *Bridge) pump() {
	for {
		select {
		case e := <-b.out:
			b.post(e)
		case <-b.done:
			return
		}
	}
}

// post encodes and hands one envelope to the JavaScript transport.
func (b *Bridge) post(e proto.Envelope) {
	text, err := proto.Encode(e)
	if err != nil {
		consoleError(fmt.Sprintf("link: %v", err))
		return
	}

	fn := js.Global().Get(PostName)

	if fn.Type() != js.TypeFunction {
		consoleError("link: " + PostName + " is not defined by the bootstrap")
		return
	}

	fn.Invoke(text)
}

// Deliver is called by JavaScript for every message from the peer. It must
// never block: a JavaScript event handler that blocks stops the realm, so a
// full buffer drops the message and says so.
func (b *Bridge) Deliver(text string) {
	e, err := proto.Decode(text)
	if err != nil {
		consoleError(fmt.Sprintf("link: %v", err))
		return
	}

	select {
	case b.in <- e:
	case <-b.done:
	default:
		consoleError("link: inbound buffer full, dropped a message")
	}
}

// Close stops the outbound pump. Deliver becomes a no-op afterwards.
func (b *Bridge) Close() {
	select {
	case <-b.done:
	default:
		close(b.done)
	}
}

// consoleError reports through the browser console so a broken transport is
// visible in the one place an operator will look.
func consoleError(msg string) {
	js.Global().Get("console").Call("error", msg)
}
