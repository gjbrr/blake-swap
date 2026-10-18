// Package worker is the service-worker-side domain.
//
// It is the second wasm instance, and the place the backend work lives: relay
// proxying, signature checking, Nostr event handling, and the swap domains
// that need to keep running while the page is busy. None of that is here yet.
// What is here is the event loop, the channel shim end, and one answer that
// proves the two instances are actually talking.
//
// The real file is worker_js.go; this file keeps the package documented and
// buildable for native tooling.
package worker
