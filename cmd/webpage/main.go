// Command webpage is the page's wasm entry point.
//
// The entry point is the one place allowed to hold process-lifetime handles:
// it creates the link, exports the JavaScript-facing functions, spawns the
// domain tree, and then blocks for the life of the page. A spawned domain may
// not do any of that — see the tiny-spawn rules.
//
// Build with: tinygo build -target=wasm -o web/page.wasm ./cmd/webpage

//go:build js && wasm

package main

import (
	"syscall/js"

	"git.smesh.lol/tiny-spawn"

	"github.com/gjbrr/blake-swap/pkg/link"
	"github.com/gjbrr/blake-swap/pkg/page"
)

func main() {
	bridge, duplex := link.New(32)
	ui := make(chan page.UICmd, 8)
	stop := tinyspawn.NewBreaker()

	//
	// Keep the exported functions reachable for the life of the program.
	// js.Func values must not be garbage collected while JavaScript can
	// still call them.
	//
	deliver := exportDeliver(bridge)
	uiHook := exportUI(ui)

	js.Global().Get("console").Call(
		"log",
		"page: wasm started",
	)

	done := tinyspawn.Spawn(page.Domain, struct{}{}, page.Chans{
		Link: duplex,
		UI:   ui,
		Stop: stop,
	})

	// Run until the domain returns. On a page that is normally forever.
	<-done

	bridge.Close()

	// These keep the hooks reachable; that is their entire purpose.
	_ = deliver
	_ = uiHook
}

// exportDeliver installs the function the bootstrap calls for every inbound
// message from the worker.
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

// exportUI installs the function the document's controls call. Today that is
// one button asking for another ping.
func exportUI(ui chan<- page.UICmd) js.Func {
	fn := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) > 0 {
			select {
			case ui <- args[0].String():
			default:
				js.Global().Get("console").Call(
					"warn",
					"page: UI queue full, command dropped",
				)
			}
		}

		return nil
	})

	js.Global().Set("__tinylink_ui", fn)

	return fn
}
