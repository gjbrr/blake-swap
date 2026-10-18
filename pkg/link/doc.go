// Package link is the channel shim that lets two wasm instances behave like
// two domains of one moxie runtime.
//
// On native moxie, a pair of domains is one duplex channel. Here the two ends
// live in different JavaScript realms — the page and a service worker — and
// the only transport between them is postMessage, which is asynchronous and
// untyped. The shim hides both facts: the app is handed a
// tinyspawn.Duplex[proto.Envelope], the same type an in-instance peer would
// hand it, and the JavaScript glue in web/page.js and web/worker.js feeds and
// drains it.
//
// The split of responsibility matters:
//
//   - Go owns the domain-facing channel (Recv, Send) and the encoding.
//   - JavaScript owns the transport: it defines __tinylink_post to send and
//     calls __tinylink_deliver when a message arrives.
//   - Neither side reaches into the other's state. The only contract is the
//     two function names and the JSON envelope.
//
// The real file is link_js.go; this file keeps the package documented and
// buildable for native tooling.
package link
