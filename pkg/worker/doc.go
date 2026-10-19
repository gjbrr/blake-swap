// Package worker is the service-worker-side domain.
//
// It is the second wasm instance, and the place the backend work lives: relay
// proxying, signature checking, Nostr event handling, and the swap domains that need
// to keep running while the page is busy. The real file is worker_js.go; this file
// keeps the package documented and buildable for native tooling.
//
// # The domains
//
// One long-lived loop dispatches a command to a domain by its prefix, and each domain
// is built the same way: the vault supplies the identity and the configuration, and a
// dialer supplies the socket. A domain never reaches for either itself, which is what
// lets every one of them be tested against a relay stub without a browser.
//
//	vault.    the encrypted state: create, unlock, lock, get, put, the self-test
//	identity. the Nostr key this participant signs with
//	proxy.    where the app server's node proxy is
//	engine.   the read-only engine over that proxy, with the coordination transport wired in
//	market.   the marketplace: relays, read, publish, take
//	coord.    the settlement transport: the seven message types, send and receive
//
// # The coordination domain
//
// `coord.` is the browser's real settlement transport. It is the same
// `coord.NostrTransport` the CLI drives — sender pinning, the reference's NIP-44
// policy, fail-closed decryption, the canonical wire schema — over the browser's own
// `platform.WebSocket`, with the relay list and the identity taken from the vault. The
// message bodies are built by `swap.MessageBody` from the swap record the worker holds,
// the same builder the engine's announce commands use, so the page never supplies a
// body and the two clients cannot drift into two wire formats.
//
// The engine domain carries the same transport as its `engine.Transport`, so the
// engine's `Announce*` and `Receive*` methods work in the browser unchanged; the
// `coord.` commands are the transport-level half, which is the part that works with
// nothing but a relay.
//
//	coord.mode | coord.set-mode MODE     the confidentiality mode: off, or nip44_v2
//	coord.open BLAKE SATS [PEER]          a local swap record to coordinate about
//	coord.<type>.send SWAP_ID            publish one signed settlement message
//	coord.<type>.receive SWAP_ID         read, validate and render one
//
// The seven types are the wire schema's: offer, accept, blake-funded, btc-funded,
// btc-claimed, blake-claimed, btc-refunded.
package worker
