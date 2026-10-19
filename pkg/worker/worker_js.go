//go:build js && wasm

package worker

import (
	"context"
	"syscall/js"
	"time"

	"git.smesh.lol/tiny-spawn"

	"github.com/gjbrr/blake-swap/pkg/platform"
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
// The domains it serves — the vault, the engine, the market and the settlement
// transport — are values built here and dispatched by command prefix, so the
// work stays in the domain that owns it. A case in this switch is an envelope
// kind, not a job.
func Domain(_ struct{}, ch Chans) {
	consoleLog("worker: domain started")

	// One vault per worker: the derived key lives here and nowhere else, and it dies
	// with the instance.
	vault := NewVaultDomain(platform.NewIndexedDB("blake-swap", "vault"), platform.Reader{})

	// Every relay connection is the browser's own WebSocket: the same relay client the
	// CLI uses, over the platform seam, with the vault's identity as the signer.
	dialer := relayDialer()

	// The engine carries the same transport, so its announce and receive commands work
	// in the browser unchanged.
	engine := newEngineDomain(vault, dialer)

	// The market's connection is that dialer over the vault's relay list. Its accept path
	// also needs the engine, because accepting a take creates a swap.
	market := newMarketDomain(vault, dialer).withEngine(engine.build)

	// The settlement transport is the same dialer and the same relay list, with the
	// seven message types the engine already knows how to build and check.
	settlement := newCoordDomain(vault, dialer)

	ctx := context.Background()

	//
	// Announce readiness before serving anything: the page's boot screen
	// needs to know the module is running, and that is a different fact
	// from the worker having answered a request.
	//
	ch.Link.Send(proto.Envelope{Kind: proto.WorkerReady})

	for {
		select {
		case e := <-ch.Link.Recv():
			switch e.Kind {
			case proto.Hello:
				answerHello(e, ch.Link)

			case proto.WorkerReady:
				// A peer that announces readiness twice is harmless;
				// there is nothing to do with the second one.

			case proto.Command:
				//
				// Log the dispatch: a command that arrives and never answers is the
				// hardest failure to see in a browser, and the two timestamps make it
				// obvious whether the work started.
				//
				started := time.Now()

				consoleLog("worker: command received (seq " + itoa(e.Seq) + ")")

				result := handleCommand(ctx, vault, engine, market, settlement, e.Body)

				consoleLog("worker: command finished (ok=" + boolText(result.OK) +
					", " + itoa(uint32(time.Since(started).Milliseconds())) + " ms)")

				body, encodeErr := proto.EncodeResult(result)

				if encodeErr != nil {
					consoleLog("worker: could not encode a result: " + encodeErr.Error())

					continue
				}

				ch.Link.Send(proto.Envelope{
					Kind: proto.CommandResult,
					Seq:  e.Seq,
					Body: body,
					AtMS: e.AtMS,
				})

			default:
				//
				// An unknown kind is logged and ignored. The relay work is
				// not a message kind at all: it is the `coord.` commands,
				// which the page sends over this same channel like every
				// other command. A worker that guesses at unknown messages
				// is how two implementations drift apart.
				//
				consoleLog("worker: ignoring unexpected kind " + string(e.Kind))
			}

		case <-ch.Stop.Done():
			consoleLog("worker: domain stopping")
			return
		}
	}
}

// boolText renders a boolean for a log line.
func boolText(value bool) string {
	if value {
		return "true"
	}

	return "false"
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

func consoleLog(msg string) {
	js.Global().Get("console").Call("log", msg)
}
