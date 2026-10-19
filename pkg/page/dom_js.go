//go:build js && wasm

package page

import "syscall/js"

//
// This file is the page's only route to the document. Keeping DOM access in
// one place means the domains read as protocol code, and the browser surface can
// be replaced (or mocked) without touching them.
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
//
// The class and the text go together: a colour alone is not a state label, and a
// reader who cannot see the colour must still be able to tell "ok" from "failed".
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

// markWorkerReady records that the worker's domain loop is running. The boot
// overlay reads these flags rather than scraping status text, so presentation
// changes cannot break the gate.
func markWorkerReady() {
	mark("data-worker-ready")
}

// markVerified records success on the document so the headless check does not
// have to match presentation prose.
func markVerified() {
	mark(VerifiedAttribute)
}

// mark sets one document marker.
//
// The markers are the page's contract with the headless check: a check that asserted
// panel text would break on every wording change, and a check that asserted nothing
// would pass on a page that never ran the command.
func mark(attribute string) {
	js.Global().Get("document").Get("body").Call(
		"setAttribute",
		attribute,
		"true",
	)
}

// inputValue reads a text field's current value. An absent field reads as empty, which
// the worker then refuses with its own message instead of the page inventing a default.
func inputValue(id string) string {
	el := element(id)

	if el.IsUndefined() {
		return ""
	}

	value := el.Get("value")

	if value.Type() != js.TypeString {
		return ""
	}

	return value.String()
}

// setInputValue puts a value into a field.
func setInputValue(id, value string) {
	el := element(id)

	if el.IsUndefined() {
		return
	}

	el.Set("value", value)
}

// unmark removes a document marker, for a fact that stopped being true.
func unmark(attribute string) {
	js.Global().Get("document").Get("body").Call(
		"removeAttribute",
		attribute,
	)
}

// setVaultText renders the worker's command output.
func setVaultText(text string) {
	setText(ElementVault, text)
}

func consoleLog(msg string) {
	js.Global().Get("console").Call("log", msg)
}

// ------------------------------------------------------------------ elements ---

// node is one element under construction.
//
// It exists so a render function reads as the document it builds rather than as a
// sequence of syscall/js calls, and so the only file that touches the DOM stays this one.
type node struct{ value js.Value }

// elem creates an element.
func elem(tag string) node {
	return node{value: js.Global().Get("document").Call("createElement", tag)}
}

// text sets the element's text content.
func (n node) text(content string) node {
	n.value.Set("textContent", content)

	return n
}

// attr sets one attribute.
func (n node) attr(name, value string) node {
	n.value.Call("setAttribute", name, value)

	return n
}

// class adds one class name.
func (n node) class(name string) node {
	n.value.Get("classList").Call("add", name)

	return n
}

// child appends a child element.
func (n node) child(child node) node {
	n.value.Call("appendChild", child.value)

	return n
}

// childText appends a child element with text.
func (n node) childText(tag, content string) node {
	return n.child(elem(tag).text(content))
}

// onData records the command a click on this element sends.
func (n node) onCmd(command string) node {
	return n.attr("data-uicmd", command)
}

// put replaces a container's children with one node.
func put(containerID string, content node) {
	el := element(containerID)

	if el.IsUndefined() {
		return
	}

	el.Set("textContent", "")
	el.Call("appendChild", content.value)
}

// clear empties a container.
func clear(containerID string) {
	el := element(containerID)

	if el.IsUndefined() {
		return
	}

	el.Set("textContent", "")
}

// appendTo adds one node to a container.
func appendTo(containerID string, content node) {
	el := element(containerID)

	if el.IsUndefined() {
		return
	}

	el.Call("appendChild", content.value)
}

// jsSetBodyAttr sets one attribute on the document body.
func jsSetBodyAttr(name, value string) {
	js.Global().Get("document").Get("body").Call("setAttribute", name, value)
}

// documentOrigin is where this document was loaded from.
func documentOrigin() string {
	origin := js.Global().Get("location").Get("origin")

	if origin.Type() != js.TypeString {
		return ""
	}

	return origin.String()
}

// setAttr sets one attribute on an element by id.
func setAttr(id, name, value string) {
	el := element(id)

	if el.IsUndefined() {
		return
	}

	el.Call("setAttribute", name, value)
}

// toggle shows or hides an element.
func toggle(id string, visible bool) {
	el := element(id)

	if el.IsUndefined() {
		return
	}

	el.Set("hidden", !visible)
}

// setDisabled disables or enables a button by id.
func setDisabled(id string, disabled bool) {
	el := element(id)

	if el.IsUndefined() {
		return
	}

	el.Set("disabled", disabled)
}

// -------------------------------------------------------------------- dialog ---

// dialogOpen shows the confirmation dialog with a title and a body.
func dialogOpen(title string, body node) {
	openDialog(element(DialogConfirm), title, body)
}

// openDialog fills and opens the dialog element.
func openDialog(dialog js.Value, title string, body node) {
	if dialog.IsUndefined() {
		return
	}

	setText(DialogTitle, title)
	put(DialogBody, body)

	dialog.Call("showModal")
	setAttr(DialogConfirm, "data-confirm-open", "true")
}

// dialogClose closes the dialog, whatever opened it.
func dialogClose() {
	dialog := element(DialogConfirm)

	if dialog.IsUndefined() {
		return
	}

	if dialog.Get("open").Truthy() {
		dialog.Call("close")
	}

	setAttr(DialogConfirm, "data-confirm-open", "false")
}

// dialogIsOpen reports whether the dialog is showing.
func dialogIsOpen() bool {
	dialog := element(DialogConfirm)

	if dialog.IsUndefined() {
		return false
	}

	return dialog.Get("open").Truthy()
}

// focusElement moves keyboard focus to an element by id.
func focusElement(id string) {
	el := element(id)

	if el.IsUndefined() {
		return
	}

	el.Call("focus")
}

// activeElementID is the id of the element that has focus, which is how the dialog
// remembers where to return it to.
func activeElementID() string {
	el := js.Global().Get("document").Get("activeElement")

	if el.IsUndefined() || el.Type() != js.TypeObject {
		return ""
	}

	id := el.Get("id")

	if id.Type() != js.TypeString {
		return ""
	}

	return id.String()
}

// focusCommand focuses the first element carrying a data-uicmd value.
func focusCommand(command string) {
	el := js.Global().Get("document").Call(
		"querySelector", "[data-uicmd='"+command+"']",
	)

	if el.IsUndefined() || el.Type() != js.TypeObject {
		return
	}

	el.Call("focus")
}
