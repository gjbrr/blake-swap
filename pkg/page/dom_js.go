//go:build js && wasm

package page

import "syscall/js"

//
// This file is the page's only route to the document. Keeping DOM access in
// one place means the domain's loop reads as protocol code, and the browser
// surface can be replaced (or mocked) without touching it.
//

func element(id string) js.Value {
	el := js.Global().Get("document").Call("getElementById", id)

	if el.Type() == js.TypeNull || el.Type() == js.TypeUndefined {
		return js.Undefined()
	}

	return el
}

func setText(id, text string) {
	el := element(id)

	if el.IsUndefined() {
		return
	}

	el.Set("textContent", text)
}

// setPageStatus renders the page instance's own state.
func setPageStatus(text, state string) {
	setText(ElementPageStatus, text)
	setStateClass(ElementPageStatus, state)
}

// setWorkerStatus renders what the page knows about the worker. The worker
// cannot touch the document: it is a different realm, so this is the page
// reflecting an observation, not the worker reporting to the DOM.
func setWorkerStatus(text, state string) {
	setText(ElementWorkerStatus, text)
	setStateClass(ElementWorkerStatus, state)
}

func setHello(text string) {
	setText(ElementHello, text)
}

func setRoundTrip(text string) {
	setText(ElementRoundTrip, text)
}

// setStateClass replaces the state marker class on an element.
func setStateClass(id, state string) {
	el := element(id)

	if el.IsUndefined() {
		return
	}

	list := el.Get("classList")

	for _, name := range []string{"state-wait", "state-ok", "state-bad"} {
		list.Call("remove", name)
	}

	switch state {
	case "ok":
		list.Call("add", "state-ok")
	case "bad":
		list.Call("add", "state-bad")
	default:
		list.Call("add", "state-wait")
	}
}

// markVerified records success on the document so the headless check does not
// have to match presentation prose.
func markVerified() {
	js.Global().Get("document").Get("body").Call(
		"setAttribute",
		VerifiedAttribute,
		"true",
	)
}

func consoleLog(msg string) {
	js.Global().Get("console").Call("log", msg)
}
