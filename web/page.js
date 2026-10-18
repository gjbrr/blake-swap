"use strict";

/*
 * Page bootstrap.
 *
 * This file is transport, not application logic. It has exactly four jobs:
 *
 *   1. start the page wasm instance;
 *   2. create the service worker and start its wasm instance;
 *   3. define __tinylink_post so Go can send;
 *   4. hand every inbound message to __tinylink_deliver, which Go installs.
 *
 * It deliberately contains no protocol knowledge. Kinds, envelopes and
 * dispatch live in pkg/proto and the Go domains, so the two instances cannot
 * drift from each other.
 */
(() => {
  const trace = (text) => {
    const el = document.getElementById("trace");
    if (el) el.textContent += text + "\n";
  };

  const status = (id, text, state) => {
    const el = document.getElementById(id);
    if (!el) return;
    el.textContent = text;
    el.classList.remove("state-wait", "state-ok", "state-bad");
    el.classList.add(state || "state-wait");
  };

  /*
   * Messages can arrive before the Go runtime has finished starting (the wasm
   * module compiles after the Worker object exists). Queue them rather than
   * dropping: the worker may legitimately answer the opening hello before the
   * page's deliver hook is installed.
   */
  const pending = [];
  let deliver = null;

  const drain = () => {
    while (pending.length > 0 && deliver) {
      deliver(pending.shift());
    }
  };

  let worker;
  try {
    worker = new Worker("worker.js");
  } catch (err) {
    status("worker-status", "worker failed to start", "state-bad");
    console.error("page: could not create the worker:", err);
    return;
  }

  worker.onmessage = (event) => {
    if (String(event.data).startsWith("TRACE: ")) {
      trace(String(event.data));
    }
    if (deliver) {
      deliver(event.data);
    } else {
      pending.push(event.data);
    }
  };

  globalThis.__tinylink_trace = trace;

  worker.onerror = (event) => {
    status("worker-status", "worker error: " + (event.message || "unknown"), "state-bad");
    console.error("page: worker error:", event);
  };

  /*
   * The transport hook Go calls. It must exist before the wasm module starts
   * sending, which is why it is defined before instantiation.
   */
  globalThis.__tinylink_post = (text) => worker.postMessage(text);

  document.getElementById("ping").addEventListener("click", () => {
    if (globalThis.__tinylink_ui) {
      globalThis.__tinylink_ui("ping");
    }
  });

  /*
   * ?autoping=N clicks the button N times shortly after load. It exists so the
   * headless check can exercise the UI command channel and a second round trip
   * without a browser driver.
   */
  const autoping = Number(new URLSearchParams(location.search).get("autoping") || 0);

  if (autoping > 0) {
    setTimeout(() => {
      const button = document.getElementById("ping");
      for (let i = 0; i < autoping; i += 1) button.click();
    }, 1500);
  }

  /*
   * Go installs __tinylink_deliver from main(). Poll for it, then replay
   * anything that arrived early. This is installed before the Go program
   * starts: go.run() does not return while main() is alive, so a poll placed
   * after it would never run.
   */
  setInterval(() => {
    if (!deliver && globalThis.__tinylink_deliver) {
      deliver = globalThis.__tinylink_deliver;
      trace("deliver hook connected, pending=" + pending.length);
      drain();
    }
  }, 10);

  const go = new Go();

  WebAssembly.instantiateStreaming(fetch("page.wasm"), go.importObject)
    .then(({ instance }) => {
      // Deliberately not awaited: the page's main() runs for the life of
      // the document, and the hooks above are already in place.
      go.run(instance);
    })
    .catch((err) => {
      status("page-status", "wasm failed to load", "state-bad");
      console.error("page: page.wasm failed to load:", err);
    });
})();
