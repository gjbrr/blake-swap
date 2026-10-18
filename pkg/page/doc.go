// Package page is the page-side domain: the wasm instance running in the
// document.
//
// It owns presentation only. It sends the opening hello, renders whatever the
// worker answers, and exposes one UI command (a re-ping) so the link can be
// exercised repeatedly without reloading. Protocol work, relay proxying and
// signature checking belong in the worker; when they grow, they become more
// domains here talking over the same kind of duplex.
//
// The real file is page_js.go; this file keeps the package documented and
// buildable for native tooling.
package page
