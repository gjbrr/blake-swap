"use strict";

/*
 * Worker bootstrap.
 *
 * The mirror image of page.js: start worker.wasm inside the worker's global
 * scope, define __tinylink_post to reply to the page, and deliver inbound
 * messages to __tinylink_deliver.
 *
 * Order matters. Two things must exist before go.run(instance):
 *
 *   1. the post hook, because the worker may answer immediately; and
 *   2. the deliver pump, because the page's opening hello is queued by the
 *      browser before this script has even run.
 *
 * go.run() does not return while main() is alive, so anything placed after it
 * never executes. That is the one non-obvious rule of this file.
 *
 * Nothing in this file knows what a Nostr event is. The relay proxy, signature
 * checking and subscription management are Go domains that will be spawned from
 * cmd/webworker, not JavaScript behaviour, so this file should stay this small.
 */
importScripts("wasm_exec.js");

const pending = [];
let deliver = null;

const drain = () => {
  while (pending.length > 0 && deliver) {
    deliver(pending.shift());
  }
};

/*
 * Poll for the deliver hook Go installs from main(). This is installed before
 * the Go program starts, so a message that arrives during instantiation is
 * still delivered once the domain is ready.
 */
setInterval(() => {
  if (!deliver && globalThis.__tinylink_deliver) {
    deliver = globalThis.__tinylink_deliver;
    drain();
  }
}, 10);

globalThis.__tinylink_post = (text) => self.postMessage(text);

/*
 * The transport is typed: an envelope is a JSON object. Anything else is
 * bootstrap noise and must never reach the link, which would refuse to parse it
 * and log a decode error that looks like a protocol bug.
 */
const isEnvelope = (data) => typeof data === "string" && data.startsWith("{");

self.onmessage = (event) => {
  if (!isEnvelope(event.data)) {
    console.warn(
      "worker: dropped a non-envelope message:",
      String(event.data).slice(0, 80),
    );
    return;
  }

  if (deliver) {
    deliver(event.data);
  } else if (pending.length < 64) {
    pending.push(event.data);
  } else {
    console.warn("worker: early-message queue full, dropped one");
  }
};

const go = new Go();

WebAssembly.instantiateStreaming(fetch("worker.wasm"), go.importObject)
  .then(({ instance }) => {
    // Deliberately not awaited: main() never returns, and the hooks above
    // already handle everything that arrives while it runs.
    go.run(instance);
  })
  .catch((err) => {
    console.error("worker: worker.wasm failed to load:", err);
  });
