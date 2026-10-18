#!/usr/bin/env bash
#
# Prove the page and the worker are really talking.
#
# The check loads the application in a headless browser, clicks the re-ping
# control twice through ?autoping=2, and then asserts on the rendered document:
#
#   * the hello box shows the greeting the worker sent;
#   * the worker status reads "verified";
#   * the round trip sequence reached 3 (the opening hello plus two pings),
#     which is only possible if the UI command channel and the link both work;
#   * the document carries data-hello-verified="true";
#   * the boot gate cleared (data-boot-complete="true") and the overlay is
#     hidden, so the app is only presented once it is actually running.
#
# Two screenshots are written next to the app so the result can be looked at,
# not only grepped: the running app, and the boot overlay as a user sees it
# during the wasm compile (captured from a script-free copy, because any
# screenshot of the live app races the compile).
#
# Exit status: 0 pass, 1 fail, 77 skipped (no browser).

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PORT="${VERIFY_PORT:-8799}"
ADDR="127.0.0.1:${PORT}"
URL="http://${ADDR}/?autoping=2"
BUDGET="${VERIFY_BUDGET:-45000}"
SHOT="$ROOT/web/hello-screenshot.png"
BOOT_SHOT="$ROOT/web/boot-screenshot.png"

BROWSER="${BROWSER:-}"

# A BROWSER from the environment wins only if it is actually runnable; some
# desktops export a browser that is not on PATH for a non-login shell.
if [[ -n "$BROWSER" ]] && ! command -v "$BROWSER" >/dev/null 2>&1; then
    echo "note: BROWSER=$BROWSER is not runnable, looking for one on PATH" >&2
    BROWSER=""
fi

if [[ -z "$BROWSER" ]]; then
    for candidate in chromium chromium-browser google-chrome google-chrome-stable brave-browser; do
        if command -v "$candidate" >/dev/null 2>&1; then
            BROWSER="$candidate"
            break
        fi
    done
fi

if [[ -z "$BROWSER" ]]; then
    echo "SKIP: no chromium/chrome found; set BROWSER to run the visual check"
    exit 77
fi

if [[ ! -f web/page.wasm || ! -f web/worker.wasm ]]; then
    echo "ERROR: wasm modules missing; run make wasm" >&2
    exit 1
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/blake-swap-verify-XXXXXX")"
SERVER_PID=""
STATIC_PID=""

cleanup() {
    for pid in "$SERVER_PID" "$STATIC_PID"; do
        if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
            kill "$pid" 2>/dev/null || true
            wait "$pid" 2>/dev/null || true
        fi
    done

    if [[ "${VERIFY_KEEP:-0}" == "1" ]]; then
        echo "note: keeping $WORK for inspection" >&2
        return
    fi

    rm -rf "$WORK"
}

trap cleanup EXIT

fail() {
    echo
    echo "FAIL: $*" >&2

    if [[ -s "$WORK/console.log" ]]; then
        echo "--- browser console ---" >&2
        grep -E 'CONSOLE|ERROR' "$WORK/console.log" | tail -20 >&2
    fi

    exit 1
}

# The server is built with TinyGo like the wasm modules; Go is the fallback if
# someone only has the wasm toolchain half-installed.
TINYGO="${TINYGO:-tinygo}"

if command -v "$TINYGO" >/dev/null 2>&1; then
    "$TINYGO" build -o "$WORK/server" ./cmd/server
else
    "${GO:-go}" build -o "$WORK/server" ./cmd/server
fi

"$WORK/server" -addr "$ADDR" -dir web >"$WORK/server.log" 2>&1 &
SERVER_PID=$!

ready=0

for _ in $(seq 1 100); do
    if curl -fsS "http://${ADDR}/" >/dev/null 2>&1; then
        ready=1
        break
    fi
    sleep 0.1
done

[[ "$ready" == 1 ]] || fail "the server did not answer on ${ADDR}"

