"use strict";

/*
 * Page bootstrap: module startup, the boot gate, and the transport.
 *
 * It has five jobs:
 *
 *   1. drive the boot overlay from real events, not a timer;
 *   2. start the page wasm instance and the service worker's;
 *   3. define __tinylink_post so Go can send;
 *   4. hand every inbound message to __tinylink_deliver, which Go installs;
 *   5. open the app once the channel has actually been proven.
 *
 * It deliberately contains no application protocol knowledge. Kinds,
 * envelopes and dispatch live in pkg/proto and the Go domains, so the two
 * instances cannot drift from each other.
 *
 * The boot gate's completion condition is the one that matters: the greeting
 * has been rendered, which is only possible if the loader, both wasm modules,
 * the worker domain, and the link are all working. A step list that turns green
 * on timers would prove nothing.
 */
(() => {
  /*
   * ---------------------------------------------------------------- boot ---
   */
  const boot = (() => {
    const root = document.getElementById("boot");
    const title = document.getElementById("boot-title");
    const detail = document.getElementById("boot-detail");
    const spinner = document.getElementById("boot-spinner");
    const continueButton = document.getElementById("boot-continue");
    const steps = new Map(
      Array.from(document.querySelectorAll("[data-step]")).map((li) => [li.dataset.step, li]),
    );

    const set = (step, state, text) => {
      const li = steps.get(step);
      if (!li) return;

      li.className = "step step-" + state;

      const label = li.querySelector(".step-text");
      if (label && text) label.textContent = text;
    };

    const say = (text) => {
      if (detail) detail.textContent = text;
    };

    let done = false;

    const complete = () => {
      if (done) return;
      done = true;

      for (const step of steps.keys()) set(step, "done");

      if (title) title.textContent = "ready";
      say("both wasm instances are running");

      // The headless check asserts this rather than the overlay's opacity.
      document.body.dataset.bootComplete = "true";

      root.classList.add("boot-done");

      // Remove it from the accessibility tree and from hit testing once the
      // fade has finished.
      setTimeout(() => {
        root.hidden = true;
      }, 450);
    };

    const fail = (text) => {
      if (done) return;

      root.classList.add("boot-failed");

      if (title) title.textContent = "could not start";
      say(text);

      if (spinner) spinner.hidden = true;

      // Failing closed would strand a user with no way to see what the app
      // knows. Offer the app, without pretending the boot succeeded.
      if (continueButton) {
        continueButton.hidden = false;
        continueButton.addEventListener("click", () => {
          root.hidden = true;
        });
      }
    };

    // The loader is inline and blocking, so by the time this runs it is
    // available. Nothing else has happened yet.
    set("loader", "done", "wasm loader");

    return { set, say, complete, fail };
  })();

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
   * ------------------------------------------------------- message queue ---
   *
   * Messages can arrive before the Go runtime has finished starting (the
   * worker module compiles after the Worker object exists). Queue them rather
   * than dropping: the worker's readiness announcement can legitimately beat
   * the page's deliver hook.
   */
  const pending = [];
  let deliver = null;

  const drain = () => {
    while (pending.length > 0 && deliver) {
      deliver(pending.shift());
    }
  };

  /*
   * ---------------------------------------------------------------- worker ---
   */
  boot.set("worker", "active");

  let worker;
  try {
    worker = new Worker("worker.js");
  } catch (err) {
    status("worker-status", "worker failed to start", "state-bad");
    boot.fail("the browser refused to create the service worker: " + err);
    return;
  }

  /*
   * The transport is typed: an envelope is a JSON object. Anything else on
   * this channel is bootstrap noise, and handing it to the link makes Go log a
   * decode error that looks exactly like a protocol bug. Drop it here instead.
   */
  const isEnvelope = (data) => typeof data === "string" && data.startsWith("{");

  worker.onmessage = (event) => {
    if (!isEnvelope(event.data)) {
      const note = "page: dropped a non-envelope message: " + String(event.data).slice(0, 80);
      trace(note);
      console.warn(note);
      return;
    }

    if (deliver) {
      deliver(event.data);
    } else if (pending.length < 64) {
      pending.push(event.data);
    } else {
      trace("page: early-message queue full, dropped one");
      console.warn("page: early-message queue full, dropped one");
    }
  };

  globalThis.__tinylink_trace = trace;

  worker.onerror = (event) => {
    status("worker-status", "worker error: " + (event.message || "unknown"), "state-bad");
    boot.fail("the worker module failed: " + (event.message || "unknown"));
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
   * Every control that carries data-uicmd sends that command to the page
   * domain. One listener rather than one per button, so a new panel is markup
   * plus a Go case and nothing else.
   */
  document.addEventListener("click", (event) => {
    const target = event.target.closest("[data-uicmd]");
    if (!target || !globalThis.__tinylink_ui) return;
    globalThis.__tinylink_ui(target.dataset.uicmd);
  });

  /*
   * ?autoping=N clicks the button N times shortly after the app opens. It
   * exists so the headless check can exercise the UI command channel and a
   * second round trip without a browser driver.
   */
  const autoping = Number(new URLSearchParams(location.search).get("autoping") || 0);

  /*
   * ?vaulttest=1 clicks the encrypted self-test once the app is running. The
   * check asserts data-vault-verified afterwards, so a browser that cannot run
   * the encrypted round trip fails loudly instead of showing an empty panel.
   */
  if (new URLSearchParams(location.search).get("vaulttest") === "1") {
    const timer = setInterval(() => {
      if (document.body.dataset.bootComplete !== "true") return;
      clearInterval(timer);
      const button = document.querySelector("[data-uicmd='vault-selftest']");
      if (button) button.click();
    }, 50);
  }

  if (autoping > 0) {
    const timer = setInterval(() => {
      if (document.body.dataset.bootComplete !== "true") return;
      clearInterval(timer);
      const button = document.getElementById("ping");
      for (let i = 0; i < autoping; i += 1) button.click();
    }, 50);
  }

  /*
   * --------------------------------------------------------- boot watcher ---
   *
   * Two document flags, both set by the page domain when it sees the
   * corresponding protocol message:
   *
   *   data-worker-ready    the worker's loop is running
   *   data-hello-verified  the greeting came back and rendered
   *
   * The second implies the first, the link, and both wasm modules.
   */
  let deadline = null;

  const watch = setInterval(() => {
    const dataset = document.body.dataset;

    if (dataset.workerReady === "true") {
      boot.set("worker", "done");
      boot.say("worker module running");
    }

    if (deliver) {
      boot.set("link", "done");
    }

    if (dataset.helloVerified === "true") {
      clearInterval(watch);

      if (deadline !== null) clearTimeout(deadline);

      boot.set("link", "done");
      boot.set("page", "done");
      boot.set("worker", "done");
      boot.complete();
    }
  }, 50);

  /*
   * The deadline is a guard, not a completion path: if the greeting never
   * arrives, the overlay must say so instead of spinning forever.
   */
  deadline = setTimeout(() => {
    if (document.body.dataset.bootComplete === "true") return;
    boot.fail("the worker did not answer within 20 seconds");
  }, 20000);

  /*
   * --------------------------------------------------------------- page ---
   */
  boot.set("page", "active");
  boot.say("loading the page module…");

  const go = new Go();

  WebAssembly.instantiateStreaming(fetch("page.wasm"), go.importObject)
    .then(({ instance }) => {
      // Deliberately not awaited: the page's main() runs for the life of
      // the document, and the hooks above are already in place.
      go.run(instance);
    })
    .catch((err) => {
      status("page-status", "wasm failed to load", "state-bad");
      boot.fail("the page module failed to load: " + err);
      console.error("page: page.wasm failed to load:", err);
    });

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
      boot.set("link", "done");
      boot.say("channel link connected");
    }
  }, 10);
})();
