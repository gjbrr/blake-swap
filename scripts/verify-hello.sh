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
#   * the document carries data-hello-verified="true".
#
# A screenshot is written next to the app so the result can be looked at, not
# only grepped.
#
# Exit status: 0 pass, 1 fail, 77 skipped (no browser).

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PORT="${VERIFY_PORT:-8799}"
ADDR="127.0.0.1:${PORT}"
URL="http://${ADDR}/?autoping=2"
BUDGET="${VERIFY_BUDGET:-20000}"
SHOT="$ROOT/web/hello-screenshot.png"

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

cleanup() {
    if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
        kill "$SERVER_PID" 2>/dev/null || true
        wait "$SERVER_PID" 2>/dev/null || true
    fi

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

    if [[ -s "$WORK/browser.log" ]]; then
        echo "--- browser log ---" >&2
        tail -20 "$WORK/browser.log" >&2
    fi

    exit 1
}

GO="${GO:-go}"

"$GO" build -o "$WORK/server" ./cmd/server

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
        && grep -q 'data-hello-verified="true"' "$1"
}

attempt=0
ok=0

while [[ "$attempt" -lt 3 ]]; do
    attempt=$((attempt + 1))

    if "$BROWSER" "${common[@]}" --dump-dom "$URL" >"$WORK/dom.html" 2>"$WORK/browser.log"; then
        if check_dom "$WORK/dom.html"; then
            ok=1
            break
        fi
    fi

    echo "note: attempt $attempt did not see a verified page, retrying" >&2
    sleep 0.5
done

[[ "$ok" == 1 ]] || fail "the worker's greeting did not reach the page after $attempt attempts"

"$BROWSER" "${common[@]}" --window-size=980,720 \
    --screenshot="$SHOT" "$URL" >/dev/null 2>>"$WORK/browser.log" \
    || echo "note: screenshot failed (the DOM check is what gates this run)"

echo "PASS: the page rendered the worker's greeting"
echo "      greeting: hello world"
echo "      round trips: 3 (opening hello + two UI pings)"

if [[ -f "$SHOT" ]]; then
    echo "      screenshot: $SHOT"
fi