common=(
    --headless=new
    --no-sandbox
    --disable-gpu
    --hide-scrollbars
    --user-data-dir="$WORK/profile"
    --virtual-time-budget="$BUDGET"
)

#
# A headless browser on a busy machine can hit the virtual-time budget before
# both wasm modules have compiled, which looks exactly like a broken link. Retry
# rather than reporting a defect the code does not have.
#
check_dom() {
    grep -q '>hello world<' "$1" \
        && grep -q 'id="worker-status" class="pill state-ok">verified<' "$1" \
        && grep -q 'seq 3' "$1" \
        && grep -q 'data-hello-verified="true"' "$1" \
        && grep -q 'data-boot-complete="true"' "$1" \
        && grep -q '<div id="boot"[^>]*hidden' "$1"
}

attempt=0
ok=0

while [[ "$attempt" -lt 3 ]]; do
    attempt=$((attempt + 1))

    if "$BROWSER" "${common[@]}" --enable-logging=stderr --v=0 \
        --dump-dom "$URL" >"$WORK/dom.html" 2>"$WORK/console.log"; then
        #
        # A message that is not an envelope must never reach the link: the Go
        # side would refuse to decode it and log exactly this. It happened once
        # when the worker's bootstrap posted a raw debug line on the protocol
        # channel, so it is checked rather than assumed.
        #
        if check_dom "$WORK/dom.html" \
            && ! grep -qE 'decode envelope|envelope has no kind|dropped a non-envelope' "$WORK/console.log"; then
            ok=1
            break
        fi
    fi

    echo "note: attempt $attempt did not see a clean, verified page, retrying" >&2
    sleep 0.5
done

[[ "$ok" == 1 ]] || fail "the worker's greeting did not reach the page after $attempt attempts"

"$BROWSER" "${common[@]}" --window-size=980,720 \
    --screenshot="$SHOT" "$URL" >/dev/null 2>>"$WORK/browser.log" \
    || echo "note: app screenshot failed (the DOM check is what gates this run)"

#
# Capture the boot overlay itself. The overlay is plain HTML and is driven by
# page.js, so a copy of the app without its scripts shows exactly what a user
# sees during the wasm compile. Taking the screenshot from the live app would
# race the compile instead of showing it.
#
BOOT_ADDR="127.0.0.1:$((PORT + 1))"
mkdir -p "$WORK/boot-static"
cp web/index.html web/style.css "$WORK/boot-static/"

"$WORK/server" -addr "$BOOT_ADDR" -dir "$WORK/boot-static" >"$WORK/boot-server.log" 2>&1 &
STATIC_PID=$!

for _ in $(seq 1 100); do
    if curl -fsS "http://${BOOT_ADDR}/" >/dev/null 2>&1; then
        break
    fi
    sleep 0.1
done

"$BROWSER" --headless=new --no-sandbox --disable-gpu --hide-scrollbars \
    --user-data-dir="$WORK/profile-boot" --virtual-time-budget=1500 \
    --window-size=980,720 --screenshot="$BOOT_SHOT" "http://${BOOT_ADDR}/" \
    >/dev/null 2>>"$WORK/browser.log" \
    || echo "note: boot screenshot failed (the DOM check is what gates this run)"

kill "$STATIC_PID" 2>/dev/null || true
wait "$STATIC_PID" 2>/dev/null || true
STATIC_PID=""

echo "PASS: the app opened, the greeting rendered, and the boot gate cleared"
echo "      greeting: hello world"
echo "      round trips: 3 (opening hello + two UI pings)"
echo "      boot gate: cleared (data-boot-complete=true, overlay hidden)"

if [[ -f "$SHOT" ]]; then
    echo "      app screenshot:  $SHOT"
fi

if [[ -f "$BOOT_SHOT" ]]; then
    echo "      boot screenshot: $BOOT_SHOT"
fi
