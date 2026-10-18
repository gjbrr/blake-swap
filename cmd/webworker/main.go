// Command webworker is the service worker's wasm entry point.
//
// It is the second wasm instance. The entry point owns the process-lifetime
// handles: the link, the exported JavaScript functions, and the domain tree.
// When the relay proxy, signature verification and Nostr work arrive, they are
// spawned here as sibling domains with their own channels — the page never
// gets a direct reference to any of them.
//
// Build with: tinygo build -target=wasm -o web/worker.wasm ./cmd/webworker

//go:build js && wasm

package main

import (
	"syscall/js"

	"git.smesh.lol/tiny-spawn"

	"github.com/gjbrr/blake-swap/pkg/link"
	"github.com/gjbrr/blake-swap/pkg/worker"
)

func main() {
	bridge, duplex := link.New(32)
	stop := tinyspawn.NewBreaker()

	deliver := exportDeliver(bridge)

	js.Global().Get("console").Call(
		"log",
		"worker: wasm started",
	)

	done := tinyspawn.Spawn(worker.Domain, struct{}{}, worker.Chans{
		Link: duplex,
		Stop: stop,
	})

	<-done

	bridge.Close()

	_ = deliver
}

// exportDeliver installs the function the worker bootstrap calls for every
// inbound message from the page.
func exportDeliver(bridge *link.Bridge) js.Func {
	fn := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) > 0 {
			bridge.Deliver(args[0].String())
		}

		return nil
	})

	js.Global().Set(link.DeliverName, fn)

	return fn
}
