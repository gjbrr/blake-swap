//go:build js && wasm

package worker

import (
	"syscall/js"

	"git.smesh.lol/tiny-spawn"

	"github.com/gjbrr/blake-swap/pkg/proto"
)

// Chans is the worker domain's fixed channel set.
type Chans struct {
	Link tinyspawn.Duplex[proto.Envelope]
	Stop tinyspawn.Breaker
}

// Greeting is what the worker answers with. The page displays it, so a change
// here is visible on screen rather than hidden in a log.
const Greeting = "hello world"

// Domain is the worker's event loop.
//
// It is intentionally a server: one long-lived loop that dispatches on Kind.
// The work that will fill it — relay proxy connections, signature verification,
// Nostr subscriptions — becomes sibling domains spawned by cmd/webworker, each
// with its own channels, not more cases in this switch. A case here is a
// message type, not a job.
func Domain(_ struct{}, ch Chans) {
	consoleLog("worker: domain started")

	for {
		select {
		case e := <-ch.Link.Recv():
			switch e.Kind {
			case proto.Hello:
				answerHello(e, ch.Link)

			default:
				//
				// TODO: relay proxy, signature checks, Nostr subscription
				// management. Until then an unknown kind is logged and
				// ignored: a worker that guesses at unknown messages is how
				// two implementations drift apart.
				//
				consoleLog("worker: ignoring unexpected kind " + string(e.Kind))
			}

		case <-ch.Stop.Done():
			consoleLog("worker: domain stopping")
			return
		}
	}
}

// answerHello replies with the greeting, echoing the caller's sequence and
// timestamp so the page can meter the round trip with its own clock.
func answerHello(e proto.Envelope, link tinyspawn.Duplex[proto.Envelope]) {
	consoleLog("worker: hello seq " + itoa(e.Seq) + " (" + e.Body + ")")

	link.Send(proto.Envelope{
		Kind: proto.HelloAck,
		Seq:  e.Seq,
		Body: Greeting,
		AtMS: e.AtMS,
	})
}

func itoa(n uint32) string {
	if n == 0 {
		return "0"
	}

	var buf [10]byte
	i := len(buf)

	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}

	return string(buf[i:])
}

func consoleLog(msg string) {
	js.Global().Get("console").Call("log", msg)
}
